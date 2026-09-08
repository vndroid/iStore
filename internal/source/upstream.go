package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Upstream fetches source objects over HTTP from one fixed origin.
//
// The origin is configuration, never anything the caller can influence: a
// request's path is appended to a base URL fixed at startup. That is the whole
// security argument for this mode. imgproxy takes the source URL from the
// request and therefore needs a host allowlist to stay out of a cloud metadata
// service; iStore cannot be pointed anywhere, so the only way back to that
// problem is following a redirect, which is why this does not follow them.
//
// What the caller does control is the path, so the path is normalised before it
// is joined: `..` is collapsed against a rooted path, which cannot climb above
// the base's own prefix. Without that, ISTORE_UPSTREAM="http://origin/images"
// would still serve http://origin/etc/secrets to anyone who asked for
// /../etc/secrets.
type Upstream struct {
	base    *url.URL
	client  *http.Client
	timeout time.Duration
}

var _ Source = (*Upstream)(nil)

// NewUpstream validates a base URL and returns a Source that reads from it.
//
// timeout bounds one fetch end to end, separately from the per-request
// processing timeout: a slow origin should not be able to spend the budget that
// exists for encoding.
func NewUpstream(base string, timeout time.Duration) (*Upstream, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return nil, fmt.Errorf("source: upstream %q: %w", base, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("source: upstream %q must be an http:// or https:// URL", base)
	case u.Host == "":
		return nil, fmt.Errorf("source: upstream %q has no host", base)
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("source: upstream %q must not carry a query string", base)
	case u.Fragment != "":
		return nil, fmt.Errorf("source: upstream %q must not carry a fragment", base)
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// A copy, so the caller cannot mutate the base after construction.
	b := *u
	b.Path = path.Clean("/" + strings.Trim(b.Path, "/"))
	if b.Path == "/" {
		b.Path = ""
	}
	b.RawPath = ""

	return &Upstream{
		base:    &b,
		timeout: timeout,
		client: &http.Client{
			// Redirects are not followed, deliberately. Following one hands the
			// choice of destination back to the origin, which is the SSRF this
			// mode otherwise does not have. A 3xx is reported as a failed fetch.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}, nil
}

// Describe implements Source. Any credentials in the base URL are redacted:
// this string goes to the startup log.
func (u *Upstream) Describe() string { return u.base.Redacted() }

// target joins a request path onto the base URL.
func (u *Upstream) target(urlPath string) (*url.URL, error) {
	if strings.ContainsRune(urlPath, 0) {
		return nil, ErrOutsideRoot
	}

	// Rooting the path before cleaning is what makes `..` harmless: path.Clean
	// on "/a/../../b" is "/b", never "/../b", so the join below cannot climb out
	// of the base's prefix.
	clean := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))

	t := *u.base
	t.Path = u.base.Path + clean
	// Leave RawPath unset so URL.String re-escapes from Path. net/http handed us
	// a decoded path; re-encoding it here is what puts spaces and non-ASCII back
	// on the wire correctly.
	t.RawPath = ""

	return &t, nil
}

// Stat asks the origin for the object's identity without its body.
//
// HEAD is the right request for it, and on a cache hit it is the only request
// the whole transform costs. Not every origin implements HEAD, so a refusal
// falls back to a one-byte ranged GET, which carries the same headers.
func (u *Upstream) Stat(ctx context.Context, urlPath string) (*Info, error) {
	t, err := u.target(urlPath)
	if err != nil {
		return nil, err
	}

	resp, err := u.do(ctx, http.MethodHead, t, "")
	if err != nil {
		if !headUnsupported(err) {
			return nil, err
		}
		if resp, err = u.do(ctx, http.MethodGet, t, "bytes=0-0"); err != nil {
			return nil, err
		}
	}
	defer drainClose(resp.Body)

	info := infoFrom(t, resp)
	if resp.StatusCode == http.StatusPartialContent {
		if total, ok := totalFromContentRange(resp.Header.Get("Content-Range")); ok {
			info.Size = total
		} else {
			info.Size = -1
		}
	}
	return &info, nil
}

// headUnsupported reports the two statuses that mean "this origin does not do
// HEAD" rather than "this object is not available".
func headUnsupported(err error) bool {
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Status == http.StatusMethodNotAllowed || ue.Status == http.StatusNotImplemented
}

// OpenHeader fetches at most n leading bytes.
//
// This is the request that keeps `info` cheap. An origin that honours Range
// answers 206 with n bytes and a Content-Range naming the real length, so the
// whole endpoint costs one small response however large the image is. An origin
// that ignores Range answers 200 with the whole object; the body is capped at n
// and closed, so the transfer is aborted rather than pulled down in full.
func (u *Upstream) OpenHeader(ctx context.Context, urlPath string, n int) (*Object, error) {
	if n <= 0 || n > HeaderWindow {
		n = HeaderWindow
	}

	t, err := u.target(urlPath)
	if err != nil {
		return nil, err
	}

	resp, err := u.do(ctx, http.MethodGet, t, fmt.Sprintf("bytes=0-%d", n-1))
	if err != nil {
		// 416 means the origin took the range and disliked it, which happens on
		// a zero-length object. Ask again without one so the caller gets the
		// real answer — an empty body, and then "not an image" — rather than a
		// range error.
		var ue *UpstreamError
		if errors.As(err, &ue) && ue.Status == http.StatusRequestedRangeNotSatisfiable {
			return u.Open(ctx, urlPath)
		}
		return nil, err
	}

	info := infoFrom(t, resp)

	if resp.StatusCode == http.StatusPartialContent {
		// Content-Range carries the object's real length; Content-Length here is
		// only the length of this slice of it.
		if total, ok := totalFromContentRange(resp.Header.Get("Content-Range")); ok {
			info.Size = total
		} else {
			info.Size = -1
		}
		o := newObject(info, resp.Body, resp.Body)
		o.ContentType = contentType(resp)
		o.Partial = info.Size < 0 || info.Size > int64(n)
		return o, nil
	}

	// 200: the origin ignored Range. Take the prefix and drop the rest.
	o := newObject(info, io.LimitReader(resp.Body, int64(n)), resp.Body)
	o.ContentType = contentType(resp)
	o.Partial = info.Size < 0 || info.Size > int64(n)
	return o, nil
}

// Open fetches the whole object.
func (u *Upstream) Open(ctx context.Context, urlPath string) (*Object, error) {
	t, err := u.target(urlPath)
	if err != nil {
		return nil, err
	}

	resp, err := u.do(ctx, http.MethodGet, t, "")
	if err != nil {
		return nil, err
	}

	o := newObject(infoFrom(t, resp), resp.Body, resp.Body)
	o.ContentType = contentType(resp)
	return o, nil
}

// do issues one request and turns everything that is not a usable response into
// an UpstreamError.
func (u *Upstream) do(ctx context.Context, method string, t *url.URL, rangeHdr string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, u.timeout)

	req, err := http.NewRequestWithContext(ctx, method, t.String(), nil)
	if err != nil {
		cancel()
		return nil, &UpstreamError{URL: t.String(), status: http.StatusBadGateway, Err: err}
	}
	req.Header.Set("User-Agent", "istore")
	req.Header.Set("Accept", "*/*")
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		cancel()
		return nil, transportError(t, err)
	}

	// The timeout has to outlive this function: it bounds the body read too, and
	// cancelling here would cut the transfer off mid-object. Closing the body
	// releases it.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	if err := statusError(t, resp); err != nil {
		drainClose(resp.Body)
		return nil, err
	}

	return resp, nil
}

// statusError maps the origin's status onto iStore's, per the rule that a
// status the origin produced is passed through and one iStore had to invent is
// a gateway error.
func statusError(t *url.URL, resp *http.Response) error {
	code := resp.StatusCode

	switch {
	case code == http.StatusOK || code == http.StatusPartialContent:
		return nil

	case code == http.StatusNotFound || code == http.StatusGone:
		// The one status with a meaning of its own here: the handler turns it
		// into the same 404 NoSuchKey a missing local file gets.
		return ErrNotFound

	case code >= 300 && code < 400:
		// Redirects are not followed, so a 3xx is "the origin did not give us
		// the object". Passing it through would be worse than a 502: the client
		// would follow it and fetch the unprocessed original.
		return &UpstreamError{URL: t.String(), Status: code, status: http.StatusBadGateway,
			Err: fmt.Errorf("origin redirected to %q and redirects are not followed",
				resp.Header.Get("Location"))}

	case code >= 200 && code < 300:
		// 204, 202 and friends: a success with nothing to process.
		return &UpstreamError{URL: t.String(), Status: code, status: http.StatusBadGateway,
			Err: fmt.Errorf("origin returned %d with no object", code)}

	default:
		// 4xx and 5xx go back to the client as the origin sent them.
		return &UpstreamError{URL: t.String(), Status: code, status: code,
			Err: fmt.Errorf("origin returned %d", code)}
	}
}

func transportError(t *url.URL, err error) error {
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		status = http.StatusGatewayTimeout
	}
	return &UpstreamError{URL: t.String(), status: status, Err: err}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// UpstreamError is a fetch the origin did not satisfy.
//
// It carries two statuses because they answer different questions. Status is
// what the origin said, which belongs in the log. StatusCode is what iStore
// answers with, which is the origin's own status wherever passing it through
// tells the client something true, and 502 or 504 where iStore had to invent
// one.
type UpstreamError struct {
	URL    string // the joined origin URL; internal, never sent to a client
	Status int    // the origin's status, 0 when no response arrived
	Err    error

	status int
}

func (e *UpstreamError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("source: upstream %s: %v", http.StatusText(e.Status), e.Err)
	}
	return fmt.Sprintf("source: upstream unreachable: %v", e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// StatusCode is what the HTTP layer answers with.
func (e *UpstreamError) StatusCode() int {
	if e.status == 0 {
		return http.StatusBadGateway
	}
	return e.status
}

// PublicMessage is deliberately vague about the origin. Which internal host
// iStore talks to, and what it said, is not the client's business; the operator
// gets both in the log.
func (e *UpstreamError) PublicMessage() string {
	switch e.StatusCode() {
	case http.StatusGatewayTimeout:
		return "the upstream origin did not respond in time"
	case http.StatusBadGateway:
		return "the upstream origin did not return a usable object"
	default:
		return fmt.Sprintf("the upstream origin returned %d", e.Status)
	}
}

// ---------------------------------------------------------------- header bits

func infoFrom(t *url.URL, resp *http.Response) Info {
	size := int64(-1)
	if v := resp.Header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			size = n
		}
	}
	return Info{
		Key:     t.String(),
		Size:    size,
		Version: version(resp),
	}
}

// version prefers the ETag and falls back to Last-Modified.
//
// Both are tagged, so an ETag whose value happens to look like an HTTP date
// cannot collide with a Last-Modified that says the same thing. An origin that
// sends neither yields "", and the cache falls back to a time bucket — see
// cache.Key.
func version(resp *http.Response) string {
	if v := strings.TrimSpace(resp.Header.Get("ETag")); v != "" {
		return "etag:" + v
	}
	if v := strings.TrimSpace(resp.Header.Get("Last-Modified")); v != "" {
		return "mtime:" + v
	}
	return ""
}

func contentType(resp *http.Response) string {
	v := resp.Header.Get("Content-Type")
	if v == "" {
		return ""
	}
	t, _, err := mime.ParseMediaType(v)
	if err != nil {
		return v
	}
	return t
}

// totalFromContentRange reads the object's full length out of a 206's
// `Content-Range: bytes 0-65535/12345678`. A `*` length means the origin is not
// saying.
func totalFromContentRange(v string) (int64, bool) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// drainClose reads a little of a body being discarded before closing it, so the
// connection can go back in the pool instead of being torn down. The cap keeps
// that from turning into a download of an error page.
func drainClose(rc io.ReadCloser) {
	io.CopyN(io.Discard, rc, 8<<10)
	rc.Close()
}

// cancelOnClose ties a request context's cancel to the body's Close, so the
// fetch timeout covers the body read without cutting it short.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

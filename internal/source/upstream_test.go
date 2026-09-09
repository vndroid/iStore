package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newUpstream(t *testing.T, h http.Handler) (*Upstream, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	u, err := NewUpstream(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return u, srv
}

func TestNewUpstreamRejectsUnusableBases(t *testing.T) {
	cases := []string{
		"",
		"localhost:3030",      // no scheme
		"ftp://origin/",       // not http
		"http://",             // no host
		"http://origin/?a=b",  // query
		"http://origin/#frag", // fragment
		"file:///var/www",     // not http
		"://origin",           // unparseable
	}
	for _, c := range cases {
		if _, err := NewUpstream(c, time.Second); err == nil {
			t.Errorf("NewUpstream(%q): expected refusal, got success", c)
		}
	}
}

func TestNewUpstreamAcceptsBaseWithPathPrefix(t *testing.T) {
	u, err := NewUpstream("http://origin:3030/images/", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := u.target("/a/b.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://origin:3030/images/a/b.jpg"; got.String() != want {
		t.Errorf("target = %q, want %q", got.String(), want)
	}
}

// The request path is the one thing about the outgoing URL a caller controls,
// so it is the one thing that has to be normalised. Without it a base with a
// path prefix is not a boundary at all.
func TestTargetCannotEscapeTheBasePrefix(t *testing.T) {
	u, err := NewUpstream("http://origin/images", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"/../secrets.txt",
		"/a/../../secrets.txt",
		"/../../../etc/passwd",
		"////../secrets.txt",
		"/a/./../../secrets.txt",
	}
	for _, p := range cases {
		got, err := u.target(p)
		if err != nil {
			continue // refused outright is also fine
		}
		if !strings.HasPrefix(got.Path, "/images/") && got.Path != "/images" {
			t.Errorf("target(%q) = %q, which is outside the base prefix", p, got.Path)
		}
	}
}

func TestTargetRejectsNUL(t *testing.T) {
	u, err := NewUpstream("http://origin", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.target("/a\x00b.jpg"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("got %v, want ErrOutsideRoot", err)
	}
}

// The path arrives decoded from net/http, so it has to be re-encoded on the way
// out or a filename with a space becomes a malformed request line.
func TestTargetReencodesThePath(t *testing.T) {
	u, err := NewUpstream("http://origin", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := u.target("/holiday photos/图片.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.String(), " ") {
		t.Errorf("target = %q, which has a raw space in it", got.String())
	}
	if got.Path != "/holiday photos/图片.jpg" {
		t.Errorf("decoded path = %q, want it unchanged", got.Path)
	}
}

// ------------------------------------------------------------------- fetching

const body = "0123456789abcdefghijklmnopqrstuvwxyz"

// rangeServer honours Range the way a static file server does.
func rangeServer(payload string, etag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		http.ServeContent(w, r, "x.bin", time.Unix(1700000000, 0), strings.NewReader(payload))
	})
}

func TestOpenHeaderUsesRangeWhenTheOriginHonoursIt(t *testing.T) {
	u, _ := newUpstream(t, rangeServer(body, `"v1"`))

	obj, err := u.OpenHeader(context.Background(), "/x.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	if !obj.Partial {
		t.Error("Partial = false, want true: only a prefix was requested")
	}
	// Content-Range carries the whole object's length, not the slice's.
	if obj.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", obj.Size, len(body))
	}
	if obj.Version != `etag:"v1"` {
		t.Errorf("Version = %q, want the ETag", obj.Version)
	}

	b, err := obj.ReadAll(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != body[:8] {
		t.Errorf("body = %q, want %q", b, body[:8])
	}
}

// An origin that ignores Range answers 200 with everything. The fetch must take
// its prefix and stop, not pull the whole object down to read a header.
func TestOpenHeaderTruncatesAnOriginThatIgnoresRange(t *testing.T) {
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	}))

	obj, err := u.OpenHeader(context.Background(), "/x.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	if !obj.Partial {
		t.Error("Partial = false, want true")
	}
	if obj.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", obj.Size, len(body))
	}

	b, err := obj.ReadAll(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != body[:8] {
		t.Errorf("body = %q, want the first 8 bytes only", b)
	}
}

// An object shorter than the window is complete, and saying so is what stops
// the info handler from making a second request for the rest of nothing.
func TestOpenHeaderMarksAShortObjectComplete(t *testing.T) {
	u, _ := newUpstream(t, rangeServer("tiny", ""))

	obj, err := u.OpenHeader(context.Background(), "/x.bin", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	if obj.Partial {
		t.Error("Partial = true, want false: the whole object arrived")
	}
	if obj.Size != 4 {
		t.Errorf("Size = %d, want 4", obj.Size)
	}
}

func TestPeekLeavesTheBytesForReadAll(t *testing.T) {
	u, _ := newUpstream(t, rangeServer(body, ""))

	obj, err := u.Open(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	head, err := obj.Peek(4)
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != body[:4] {
		t.Fatalf("Peek = %q, want %q", head, body[:4])
	}

	b, err := obj.ReadAll(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != body {
		t.Errorf("ReadAll after Peek = %q, want the whole object", b)
	}
}

func TestReadAllRefusesPastTheLimit(t *testing.T) {
	u, _ := newUpstream(t, rangeServer(body, ""))

	obj, err := u.Open(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	if _, err := obj.ReadAll(8); !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

// The bound has to hold even when the origin declares no length at all, which
// is the case a Content-Length check alone would wave straight through.
func TestReadAllBoundsAChunkedOrigin(t *testing.T) {
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		for range 100 {
			io.WriteString(w, body)
			w.(http.Flusher).Flush()
		}
	}))

	obj, err := u.Open(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	if obj.Size != -1 {
		t.Errorf("Size = %d, want -1 for an origin that declared none", obj.Size)
	}
	if _, err := obj.ReadAll(64); !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

// --------------------------------------------------------------------- status

func TestStatusMapping(t *testing.T) {
	tests := []struct {
		name     string
		origin   int
		location string
		want     int  // what iStore answers with
		notFound bool // ... unless it collapses to ErrNotFound
	}{
		{name: "404 is a miss", origin: 404, notFound: true},
		{name: "410 is a miss", origin: 410, notFound: true},
		{name: "403 passes through", origin: 403, want: 403},
		{name: "401 passes through", origin: 401, want: 401},
		{name: "429 passes through", origin: 429, want: 429},
		{name: "500 passes through", origin: 500, want: 500},
		{name: "503 passes through", origin: 503, want: 503},
		// Redirects are not followed, so there is no object; passing the 3xx to
		// the client would send it to fetch the unprocessed original.
		{name: "302 becomes 502", origin: 302, location: "http://elsewhere/x", want: 502},
		{name: "301 becomes 502", origin: 301, location: "http://elsewhere/x", want: 502},
		{name: "204 becomes 502", origin: 204, want: 502},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.location != "" {
					w.Header().Set("Location", tt.location)
				}
				w.WriteHeader(tt.origin)
			}))

			_, err := u.Open(context.Background(), "/x.bin")
			if err == nil {
				t.Fatal("expected a failure")
			}

			if tt.notFound {
				if !errors.Is(err, ErrNotFound) {
					t.Errorf("got %v, want ErrNotFound", err)
				}
				return
			}

			var ue *UpstreamError
			if !errors.As(err, &ue) {
				t.Fatalf("got %v, want an *UpstreamError", err)
			}
			if ue.StatusCode() != tt.want {
				t.Errorf("StatusCode() = %d, want %d", ue.StatusCode(), tt.want)
			}
			if ue.Status != tt.origin {
				t.Errorf("Status = %d, want the origin's %d", ue.Status, tt.origin)
			}
			if ue.PublicMessage() == "" {
				t.Error("PublicMessage() is empty")
			}
		})
	}
}

func TestUnreachableOriginIs502(t *testing.T) {
	// Port 1 on the loopback refuses immediately on every platform CI runs on.
	u, err := NewUpstream("http://127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, err = u.Open(context.Background(), "/x.bin")
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v, want an *UpstreamError", err)
	}
	if ue.StatusCode() != http.StatusBadGateway {
		t.Errorf("StatusCode() = %d, want 502", ue.StatusCode())
	}
	if ue.Status != 0 {
		t.Errorf("Status = %d, want 0: there was no response", ue.Status)
	}
}

func TestSlowOriginIs504(t *testing.T) {
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		io.WriteString(w, body)
	}))
	u.timeout = 50 * time.Millisecond

	_, err := u.Open(context.Background(), "/x.bin")
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v, want an *UpstreamError", err)
	}
	if ue.StatusCode() != http.StatusGatewayTimeout {
		t.Errorf("StatusCode() = %d, want 504", ue.StatusCode())
	}
}

// Following a redirect would hand the choice of destination back to the origin,
// which is the SSRF this mode otherwise does not have.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		io.WriteString(w, body)
	}))
	defer target.Close()

	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x.bin", http.StatusFound)
	}))

	if _, err := u.Open(context.Background(), "/x.bin"); err == nil {
		t.Fatal("expected a failure")
	}
	if reached {
		t.Error("the redirect was followed")
	}
}

// ----------------------------------------------------------------------- stat

func TestStatUsesHEAD(t *testing.T) {
	var method string
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.Header().Set("ETag", `"v7"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
	}))

	info, err := u.Stat(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodHead {
		t.Errorf("method = %s, want HEAD", method)
	}
	if info.Version != `etag:"v7"` {
		t.Errorf("Version = %q", info.Version)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", info.Size, len(body))
	}
}

func TestStatFallsBackWhenHEADIsRefused(t *testing.T) {
	var methods []string
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		http.ServeContent(w, r, "x.bin", time.Time{}, strings.NewReader(body))
	}))

	info, err := u.Stat(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != http.MethodHead || methods[1] != http.MethodGet {
		t.Fatalf("methods = %v, want a HEAD then a GET", methods)
	}
	// The ranged fallback must still report the object's real length, not the
	// one byte it asked for — the cache key is built from it.
	if info.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", info.Size, len(body))
	}
	if !strings.HasPrefix(info.Version, "mtime:") {
		t.Errorf("Version = %q, want the Last-Modified fallback", info.Version)
	}
}

func TestVersionIsEmptyWithoutAValidator(t *testing.T) {
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Last-Modified"] = nil
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
	}))

	info, err := u.Stat(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "" {
		t.Errorf("Version = %q, want empty so the caller falls back to a TTL", info.Version)
	}
}

func TestDescribeRedactsCredentials(t *testing.T) {
	u, err := NewUpstream("http://alice:hunter2@origin:3030/img", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u.Describe(), "hunter2") {
		t.Errorf("Describe() = %q, which leaks the password into the log", u.Describe())
	}
}

// ----------------------------------------------------------------- redaction

// UpstreamError.URL exists to be logged, and ISTORE_UPSTREAM may carry
// credentials for the origin. net/http redacts the URL in its own *url.Error;
// the copy iStore keeps has to do the same, or the password lands in a WARN
// line on every failed fetch.
func TestUpstreamErrorRedactsCredentials(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		u, err := NewUpstream("http://alice:hunter2@127.0.0.1:1", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = u.Open(context.Background(), "/x.bin")
		assertNoSecret(t, err)
	})

	t.Run("status error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		u, err := NewUpstream(strings.Replace(srv.URL, "http://", "http://alice:hunter2@", 1), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = u.Open(context.Background(), "/x.bin")
		assertNoSecret(t, err)
	})

	t.Run("cache key", func(t *testing.T) {
		srv := httptest.NewServer(rangeServer(body, `"v1"`))
		defer srv.Close()

		u, err := NewUpstream(strings.Replace(srv.URL, "http://", "http://alice:hunter2@", 1), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		info, err := u.Stat(context.Background(), "/x.bin")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(info.Key, "hunter2") {
			t.Errorf("Info.Key = %q, which carries the password", info.Key)
		}
	})
}

func assertNoSecret(t *testing.T, err error) {
	t.Helper()

	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v, want an *UpstreamError", err)
	}
	if strings.Contains(ue.URL, "hunter2") {
		t.Errorf("UpstreamError.URL = %q, which carries the password", ue.URL)
	}
	if strings.Contains(ue.Error(), "hunter2") {
		t.Errorf("Error() = %q, which carries the password", ue.Error())
	}
	if strings.Contains(ue.PublicMessage(), "hunter2") {
		t.Errorf("PublicMessage() carries the password")
	}
	// The username is kept: it is what makes a redacted URL useful in a log.
	if !strings.Contains(ue.URL, "alice") {
		t.Errorf("UpstreamError.URL = %q, want the username kept", ue.URL)
	}
}

// -------------------------------------------------------- body-phase failures

// The fetch timeout has to mean the same thing after the response headers as
// before them. It fires either way; what was missing is the classification, so
// a stalled body surfaced as an unrecognised read error rather than a 504.
func TestBodyReadTimeoutIsAnUpstreamError(t *testing.T) {
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "start")
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second)
	}))
	u.timeout = 200 * time.Millisecond

	obj, err := u.Open(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	_, err = obj.ReadAll(0)
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v (%T), want an *UpstreamError", err, err)
	}
	if ue.StatusCode() != http.StatusGatewayTimeout {
		t.Errorf("StatusCode() = %d, want 504", ue.StatusCode())
	}
}

// An origin that hangs up short of its own Content-Length did not give us the
// object. That is a gateway failure, not an internal one.
func TestTruncatedBodyIsAnUpstreamError(t *testing.T) {
	u, _ := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}))

	obj, err := u.Open(context.Background(), "/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	_, err = obj.ReadAll(0)
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v (%T), want an *UpstreamError", err, err)
	}
	if ue.StatusCode() != http.StatusBadGateway {
		t.Errorf("StatusCode() = %d, want 502", ue.StatusCode())
	}
}

// A configured URL may hold credentials, and a rejected configuration is the
// one place a secret gets printed repeatedly rather than once: the error goes
// to a fatal log line, the supervisor restarts the process, and it fails again.
// So no rejection may echo the input.
func TestStartupRefusalNamesNoSecret(t *testing.T) {
	const pw = "hunter2"
	const tok = "s3cr3t-token"

	cases := []struct {
		name string
		base string
	}{
		{"query string", "http://alice:" + pw + "@origin/images?token=" + tok},
		{"fragment", "http://alice:" + pw + "@origin/images#" + tok},
		{"unsupported scheme", "ftp://alice:" + pw + "@origin/images"},
		{"no host", "http://alice:" + pw + "@/images"},
		{"unparseable escape", "http://alice:" + pw + "@origin/%zz"},
		{"control character", "http://alice:" + pw + "@ori\x7fgin/"},
		{"secret in the path", "http://origin/" + tok + "?a=b"},
		{"empty", ""},
		{"no scheme", "localhost:3030"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewUpstream(c.base, time.Second)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			msg := err.Error()
			t.Logf("%s", msg)

			if strings.Contains(msg, pw) {
				t.Errorf("the password is in the error")
			}
			if strings.Contains(msg, tok) {
				// Redacted() would pass the password check and fail this one:
				// it replaces the password and keeps the query and path.
				t.Errorf("a query or path secret is in the error")
			}
			if strings.Contains(msg, "alice") {
				t.Errorf("the username is in the error")
			}
			if !strings.Contains(msg, "ISTORE_UPSTREAM") {
				t.Errorf("the error does not say which setting is wrong")
			}
		})
	}
}

// Saying nothing at all would be safe and useless. The host is what an operator
// needs to see to recognise their own typo.
func TestStartupRefusalNamesTheHost(t *testing.T) {
	_, err := NewUpstream("http://alice:hunter2@images.internal:8080/x?token=s3cr3t", time.Second)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "images.internal:8080") {
		t.Errorf("error = %q, want the host in it", err)
	}
}

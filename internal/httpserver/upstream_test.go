package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/source"
)

// newUpstreamServer wires a Server onto an origin, with the two upstream bounds
// spelled out so a test can drive either of them.
func newUpstreamServer(t *testing.T, origin string, maxSource, infoMax int64) *Server {
	t.Helper()

	s, err := New(
		Config{
			Upstream:             origin,
			MaxSourceBytes:       maxSource,
			UpstreamInfoMaxBytes: infoMax,
			UpstreamTimeout:      5 * time.Second,
			UpstreamTTL:          5 * time.Minute,
		},
		func(auximageprovider.Provider) (*processing.Processor, error) { return nil, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fileOrigin serves one directory the way a static file server does, Range
// included, and counts what it was asked for.
func fileOrigin(t *testing.T, dir string) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()

	var n atomic.Int32
	var ranges []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeFile(w, r, filepath.Join(dir, filepath.Clean("/"+r.URL.Path)))
	}))
	t.Cleanup(srv.Close)

	return srv, &n, &ranges
}

// ------------------------------------------------------------- configuration

func TestNewRequiresExactlyOneSource(t *testing.T) {
	newProc := func(auximageprovider.Provider) (*processing.Processor, error) { return nil, nil }

	if _, err := New(Config{}, newProc); err == nil {
		t.Error("New with neither Root nor Upstream: expected a refusal")
	}
	if _, err := New(Config{Root: t.TempDir(), Upstream: "http://origin"}, newProc); err == nil {
		t.Error("New with both Root and Upstream: expected a refusal")
	}
}

// ---------------------------------------------------------------------- info

// The whole reason info stays cheap against an origin: one ranged request,
// however large the image is. A regression here does not fail anything visible
// — the answer stays correct — it just quietly starts pulling whole files
// across the network, so the request count is the assertion.
func TestUpstreamInfoCostsOneRangedRequest(t *testing.T) {
	dir := t.TempDir()
	// Padded well past the 64 KiB header window, so a whole-object read would be
	// unmistakably more than a ranged one.
	b := testJPEG(t, 400, 267, 300<<10)
	if err := os.WriteFile(filepath.Join(dir, "big.jpg"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	origin, calls, ranges := fileOrigin(t, dir)
	s := newUpstreamServer(t, origin.URL, 100<<20, 10<<20)

	w := httptest.NewRecorder()
	s.serveInfo(w, httptest.NewRequest(http.MethodGet, "/big.jpg", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"ImageWidth":{"value":"400"}`) {
		t.Errorf("body = %s, want the real dimensions", w.Body)
	}
	// FileSize comes from Content-Range, not from what arrived.
	if !strings.Contains(w.Body.String(), `"FileSize":{"value":"307200"}`) {
		t.Errorf("body = %s, want the object's real length", w.Body)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("origin was called %d times, want 1", got)
	}
	if len(*ranges) != 1 || (*ranges)[0] == "" {
		t.Errorf("ranges = %q, want one ranged request", *ranges)
	}
}

// The bound that decision made explicit: in upstream mode the whole-object
// fallback — and only that fallback — is refused past UpstreamInfoMaxBytes,
// well below MaxSourceBytes. A BMP has no header walk, so it always takes it.
func TestUpstreamInfoRefusesOversizedFallback(t *testing.T) {
	dir := t.TempDir()
	bmpFile(t, dir, "big.bmp", 4096)

	origin, _, _ := fileOrigin(t, dir)
	s := newUpstreamServer(t, origin.URL, 100<<20, 1024)

	w := httptest.NewRecorder()
	s.serveInfo(w, httptest.NewRequest(http.MethodGet, "/big.bmp", nil))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "SourceTooLarge") {
		t.Errorf("body = %s, want a SourceTooLarge envelope", w.Body)
	}
}

// The same image is served, not refused, when the header window answers — which
// is the point of putting the bound on the fallback rather than on the request.
func TestUpstreamInfoServesLargeSourceItCanAnswerFromTheHeader(t *testing.T) {
	dir := t.TempDir()
	b := testJPEG(t, 400, 267, 300<<10)
	if err := os.WriteFile(filepath.Join(dir, "big.jpg"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	origin, _, _ := fileOrigin(t, dir)
	s := newUpstreamServer(t, origin.URL, 100<<20, 1024) // 1 KiB fallback bound

	w := httptest.NewRecorder()
	s.serveInfo(w, httptest.NewRequest(http.MethodGet, "/big.jpg", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
}

// ------------------------------------------------------------------ original

func TestUpstreamOriginalIsPassedThrough(t *testing.T) {
	dir := t.TempDir()
	b := testJPEG(t, 32, 32, 0)
	if err := os.WriteFile(filepath.Join(dir, "x.jpg"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	origin, _, _ := fileOrigin(t, dir)
	s := newUpstreamServer(t, origin.URL, 100<<20, 10<<20)

	w := httptest.NewRecorder()
	s.serveOriginal(w, httptest.NewRequest(http.MethodGet, "/x.jpg", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	if w.Body.Len() != len(b) {
		t.Errorf("body is %d bytes, want %d", w.Body.Len(), len(b))
	}
}

// ---------------------------------------------------------------- status map

func TestUpstreamStatusReachesTheClient(t *testing.T) {
	tests := []struct {
		origin int
		want   int
		code   string
	}{
		{origin: 404, want: 404, code: "NoSuchKey"},
		{origin: 403, want: 403, code: "UpstreamError"},
		{origin: 429, want: 429, code: "UpstreamError"},
		{origin: 500, want: 500, code: "UpstreamError"},
		{origin: 503, want: 503, code: "UpstreamError"},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.origin), func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.origin)
			}))
			defer origin.Close()

			s := newUpstreamServer(t, origin.URL, 100<<20, 10<<20)

			w := httptest.NewRecorder()
			s.serveInfo(w, httptest.NewRequest(http.MethodGet, "/x.jpg", nil))

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.want, w.Body)
			}
			if !strings.Contains(w.Body.String(), tt.code) {
				t.Errorf("body = %s, want a %s envelope", w.Body, tt.code)
			}
			// Whatever else it says, it must not name the origin.
			if strings.Contains(w.Body.String(), "127.0.0.1") {
				t.Errorf("body = %s, which leaks the origin address", w.Body)
			}
		})
	}
}

// ----------------------------------------------------------------- cache key

func TestCacheKeyPrefersTheOriginValidator(t *testing.T) {
	s := newUpstreamServer(t, "http://origin", 100<<20, 10<<20)

	a := s.cacheKey(&source.Info{Key: "http://origin/x.jpg", Size: 10, Version: `etag:"v1"`}, "c")
	b := s.cacheKey(&source.Info{Key: "http://origin/x.jpg", Size: 10, Version: `etag:"v2"`}, "c")

	if a.SourceVersion != `etag:"v1"` {
		t.Errorf("SourceVersion = %q, want the ETag", a.SourceVersion)
	}
	if a.Hash() == b.Hash() {
		t.Error("two ETags produced the same key: a replaced object would be served stale forever")
	}
}

// With no ETag and no Last-Modified there is nothing to key on, so the key
// carries a time bucket instead. That is the TTL: same bucket, same key; next
// bucket, a miss that goes back to the origin.
func TestCacheKeyFallsBackToATimeBucket(t *testing.T) {
	s := newUpstreamServer(t, "http://origin", 100<<20, 10<<20)

	info := &source.Info{Key: "http://origin/x.jpg", Size: 10}

	a := s.cacheKey(info, "c")
	b := s.cacheKey(info, "c")
	if a.Hash() != b.Hash() {
		t.Error("two calls in the same bucket produced different keys")
	}
	if !strings.HasPrefix(a.SourceVersion, "ttl:") {
		t.Errorf("SourceVersion = %q, want a ttl: bucket", a.SourceVersion)
	}

	// A one-nanosecond TTL puts every call in its own bucket, which is the same
	// mechanism a five-minute one uses, just observable inside a test.
	s.cfg.UpstreamTTL = 1
	c := s.cacheKey(info, "c")
	time.Sleep(time.Millisecond)
	d := s.cacheKey(info, "c")
	if c.Hash() == d.Hash() {
		t.Error("the bucket did not advance: an expired entry would never be refetched")
	}
}

// A local deployment's keys must be untouched by any of this, or upgrading
// throws away a warm cache for nothing.
func TestLocalCacheKeyIsUnchanged(t *testing.T) {
	s := newLimitedServer(t, t.TempDir(), 1024)

	k := s.cacheKey(&source.Info{Key: "/srv/img/x.jpg", Size: 10, Version: "1700000000"}, "c")

	if k.SourceVersion != "" {
		t.Errorf("SourceVersion = %q, want empty for a local source", k.SourceVersion)
	}
	if k.SourceMod != 1700000000 {
		t.Errorf("SourceMod = %d, want the mtime", k.SourceMod)
	}
}

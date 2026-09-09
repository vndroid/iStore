package httpserver

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/source"
)

// Regression tests for five defects found in review of the upstream mode. Each
// one failed before its fix; the comment on each says what went wrong, because
// a test that only asserts the good outcome does not explain why anyone bothered.

// ---------------------------------------------------------------------------
// A password in ISTORE_UPSTREAM used to reach the log, in cleartext, on every
// failed fetch — WARN level, so on by default, and repeated for as long as the
// origin was unhappy. net/http redacted its own copy of the URL in the same log
// line; iStore's copy was the one that leaked.
func TestUpstreamFailureLogsNoCredentials(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	// Port 1 refuses instantly.
	s := newUpstreamServer(t, "http://alice:hunter2@127.0.0.1:1", 100<<20, 10<<20)

	w := httptest.NewRecorder()
	s.serveInfo(w, httptest.NewRequest(http.MethodGet, "/x.jpg", nil))

	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("the password reached the log:\n%s", buf.String())
	}
	if strings.Contains(w.Body.String(), "hunter2") || strings.Contains(w.Body.String(), "127.0.0.1:1") {
		t.Errorf("the response names the origin: %s", w.Body)
	}
	if !strings.Contains(buf.String(), "alice") {
		t.Errorf("the username was dropped too, which makes the log useless:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// The read path used to release the lock and *then* take its reference. An
// expiry landing in that window dropped the map's reference to zero, freed the
// image, and the waiting request panicked in Ref. -race never saw it: the
// refcount is atomic and the map was locked throughout, so there was no data
// race to find — only a use-after-free reached through correct synchronisation.
//
// The steps below are the ones Get performs, in order, with the scheduler pause
// made explicit.
func TestWatermarkReferenceSurvivesAnExpiry(t *testing.T) {
	p := newTestProvider(t)
	p.ttl = 10 * time.Millisecond

	o := imageOptions("wm.png")

	// Populate; afterwards the map holds the only reference.
	d0, _, err := p.Get(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	d0.Close()

	// A request passes the freshness check and takes its reference.
	d := p.hit("wm.png", time.Now())
	if d == nil {
		t.Fatal("setup: the entry should be present and fresh")
	}

	// It is then descheduled past the TTL, and another request expires the
	// entry and installs a replacement.
	time.Sleep(20 * time.Millisecond)
	dW, _, err := p.Get(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	dW.Close()

	// The first request resumes and uses what it was given. Before the fix the
	// equivalent moment was a panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a handed-out watermark was freed underneath its holder: %v", r)
		}
	}()
	if _, err := d.Size(); err != nil {
		t.Fatalf("Size on a held reference: %v", err)
	}
	d.Close()
}

// The concurrent version. It cannot fail deterministically, so it is here to
// catch a reintroduction under load rather than to prove the invariant.
func TestWatermarkExpiryUnderConcurrency(t *testing.T) {
	p := newTestProvider(t)
	p.ttl = time.Millisecond

	o := imageOptions("wm.png")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					failures = append(failures, "panic")
					mu.Unlock()
				}
			}()
			d, _, err := p.Get(context.Background(), o)
			if err != nil || d == nil {
				return
			}
			time.Sleep(time.Millisecond)
			if _, err := d.Size(); err != nil {
				mu.Lock()
				failures = append(failures, err.Error())
				mu.Unlock()
			}
			d.Close()
		}()
		if i%25 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Errorf("%d requests failed on a freed watermark: %v", len(failures), failures[:min(3, len(failures))])
	}
}

// ---------------------------------------------------------------------------
// The TTL used to bound how stale the cache got and not how large it grew:
// an entry was only ever replaced when its own key was asked for a second time,
// so a key requested once stayed resident for the life of the process. The old
// argument for leaving it unbounded — the key is an object path, so the count is
// bounded by the objects that exist — held for a directory and does not hold for
// an origin, which can mint a valid image for any path.
func TestWatermarkCacheIsBounded(t *testing.T) {
	const n = maxWatermarks * 4

	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	names := make([]string, n)
	for i := range n {
		names[i] = "wm" + strconv.Itoa(i) + ".png"
		if err := os.WriteFile(filepath.Join(dir, names[i]), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	src, err := source.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("distinct keys, no expiry", func(t *testing.T) {
		p := newWatermarkProvider(src, 100<<20, time.Hour)
		defer p.Close()

		for _, name := range names {
			d, _, err := p.Get(context.Background(), imageOptions(name))
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
		}
		if got := p.size(); got > maxWatermarks {
			t.Errorf("map holds %d entries, cap is %d", got, maxWatermarks)
		}
	})

	t.Run("expired entries are swept", func(t *testing.T) {
		p := newWatermarkProvider(src, 100<<20, time.Nanosecond)
		defer p.Close()

		for _, name := range names {
			d, _, err := p.Get(context.Background(), imageOptions(name))
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
		}
		// Everything expires immediately, so the sweep on each insert should
		// leave at most the entry just added.
		if got := p.size(); got > 1 {
			t.Errorf("map holds %d expired entries, want them swept", got)
		}
	})

	t.Run("a local source still never expires", func(t *testing.T) {
		p := newWatermarkProvider(src, 100<<20, 0)
		defer p.Close()

		d, _, err := p.Get(context.Background(), imageOptions(names[0]))
		if err != nil {
			t.Fatal(err)
		}
		d.Close()

		p.mu.Lock()
		c := p.loaded[names[0]]
		p.mu.Unlock()
		if c.stale(time.Now().Add(100 * 365 * 24 * time.Hour)) {
			t.Error("a zero-TTL entry went stale")
		}
	})
}

// ---------------------------------------------------------------------------
// The fetch timeout was only classified on the response headers. A body that
// stalled mid-object still hit the deadline, but the error arrived as a bare
// context.DeadlineExceeded that the HTTP layer could not tell from any other
// read failure, so a stalled origin answered 500 InternalError instead of 504.
func TestStalledBodyIsAGatewayTimeout(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second)
	}))
	defer origin.Close()

	s, err := New(
		Config{
			Upstream: origin.URL, MaxSourceBytes: 100 << 20,
			UpstreamInfoMaxBytes: 10 << 20,
			UpstreamTimeout:      200 * time.Millisecond,
			UpstreamTTL:          time.Minute,
		},
		func(auximageprovider.Provider) (*processing.Processor, error) { return nil, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w := httptest.NewRecorder()
	s.serveAverageHue(w, httptest.NewRequest(http.MethodGet, "/x.jpg", nil))

	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504; body: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "UpstreamError") {
		t.Errorf("body = %s, want an UpstreamError envelope", w.Body)
	}
}

// ---------------------------------------------------------------------------
// A client HEAD of the untouched source used to open the whole object at the
// origin and throw every byte away. It now asks for a header window, which is
// enough to sniff the type and carries the real length in Content-Range.
func TestHeadDoesNotFetchTheWholeObject(t *testing.T) {
	dir := t.TempDir()
	full := testJPEG(t, 64, 64, 200<<10)
	if err := os.WriteFile(filepath.Join(dir, "x.jpg"), full, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var ranges []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		http.ServeFile(w, r, filepath.Join(dir, filepath.Clean("/"+r.URL.Path)))
	}))
	defer origin.Close()

	s := newUpstreamServer(t, origin.URL, 100<<20, 10<<20)

	w := httptest.NewRecorder()
	s.serveOriginal(w, httptest.NewRequest(http.MethodHead, "/x.jpg", nil))

	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("origin saw %d requests, want 1: %v", len(got), got)
	}
	if got[0] == "" {
		t.Errorf("the origin request was not ranged, so a HEAD still costs the whole object")
	}

	// The response must still be a correct HEAD: right type, right length, no body.
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(len(full)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(full))
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD returned %d bytes of body", w.Body.Len())
	}
}

// A GET is unchanged: still one request, still the whole object.
func TestGetStillFetchesTheWholeObject(t *testing.T) {
	dir := t.TempDir()
	full := testJPEG(t, 64, 64, 0)
	if err := os.WriteFile(filepath.Join(dir, "x.jpg"), full, 0o600); err != nil {
		t.Fatal(err)
	}
	origin, _, _ := fileOrigin(t, dir)
	s := newUpstreamServer(t, origin.URL, 100<<20, 10<<20)

	w := httptest.NewRecorder()
	s.serveOriginal(w, httptest.NewRequest(http.MethodGet, "/x.jpg", nil))

	if w.Code != http.StatusOK || w.Body.Len() != len(full) {
		t.Errorf("status %d, %d bytes; want 200 and %d", w.Code, w.Body.Len(), len(full))
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
}

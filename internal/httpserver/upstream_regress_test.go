package httpserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
	"sync/atomic"
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
		p := newWatermarkProvider(src, NewDefaultConfig(), time.Hour)
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
		p := newWatermarkProvider(src, NewDefaultConfig(), time.Nanosecond)
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
		p := newWatermarkProvider(src, NewDefaultConfig(), 0)
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

// ---------------------------------------------------------------------------
// Watermark budgets. The entry cap alone bounded how many watermarks were
// resident and nothing about what they weighed — 64 entries at the source
// image's 100 MiB limit came to 6.25 GiB — and no byte limit bounds the decode:
// a 440 KB PNG of a flat colour decodes to 144 MP and cost a measured 444 MB of
// RSS for one request, which the source image's own 250 MP budget waved through.

// solidPNG returns a PNG of side x side that compresses to almost nothing —
// small on the wire, enormous once decoded.
func solidPNG(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for y := range side {
		for x := range side {
			img.Set(x, y, color.RGBA{10, 20, 30, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisePNG returns a PNG of roughly approxBytes that does not compress away.
func noisePNG(t *testing.T, approxBytes int) []byte {
	t.Helper()
	side := 1
	for side*side*3 < approxBytes {
		side++
	}
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	seed := uint32(1)
	for y := range side {
		for x := range side {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{uint8(seed >> 16), uint8(seed >> 8), uint8(seed), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func watermarkDir(t *testing.T, files map[string][]byte) source.Source {
	t.Helper()
	dir := t.TempDir()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := source.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func heldBytes(p *watermarkProvider) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.usedBytes
}

// The pixel budget is the one that protects the process, and it has to be
// enforced from the header — refusing after the decode is refusing after the
// damage.
func TestWatermarkResolutionIsBounded(t *testing.T) {
	small := solidPNG(t, 1000) // 1 MP
	big := solidPNG(t, 4000)   // 16 MP
	t.Logf("1 MP is %d bytes on the wire; 16 MP is %d", len(small), len(big))

	src := watermarkDir(t, map[string][]byte{"small.png": small, "big.png": big})

	cfg := NewDefaultConfig()
	cfg.MaxWatermarkResolution = 4_000_000 // 4 MP
	p := newWatermarkProvider(src, cfg, 0)
	defer p.Close()

	d, _, err := p.Get(context.Background(), imageOptions("small.png"))
	if err != nil {
		t.Fatalf("1 MP watermark refused: %v", err)
	}
	d.Close()

	_, _, err = p.Get(context.Background(), imageOptions("big.png"))
	if !errors.Is(err, ErrWatermarkTooLarge) {
		t.Errorf("16 MP watermark: err = %v, want ErrWatermarkTooLarge", err)
	}
	if p.size() != 1 {
		t.Errorf("cache holds %d entries, want only the accepted one", p.size())
	}

	// The wire size is no defence here: the refused image is smaller than the
	// accepted one would be at any sane byte limit.
	if len(big) > 2<<20 {
		t.Errorf("fixture is %d bytes, too big to make the point", len(big))
	}
}

// The byte total, not just the entry count.
func TestWatermarkCacheBytesAreBounded(t *testing.T) {
	const each = 1 << 20
	blob := noisePNG(t, each)

	files := map[string][]byte{}
	names := make([]string, 32)
	for i := range names {
		names[i] = "wm" + strconv.Itoa(i) + ".png"
		files[names[i]] = blob
	}
	src := watermarkDir(t, files)

	cfg := NewDefaultConfig()
	cfg.WatermarkCacheBytes = 8 << 20 // room for ~8 of them
	p := newWatermarkProvider(src, cfg, 0)
	defer p.Close()

	for _, name := range names {
		d, _, err := p.Get(context.Background(), imageOptions(name))
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
	}

	held := heldBytes(p)
	t.Logf("%d x %d-byte watermarks -> %d entries, %d bytes held (budget %d)",
		len(names), len(blob), p.size(), held, cfg.WatermarkCacheBytes)

	if held > cfg.WatermarkCacheBytes {
		t.Errorf("held %d bytes, budget is %d", held, cfg.WatermarkCacheBytes)
	}
	if p.size() == 0 {
		t.Error("the cache evicted everything; the budget should hold several")
	}
}

// The byte accounting has to survive eviction, expiry and replacement, or the
// budget drifts until it stops binding.
func TestWatermarkByteAccountingStaysExact(t *testing.T) {
	blob := noisePNG(t, 256<<10)
	files := map[string][]byte{}
	names := make([]string, 20)
	for i := range names {
		names[i] = "wm" + strconv.Itoa(i) + ".png"
		files[names[i]] = blob
	}
	src := watermarkDir(t, files)

	cfg := NewDefaultConfig()
	cfg.WatermarkCacheBytes = 2 << 20
	p := newWatermarkProvider(src, cfg, 50*time.Millisecond)
	defer p.Close()

	check := func(stage string) {
		p.mu.Lock()
		var want int64
		for _, c := range p.loaded {
			want += c.size
		}
		got := p.usedBytes
		p.mu.Unlock()
		if got != want {
			t.Errorf("%s: usedBytes = %d, entries add up to %d", stage, got, want)
		}
	}

	for _, name := range names {
		d, _, err := p.Get(context.Background(), imageOptions(name))
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
	check("after LRU eviction")

	time.Sleep(80 * time.Millisecond) // everything expires
	d, _, err := p.Get(context.Background(), imageOptions(names[0]))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	check("after an expiry sweep")

	d, _, err = p.Get(context.Background(), imageOptions(names[0]))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	check("after a hit")

	p.Close()
	if got := heldBytes(p); got != 0 {
		t.Errorf("after Close, usedBytes = %d, want 0", got)
	}
}

// Concurrent first-time requests for one watermark used to be one download each.
func TestWatermarkConcurrentMissFetchesOnce(t *testing.T) {
	blob := noisePNG(t, 256<<10)

	var fetches atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		w.Write(blob)
	}))
	defer origin.Close()

	up, err := source.NewUpstream(origin.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p := newWatermarkProvider(up, NewDefaultConfig(), 5*time.Minute)
	defer p.Close()

	const n = 16
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, _, err := p.Get(context.Background(), imageOptions("wm.png"))
			if err != nil || d == nil {
				bad.Add(1)
				return
			}
			if _, err := d.Size(); err != nil {
				bad.Add(1)
			}
			d.Close()
		}()
	}
	wg.Wait()

	if got := bad.Load(); got != 0 {
		t.Errorf("%d of %d requests failed", got, n)
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("%d concurrent requests caused %d origin fetches, want 1", n, got)
	}
	if p.size() != 1 {
		t.Errorf("cache holds %d entries, want 1", p.size())
	}
}

// ---------------------------------------------------------------------------
// Three further defects, from a second review of the watermark budgets.

// The pixel budget used to be computed as w*h*frames in `int`. Every one of
// those comes from an image header, which is attacker-supplied: PNG stores all
// three as uint32, so 4294967295 x 4294967295 wraps to -8589934591 — under any
// limit — and the guard disappears exactly when it is needed.
func TestWatermarkPixelCountDoesNotOverflow(t *testing.T) {
	tests := []struct {
		name         string
		w, h, frames int
		wantOK       bool
		want         int64
	}{
		{"ordinary", 4000, 4000, 1, true, 16_000_000},
		{"animated", 1000, 1000, 60, true, 60_000_000},
		{"large but representable", 100000, 100000, 1, true, 10_000_000_000},
		{"uint32 squared overflows int64", 4294967295, 4294967295, 1, false, 0},
		{"frames push it over", 3037000500, 3037000500, 2, false, 0},
		{"zero width", 0, 100, 1, false, 0},
		{"negative height", 100, -1, 1, false, 0},
		{"zero frames", 100, 100, 0, false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pixelCount(tt.w, tt.h, tt.frames)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %d)", ok, tt.wantOK, got)
			}
			if ok && got != tt.want {
				t.Errorf("= %d, want %d", got, tt.want)
			}
		})
	}
}

// The same thing through the check that uses it: a crafted header must be
// refused, not waved through by a wrapped multiplication.
func TestWatermarkHeaderOverflowIsRefused(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()
	p.maxResolution = 8_000_000

	pngHeader := func(w, h uint32) []byte {
		b := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
		ihdr := make([]byte, 25)
		binary.BigEndian.PutUint32(ihdr[0:], 13)
		copy(ihdr[4:], "IHDR")
		binary.BigEndian.PutUint32(ihdr[8:], w)
		binary.BigEndian.PutUint32(ihdr[12:], h)
		ihdr[16] = 8
		ihdr[17] = 6
		return append(b, ihdr...)
	}

	for _, c := range []struct {
		name string
		w, h uint32
	}{
		{"honest oversize", 4000, 4000},
		{"crafted to overflow", 4294967295, 4294967295},
		{"crafted, one dimension maximal", 4294967295, 2147483648},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := p.checkResolution(pngHeader(c.w, c.h)); err == nil {
				t.Errorf("%dx%d passed an 8 MP limit", c.w, c.h)
			}
		})
	}

	// And an honest small one still passes.
	if err := p.checkResolution(pngHeader(100, 100)); err != nil {
		t.Errorf("100x100 refused: %v", err)
	}
}

// The pre-render estimate must never refuse text that would have fitted. The
// narrowest glyphs measured advance 0.24 of the font size; the estimate uses
// 0.2, so anything at or above that ratio has to survive it.
func TestWatermarkTextEstimateDoesNotOverRefuse(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()
	p.maxResolution = 8_000_000

	ok := []struct {
		text string
		size int
	}{
		{"hello", 40},
		{"© Example Corp 2026", 40},
		{strings.Repeat("i", 200), 40},
		{"图片水印测试", 100},
		{strings.Repeat("W", 20), 200},
		{"one\ntwo\nthree", 100},
		{strings.Repeat("W", 20), 1000}, // renders at 14 MP; the estimate must not be the thing that stops it
	}
	for _, c := range ok {
		if err := p.checkTextEstimate(watermarkSpec{text: c.text, size: c.size}); err != nil {
			t.Errorf("estimate refused %d runes at size %d: %v", len([]rune(c.text)), c.size, err)
		}
	}

	// And it must refuse what cannot possibly fit.
	tooBig := []struct {
		text string
		size int
	}{
		{strings.Repeat("W", 8000), 1000},
		{strings.Repeat("W", 400), 1000},
		{strings.Repeat("W\n", 5000), 1000},
	}
	for _, c := range tooBig {
		if err := p.checkTextEstimate(watermarkSpec{text: c.text, size: c.size}); !errors.Is(err, ErrWatermarkTooLarge) {
			t.Errorf("estimate accepted %d runes at size %d: %v", len([]rune(c.text)), c.size, err)
		}
	}
}

// A build slower than the TTL used to store an entry that was already expired,
// because the timestamp was taken before the build started. Every later request
// then missed and rebuilt, so the cache stopped working entirely — the opposite
// of what the coalescing was added for.
func TestWatermarkSlowBuildStillCaches(t *testing.T) {
	blob := noisePNG(t, 64<<10)

	var fetches atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		time.Sleep(150 * time.Millisecond) // longer than the TTL below
		w.Header().Set("Content-Type", "image/png")
		w.Write(blob)
	}))
	defer origin.Close()

	up, err := source.NewUpstream(origin.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p := newWatermarkProvider(up, NewDefaultConfig(), 50*time.Millisecond)
	defer p.Close()

	d, _, err := p.Get(context.Background(), imageOptions("wm.png"))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Immediately afterwards the entry must be usable: it was stored when the
	// build finished, not when the request started.
	if got := p.hit("wm.png", time.Now()); got == nil {
		t.Fatal("the entry a slow build stored is already expired")
	} else {
		got.Close()
	}

	d, _, err = p.Get(context.Background(), imageOptions("wm.png"))
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	if got := fetches.Load(); got != 1 {
		t.Errorf("%d origin fetches for two requests inside the TTL, want 1", got)
	}
}

// The same under a burst: a slow build plus a short TTL must not degrade into
// one download per waiter.
func TestWatermarkSlowBuildCoalescesTheBurst(t *testing.T) {
	blob := noisePNG(t, 64<<10)

	var fetches atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		w.Write(blob)
	}))
	defer origin.Close()

	up, err := source.NewUpstream(origin.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p := newWatermarkProvider(up, NewDefaultConfig(), 40*time.Millisecond)
	defer p.Close()

	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, _, err := p.Get(context.Background(), imageOptions("wm.png"))
			if err != nil || d == nil {
				bad.Add(1)
				return
			}
			d.Close()
		}()
	}
	wg.Wait()

	if got := bad.Load(); got != 0 {
		t.Errorf("%d of 12 requests failed", got)
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("%d origin fetches for one burst, want 1", got)
	}
}

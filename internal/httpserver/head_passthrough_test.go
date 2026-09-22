package httpserver

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/ossprocess"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/security"
	"github.com/vndroid/istore/internal/singleflight"
)

func TestRecoveredFlightPanicIsHTTP500(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).failProcess(w, httptest.NewRequest(http.MethodGet, "/x.jpg", nil),
		singleflight.PanicError{Value: "boom"})
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestProcessedHeadMissSkipsEncoding(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), testJPEG(t, 20, 20, 0), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Root: dir}, func(auximageprovider.Provider) (*processing.Processor, error) {
		// A HEAD that reaches the encoder would panic on the nil processor.
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	for _, tc := range []struct{ chain, mime string }{
		{"image/resize,w_10/format,png", "image/png"},
		{"image/resize,w_10", ""},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/a.jpg?x-oss-process="+tc.chain, nil))
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Fatalf("HEAD %q: status %d, body %q", tc.chain, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Content-Length"); got != "" {
			t.Errorf("HEAD %q: Content-Length = %q, want omitted", tc.chain, got)
		}
		if got := w.Header().Get("Content-Type"); got != tc.mime {
			t.Errorf("HEAD %q: Content-Type = %q, want %q", tc.chain, got, tc.mime)
		}
	}
}

func TestOriginalNonImagePolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.txt"), []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		s := &Server{cfg: Config{Root: dir}}
		var err error
		s.src, _, err = newSource(s.cfg)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.serveOriginal(w, httptest.NewRequest(method, "/page.txt", nil))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("strict %s: status = %d", method, w.Code)
		}

		s.cfg.PassthroughNonImages = true
		w = httptest.NewRecorder()
		s.serveOriginal(w, httptest.NewRequest(method, "/page.txt", nil))
		if w.Code != http.StatusOK {
			t.Errorf("passthrough %s: status = %d", method, w.Code)
		}
		if method == http.MethodGet && !strings.Contains(w.Body.String(), "not an image") {
			t.Errorf("passthrough GET lost the original bytes")
		}
	}
}

func TestUpstreamProcessedHeadMissUsesRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), testJPEG(t, 20, 20, 200<<10), 0600); err != nil {
		t.Fatal(err)
	}
	origin, _, ranges := fileOrigin(t, dir)
	s := newUpstreamServer(t, origin.URL, 100<<20, 10<<20)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodHead,
		"/a.jpg?x-oss-process=image/resize,w_10/format,png", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(*ranges) != 2 || (*ranges)[0] != "" || (*ranges)[1] == "" {
		t.Fatalf("origin requests must be HEAD then ranged GET; ranges = %v", *ranges)
	}
}

func TestUpstreamOriginalRejectsHTML(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>listing</html>"))
	}))
	defer origin.Close()
	s := newUpstreamServer(t, origin.URL, 100<<20, 10<<20)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, "/directory/", nil))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("upstream %s: status = %d, want 415", method, w.Code)
		}
	}
}

func newHeadCheckedServer(t *testing.T, root string) *Server {
	t.Helper()
	c := security.NewDefaultConfig()
	checker, err := security.New(&c)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Root: root, MaxSourceBytes: 100 << 20, HeaderChecker: checker},
		func(auximageprovider.Provider) (*processing.Processor, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProcessedHeadPreflightRejectsKnownBadHeaders(t *testing.T) {
	dir := t.TempDir()
	png := make([]byte, 33)
	copy(png, "\x89PNG\r\n\x1a\n")
	copy(png[12:], "IHDR")
	binary.BigEndian.PutUint32(png[16:], 20_000)
	binary.BigEndian.PutUint32(png[20:], 12_600)
	if err := os.WriteFile(filepath.Join(dir, "huge.png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.jpg"),
		[]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}, 0600); err != nil {
		t.Fatal(err)
	}
	s := newHeadCheckedServer(t, dir)
	for _, path := range []string{"/huge.png", "/broken.jpg"} {
		w := httptest.NewRecorder()
		chain, err := ossprocess.Parse("image/resize,w_1")
		if err != nil {
			t.Fatal(err)
		}
		s.serveProcessedHeadMiss(w, httptest.NewRequest(http.MethodHead, path, nil), chain, imagetype.Unknown)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: HEAD status = %d, want 422", path, w.Code)
		}
	}
}

func TestProcessedHeadKeepsLongButValidJPEGHeaderOptimistic(t *testing.T) {
	dir := t.TempDir()
	base := testJPEG(t, 20, 20, 0)
	app := make([]byte, 32760)
	app[0], app[1] = 0xff, 0xe2
	binary.BigEndian.PutUint16(app[2:], uint16(len(app)-2))
	long := append(append(append([]byte{}, base[:2]...), app...), base[2:]...)
	if err := os.WriteFile(filepath.Join(dir, "long.jpg"), long, 0600); err != nil {
		t.Fatal(err)
	}
	s := newHeadCheckedServer(t, dir)
	chain, _ := ossprocess.Parse("image/resize,w_10")
	w := httptest.NewRecorder()
	s.serveProcessedHeadMiss(w, httptest.NewRequest(http.MethodHead, "/long.jpg", nil), chain, imagetype.Unknown)
	if w.Code != http.StatusOK {
		t.Errorf("long JPEG header: HEAD status = %d, want optimistic 200", w.Code)
	}
}

func TestProcessedHeadRejectsKnownExcessFrames(t *testing.T) {
	dir := t.TempDir()
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{}
	for range 400 {
		frame := image.NewPaletted(image.Rect(0, 0, 1, 1), pal)
		g.Image = append(g.Image, frame)
		g.Delay = append(g.Delay, 1)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.gif"), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	s := newHeadCheckedServer(t, dir)
	chain, _ := ossprocess.Parse("image/resize,w_1/format,webp")
	w := httptest.NewRecorder()
	s.serveProcessedHeadMiss(w, httptest.NewRequest(http.MethodHead, "/many.gif", nil), chain, imagetype.Unknown)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("400-frame GIF: HEAD status = %d, want 422", w.Code)
	}
}

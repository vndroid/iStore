package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/processing"
)

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

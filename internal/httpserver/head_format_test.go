package httpserver

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/security"
)

// writeGIF writes a GIF of n one-pixel frames on a w×h logical screen. The
// frames are what the header walk counts; the screen is what the pixel budget
// multiplies. Both fit in a few KiB, well inside the HEAD's header window.
func writeGIF(t *testing.T, dir, name string, n, w, h int) {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{Config: image.Config{Width: w, Height: h, ColorModel: pal}}
	for i := range n {
		f := image.NewPaletted(image.Rect(0, 0, 1, 1), pal)
		f.SetColorIndex(0, 0, uint8(i%2))
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 4)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

// newFullServer is a server with the real processor, so GET and HEAD can be
// compared on the same request.
func newFullServer(t *testing.T, root string, edit func(*processing.Config)) *Server {
	return newFullServerWith(t, root, nil, edit)
}

// newFullServerWith also lets a test change the security limits. HEAD and GET
// share the one checker, as they do in cmd/istore.
func newFullServerWith(t *testing.T, root string, editSec func(*security.Config), edit func(*processing.Config)) *Server {
	t.Helper()
	sc := security.NewDefaultConfig()
	if editSec != nil {
		editSec(&sc)
	}
	checker, err := security.New(&sc)
	if err != nil {
		t.Fatal(err)
	}
	pc := processing.NewDefaultConfig()
	if edit != nil {
		edit(&pc)
	}
	s, err := New(Config{Root: root, MaxSourceBytes: 100 << 20, HeaderChecker: checker},
		func(p auximageprovider.Provider) (*processing.Processor, error) {
			return processing.New(&pc, checker, p)
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func status(s *Server, method, url, accept string) int {
	r := httptest.NewRequest(method, url, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Code
}

// A cold HEAD used to consult only a *named* output format, so a bare
// transform — which keeps a GIF animated — and format,auto both skipped the
// frame cap and the per-frame pixel budget that GET then enforced. HEAD and GET
// must now agree on every row, in both directions.
func TestProcessedHeadAgreesWithGetOnImplicitFormats(t *testing.T) {
	dir := t.TempDir()
	writeGIF(t, dir, "many.gif", 350, 4, 4)      // over the 300-frame cap
	writeGIF(t, dir, "ok.gif", 20, 4, 4)         // an ordinary animation
	writeGIF(t, dir, "wide.gif", 70, 2000, 2000) // 280 MP across its frames, 4 MP each
	s := newFullServer(t, dir, nil)

	for i, tc := range []struct {
		path, chain, accept string
		want                int
	}{
		{"/many.gif", "image/resize,w_3", "", http.StatusUnprocessableEntity},
		{"/many.gif", "image/format,auto", "image/webp", http.StatusUnprocessableEntity},
		{"/many.gif", "image/format,auto", "image/jpeg", http.StatusUnprocessableEntity},
		{"/many.gif", "image/format,webp", "", http.StatusUnprocessableEntity},
		{"/many.gif", "image/format,jpg", "", http.StatusOK},
		{"/many.gif", "image/format,png", "", http.StatusOK},
		{"/ok.gif", "image/resize,w_3", "", http.StatusOK},
		{"/ok.gif", "image/format,auto", "image/webp", http.StatusOK},
		{"/wide.gif", "image/resize,w_20", "", http.StatusUnprocessableEntity},
		{"/wide.gif", "image/format,jpg", "", http.StatusOK},
	} {
		// A distinct quality per row keeps every HEAD a cold miss.
		url := tc.path + "?x-oss-process=" + tc.chain + "/quality,q_" + strconv.Itoa(50+i)
		head := status(s, http.MethodHead, url, tc.accept)
		get := status(s, http.MethodGet, url, tc.accept)
		if head != tc.want || get != tc.want {
			t.Errorf("%s %s (Accept %q): HEAD %d, GET %d, want both %d",
				tc.path, tc.chain, tc.accept, head, get, tc.want)
		}
	}
}

// A format the operator passes through untouched is measured by GET only as
// the one frame it loaded to decide that: no frame cap, and a pixel budget of
// width × height. HEAD has to apply exactly that — refusing on the frame count
// would refuse what GET serves, and skipping the pixel check altogether would
// pass what GET refuses.
func TestProcessedHeadMeasuresSkippedFormatsAsOneFrame(t *testing.T) {
	dir := t.TempDir()
	writeGIF(t, dir, "many.gif", 350, 4, 4)      // over the frame cap, tiny frames
	writeGIF(t, dir, "big.gif", 1, 2000, 2000)   // one 4 MP frame
	writeGIF(t, dir, "wide.gif", 70, 2000, 2000) // 4 MP per frame, 280 MP in all
	s := newFullServerWith(t, dir, func(c *security.Config) {
		c.MaxSrcResolution = 5_000_000
	}, func(c *processing.Config) {
		c.SkipProcessingFormats = []imagetype.Type{imagetype.GIF}
	})
	for _, tc := range []struct {
		path, chain string
		want        int
	}{
		{"/many.gif", "image/resize,w_3", http.StatusOK},
		{"/many.gif", "image/format,gif", http.StatusOK},
		{"/wide.gif", "image/format,gif", http.StatusOK}, // 4 MP frame fits; the 280 MP total is not counted
		{"/big.gif", "image/format,gif", http.StatusOK},
	} {
		url := tc.path + "?x-oss-process=" + tc.chain
		head := status(s, http.MethodHead, url, "")
		get := status(s, http.MethodGet, url, "")
		if head != tc.want || get != tc.want {
			t.Errorf("skipped GIF %s %s: HEAD %d, GET %d, want both %d", tc.path, tc.chain, head, get, tc.want)
		}
	}

	// Now a budget the single frame itself exceeds.
	s = newFullServerWith(t, dir, func(c *security.Config) {
		c.MaxSrcResolution = 1_000_000
	}, func(c *processing.Config) {
		c.SkipProcessingFormats = []imagetype.Type{imagetype.GIF}
	})
	for _, chain := range []string{"image/resize,w_3", "image/format,gif"} {
		url := "/big.gif?x-oss-process=" + chain
		head := status(s, http.MethodHead, url, "")
		get := status(s, http.MethodGet, url, "")
		if head != http.StatusUnprocessableEntity || get != http.StatusUnprocessableEntity {
			t.Errorf("skipped 4 MP GIF over a 1 MP budget, %s: HEAD %d, GET %d, want both 422", chain, head, get)
		}
	}
}

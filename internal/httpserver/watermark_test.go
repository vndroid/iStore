package httpserver

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/ossprocess"
	"github.com/vndroid/istore/internal/source"
)

// newTestProvider returns a provider over a temp root holding one 4x4 PNG named
// wm.png.
func newTestProvider(t *testing.T) *watermarkProvider {
	t.Helper()

	dir := t.TempDir()

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wm.png"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	src, err := source.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}

	// No TTL: a local source's cached entries never expire, which is the
	// behaviour these tests are about.
	return newWatermarkProvider(src, 100<<20, 0)
}

func imageOptions(path string) *options.Options {
	o := options.New()
	o.Set(ossprocess.KeyWatermarkPath, path)
	return o
}

func textOptions(text string) *options.Options {
	o := options.New()
	o.Set(ossprocess.KeyWatermarkText, text)
	return o
}

func (p *watermarkProvider) size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.loaded)
}

func TestWatermarkSpecCacheable(t *testing.T) {
	tests := []struct {
		name string
		spec watermarkSpec
		want bool
	}{
		{"object key", watermarkSpec{path: "wm.png"}, true},
		{"text", watermarkSpec{text: "hello"}, false},
		{"both", watermarkSpec{path: "wm.png", text: "hello"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.cacheable(); got != tt.want {
				t.Errorf("cacheable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWatermarkImageIsCached is the reason the map exists: a site's one
// watermark is read off disk once, not per request.
func TestWatermarkImageIsCached(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()

	first, _, err := p.Get(context.Background(), imageOptions("wm.png"))
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	defer first.Close()

	second, _, err := p.Get(context.Background(), imageOptions("wm.png"))
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	defer second.Close()

	if first != second {
		t.Error("second Get returned a different instance; the image was rebuilt")
	}
	if got := p.size(); got != 1 {
		t.Errorf("cache holds %d entries, want 1", got)
	}
}

// TestWatermarkTextIsNotCached is the bound. The cache key for a text watermark
// would be made of request parameters, so caching one lets a caller grow the map
// with a loop of distinct strings until the process runs out of memory.
//
// The rendering itself needs a live libvips, which a unit test does not have, so
// each Get here fails — but failing or succeeding, nothing may be retained, and
// that is exactly what is asserted.
func TestWatermarkTextIsNotCached(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()

	for i := range 100 {
		d, _, err := p.Get(context.Background(), textOptions(fmt.Sprintf("attack-%d", i)))
		if err == nil {
			d.Close()
		}
	}

	if got := p.size(); got != 0 {
		t.Errorf("cache holds %d entries after 100 distinct texts, want 0", got)
	}
}

// TestWatermarkTextWithImageIsNotCached covers the mixed form: an object key
// plus text is still keyed on the text, so it is still uncacheable, even though
// the object-key half on its own would not be.
func TestWatermarkTextWithImageIsNotCached(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()

	for i := range 20 {
		o := imageOptions("wm.png")
		o.Set(ossprocess.KeyWatermarkText, fmt.Sprintf("attack-%d", i))

		d, _, err := p.Get(context.Background(), o)
		if err == nil {
			d.Close()
		}
	}

	if got := p.size(); got != 0 {
		t.Errorf("cache holds %d entries, want 0", got)
	}
}

func TestWatermarkNoneRequested(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()

	d, _, err := p.Get(context.Background(), options.New())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d != nil {
		t.Error("Get returned an image for a request with no watermark")
	}
}

func TestWatermarkMissingObjectKey(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()

	if _, _, err := p.Get(context.Background(), imageOptions("nope.png")); err != ErrBadWatermark {
		t.Errorf("Get = %v, want %v", err, ErrBadWatermark)
	}
	if got := p.size(); got != 0 {
		t.Errorf("cache holds %d entries after a failed build, want 0", got)
	}
}

// TestWatermarkTraversal: an object key is resolved through the same resolver as
// the image being served, so it cannot escape the root either.
//
// Note what a `..` key does: the resolver anchors at the root and cleans, so
// "../../etc/passwd" is the root's own etc/passwd, which does not exist. It is
// not an escape and never was — the assertion is that it stays inside.
func TestWatermarkTraversal(t *testing.T) {
	p := newTestProvider(t)
	defer p.Close()

	for _, key := range []string{"../../etc/passwd", "/etc/passwd", "../../../../../../etc/hosts"} {
		if _, _, err := p.Get(context.Background(), imageOptions(key)); err != ErrBadWatermark {
			t.Errorf("Get(%q) = %v, want %v", key, err, ErrBadWatermark)
		}
	}
}

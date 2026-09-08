package processing

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"os"
	"strings"
	"testing"

	"github.com/kane/istore/internal/imagedata"
	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/security"
	"github.com/kane/istore/internal/vips"
)

// The pipeline cannot be tested without libvips, so this package's tests bring
// one up. That is the price of testing the part of iStore that actually moves
// pixels.

func TestMain(m *testing.M) {
	c := vips.NewDefaultConfig()
	if err := vips.Init(&c); err != nil {
		panic(err)
	}
	code := m.Run()
	vips.Shutdown()
	os.Exit(code)
}

// animatedGIF encodes a GIF with the given number of distinct frames.
func animatedGIF(t *testing.T, w, h, frames int) imagedata.ImageData {
	t.Helper()

	pal := make(color.Palette, 0, 256)
	for i := range 256 {
		pal = append(pal, color.RGBA{R: uint8(i), G: uint8(255 - i), B: uint8(i * 7), A: 0xFF})
	}

	g := &gif.GIF{}
	for i := range frames {
		p := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		// Every frame differs from the last, so nothing downstream can merge
		// them and change the count for reasons of its own.
		for y := range h {
			for x := range w {
				p.SetColorIndex(x, y, uint8((x+y+i*3)%256))
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 4)
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}

	d, err := imagedata.NewFromBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newProcessor builds a Processor whose only non-default security setting is the
// animation frame cap.
func newProcessor(t *testing.T, maxFrames int) *Processor {
	t.Helper()

	sc := security.NewDefaultConfig()
	sc.MaxAnimationFrames = maxFrames

	checker, err := security.New(&sc)
	if err != nil {
		t.Fatal(err)
	}

	pc := NewDefaultConfig()
	proc, err := New(&pc, checker, nil)
	if err != nil {
		t.Fatal(err)
	}
	return proc
}

func processTo(t *testing.T, proc *Processor, src imagedata.ImageData, format imagetype.Type) (imagedata.ImageData, error) {
	t.Helper()

	o := options.New()
	o.Set(keys.Format, format)

	res, err := proc.ProcessImage(context.Background(), src, o)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { res.OutData.Close() })
	return res.OutData, nil
}

func pagesOf(t *testing.T, d imagedata.ImageData) int {
	t.Helper()

	img := new(vips.Image)
	defer img.Clear()

	if err := img.Load(d, 1.0, 0, 1); err != nil {
		t.Fatalf("reloading the output failed: %v", err)
	}
	return img.Pages()
}

// The default used to be one frame, so an animated GIF asked for as WebP came
// back as a still of its first frame — silently, and unlike OSS, which keeps the
// animation. This is the fix, stated as behaviour rather than as a constant.
func TestAnimationSurvivesTheDefaultConfig(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	sc := security.NewDefaultConfig()
	checker, err := security.New(&sc)
	if err != nil {
		t.Fatal(err)
	}
	pc := NewDefaultConfig()
	proc, err := New(&pc, checker, nil)
	if err != nil {
		t.Fatal(err)
	}

	src := animatedGIF(t, 32, 32, 5)

	out, err := processTo(t, proc, src, imagetype.WEBP)
	if err != nil {
		t.Fatalf("ProcessImage: %v", err)
	}

	if got := pagesOf(t, out); got != 5 {
		t.Errorf("output has %d frames, want 5 — the default config dropped the animation", got)
	}
}

// A source at exactly the cap is allowed. Off-by-one here would refuse the
// commonest interesting case, so it is worth pinning.
func TestAnimationAtTheCapIsAllowed(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	proc := newProcessor(t, 5)
	src := animatedGIF(t, 32, 32, 5)

	out, err := processTo(t, proc, src, imagetype.WEBP)
	if err != nil {
		t.Fatalf("a 5-frame source with a cap of 5 was refused: %v", err)
	}
	if got := pagesOf(t, out); got != 5 {
		t.Errorf("output has %d frames, want 5", got)
	}
}

// Over the cap the request is refused rather than truncated. Truncation is what
// this replaced: the output was a well-formed animation of the wrong length,
// which no caller could detect — least of all one that had just asked
// image/info and been told the real number.
func TestAnimationOverTheCapIsRefused(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	proc := newProcessor(t, 3)
	src := animatedGIF(t, 32, 32, 8)

	out, err := processTo(t, proc, src, imagetype.WEBP)
	if err == nil {
		t.Fatalf("an 8-frame source with a cap of 3 produced %d frames instead of an error", pagesOf(t, out))
	}

	var coded interface{ StatusCode() int }
	if !errors.As(err, &coded) {
		t.Fatalf("error carries no status code: %v", err)
	}
	if coded.StatusCode() != 422 {
		t.Errorf("status = %d, want 422", coded.StatusCode())
	}

	// The message has to name both numbers; "invalid source image" would leave
	// the caller with nothing to act on.
	msg := err.Error()
	if !strings.Contains(msg, "8") || !strings.Contains(msg, "3") {
		t.Errorf("message %q does not name the frame count and the limit", msg)
	}
}

// The cap governs animated output only. The same over-long source converted to a
// still format loses its animation on a different path, and must not be refused
// on the way.
func TestOverTheCapStillConvertsToAStillFormat(t *testing.T) {
	proc := newProcessor(t, 3)
	src := animatedGIF(t, 32, 32, 8)

	for _, format := range []imagetype.Type{imagetype.JPEG, imagetype.PNG} {
		if !vips.SupportsSave(format) {
			continue
		}
		if _, err := processTo(t, proc, src, format); err != nil {
			t.Errorf("%s: an 8-frame source was refused even though the output is a still: %v", format, err)
		}
	}
}

// A cap of 1 is still a valid configuration and still means "flatten
// everything": it must go on producing a still, not start refusing animations.
func TestCapOfOneFlattensRatherThanRefusing(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	proc := newProcessor(t, 1)
	src := animatedGIF(t, 32, 32, 8)

	out, err := processTo(t, proc, src, imagetype.WEBP)
	if err != nil {
		t.Fatalf("a cap of 1 refused an animation instead of flattening it: %v", err)
	}
	if got := pagesOf(t, out); got != 1 {
		t.Errorf("output has %d frames, want 1", got)
	}
}

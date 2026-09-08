package processing

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/kane/istore/internal/imagedata"
	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/vips"
)

// tinyAPNG is an 8x8 three-frame APNG, 280 bytes. It is committed as base64
// rather than generated, because Go's image/png writes no acTL — which is the
// whole point of the fixture. Every APNG-aware tool reads three frames from it;
// libvips reads one.
const tinyAPNG = "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAACGFjVEwAAAADAAAAAM7tusAAAAAa" +
	"ZmNUTAAAAAAAAAAIAAAACAAAAAAAAAAAAAEACgAA8k66YgAAABJJREFUeJxj/M+AHTDhEB+kEgDN" +
	"QQEP6EHibwAAABpmY1RMAAAAAQAAAAgAAAAIAAAAAAAAAAAAAQAKAABpPVC2AAAAF2ZkQVQAAAAC" +
	"eJxjZPjPgBUwYRcerBIAzEIBD4VMhLYAAAAaZmNUTAAAAAMAAAAIAAAACAAAAAAAAAAAAAEACgAA" +
	"hKuDXwAAABhmZEFUAAAABHicY2Rg+M+ADTBhFR20EgDLQwEPfaN5agAAAABJRU5ErkJggg=="

func apngSource(t *testing.T) imagedata.ImageData {
	t.Helper()

	b, err := base64.StdEncoding.DecodeString(tinyAPNG)
	if err != nil {
		t.Fatal(err)
	}

	d, err := imagedata.NewFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	if d.Format() != imagetype.PNG {
		t.Fatalf("fixture detected as %s, want png", d.Format())
	}
	return d
}

// The premise of the whole check, asserted rather than assumed: iStore's header
// walk and libvips disagree about this file. If a future libvips reads APNG,
// this test says so plainly and the refusal below stops firing on its own.
func TestAPNGDisagreementExists(t *testing.T) {
	src := apngSource(t)

	if got := containerFrameCount(src); got != 3 {
		t.Errorf("containerFrameCount = %d, want 3 — iStore's own acTL walk", got)
	}

	img := new(vips.Image)
	defer img.Clear()
	if err := img.Load(src, 1.0, 0, -1); err != nil {
		t.Fatal(err)
	}

	if img.Pages() > 1 {
		t.Skip("this libvips reads APNG; the asymmetry this package guards against is gone")
	}
	if img.IsAnimated() {
		t.Error("libvips reports the APNG as animated but only one page")
	}
}

func processAPNG(t *testing.T, proc *Processor, set func(*options.Options)) error {
	t.Helper()

	o := options.New()
	set(o)

	res, err := proc.ProcessImage(t.Context(), apngSource(t), o)
	if err == nil {
		t.Cleanup(func() { res.OutData.Close() })
	}
	return err
}

// Asking for WebP — a format that holds animation perfectly well — must not
// quietly return a one-frame WebP for a three-frame source.
func TestAPNGToAnimatedFormatIsRefused(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	proc := newProcessor(t, 300)

	err := processAPNG(t, proc, func(o *options.Options) {
		o.Set(keys.Format, imagetype.WEBP)
	})
	if err == nil {
		t.Fatal("a three-frame APNG was flattened into WebP without complaint")
	}

	var coded interface{ StatusCode() int }
	if !errors.As(err, &coded) {
		t.Fatalf("error carries no status code: %v", err)
	}
	if coded.StatusCode() != 422 {
		t.Errorf("status = %d, want 422", coded.StatusCode())
	}
	if msg := err.Error(); !strings.Contains(msg, "3") || !strings.Contains(strings.ToLower(msg), "png") {
		t.Errorf("message %q names neither the frame count nor the format", msg)
	}
}

// Still formats flatten, as they always did and should: that is the honest
// answer to "give me a JPEG", and the escape hatch the refusal above points at.
func TestAPNGToStillFormatIsServed(t *testing.T) {
	proc := newProcessor(t, 300)

	for _, format := range []imagetype.Type{imagetype.JPEG, imagetype.PNG, imagetype.AVIF} {
		if !vips.SupportsSave(format) {
			continue
		}
		err := processAPNG(t, proc, func(o *options.Options) { o.Set(keys.Format, format) })
		if err != nil {
			t.Errorf("%s: refused a still conversion of an APNG: %v", format, err)
		}
	}
}

// format,auto delegates the choice of format to the server, so answering with a
// still is the server's call to make. Refusing here would turn an ordinary
// browser fetch into a 422.
func TestAPNGUnderFormatAutoIsServed(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	proc := newProcessor(t, 300)

	// What the HTTP layer sets for format,auto with an Accept that names WebP:
	// no explicit format, just a preference.
	err := processAPNG(t, proc, func(o *options.Options) {
		o.Set(keys.PreferWebP, true)
	})
	if err != nil {
		t.Errorf("format,auto on an APNG was refused instead of being answered: %v", err)
	}
}

// A chain that names no format at all is not asking for animation either.
func TestAPNGWithNoFormatIsServed(t *testing.T) {
	proc := newProcessor(t, 300)

	err := processAPNG(t, proc, func(o *options.Options) { o.Set(keys.Width, 4) })
	if err != nil {
		t.Errorf("resize with no format on an APNG was refused: %v", err)
	}
}

// Configured to flatten everything, the server must go on flattening rather than
// start refusing — the operator has already said what they want.
func TestAPNGUnderCapOfOneIsServed(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	proc := newProcessor(t, 1)

	err := processAPNG(t, proc, func(o *options.Options) {
		o.Set(keys.Format, imagetype.WEBP)
	})
	if err != nil {
		t.Errorf("a cap of 1 refused an APNG instead of flattening it: %v", err)
	}
}

// A GIF is animation libvips *can* read, so it must pass the same gate
// untouched — the check has to fire on the disagreement, not on "the source is
// animated".
func TestReadableAnimationIsNotRefused(t *testing.T) {
	if !vips.SupportsSave(imagetype.WEBP) {
		t.Skip("this build cannot save WebP")
	}

	proc := newProcessor(t, 300)
	src := animatedGIF(t, 16, 16, 4)

	out, err := processTo(t, proc, src, imagetype.WEBP)
	if err != nil {
		t.Fatalf("an ordinary animated GIF was refused: %v", err)
	}
	if got := pagesOf(t, out); got != 4 {
		t.Errorf("output has %d frames, want 4", got)
	}
}

// containerFrameCount must stay silent about containers it cannot parse, or the
// comparison would refuse every AVIF.
func TestContainerFrameCountIsSilentOnUnparsedContainers(t *testing.T) {
	src := animatedGIF(t, 8, 8, 3)
	if got := containerFrameCount(src); got != 3 {
		t.Errorf("GIF: containerFrameCount = %d, want 3", got)
	}

	// A container imageinfo has no walk for reports nothing rather than 1, so a
	// comparison against it can never fire.
	avif, err := imagedata.NewFromBytes(append(
		[]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'},
		make([]byte, 64)...,
	))
	if err != nil {
		t.Skipf("the AVIF stub is not detected as an image: %v", err)
	}
	defer avif.Close()

	if got := containerFrameCount(avif); got != 0 {
		t.Errorf("AVIF: containerFrameCount = %d, want 0 (no claim)", got)
	}
}

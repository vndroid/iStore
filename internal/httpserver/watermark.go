package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/kane/istore/internal/imagedata"
	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/ossprocess"
	"github.com/kane/istore/internal/source"
	"github.com/kane/istore/internal/vips"
	"github.com/kane/istore/internal/vips/color"
)

// watermarkProvider supplies the watermark image the pipeline composites.
//
// imgproxy's provider is configured once at startup and returns the same image
// for every request. OSS names the watermark per request — an object key, a line
// of text, or both side by side — so this one reads what the parser left in the
// options bag and builds the image.
//
// An object key is loaded from the same root the request itself is served from,
// which means the path resolver, and therefore the traversal and symlink checks,
// apply to watermarks too.
type watermarkProvider struct {
	src *source.Local

	// Watermarks repeat across requests far more than sources do — a site
	// usually has one — so they are held in memory after the first build. The
	// map is keyed by everything that went into the image, and never evicted:
	// the number of distinct watermarks a deployment uses is small and bounded
	// by what its own URLs reference.
	mu     sync.RWMutex
	loaded map[string]imagedata.ImageData
}

func newWatermarkProvider(src *source.Local) *watermarkProvider {
	return &watermarkProvider{
		src:    src,
		loaded: make(map[string]imagedata.ImageData),
	}
}

// watermarkSpec is the request's watermark, read out of the options bag.
type watermarkSpec struct {
	path string

	text   string
	font   string
	color  color.RGB
	size   int
	shadow float64
	rotate int

	order    int
	align    int
	interval int
}

func readWatermarkSpec(o *options.Options) watermarkSpec {
	m := o.Main()

	return watermarkSpec{
		path:     m.GetString(ossprocess.KeyWatermarkPath, ""),
		text:     m.GetString(ossprocess.KeyWatermarkText, ""),
		font:     m.GetString(ossprocess.KeyWatermarkFont, "sans"),
		color:    options.Get(m, ossprocess.KeyWatermarkColor, color.Black),
		size:     m.GetInt(ossprocess.KeyWatermarkSize, 40),
		shadow:   m.GetFloat(ossprocess.KeyWatermarkShadow, 0),
		rotate:   m.GetInt(ossprocess.KeyWatermarkRotate, 0),
		order:    m.GetInt(ossprocess.KeyWatermarkOrder, 0),
		align:    m.GetInt(ossprocess.KeyWatermarkAlign, 2),
		interval: m.GetInt(ossprocess.KeyWatermarkInterval, 0),
	}
}

// cacheKey identifies the built image. Everything that changes a pixel is in it;
// nothing that only changes where the finished watermark is placed.
func (s watermarkSpec) cacheKey() string {
	if s.text == "" {
		return "img\x00" + s.path
	}

	return strings.Join([]string{
		"mix", s.path, s.text, s.font, s.color.String(),
		fmt.Sprint(s.size), fmt.Sprint(s.shadow), fmt.Sprint(s.rotate),
		fmt.Sprint(s.order), fmt.Sprint(s.align), fmt.Sprint(s.interval),
	}, "\x00")
}

// Get implements auximageprovider.Provider.
func (p *watermarkProvider) Get(_ context.Context, o *options.Options) (imagedata.ImageData, http.Header, error) {
	spec := readWatermarkSpec(o)
	if spec.path == "" && spec.text == "" {
		// No watermark requested. The pipeline treats a nil image as "skip".
		return nil, nil, nil
	}

	key := spec.cacheKey()

	p.mu.RLock()
	d, ok := p.loaded[key]
	p.mu.RUnlock()
	if ok {
		// Ref so the caller's Close does not release the cached copy.
		return d.Ref(), make(http.Header), nil
	}

	d, err := p.build(spec)
	if err != nil {
		return nil, nil, err
	}

	p.mu.Lock()
	// Another request may have built it while this one was working; keep
	// whichever landed first so there is exactly one cached instance.
	if existing, ok := p.loaded[key]; ok {
		p.mu.Unlock()
		d.Close()
		return existing.Ref(), make(http.Header), nil
	}
	p.loaded[key] = d
	p.mu.Unlock()

	return d.Ref(), make(http.Header), nil
}

// build produces the watermark image for a spec.
//
// A plain image watermark is read straight off disk — the common case, and no
// libvips work at all. Anything involving text is rendered and handed back as
// PNG bytes, because the pipeline's watermark input is image data, not a live
// vips image. The re-encode costs a few hundred microseconds on a line of text
// and buys the whole existing watermark path unchanged.
func (p *watermarkProvider) build(spec watermarkSpec) (imagedata.ImageData, error) {
	if spec.text == "" {
		name, err := p.resolve(spec.path)
		if err != nil {
			return nil, err
		}

		d, err := imagedata.NewFromFile(name)
		if err != nil {
			return nil, ErrBadWatermark
		}

		return d, nil
	}

	img, err := p.renderText(spec)
	if err != nil {
		return nil, err
	}
	defer img.Clear()

	if spec.path != "" {
		if err := p.joinImageAndText(img, spec); err != nil {
			return nil, err
		}
	}

	// Nothing about a watermark image wants progressive encoding: it is an
	// intermediate the pipeline immediately reloads.
	return img.Save(imagetype.PNG, 100, vips.SaveOverrides{})
}

// renderText draws the text layer, including its rotation.
func (p *watermarkProvider) renderText(spec watermarkSpec) (*vips.Image, error) {
	// Pango sizes in points; at 72 dpi a point is a pixel, which is what OSS's
	// `size_` means.
	img, err := vips.NewText(vips.TextOptions{
		Text:          spec.text,
		Font:          fmt.Sprintf("%s %d", spec.font, spec.size),
		DPI:           72,
		Color:         spec.color,
		ShadowOpacity: spec.shadow,
		// OSS specifies only the shadow's transparency. Scaling its offset and
		// blur from the font size keeps a 12px caption and a 200px overlay
		// looking like the same effect.
		ShadowOffset: max(1, spec.size/12),
		ShadowSigma:  max(1.0, float64(spec.size)/20),
	})
	if err != nil {
		return nil, ErrBadWatermarkText
	}

	if spec.rotate%360 != 0 {
		if err := img.RotateAny(float64(spec.rotate)); err != nil {
			img.Clear()
			return nil, err
		}
	}

	return img, nil
}

// joinImageAndText lays the object-key watermark and the text side by side,
// leaving the combined image in text.
//
// OSS's `order_` decides which is on the left, `interval_` is the gap, and
// `align_` is how the shorter of the two sits against the taller: top, middle or
// bottom.
func (p *watermarkProvider) joinImageAndText(text *vips.Image, spec watermarkSpec) error {
	name, err := p.resolve(spec.path)
	if err != nil {
		return err
	}

	data, err := imagedata.NewFromFile(name)
	if err != nil {
		return ErrBadWatermark
	}
	defer data.Close()

	pic := new(vips.Image)
	defer pic.Clear()

	if err := pic.Load(data, 1.0, 0, 1); err != nil {
		return ErrBadWatermark
	}

	// Both layers need an alpha channel: the canvas is transparent everywhere
	// neither of them covers, and vips_embed only leaves a transparent margin on
	// an image that has somewhere to put it.
	if err := pic.EnsureAlpha(); err != nil {
		return err
	}
	if err := text.EnsureAlpha(); err != nil {
		return err
	}

	first, second := pic, text
	if spec.order == 1 {
		first, second = text, pic
	}

	width := first.Width() + spec.interval + second.Width()
	height := max(first.Height(), second.Height())

	firstY := alignOffset(spec.align, height, first.Height())
	secondY := alignOffset(spec.align, height, second.Height())
	secondX := first.Width() + spec.interval

	// Grow the first layer into the full canvas, then composite the second onto
	// it. Embed fills the new area with zeroes, which with an alpha band means
	// transparent.
	if err := first.Embed(width, height, 0, firstY); err != nil {
		return err
	}
	if err := first.ApplyWatermark(second, secondX, secondY, 1.0); err != nil {
		return err
	}

	// The caller owns `text`, so the combined result has to end up there.
	if first != text {
		text.Swap(first)
	}

	return nil
}

// alignOffset places an inner height inside an outer one: 0 top, 1 middle,
// anything else bottom, which is OSS's default.
func alignOffset(align, outer, inner int) int {
	switch align {
	case 0:
		return 0
	case 1:
		return (outer - inner) / 2
	default:
		return outer - inner
	}
}

// ErrBadWatermark reports a watermark key that names nothing servable.
//
// Missing, unreadable, outside the root and "is a directory" all collapse to
// this one error on purpose: the alternative tells a prober which paths exist,
// which is the same reason the source handler answers 404 for an escape attempt.
var ErrBadWatermark = errors.New("watermark image not found")

// ErrBadWatermarkText reports text libvips could not render — in practice a font
// name that fontconfig resolves to nothing usable.
var ErrBadWatermarkText = errors.New("watermark text could not be rendered")

// resolve maps the object key through the same safety checks as a source path.
func (p *watermarkProvider) resolve(path string) (string, error) {
	name, _, err := p.src.Stat("/" + path)
	if err != nil {
		return "", ErrBadWatermark
	}
	return name, nil
}

// Close releases every cached watermark.
func (p *watermarkProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, d := range p.loaded {
		d.Close()
		delete(p.loaded, k)
	}
	return nil
}

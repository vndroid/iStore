package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/vndroid/istore/internal/imagedata"
	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/ossprocess"
	"github.com/vndroid/istore/internal/source"
	"github.com/vndroid/istore/internal/vips"
	"github.com/vndroid/istore/internal/vips/color"
)

// watermarkProvider supplies the watermark image the pipeline composites.
//
// imgproxy's provider is configured once at startup and returns the same image
// for every request. OSS names the watermark per request — an object key, a line
// of text, or both side by side — so this one reads what the parser left in the
// options bag and builds the image.
//
// An object key is fetched from the same source the request itself is served
// from: on local disk that means the path resolver, and therefore the traversal
// and symlink checks, apply to watermarks too; against an origin it means the
// watermark comes from the origin, through the same bounded fetch.
type watermarkProvider struct {
	src      source.Source
	maxBytes int64

	// Image watermarks repeat across requests far more than sources do — a site
	// usually has one — so they are held in memory after the first read. The map
	// is keyed by the object path and cannot grow without bound, which is safe
	// precisely because that key is a path: the number of distinct entries is
	// bounded by the number of objects the source has, the same bound the disk
	// cache already lives with.
	//
	// Nothing built from `text_` goes in here. See cacheable.
	//
	// ttl is zero for a local source, whose entries never expire: a watermark
	// read off disk is as current as the disk. For an upstream source it is not
	// zero, and there is nothing here to revalidate against — the map holds a
	// decoded image, not an HTTP response — so entries carry a deadline and are
	// rebuilt past it. Without that, replacing the watermark at the origin would
	// not take effect until iStore restarted.
	ttl time.Duration

	mu     sync.RWMutex
	loaded map[string]*cachedWatermark
}

// cachedWatermark is one memoised watermark image. A zero expiry never expires.
type cachedWatermark struct {
	data    imagedata.ImageData
	expires time.Time
}

func (c *cachedWatermark) stale(now time.Time) bool {
	return !c.expires.IsZero() && now.After(c.expires)
}

func newWatermarkProvider(src source.Source, maxBytes int64, ttl time.Duration) *watermarkProvider {
	return &watermarkProvider{
		src:      src,
		maxBytes: maxBytes,
		ttl:      ttl,
		loaded:   make(map[string]*cachedWatermark),
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

// cacheable reports whether the image built from this spec may be held in the
// in-memory map.
//
// Only a plain object-key watermark is. Its key is the object path, so the set
// of possible keys is the set of files under the root — a bound the deployment
// already accepts.
//
// A text watermark is not, and this is the whole point of the method. `text_`,
// `type_`, `color_`, `size_`, `shadow_` and `rotate_` are free parameters of the
// request, so a key built from them is chosen by whoever is calling: a loop over
// `watermark,text_<random>` would grow the map until the process is killed, from
// an unauthenticated GET. Rendering a line of Pango text costs a few hundred
// microseconds, which is not worth a cache whose size a stranger picks.
func (s watermarkSpec) cacheable() bool { return s.text == "" }

// cacheKey identifies a cacheable image. Only ever called when cacheable() is
// true, so the object path is the entire key.
func (s watermarkSpec) cacheKey() string { return s.path }

// Get implements auximageprovider.Provider.
func (p *watermarkProvider) Get(ctx context.Context, o *options.Options) (imagedata.ImageData, http.Header, error) {
	spec := readWatermarkSpec(o)
	if spec.path == "" && spec.text == "" {
		// No watermark requested. The pipeline treats a nil image as "skip".
		return nil, nil, nil
	}

	if !spec.cacheable() {
		// Built fresh and handed straight to the caller, who closes it. Nothing
		// is retained, so nothing accumulates.
		d, err := p.build(ctx, spec)
		if err != nil {
			return nil, nil, err
		}
		return d, make(http.Header), nil
	}

	key := spec.cacheKey()
	now := time.Now()

	p.mu.RLock()
	c, ok := p.loaded[key]
	p.mu.RUnlock()
	if ok && !c.stale(now) {
		// Ref so the caller's Close does not release the cached copy.
		return c.data.Ref(), make(http.Header), nil
	}

	d, err := p.build(ctx, spec)
	if err != nil {
		return nil, nil, err
	}

	entry := &cachedWatermark{data: d}
	if p.ttl > 0 {
		entry.expires = now.Add(p.ttl)
	}

	p.mu.Lock()
	// Another request may have built it while this one was working. Keep
	// whichever landed first, so there is exactly one cached instance — unless
	// what is there has expired, in which case this fresher one replaces it and
	// the map's reference to the old one is released. Any request still holding
	// the old image keeps its own reference, so it stays alive until that
	// request is done with it.
	if existing, ok := p.loaded[key]; ok && !existing.stale(now) {
		p.mu.Unlock()
		d.Close()
		return existing.data.Ref(), make(http.Header), nil
	} else if ok {
		existing.data.Close()
	}
	p.loaded[key] = entry
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
func (p *watermarkProvider) build(ctx context.Context, spec watermarkSpec) (imagedata.ImageData, error) {
	if spec.text == "" {
		b, err := p.fetch(ctx, spec.path)
		if err != nil {
			return nil, err
		}

		d, err := imagedata.NewFromBytes(b)
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
		if err := p.joinImageAndText(ctx, img, spec); err != nil {
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
func (p *watermarkProvider) joinImageAndText(ctx context.Context, text *vips.Image, spec watermarkSpec) error {
	b, err := p.fetch(ctx, spec.path)
	if err != nil {
		return err
	}

	data, err := imagedata.NewFromBytes(b)
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

// fetch reads a watermark object through the same source, and the same size
// bound, as the image being watermarked.
//
// Every failure collapses to ErrBadWatermark, including an origin that is down.
// That is a deliberate loss of detail: this runs inside the pipeline, where the
// alternative is leaking which watermark keys exist to anyone who can guess at
// them. The operator gets the real error in the log.
func (p *watermarkProvider) fetch(ctx context.Context, key string) ([]byte, error) {
	obj, err := p.src.Open(ctx, "/"+key)
	if err != nil {
		slog.Debug("watermark fetch failed", "key", key, "error", err)
		return nil, ErrBadWatermark
	}
	defer obj.Close()

	b, err := readAll(obj, p.maxBytes)
	if err != nil {
		slog.Debug("watermark read failed", "key", key, "error", err)
		return nil, ErrBadWatermark
	}
	return b, nil
}

// Close releases every cached watermark.
func (p *watermarkProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, c := range p.loaded {
		c.data.Close()
		delete(p.loaded, k)
	}
	return nil
}

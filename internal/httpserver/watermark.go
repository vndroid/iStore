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
	// usually has one — so they are held in memory after the first read, keyed
	// by the object path. Nothing built from `text_` goes in here: see
	// cacheable.
	//
	// ttl is zero for a local source, whose entries never expire: a watermark
	// read off disk is as current as the disk. For an upstream source it is not
	// zero, and there is nothing here to revalidate against — the map holds a
	// decoded image, not an HTTP response — so entries carry a deadline and are
	// rebuilt past it. Without that, replacing the watermark at the origin would
	// not take effect until iStore restarted.
	//
	// The size bound is separate from the TTL and does not follow from it; see
	// evictLocked. One plain Mutex rather than an RWMutex because every hit now
	// takes a reference, which is a write to the refcount and to the entry's
	// last-used time, and because at this size the critical section is a map
	// lookup either way.
	ttl time.Duration

	mu     sync.Mutex
	loaded map[string]*cachedWatermark
}

// maxWatermarks bounds the number of decoded watermark images held in memory.
//
// A deployment has one watermark, or a handful. 64 is far above any real use
// and far below anything that costs real memory, which is what a cap should be:
// invisible in normal operation, and a hard stop on the pathological case.
const maxWatermarks = 64

// cachedWatermark is one memoised watermark image. A zero expiry never expires.
type cachedWatermark struct {
	data    imagedata.ImageData
	expires time.Time
	used    time.Time
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

	if d := p.hit(key, now); d != nil {
		return d, make(http.Header), nil
	}

	d, err := p.build(ctx, spec)
	if err != nil {
		return nil, nil, err
	}

	return p.store(key, d, now), make(http.Header), nil
}

// hit returns a new reference to the cached image, or nil.
//
// Taking the reference *inside* the lock is the whole point of the method. The
// obvious shape — read the entry under a read lock, release it, then Ref — has
// a window between the two in which an expiry can drop the map's reference to
// zero and free the image, and Ref on a freed ImageData panics. It is not a
// data race, so -race never sees it: the refcount is atomic and the map is
// locked. It is a use-after-free reached through correctly synchronised code,
// and the only fix is to make "still fresh" and "now referenced" one step.
func (p *watermarkProvider) hit(key string, now time.Time) imagedata.ImageData {
	p.mu.Lock()
	defer p.mu.Unlock()

	c, ok := p.loaded[key]
	if !ok || c.stale(now) {
		return nil
	}
	c.used = now
	return c.data.Ref()
}

// store installs d in the cache and returns the caller's own reference.
func (p *watermarkProvider) store(key string, d imagedata.ImageData, now time.Time) imagedata.ImageData {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Another request may have built the same image while this one was working.
	// Keep whichever landed first so there is exactly one cached instance —
	// unless it has expired since, in which case this fresher one replaces it.
	if existing, ok := p.loaded[key]; ok {
		if !existing.stale(now) {
			existing.used = now
			d.Close()
			return existing.data.Ref()
		}
		existing.data.Close()
		delete(p.loaded, key)
	}

	p.evictLocked(now)

	entry := &cachedWatermark{data: d, used: now}
	if p.ttl > 0 {
		entry.expires = now.Add(p.ttl)
	}
	p.loaded[key] = entry

	return d.Ref()
}

// evictLocked makes room for one more entry. The caller holds the lock.
//
// Expired entries go first. Nothing else ever removes one — an entry is only
// replaced when its own key is asked for again — so without this sweep a TTL
// bounds how *stale* the cache gets and not how *large*, and a key that is
// never requested twice stays resident for the life of the process.
//
// Then the least recently used, until there is room. The cap is what makes this
// a cache rather than an index of every watermark the source has ever been
// asked for. The old argument for leaving it unbounded — that the key is an
// object path, so the entry count is bounded by the number of objects that
// exist — held for a directory on disk and does not hold for an origin, which
// may mint a valid image for any path it likes. These are decoded images in
// memory, so the bound has to be iStore's, not the source's.
func (p *watermarkProvider) evictLocked(now time.Time) {
	for k, c := range p.loaded {
		if c.stale(now) {
			c.data.Close()
			delete(p.loaded, k)
		}
	}

	for len(p.loaded) >= maxWatermarks {
		oldestKey, oldest := "", time.Time{}
		for k, c := range p.loaded {
			if oldestKey == "" || c.used.Before(oldest) {
				oldestKey, oldest = k, c.used
			}
		}
		p.loaded[oldestKey].data.Close()
		delete(p.loaded, oldestKey)
	}
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

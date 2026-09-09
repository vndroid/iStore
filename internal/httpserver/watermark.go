package httpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vndroid/istore/internal/imagedata"
	"github.com/vndroid/istore/internal/imageinfo"
	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/ossprocess"
	"github.com/vndroid/istore/internal/singleflight"
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
//
// A watermark has three separate budgets, and they bound three different things.
// Getting one of them right does not get the others right, which is the reason
// each is spelled out rather than reusing the source image's limits:
//
//   - maxBytes bounds one watermark on the wire. It is what stops a single
//     enormous file, and it is the cheapest check because it needs no decode.
//
//   - maxResolution bounds one watermark in pixels, and it is the one that
//     actually protects the process. Bytes and pixels are only loosely related:
//     a 440 KB PNG of a flat colour decodes to 144 megapixels and cost a
//     measured 444 MB of RSS for a single request — about a thousand times its
//     size on the wire. No byte limit low enough to be useful would have caught
//     it. The source image's own pixel budget did not either: it is 250 MP,
//     which is a sane ceiling for a photograph and an absurd one for a logo.
//
//   - cacheBytes bounds every retained watermark added together. maxBytes alone
//     bounds one entry, and the entry count alone bounds how many, but their
//     product is what is actually resident — 64 x 100 MiB was 6.25 GiB.
//
// The retained bytes are the encoded file, not decoded pixels: the map holds
// what came off the wire, and the decode happens per use inside the pipeline.
// That is why cacheBytes can be modest while maxResolution has to be strict.
type watermarkProvider struct {
	src source.Source

	maxBytes      int64 // one watermark, on the wire
	maxResolution int   // one watermark, in pixels (width x height x frames)
	cacheBytes    int64 // every retained watermark, added together

	// Image watermarks repeat across requests far more than sources do — a site
	// usually has one — so they are held in memory after the first read, keyed
	// by the object path. Nothing built from `text_` goes in here: see
	// cacheable.
	//
	// ttl is zero for a local source, whose entries never expire: a watermark
	// read off disk is as current as the disk. For an upstream source it is not
	// zero, and there is nothing here to revalidate against — the map holds an
	// image, not an HTTP response — so entries carry a deadline and are rebuilt
	// past it. Without that, replacing the watermark at the origin would not
	// take effect until iStore restarted.
	//
	// One plain Mutex rather than an RWMutex because every hit takes a
	// reference, which is a write to the refcount and to the entry's last-used
	// time, and because at this size the critical section is a map lookup
	// either way.
	ttl time.Duration

	mu        sync.Mutex
	loaded    map[string]*cachedWatermark
	usedBytes int64

	// flight collapses concurrent first-time builds of the same key. Without
	// it, N simultaneous requests for one uncached watermark were N downloads
	// and N decodes — measured at 16 origin fetches for 16 requests. Only one
	// of the results was ever kept; the rest were built and thrown away, and
	// the transient peak had already happened.
	flight singleflight.Group
}

// maxWatermarks bounds how many watermarks are retained, whatever they weigh.
//
// A deployment has one watermark, or a handful. This is the second line behind
// cacheBytes: it keeps the map small enough that the linear eviction scan stays
// trivial, and it bounds the per-entry overhead that a byte budget cannot see.
const maxWatermarks = 64

// cachedWatermark is one memoised watermark image. A zero expiry never expires.
type cachedWatermark struct {
	data    imagedata.ImageData
	size    int64
	expires time.Time
	used    time.Time
}

func (c *cachedWatermark) stale(now time.Time) bool {
	return !c.expires.IsZero() && now.After(c.expires)
}

func newWatermarkProvider(src source.Source, cfg Config, ttl time.Duration) *watermarkProvider {
	maxBytes := cfg.MaxWatermarkBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxWatermarkBytes
	}
	cacheBytes := cfg.WatermarkCacheBytes
	if cacheBytes <= 0 {
		cacheBytes = DefaultWatermarkCacheBytes
	}
	// A single admissible watermark must always fit the cache, or the store
	// path starts declining entries it just paid to fetch. Where the two
	// disagree the total wins and the per-item limit comes down to meet it:
	// the total is the memory bound, and a memory bound that an operator set
	// deliberately should not be quietly raised by a per-item default.
	if cacheBytes < maxBytes {
		slog.Warn("watermark cache budget is below the per-watermark limit; lowering the per-watermark limit to match",
			"cache_bytes", cacheBytes, "was_max_bytes", maxBytes)
		maxBytes = cacheBytes
	}
	res := cfg.MaxWatermarkResolution
	if res <= 0 {
		res = DefaultMaxWatermarkResolution
	}

	return &watermarkProvider{
		src:           src,
		maxBytes:      maxBytes,
		maxResolution: res,
		cacheBytes:    cacheBytes,
		ttl:           ttl,
		loaded:        make(map[string]*cachedWatermark),
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

	if d := p.hit(key, time.Now()); d != nil {
		return d, make(http.Header), nil
	}

	// One build per key, however many requests arrive at once. The call
	// deliberately returns nothing but an error: the built image goes into the
	// cache, and every caller — the one that did the work included — takes its
	// own reference from there, under the lock. Handing the shared value back
	// through the group would mean waiters referencing an image whose only
	// other holder might be closing it at that moment, which is exactly the
	// use-after-free this cache already had once.
	// Every time is read fresh, never carried across the build. A build can
	// outrun the TTL — a slow origin and ISTORE_UPSTREAM_TTL_SEC=1 is enough —
	// and stamping the finished entry with the timestamp from before it started
	// stores something already expired. The entry then misses for every later
	// request, each of which falls past the group and downloads again: the
	// coalescing still holds for one burst and then stops working entirely.
	_, err, _ := p.flight.Do(key, func() (any, error) {
		if d := p.hit(key, time.Now()); d != nil {
			d.Close()
			return nil, nil
		}
		d, err := p.build(ctx, spec)
		if err != nil {
			return nil, err
		}
		p.store(key, d)
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}

	if d := p.hit(key, time.Now()); d != nil {
		return d, make(http.Header), nil
	}

	// Reached only when the cache would not keep this watermark at all — it is
	// larger than the whole cache budget — which newWatermarkProvider's clamp
	// makes unreachable, or when this goroutine was descheduled for a whole TTL
	// between the store and the hit above.
	//
	// It deliberately does not go back through the group. An entry the cache
	// declines is one no amount of coalescing will make shareable, and retrying
	// the group for it would serialise every request for that key behind a
	// build whose result is thrown away each time.
	slog.Debug("watermark not retained; building for this request only", "key", key)
	d, err := p.build(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	return d, make(http.Header), nil
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

// store hands the cache ownership of d. It always consumes the reference: what
// it does not retain, it closes.
//
// The clock is read here rather than taken from the caller, deliberately: the
// caller's timestamp predates a build that may have taken longer than the TTL,
// and an entry stamped with it would be born expired.
func (p *watermarkProvider) store(key string, d imagedata.ImageData) {
	size := int64(0)
	if n, err := d.Size(); err == nil {
		size = int64(n)
	}

	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Another request may have built the same image while this one was working.
	// Keep whichever landed first so there is exactly one cached instance —
	// unless it has expired since, in which case this fresher one replaces it.
	if existing, ok := p.loaded[key]; ok {
		if !existing.stale(now) {
			d.Close()
			return
		}
		p.removeLocked(key)
	}

	if p.cacheBytes > 0 && size > p.cacheBytes {
		d.Close()
		return
	}

	p.evictLocked(now, size)

	entry := &cachedWatermark{data: d, size: size, used: now}
	if p.ttl > 0 {
		entry.expires = now.Add(p.ttl)
	}
	p.loaded[key] = entry
	p.usedBytes += size
}

// removeLocked drops one entry and the bytes it accounted for.
func (p *watermarkProvider) removeLocked(key string) {
	c, ok := p.loaded[key]
	if !ok {
		return
	}
	c.data.Close()
	p.usedBytes -= c.size
	delete(p.loaded, key)
}

// evictLocked makes room for one more entry of `incoming` bytes.
//
// Expired entries go first. Nothing else ever removes one — an entry is only
// replaced when its own key is asked for again — so without this sweep a TTL
// bounds how *stale* the cache gets and not how *large*, and a key that is
// never requested twice stays resident for the life of the process.
//
// Then the least recently used, until both budgets fit. Two budgets rather than
// one because they fail differently: the entry count keeps the map small enough
// for this linear scan to be free, and the byte total is what actually bounds
// memory. Neither implies the other — 64 entries of 100 MiB each satisfied the
// count and came to 6.25 GiB.
//
// The old argument for no bound at all — that the key is an object path, so the
// entry count is bounded by the number of objects that exist — held for a
// directory on disk and does not hold for an origin, which may mint a valid
// image for any path it likes.
func (p *watermarkProvider) evictLocked(now time.Time, incoming int64) {
	for k, c := range p.loaded {
		if c.stale(now) {
			p.removeLocked(k)
		}
	}

	for len(p.loaded) > 0 {
		overCount := len(p.loaded) >= maxWatermarks
		overBytes := p.cacheBytes > 0 && p.usedBytes+incoming > p.cacheBytes
		if !overCount && !overBytes {
			return
		}

		oldestKey, oldest := "", time.Time{}
		for k, c := range p.loaded {
			if oldestKey == "" || c.used.Before(oldest) {
				oldestKey, oldest = k, c.used
			}
		}
		p.removeLocked(oldestKey)
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
		// The join widens the canvas by the object watermark plus interval_,
		// so the combined result needs checking even though both halves passed.
		if err := p.checkCanvas(img, "combined watermark"); err != nil {
			return nil, err
		}
	}

	// Nothing about a watermark image wants progressive encoding: it is an
	// intermediate the pipeline immediately reloads.
	return img.Save(imagetype.PNG, 100, vips.SaveOverrides{})
}

// checkCanvas applies the pixel budget to a rendered image.
//
// The budget has to reach the text path, not only the object path. `text_`,
// `size_` and `rotate_` are free request parameters, and the canvas they
// produce is not bounded by any file: measured, `text_<30 chars>,size_1000`
// served a 200 while adding 350 MB of RSS, and `text_<20 chars>,size_1000,
// rotate_45` was refused downstream only *after* spending 598 MB — a refusal
// that costs more than the work it refuses is not a limit.
func (p *watermarkProvider) checkCanvas(img *vips.Image, what string) error {
	if p.maxResolution <= 0 {
		return nil
	}
	px, ok := pixelCount(img.Width(), img.Height(), 1)
	if !ok || px > int64(p.maxResolution) {
		slog.Debug("watermark canvas too large",
			"what", what, "width", img.Width(), "height", img.Height(), "limit", p.maxResolution)
		return ErrWatermarkTooLarge
	}
	return nil
}

// checkTextEstimate refuses text that cannot fit the budget, before Pango
// renders anything.
//
// The estimate deliberately under-states the canvas, so it never refuses text
// that would have fitted. Measured advance per character as a fraction of the
// font size: 0.99 for a run of "W", 0.98 for CJK, 0.24 for a run of "i" — 0.2
// is below anything observed. Line height measured at 0.73 of the font size;
// 0.7 is below it.
//
// It cannot make the render free, and does not pretend to. Text that clears the
// estimate but not the real budget is rendered and then refused by checkCanvas,
// and that render allocates. What bounds the waste is Pango's own surface
// limit — it refuses to produce a canvas much past 32767 px wide — so the worst
// case is one render of roughly that width, and the estimate's job is only to
// keep the obviously absurd requests from reaching it at all.
func (p *watermarkProvider) checkTextEstimate(spec watermarkSpec) error {
	if p.maxResolution <= 0 || spec.size <= 0 || spec.text == "" {
		return nil
	}

	lines := strings.Split(spec.text, "\n")
	longest := 0
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > longest {
			longest = n
		}
	}

	w := int64(longest) * int64(spec.size) * 2 / 10
	h := int64(len(lines)) * int64(spec.size) * 7 / 10
	if w <= 0 || h <= 0 {
		return nil
	}
	if w > math.MaxInt64/h || w*h > int64(p.maxResolution) {
		slog.Debug("watermark text refused before rendering",
			"runes", longest, "lines", len(lines), "size", spec.size,
			"at_least", w*h, "limit", p.maxResolution)
		return ErrWatermarkTooLarge
	}
	return nil
}

// renderText draws the text layer, including its rotation.
//
// The budget is applied three times along the way, and each one catches
// something the others cannot: before the render, because the cheapest refusal
// is the one that never allocates; after it, because the estimate is
// approximate and the canvas is not; and after the rotation, because rotating
// a canvas 45 degrees grows its bounding box by up to half again and that
// growth is what turned a 20-character request into 598 MB.
func (p *watermarkProvider) renderText(spec watermarkSpec) (*vips.Image, error) {
	if err := p.checkTextEstimate(spec); err != nil {
		return nil, err
	}
	return p.renderTextChecked(spec)
}

func (p *watermarkProvider) renderTextChecked(spec watermarkSpec) (*vips.Image, error) {
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

	if err := p.checkCanvas(img, "text watermark"); err != nil {
		img.Clear()
		return nil, err
	}

	if spec.rotate%360 != 0 {
		if err := img.RotateAny(float64(spec.rotate)); err != nil {
			img.Clear()
			return nil, err
		}
		if err := p.checkCanvas(img, "rotated text watermark"); err != nil {
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

// ErrWatermarkTooLarge reports a watermark past ISTORE_MAX_WATERMARK_BYTES or
// ISTORE_MAX_WATERMARK_RESOLUTION.
//
// Distinct from ErrBadWatermark on purpose. That one is deliberately vague,
// because "which watermark keys exist" is information a prober wants. Here the
// object exists and the caller named it, so vagueness protects nothing and
// costs the operator the difference between raising a limit and debugging a
// missing file that is not missing.
var ErrWatermarkTooLarge = errors.New("watermark image is too large")

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
		// Too large is not the same as not found, and reporting it as such
		// sends an operator hunting for a file that is sitting right there.
		// The vagueness of ErrBadWatermark buys something real — it stops a
		// prober enumerating which keys exist — and here it buys nothing: the
		// caller named an object that does exist, and its size is a property
		// of their own request. The source path says 413 for the same reason.
		var big errSourceTooLarge
		if errors.As(err, &big) {
			slog.Debug("watermark too large", "key", key, "error", err)
			return nil, ErrWatermarkTooLarge
		}
		slog.Debug("watermark read failed", "key", key, "error", err)
		return nil, ErrBadWatermark
	}

	if err := p.checkResolution(b); err != nil {
		slog.Debug("watermark refused", "key", key, "error", err)
		return nil, ErrWatermarkTooLarge
	}

	return b, nil
}

// checkResolution refuses a watermark that is too many pixels, before anything
// decodes it.
//
// This is the check that matters, and it is here rather than in the pipeline
// for two reasons. It runs on the header alone, so an oversized watermark costs
// a header parse instead of a decode. And the pipeline's own check — the one in
// prepareWatermark — measures against the *source image's* budget, 250 MP,
// which is the right ceiling for a photograph being served and far too generous
// for a decoration composited on top of one. A 440 KB PNG that decodes to 144
// MP passes that check and cost a measured 444 MB of RSS.
//
// Frames are multiplied in, the same way the source budget counts them: an
// animated watermark is decoded frame by frame.
func (p *watermarkProvider) checkResolution(b []byte) error {
	if p.maxResolution <= 0 {
		return nil
	}

	w, h, frames, err := watermarkGeometry(b)
	if err != nil {
		// Unreadable geometry is not a size failure; let the decode produce the
		// real error, which says something more useful than "too large".
		return nil
	}

	px, ok := pixelCount(w, h, frames)
	if !ok {
		return fmt.Errorf("watermark header claims %dx%d over %d frames, which is not a real image",
			w, h, frames)
	}
	if px > int64(p.maxResolution) {
		return fmt.Errorf("watermark is %dx%d over %d frames (%d pixels), limit is %d",
			w, h, frames, px, p.maxResolution)
	}
	return nil
}

// pixelCount multiplies a geometry out without overflowing.
//
// Every input comes from an image header, which is attacker-supplied: PNG
// stores width, height and the APNG frame count as uint32, so a crafted file
// can claim 4294967295 x 4294967295. Multiplied in `int` that wraps to
// -8589934591, which is under any limit — the check passes and the guard is
// gone. ok is false for an overflow and for any non-positive dimension, both of
// which are refusals rather than sizes: no real image has them.
func pixelCount(w, h, frames int) (int64, bool) {
	if w <= 0 || h <= 0 || frames <= 0 {
		return 0, false
	}
	a, b, f := int64(w), int64(h), int64(frames)
	if a > math.MaxInt64/b {
		return 0, false
	}
	area := a * b
	if area > math.MaxInt64/f {
		return 0, false
	}
	return area * f, true
}

// watermarkGeometry reads dimensions from the header where the format allows,
// and asks libvips otherwise. The libvips path is a header load: it does not
// decode pixels, so a refusal still costs nothing.
func watermarkGeometry(b []byte) (w, h, frames int, err error) {
	info, ierr := imageinfo.Read(bytes.NewReader(b), int64(len(b)))
	if ierr == nil && info != nil && info.ImageWidth > 0 {
		return info.ImageWidth, info.ImageHeight, info.FrameCount, nil
	}

	data, derr := imagedata.NewFromBytes(b)
	if derr != nil {
		return 0, 0, 0, derr
	}
	defer data.Close()

	img := new(vips.Image)
	defer img.Clear()
	if lerr := img.Load(data, 1.0, 0, 1); lerr != nil {
		return 0, 0, 0, lerr
	}
	return img.Width(), img.PageHeight(), img.Pages(), nil
}

// Close releases every cached watermark.
func (p *watermarkProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.loaded {
		p.removeLocked(k)
	}
	return nil
}

// Package httpserver wires the OSS grammar, the source, the cache and the
// ported imgproxy pipeline into an HTTP handler.
//
// Request shape:
//
//	GET /path/to/image.jpg                              -> the source, untouched
//	GET /path/to/image.jpg?x-oss-process=image/format,avif
//	GET /path/to/image.jpg?x-oss-process=image/info     -> JSON
//
// Where the bytes come from is decided once, at construction: a directory on
// disk (Root) or an HTTP origin (Upstream). Everything below the source
// interface is identical either way.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/cache"
	"github.com/vndroid/istore/internal/imagedata"
	"github.com/vndroid/istore/internal/imageinfo"
	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
	"github.com/vndroid/istore/internal/ossprocess"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/singleflight"
	"github.com/vndroid/istore/internal/source"
	"github.com/vndroid/istore/internal/vips"
	"github.com/vndroid/istore/internal/vips/color"
)

// The info handler asks the source for exactly imageinfo's header window, so
// the source's buffer has to be able to hold it. Both are 64 KiB; this fails
// the build rather than the request if one of them ever moves.
const _ = uint(source.HeaderWindow - imageinfo.HeaderBytes)

// Config holds the knobs the HTTP layer itself needs. Image-processing config
// lives in processing.Config / vips.Config.
type Config struct {
	// Root is the directory images are served from. Mutually exclusive with
	// Upstream; exactly one of the two must be set.
	Root string

	// Upstream is the base URL images are fetched from, e.g.
	// "http://localhost:3030". A request's path is appended to it: a GET of
	// /a/b.jpg fetches <Upstream>/a/b.jpg.
	Upstream string
	// UpstreamTimeout bounds one fetch, separately from ProcessTimeout. A slow
	// origin should not be able to spend the budget that exists for encoding.
	UpstreamTimeout time.Duration
	// UpstreamTTL is how long upstream content may be reused without going back
	// to the origin. It covers two things: cached results whose origin sent no
	// ETag or Last-Modified to key on, and the in-memory watermark map, which
	// has no validator of its own.
	UpstreamTTL time.Duration
	// UpstreamInfoMaxBytes bounds the whole-object read that the info endpoint
	// falls back to when a header window cannot answer the question. It only
	// applies in upstream mode; see serveInfo.
	UpstreamInfoMaxBytes int64

	// CacheDir stores transcoded results. Empty disables caching, which is only
	// sensible for testing — see the package comment in internal/cache.
	CacheDir string
	// MaxSourceBytes rejects source objects larger than this.
	MaxSourceBytes int64
	// Concurrency caps simultaneous image processing. Zero means GOMAXPROCS.
	Concurrency int
	// ProcessTimeout bounds one transcode, not counting the fetch.
	ProcessTimeout time.Duration
	// CacheControl is sent with successful image responses.
	CacheControl string
	// Evict bounds the disk cache. Zero Interval leaves it unbounded.
	Evict cache.EvictConfig
	// Verifier checks request signatures. Nil, or one that reports signing is
	// unconfigured, serves every request unsigned.
	Verifier SignatureVerifier
}

// SignatureVerifier checks that a request was issued by someone holding the
// shared key. *security.Checker implements it.
type SignatureVerifier interface {
	// SignatureEnabled reports whether a key and salt are configured. When it is
	// false the server does not ask for a signature at all.
	SignatureEnabled() bool
	// VerifySignature returns nil if signature is valid for message.
	VerifySignature(ctx context.Context, signature, message string) error
}

// SignatureQueryKey is the query parameter carrying a request's signature.
//
// imgproxy signs in the path, because its path *is* the request: options and
// source URL are both segments of it. iStore's path is the object key and
// nothing else, so a signature segment would have to be spliced in front of a
// real filename. A query parameter leaves the key alone.
const SignatureQueryKey = "x-istore-signature"

// DefaultUpstreamTimeout bounds one fetch from the origin.
const DefaultUpstreamTimeout = 10 * time.Second

// DefaultUpstreamTTL is how stale upstream content may get when the origin
// gives nothing better to go on. Five minutes is short enough that a replaced
// object turns over on its own within a coffee break, and long enough that a
// busy path is not re-fetched per request.
const DefaultUpstreamTTL = 5 * time.Minute

// DefaultUpstreamInfoMaxBytes bounds info's whole-object fallback in upstream
// mode. See serveInfo for why the ordinary case never reaches it.
const DefaultUpstreamInfoMaxBytes = 10 << 20

// NewDefaultConfig returns usable defaults.
func NewDefaultConfig() Config {
	return Config{
		MaxSourceBytes:       100 << 20,
		ProcessTimeout:       20 * time.Second,
		CacheControl:         "public, max-age=31536000, immutable",
		UpstreamTimeout:      DefaultUpstreamTimeout,
		UpstreamTTL:          DefaultUpstreamTTL,
		UpstreamInfoMaxBytes: DefaultUpstreamInfoMaxBytes,
	}
}

// Server is the HTTP handler.
type Server struct {
	cfg   Config
	src   source.Source
	cache *cache.Disk
	proc  *processing.Processor

	// remote records that src goes over the network. Three things branch on it:
	// info's whole-object fallback gets its own smaller bound, the cache key
	// falls back to a time bucket when the origin sends no validator, and the
	// watermark map expires.
	remote bool

	// sem bounds concurrent encodes. libvips is happy to use every core for one
	// image; letting N requests each do that turns a small box into a queue with
	// extra steps.
	sem    chan struct{}
	flight singleflight.Group

	// watermarks is handed to the processor at construction time; it reads the
	// requested object key out of each request's options.
	watermarks *watermarkProvider

	// verifier is nil unless a key and salt are configured, so the check costs
	// one nil comparison on the unsigned deployment.
	verifier SignatureVerifier

	stop chan struct{}
}

// New builds a Server.
//
// newProcessor is called with the watermark provider once the source is open,
// because the provider needs the source: an OSS watermark names an object key,
// and that key is fetched exactly the way the image being served is — through
// the same traversal checks on disk, from the same origin over HTTP.
func New(cfg Config, newProcessor func(auximageprovider.Provider) (*processing.Processor, error)) (*Server, error) {
	stop := make(chan struct{})

	src, remote, err := newSource(cfg)
	if err != nil {
		return nil, err
	}

	ttl := time.Duration(0)
	if remote {
		ttl = cfg.UpstreamTTL
		if ttl <= 0 {
			ttl = DefaultUpstreamTTL
		}
	}

	watermarks := newWatermarkProvider(src, cfg.MaxSourceBytes, ttl)
	proc, err := newProcessor(watermarks)
	if err != nil {
		return nil, err
	}

	var c *cache.Disk
	if cfg.CacheDir != "" {
		if c, err = cache.NewDisk(cfg.CacheDir); err != nil {
			return nil, err
		}
		c.StartEvictor(cfg.Evict, stop)
	}

	n := cfg.Concurrency
	if n <= 0 {
		n = 1
	}

	var verifier SignatureVerifier
	if cfg.Verifier != nil && cfg.Verifier.SignatureEnabled() {
		verifier = cfg.Verifier
	}

	return &Server{
		cfg:        cfg,
		src:        src,
		remote:     remote,
		cache:      c,
		proc:       proc,
		sem:        make(chan struct{}, n),
		watermarks: watermarks,
		verifier:   verifier,
		stop:       stop,
	}, nil
}

// newSource picks the backend. Root and Upstream are mutually exclusive, and
// the refusal is here rather than only in main so that an embedder cannot end
// up with a server that silently reads local disk while its operator believes
// it is talking to an origin.
func newSource(cfg Config) (source.Source, bool, error) {
	switch {
	case cfg.Root == "" && cfg.Upstream == "":
		return nil, false, errors.New("httpserver: one of Root or Upstream must be set")
	case cfg.Root != "" && cfg.Upstream != "":
		return nil, false, errors.New("httpserver: Root and Upstream are mutually exclusive")
	case cfg.Upstream != "":
		timeout := cfg.UpstreamTimeout
		if timeout <= 0 {
			timeout = DefaultUpstreamTimeout
		}
		u, err := source.NewUpstream(cfg.Upstream, timeout)
		return u, true, err
	default:
		l, err := source.NewLocal(cfg.Root)
		return l, false, err
	}
}

// SourceDescription names where objects come from, for the startup log.
func (s *Server) SourceDescription() string { return s.src.Describe() }

// Close stops the cache evictor and releases cached watermarks.
func (s *Server) Close() error {
	close(s.stop)
	return s.watermarks.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET and HEAD are supported")
		return
	}

	// /healthz is exempt: it reports whether libvips is alive and reveals nothing
	// about the objects being served, and a load balancer has no key.
	if r.URL.Path == "/healthz" {
		s.health(w)
		return
	}

	if err := s.verifySignature(r); err != nil {
		s.failSignature(w, r, err)
		return
	}

	raw := r.URL.Query().Get(ossprocess.QueryKey)
	chain, err := ossprocess.Parse(raw)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}
	if err := chain.Validate(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}
	// Grammar is fine; now ask libvips whether it can actually produce the
	// requested format. Split from Validate because that one must stay callable
	// without a live libvips — see ossprocess.Validate.
	if err := chain.CheckEncoders(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}

	switch {
	case chain.IsInfo():
		s.serveInfo(w, r)
	case chain.IsAverageHue():
		s.serveAverageHue(w, r)
	case chain.IsEmpty():
		s.serveOriginal(w, r)
	default:
		s.serveProcessed(w, r, chain, raw)
	}
}

// ----------------------------------------------------------------- signature

// SignedMessage is what a signature covers: the object path with the process
// chain appended exactly as it appears in the URL, and nothing else.
//
//	/photo.jpg
//	/photo.jpg?x-oss-process=image/resize,w_800
//
// Both halves have to be in it. Signing the path alone would let anyone rewrite
// the transform on a URL they were handed, and an unbounded chain of resizes is
// a cheap way to spend someone else's CPU; signing the chain alone would let
// them point it at a different object. Other query parameters are outside the
// signature deliberately — cache-busters and analytics tags are added by things
// that do not have the key.
func SignedMessage(path, chain string) string {
	if chain == "" {
		return path
	}
	return path + "?" + ossprocess.QueryKey + "=" + chain
}

// verifySignature checks the request's x-istore-signature parameter.
//
// It is a no-op until ISTORE_KEY and ISTORE_SALT are both set: New leaves
// s.verifier nil otherwise, so an unsigned deployment behaves exactly as it did
// before signing existed.
func (s *Server) verifySignature(r *http.Request) error {
	if s.verifier == nil {
		return nil
	}

	q := r.URL.Query()
	msg := SignedMessage(r.URL.Path, q.Get(ossprocess.QueryKey))

	return s.verifier.VerifySignature(r.Context(), q.Get(SignatureQueryKey), msg)
}

// failSignature answers a rejected signature.
//
// The body says only "Forbidden". Which of "no signature", "wrong key" and
// "signature for a different path" it was is information a prober would use to
// work out what iStore signs, and the operator gets the real reason in the log.
func (s *Server) failSignature(w http.ResponseWriter, r *http.Request, err error) {
	slog.Debug("signature rejected", "path", r.URL.Path, "error", err)

	status := http.StatusForbidden
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) {
		status = coded.StatusCode()
	}

	s.fail(w, r, status, "AccessDenied", "Forbidden")
}

// ------------------------------------------------------------------ original

func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request) {
	obj, err := s.src.Open(r.Context(), r.URL.Path)
	if err != nil {
		s.failSource(w, r, err)
		return
	}
	defer obj.Close()

	// Sniffed, not taken from the origin's Content-Type: an origin that labels a
	// JPEG as application/octet-stream is common, and the bytes are authoritative
	// either way. Peek leaves them in place for the copy below.
	if head, perr := obj.Peek(512); perr == nil {
		if t, terr := imagetype.Detect(bytes.NewReader(head), "", ""); terr == nil {
			w.Header().Set("Content-Type", t.Mime())
		}
	}

	// A chunked origin gives no length. Leaving the header off lets net/http
	// chunk the response rather than buffering the whole object to count it.
	if obj.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
	w.Header().Set("Cache-Control", s.cfg.CacheControl)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := obj.WriteTo(w); err != nil {
		slog.Debug("write failed", "path", r.URL.Path, "error", err)
	}
}

// --------------------------------------------------------------- average-hue

// serveAverageHue answers `x-oss-process=image/average-hue` with the image's
// mean colour as `0xRRGGBB` in plain text — OSS returns it bare, not wrapped in
// JSON the way info is.
//
// Unlike info this needs the pixels, so it takes a concurrency slot and decodes
// the image. It is still far cheaper than a transform: one decode, no resize, no
// encode, and a seven-byte body.
func (s *Server) serveAverageHue(w http.ResponseWriter, r *http.Request) {
	// The decode below reads the whole object into memory, so this endpoint is
	// bounded by the same limit as a transform. It used to have no bound at all,
	// which made it the cheapest way to ask the process to allocate 2 GB.
	b, err := s.fetchAll(r.Context(), r.URL.Path, s.cfg.MaxSourceBytes)
	if err != nil {
		s.failFetch(w, r, err)
		return
	}

	hue, err := s.averageHue(b)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "InvalidImage", "the image could not be read")
		return
	}

	body := fmt.Sprintf("0x%02x%02x%02x", hue.R, hue.G, hue.B)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", s.cfg.CacheControl)
	if r.Method == http.MethodHead {
		return
	}
	w.Write([]byte(body))
}

// averageHue decodes one frame and returns its mean colour.
func (s *Server) averageHue(b []byte) (color.RGB, error) {
	s.acquire()
	defer s.release()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer vips.Cleanup()

	data, err := imagedata.NewFromBytes(b)
	if err != nil {
		return color.RGB{}, err
	}
	defer data.Close()

	img := new(vips.Image)
	defer img.Clear()

	// One frame is enough: an animation's mean colour is not worth decoding
	// every frame for, and OSS reports a single value too.
	if err := img.Load(data, 1.0, 0, 1); err != nil {
		return color.RGB{}, err
	}

	return img.AverageHue()
}

// ---------------------------------------------------------------------- info

func (s *Server) serveInfo(w http.ResponseWriter, r *http.Request) {
	obj, err := s.src.OpenHeader(r.Context(), r.URL.Path, imageinfo.HeaderBytes)
	if err != nil {
		s.failSource(w, r, err)
		return
	}
	defer obj.Close()

	head, herr := obj.Peek(imageinfo.HeaderBytes)
	if herr != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "InvalidImage", "the image could not be read")
		return
	}

	size := obj.Size
	if size < 0 {
		// A chunked origin never said how long the object is. The header window
		// is all there is, so FileSize reports what arrived.
		size = int64(len(head))
	}

	info, err := imageinfo.Read(bytes.NewReader(head), size)
	if info == nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "InvalidImage", "the image could not be read")
		return
	}

	// imageinfo parses JPEG, PNG, GIF and WebP headers directly, which is the
	// cheap path and answers most requests outright. Three things send a request
	// to libvips instead, and they are three different situations:
	//
	//   - a container imageinfo has no walk for (AVIF, HEIC, JXL, TIFF, BMP);
	//   - a JPEG whose SOF sits past the 64 KiB window, which a big EXIF
	//     thumbnail or a segmented ICC profile is enough to cause;
	//   - an animated WebP, or a GIF whose blocks outrun the window, where the
	//     geometry is known but the frame count is only a floor.
	//
	// The first two have no answer without libvips. The third has a usable one
	// already, so its fallback is best-effort: if it cannot run, the floor is
	// still served.
	//
	// Note what is *not* on that list: a missing Exif map. It is tempting to add
	// it — that is where AVIF's EXIF comes from — but "this image has no EXIF" is
	// indistinguishable from "EXIF has not been looked for yet", and a JPEG
	// without EXIF is the single most common request there is. Sending those to
	// libvips reads every one of them off disk in full: measured at +80 MB of RSS
	// across five info requests for one 40 MB JPEG that the header walk had
	// already answered from its first 64 KiB. So EXIF is only ever filled in on a
	// trip that was happening anyway, inside infoViaVips.
	var unsupported imageinfo.ErrUnsupportedContainer
	needsGeometry := errors.As(err, &unsupported) || errors.Is(err, imageinfo.ErrHeaderTooShort)

	if needsGeometry || !info.FrameCountKnown {
		refined, rerr := s.infoViaVips(r.Context(), r.URL.Path, obj, info)
		switch {
		case rerr == nil:
			info, err = refined, nil
		case needsGeometry:
			// Nothing else was going to supply the dimensions.
			err = rerr
		default:
			// Keep what the header walk found and drop the refinement.
			slog.Debug("info refinement failed", "path", r.URL.Path, "error", rerr)
			err = nil
		}
	}
	var big errSourceTooLarge
	if errors.As(err, &big) {
		s.failTooLarge(w, r, big)
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "InvalidImage", err.Error())
		return
	}

	body, err := json.Marshal(info)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "InternalError", "encoding failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", s.cfg.CacheControl)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

// infoViaVips fills in whatever the header walk could not, and only that.
//
// Each field is taken from libvips on its own condition, because the three
// callers want different subsets and there is no reason to overwrite a value
// that was read from the container itself. `partial` already carries the two
// fields that are right regardless — file size, and the format from the magic
// bytes — so they are never touched here.
//
// This needs the whole object, which is the one expensive thing the info
// endpoint can do. On local disk that is a file read. Against an origin it is a
// second request, and the reason for the separate, smaller bound: an ordinary
// info costs one ranged response whatever the image weighs, so a deployment can
// refuse the rare whole-object case well below MaxSourceBytes without making
// the endpoint unusable. When the origin ignored the range and already handed
// over the whole object, that copy is used and no second request happens.
func (s *Server) infoViaVips(ctx context.Context, urlPath string, obj *source.Object, partial *imageinfo.Info) (*imageinfo.Info, error) {
	limit := s.cfg.MaxSourceBytes
	if s.remote && s.cfg.UpstreamInfoMaxBytes > 0 && (limit <= 0 || s.cfg.UpstreamInfoMaxBytes < limit) {
		limit = s.cfg.UpstreamInfoMaxBytes
	}

	if overLimit(obj.Size, limit) {
		return nil, errSourceTooLarge{size: obj.Size, limit: limit}
	}

	// Partial is the source saying "what you can read is a prefix". When it is
	// false the object is already here in full — Peek did not consume it, so
	// ReadAll still starts at byte zero — and no second request is made. Only a
	// ranged upstream response reaches the fetch below.
	var b []byte
	var err error
	if obj.Partial {
		b, err = s.fetchAll(ctx, urlPath, limit)
	} else {
		b, err = readAll(obj, limit)
	}
	if err != nil {
		return nil, err
	}

	data, err := imagedata.NewFromBytes(b)
	if err != nil {
		return nil, err
	}
	defer data.Close()

	s.acquire()
	defer s.release()

	img := new(vips.Image)
	defer img.Clear()

	if err := img.Load(data, 1.0, 0, 1); err != nil {
		return nil, err
	}

	out := *partial

	// Zero width is how Read reports that it never got as far as the geometry.
	if out.ImageWidth == 0 {
		out.ImageWidth = img.Width()
		out.ImageHeight = img.PageHeight()
	}

	// Pages() reads the container's own page count, not the number of frames
	// this load pulled in — the load above asks for one — so it is the real
	// answer for an animated WebP or a GIF longer than the header window.
	if !out.FrameCountKnown {
		out.FrameCount = img.Pages()
		out.FrameCountKnown = true
	}

	// The EXIF libvips found, for the containers whose walk this package does
	// not have: AVIF and HEIC keep it in an ISOBMFF item, JXL in a box, and both
	// come back here as the same bare TIFF block a PNG carries in eXIf. Without
	// this the response has eight fields and — worse — reports the *defaults*
	// for XResolution, YResolution and ResolutionUnit, which is not a gap in the
	// answer but a wrong one.
	if out.Exif == nil {
		if b, err := img.GetBlob(vips.MetaEXIF); err == nil && len(b) > 0 {
			imageinfo.ApplyEXIF(&out, b)
		}
	}

	return &out, nil
}

// ----------------------------------------------------------------- processed

func (s *Server) serveProcessed(w http.ResponseWriter, r *http.Request, chain *ossprocess.Chain, raw string) {
	info, err := s.src.Stat(r.Context(), r.URL.Path)
	if err != nil {
		s.failSource(w, r, err)
		return
	}
	if overLimit(info.Size, s.cfg.MaxSourceBytes) {
		s.failTooLarge(w, r, errSourceTooLarge{size: info.Size, limit: s.cfg.MaxSourceBytes})
		return
	}

	// format,auto makes the output depend on the client, so the negotiated
	// choice has to be part of the cache key — otherwise the first visitor's
	// format is served to everyone.
	auto := chain.IsAutoFormat()
	accept := ""
	negotiated := imagetype.Unknown
	if auto {
		negotiated = negotiateFormat(r.Header.Get("Accept"), vips.SupportsSave)
		accept = acceptKey(negotiated)
		varyOnAccept(w)
	}

	key := s.cacheKey(info, raw+"\x00"+accept)

	if s.cache != nil {
		if e, err := s.cache.Get(key); err == nil {
			defer e.Close()
			s.writeImage(w, r, e, e.Size, "", "HIT")
			return
		} else if !errors.Is(err, cache.ErrMiss) {
			slog.Warn("cache read failed", "error", err)
		}
	}

	// Collapse duplicate work: a page referencing the same transform twelve
	// times should cost one encode, not twelve.
	res, err, shared := s.flight.Do(key.Hash(), func() (any, error) {
		return s.process(r.Context(), r.URL.Path, chain, negotiated)
	})
	if err != nil {
		s.failProcess(w, r, err)
		return
	}

	out := res.(*processed)
	status := "MISS"
	if shared {
		status = "COALESCED"
	}

	if s.cache != nil && !shared {
		if err := s.cache.Put(key, out.data); err != nil {
			// Serving is not blocked by a cache write failure.
			slog.Warn("cache write failed", "error", err)
		}
	}

	s.writeImageBytes(w, r, out.data, out.mime, status)
}

// cacheKey builds the entry key from a source's identity.
//
// A local source is identified by path, size and mtime, as it always was. An
// upstream one has no mtime, so the origin's validator stands in — and when the
// origin sends none, a coarse time bucket does, which is the TTL. Without that
// last clause an origin with no ETag would be cached under a key that never
// changes, and a replaced object would never be picked up.
func (s *Server) cacheKey(info *source.Info, chain string) cache.Key {
	k := cache.Key{
		SourcePath: info.Key,
		SourceSize: info.Size,
		Chain:      chain,
	}

	if !s.remote {
		// Version is the mtime; keeping it in SourceMod leaves the hash of an
		// existing local deployment's keys exactly as it was.
		if mod, err := strconv.ParseInt(info.Version, 10, 64); err == nil {
			k.SourceMod = mod
		}
		return k
	}

	if info.Version != "" {
		k.SourceVersion = info.Version
		return k
	}

	ttl := s.cfg.UpstreamTTL
	if ttl <= 0 {
		ttl = DefaultUpstreamTTL
	}
	k.SourceVersion = "ttl:" + strconv.FormatInt(time.Now().UnixNano()/int64(ttl), 10)
	return k
}

type processed struct {
	data []byte
	mime string
}

// process fetches the source and runs the pipeline over it.
//
// The fetch happens before ProcessTimeout is applied, deliberately. The two
// budgets answer different questions — how long an origin may take, and how
// long an encode may take — and folding them into one would let a slow origin
// consume the time that exists for the work.
func (s *Server) process(ctx context.Context, urlPath string, chain *ossprocess.Chain, negotiated imagetype.Type) (*processed, error) {
	b, err := s.fetchAll(ctx, urlPath, s.cfg.MaxSourceBytes)
	if err != nil {
		return nil, err
	}

	src, err := imagedata.NewFromBytes(b)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	// resize needs the source dimensions to resolve OSS's mode/limit rules. The
	// bytes are already here, so this is a header parse over memory rather than
	// the separate read it used to be — which, against an origin, was a whole
	// extra request.
	var srcW, srcH int
	if chain.NeedsSourceSize() {
		if srcW, srcH, err = s.sourceSize(b, src); err != nil {
			return nil, err
		}
	}

	if s.cfg.ProcessTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.ProcessTimeout)
		defer cancel()
	}

	o := options.New()
	if err := chain.Apply(o, srcW, srcH, src.Format()); err != nil {
		return nil, err
	}
	if chain.IsAutoFormat() {
		if negotiated == imagetype.Unknown {
			// With no acceptable modern format, format,auto means preserve the
			// source format rather than fall through to process-wide preferences
			// — but only when this build can write it.
			if f := autoFallbackFormat(src.Format(), vips.SupportsSave); f != imagetype.Unknown {
				o.Set(keys.Format, f)
			}
		} else {
			applyAutoFormat(o, negotiated)
		}
	}

	s.acquire()
	defer s.release()

	res, err := s.proc.ProcessImage(ctx, src, o)
	if err != nil {
		return nil, err
	}
	defer res.OutData.Close()

	// Copy out of the libvips-owned buffer: OutData.Close() frees it, and the
	// bytes outlive this function in the cache and the response.
	out := append([]byte(nil), imagedata.Bytes(res.OutData)...)

	return &processed{data: out, mime: res.OutData.Format().Mime()}, nil
}

// sourceSize reads the source's pixel dimensions as cheaply as the format
// allows: a header parse for JPEG/PNG/GIF/WebP, a libvips header load otherwise.
func (s *Server) sourceSize(b []byte, data imagedata.ImageData) (int, int, error) {
	info, err := imageinfo.Read(bytes.NewReader(b), int64(len(b)))

	// Same two fallback conditions as the info handler, for the same reasons: a
	// container with no header walk, or a JPEG whose SOF is past the window.
	// Missing the second one here was worse than missing it there — it turned a
	// plain `resize` on an ordinary camera JPEG into a 422, because resize is
	// exactly the action that needs the source dimensions.
	var unsupported imageinfo.ErrUnsupportedContainer
	if errors.As(err, &unsupported) || errors.Is(err, imageinfo.ErrHeaderTooShort) {
		s.acquire()
		defer s.release()

		img := new(vips.Image)
		defer img.Clear()
		if lerr := img.Load(data, 1.0, 0, 1); lerr != nil {
			return 0, 0, lerr
		}
		return img.Width(), img.PageHeight(), nil
	}
	if err != nil {
		return 0, 0, err
	}
	return info.ImageWidth, info.ImageHeight, nil
}

// ------------------------------------------------------------------- helpers

// fetchAll opens an object and reads it into memory under a hard bound.
//
// The bound is applied twice on purpose: once to the length the source declares,
// so an oversized object is refused before a byte of it is read, and once to the
// bytes that actually arrive, because an origin may send no Content-Length at
// all or may send one that is not true. The first check is the cheap one and the
// second is the one that cannot be lied to.
func (s *Server) fetchAll(ctx context.Context, urlPath string, limit int64) ([]byte, error) {
	obj, err := s.src.Open(ctx, urlPath)
	if err != nil {
		return nil, err
	}
	defer obj.Close()

	return readAll(obj, limit)
}

// overLimit reports whether a declared size is over the bound. Zero or negative
// means no bound.
//
// A negative size — the source never declared one — is not a refusal here. It
// cannot be: there is nothing yet to compare. That case is caught on the way in
// instead, by the limit ReadAll enforces on the bytes that actually arrive.
func overLimit(size, limit int64) bool {
	return limit > 0 && size > limit
}

// readAll drains an already-open object under the same bound, translating the
// source's refusal into the one the HTTP layer knows how to answer with.
func readAll(obj *source.Object, limit int64) ([]byte, error) {
	if overLimit(obj.Size, limit) {
		return nil, errSourceTooLarge{size: obj.Size, limit: limit}
	}

	b, err := obj.ReadAll(limit)
	if errors.Is(err, source.ErrTooLarge) {
		// The size was unknown by construction — that is why this path was
		// reached — so the message reports the limit and leaves it at that.
		return nil, errSourceTooLarge{size: -1, limit: limit}
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Server) failTooLarge(w http.ResponseWriter, r *http.Request, e errSourceTooLarge) {
	s.fail(w, r, http.StatusRequestEntityTooLarge, "SourceTooLarge", e.Error())
}

// failFetch answers an error that could have come from either the fetch or the
// size bound, which is the shape every whole-object handler has.
func (s *Server) failFetch(w http.ResponseWriter, r *http.Request, err error) {
	var big errSourceTooLarge
	if errors.As(err, &big) {
		s.failTooLarge(w, r, big)
		return
	}
	s.failSource(w, r, err)
}

// errSourceTooLarge carries the refusal out of a helper that cannot write the
// response itself. failProcess and the info handler turn it back into a 413.
// A negative size means the object never declared one.
type errSourceTooLarge struct{ size, limit int64 }

func (e errSourceTooLarge) Error() string {
	if e.size < 0 {
		return fmt.Sprintf("source exceeds the %d byte limit", e.limit)
	}
	return fmt.Sprintf("source is %d bytes, limit is %d", e.size, e.limit)
}

func (e errSourceTooLarge) StatusCode() int { return http.StatusRequestEntityTooLarge }

// acquire/release bound concurrent libvips work.
func (s *Server) acquire() { s.sem <- struct{}{} }
func (s *Server) release() { <-s.sem }

func (s *Server) writeImage(w http.ResponseWriter, r *http.Request, e *cache.Entry, size int64, mime, cacheStatus string) {
	if mime == "" {
		if t, err := imagetype.Detect(e.File, "", ""); err == nil {
			mime = t.Mime()
		}
		e.Seek(0, 0)
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", s.cfg.CacheControl)
	w.Header().Set("X-IStore-Cache", cacheStatus)
	if r.Method == http.MethodHead {
		return
	}
	e.WriteTo(w)
}

func (s *Server) writeImageBytes(w http.ResponseWriter, r *http.Request, b []byte, mime, cacheStatus string) {
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", s.cfg.CacheControl)
	w.Header().Set("X-IStore-Cache", cacheStatus)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(b)
}

func (s *Server) health(w http.ResponseWriter) {
	if err := vips.Health(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok\n"))
}

// ossError mirrors the error envelope OSS returns, so a client written against
// OSS can parse iStore's failures with the same code.
type ossError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	slog.Debug("request failed", "path", r.URL.Path, "status", status, "code", code, "message", msg)
	body, _ := json.Marshal(ossError{Code: code, Message: msg})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

func (s *Server) failSource(w http.ResponseWriter, r *http.Request, err error) {
	// An origin failure keeps the origin's own status wherever passing it
	// through says something true to the client, and becomes 502 or 504 where
	// iStore had to invent one. What the origin actually said, and which URL it
	// was, stay in the log: neither is the client's business.
	var up *source.UpstreamError
	if errors.As(err, &up) {
		slog.Warn("upstream fetch failed",
			"path", r.URL.Path, "url", up.URL, "origin_status", up.Status, "error", up.Err)
		s.fail(w, r, up.StatusCode(), "UpstreamError", up.PublicMessage())
		return
	}

	switch {
	case errors.Is(err, source.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
	case errors.Is(err, source.ErrOutsideRoot), errors.Is(err, source.ErrNotRegular):
		// Deliberately indistinguishable from a miss: telling a prober that a
		// path exists but is out of bounds maps the filesystem for them.
		s.fail(w, r, http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
	default:
		slog.Error("source error", "path", r.URL.Path, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "InternalError", "could not read the source object")
	}
}

func (s *Server) failProcess(w http.ResponseWriter, r *http.Request, err error) {
	// An argument the parser could only check against the source (indexcrop's
	// slice index) is still the caller's mistake, so it gets 400 and the real
	// message rather than a generic "could not be processed".
	var argErr ossprocess.ArgumentError
	if errors.As(err, &argErr) {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", argErr.Error())
		return
	}
	if errors.Is(err, ErrBadWatermark) {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", ErrBadWatermark.Error())
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		s.fail(w, r, http.StatusGatewayTimeout, "RequestTimeout", "image processing timed out")
		return
	}
	// A failure to get the bytes at all is not a processing failure, and its
	// status comes from the source rather than the pipeline.
	var big errSourceTooLarge
	var up *source.UpstreamError
	if errors.As(err, &big) || errors.As(err, &up) || errors.Is(err, source.ErrNotFound) {
		s.failFetch(w, r, err)
		return
	}
	// The pipeline's own errors carry a status code and a public message;
	// anything else is ours and gets a generic 422.
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) {
		var pub interface{ PublicMessage() string }
		msg := "the image could not be processed"
		if errors.As(err, &pub) && pub.PublicMessage() != "" {
			msg = pub.PublicMessage()
		}
		s.fail(w, r, coded.StatusCode(), "InvalidImage", msg)
		return
	}
	slog.Error("processing failed", "path", r.URL.Path, "error", err)
	s.fail(w, r, http.StatusUnprocessableEntity, "InvalidImage", "the image could not be processed")
}

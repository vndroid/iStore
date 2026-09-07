// Package httpserver wires the OSS grammar, the local source, the cache and the
// ported imgproxy pipeline into an HTTP handler.
//
// Request shape:
//
//	GET /path/to/image.jpg                              -> the source, untouched
//	GET /path/to/image.jpg?x-oss-process=image/format,avif
//	GET /path/to/image.jpg?x-oss-process=image/info     -> JSON
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/kane/istore/internal/cache"
	"github.com/kane/istore/internal/imagedata"
	"github.com/kane/istore/internal/imageinfo"
	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/ossprocess"
	"github.com/kane/istore/internal/processing"
	"github.com/kane/istore/internal/singleflight"
	"github.com/kane/istore/internal/source"
	"github.com/kane/istore/internal/vips"
)

// Config holds the knobs the HTTP layer itself needs. Image-processing config
// lives in processing.Config / vips.Config.
type Config struct {
	// Root is the directory images are served from.
	Root string
	// CacheDir stores transcoded results. Empty disables caching, which is only
	// sensible for testing — see the package comment in internal/cache.
	CacheDir string
	// MaxSourceBytes rejects source files larger than this before opening them.
	MaxSourceBytes int64
	// Concurrency caps simultaneous image processing. Zero means GOMAXPROCS.
	Concurrency int
	// ProcessTimeout bounds one transcode.
	ProcessTimeout time.Duration
	// CacheControl is sent with successful image responses.
	CacheControl string
}

// NewDefaultConfig returns usable defaults.
func NewDefaultConfig() Config {
	return Config{
		MaxSourceBytes: 100 << 20,
		ProcessTimeout: 20 * time.Second,
		CacheControl:   "public, max-age=31536000, immutable",
	}
}

// Server is the HTTP handler.
type Server struct {
	cfg   Config
	src   *source.Local
	cache *cache.Disk
	proc  *processing.Processor

	// sem bounds concurrent encodes. libvips is happy to use every core for one
	// image; letting N requests each do that turns a small box into a queue with
	// extra steps.
	sem    chan struct{}
	flight singleflight.Group
}

// New builds a Server. proc must already be constructed with a validated config.
func New(cfg Config, proc *processing.Processor) (*Server, error) {
	src, err := source.NewLocal(cfg.Root)
	if err != nil {
		return nil, err
	}

	var c *cache.Disk
	if cfg.CacheDir != "" {
		if c, err = cache.NewDisk(cfg.CacheDir); err != nil {
			return nil, err
		}
	}

	n := cfg.Concurrency
	if n <= 0 {
		n = 1
	}

	return &Server{
		cfg:   cfg,
		src:   src,
		cache: c,
		proc:  proc,
		sem:   make(chan struct{}, n),
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET and HEAD are supported")
		return
	}

	if r.URL.Path == "/healthz" {
		s.health(w)
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
	case chain.IsEmpty():
		s.serveOriginal(w, r)
	default:
		s.serveProcessed(w, r, chain, raw)
	}
}

// ------------------------------------------------------------------ original

func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request) {
	f, err := s.src.Open(r.URL.Path)
	if err != nil {
		s.failSource(w, r, err)
		return
	}
	defer f.Close()

	if t, err := detectType(f); err == nil {
		w.Header().Set("Content-Type", t.Mime())
	}
	if _, err := f.Seek(0, 0); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "InternalError", "seek failed")
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Cache-Control", s.cfg.CacheControl)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := f.WriteTo(w); err != nil {
		slog.Debug("write failed", "path", r.URL.Path, "error", err)
	}
}

// ---------------------------------------------------------------------- info

func (s *Server) serveInfo(w http.ResponseWriter, r *http.Request) {
	f, err := s.src.Open(r.URL.Path)
	if err != nil {
		s.failSource(w, r, err)
		return
	}
	defer f.Close()

	info, err := imageinfo.Read(f, f.Size)

	// imageinfo parses JPEG, PNG, GIF and WebP headers directly, which is the
	// cheap path. Formats it does not parse (AVIF, HEIC, JXL, TIFF, BMP) fall
	// back to libvips, which costs a header load but is still far short of a
	// full decode.
	var unsupported imageinfo.ErrUnsupportedContainer
	if errors.As(err, &unsupported) {
		info, err = s.infoViaVips(f, info)
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

// infoViaVips fills in dimensions and frame count for containers imageinfo does
// not parse. It reuses the partially-filled Info (format and file size are
// already right) so only the pixel geometry comes from libvips.
func (s *Server) infoViaVips(f *source.File, partial *imageinfo.Info) (*imageinfo.Info, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	data, err := imagedata.NewFromFile(f.Path)
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
	out.ImageWidth = img.Width()
	out.ImageHeight = img.PageHeight()
	out.FrameCount = img.Pages()
	return &out, nil
}

// ----------------------------------------------------------------- processed

func (s *Server) serveProcessed(w http.ResponseWriter, r *http.Request, chain *ossprocess.Chain, raw string) {
	path, st, err := s.src.Stat(r.URL.Path)
	if err != nil {
		s.failSource(w, r, err)
		return
	}
	if s.cfg.MaxSourceBytes > 0 && st.Size() > s.cfg.MaxSourceBytes {
		s.fail(w, r, http.StatusRequestEntityTooLarge, "SourceTooLarge",
			fmt.Sprintf("source is %d bytes, limit is %d", st.Size(), s.cfg.MaxSourceBytes))
		return
	}

	key := cache.Key{
		SourcePath: path,
		SourceSize: st.Size(),
		SourceMod:  st.ModTime().UnixNano(),
		Chain:      raw,
	}

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
		return s.process(r.Context(), path, chain)
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

type processed struct {
	data []byte
	mime string
}

func (s *Server) process(ctx context.Context, path string, chain *ossprocess.Chain) (*processed, error) {
	if s.cfg.ProcessTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.ProcessTimeout)
		defer cancel()
	}

	src, err := imagedata.NewFromFile(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	o := options.New()
	if err := chain.Apply(o); err != nil {
		return nil, err
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
	b := append([]byte(nil), imagedata.Bytes(res.OutData)...)

	return &processed{data: b, mime: res.OutData.Format().Mime()}, nil
}

// ------------------------------------------------------------------- helpers

// acquire/release bound concurrent libvips work.
func (s *Server) acquire() { s.sem <- struct{}{} }
func (s *Server) release() { <-s.sem }

func (s *Server) writeImage(w http.ResponseWriter, r *http.Request, e *cache.Entry, size int64, mime, cacheStatus string) {
	if mime == "" {
		if t, err := detectType(e.File); err == nil {
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

func detectType(r interface {
	Read([]byte) (int, error)
}) (imagetype.Type, error) {
	return imagetype.Detect(r, "", "")
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
	if errors.Is(err, context.DeadlineExceeded) {
		s.fail(w, r, http.StatusGatewayTimeout, "RequestTimeout", "image processing timed out")
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

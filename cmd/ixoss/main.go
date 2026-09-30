// Command ixoss serves images from a local directory, transcoding on demand
// through the Alibaba Cloud OSS `x-oss-process` grammar.
//
//	GET /photo.jpg
//	GET /photo.jpg?x-oss-process=image/format,avif
//	GET /photo.jpg?x-oss-process=image/info
//
// Configuration is environment-only; see README.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/vndroid/iXoSS/internal/auximageprovider"
	"github.com/vndroid/iXoSS/internal/cache"
	"github.com/vndroid/iXoSS/internal/httpserver"
	"github.com/vndroid/iXoSS/internal/imagetype"
	"github.com/vndroid/iXoSS/internal/ossprocess"
	"github.com/vndroid/iXoSS/internal/processing"
	"github.com/vndroid/iXoSS/internal/security"
	"github.com/vndroid/iXoSS/internal/vips"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(),
	})))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Exactly one source. Accepting both and preferring one would let an
	// operator who added IXOSS_UPSTREAM to a unit file that still carries
	// IXOSS_ROOT believe they had switched over while every request was still
	// being answered off the old disk.
	root := env("IXOSS_ROOT", "")
	upstream := env("IXOSS_UPSTREAM", "")
	switch {
	case root == "" && upstream == "":
		return errors.New("one of IXOSS_ROOT or IXOSS_UPSTREAM is required: " +
			"the directory images are served from, or the origin they are fetched from")
	case root != "" && upstream != "":
		return errors.New("IXOSS_ROOT and IXOSS_UPSTREAM are mutually exclusive: set one, not both")
	}

	vc := vips.NewDefaultConfig()
	if _, err := vips.LoadConfigFromEnv(&vc); err != nil {
		return err
	}
	if err := vips.Init(&vc); err != nil {
		return err
	}
	defer vips.Shutdown()

	// Init has just tried a real encode of every format, so this is the true
	// list, not the list of savers that happen to be compiled in. Worth a line
	// at startup: "avif is missing" is much easier to act on here than as a 400
	// from production, and the two builds that produce it — libheif without an
	// AV1 encoder, libvips without libjxl — look identical from the outside.
	slog.Info("encodable formats", "formats", formatNames(vips.SaveableTypes()))

	sc := security.NewDefaultConfig()
	if _, err := security.LoadConfigFromEnv(&sc); err != nil {
		return err
	}
	checker, err := security.New(&sc)
	if err != nil {
		return err
	}

	pc := processing.NewDefaultConfig()
	if _, err := processing.LoadConfigFromEnv(&pc); err != nil {
		return err
	}
	hc := httpserver.NewDefaultConfig()
	hc.Root = root
	hc.Upstream = upstream
	hc.UpstreamTimeout = time.Duration(envInt("IXOSS_UPSTREAM_TIMEOUT_MS",
		int(hc.UpstreamTimeout/time.Millisecond))) * time.Millisecond
	hc.UpstreamTTL = time.Duration(envInt("IXOSS_UPSTREAM_TTL_SEC",
		int(hc.UpstreamTTL/time.Second))) * time.Second
	hc.UpstreamInfoMaxBytes = int64(envInt("IXOSS_UPSTREAM_INFO_MAX_BYTES", int(hc.UpstreamInfoMaxBytes)))
	hc.MaxWatermarkBytes = int64(envInt("IXOSS_MAX_WATERMARK_BYTES", int(hc.MaxWatermarkBytes)))
	hc.WatermarkCacheBytes = int64(envInt("IXOSS_WATERMARK_CACHE_BYTES", int(hc.WatermarkCacheBytes)))
	// Megapixels, to match IXOSS_MAX_SRC_RESOLUTION's units rather than sit
	// next to it meaning something else.
	hc.MaxWatermarkResolution = envInt("IXOSS_MAX_WATERMARK_RESOLUTION",
		hc.MaxWatermarkResolution/1_000_000) * 1_000_000
	hc.CacheDir = env("IXOSS_CACHE_DIR", "")
	hc.StylesFile = env("IXOSS_STYLES", "")
	hc.PassthroughNonImages = envBool("IXOSS_PASSTHROUGH_NON_IMAGES", false)
	hc.Concurrency = envInt("IXOSS_CONCURRENCY", runtime.GOMAXPROCS(0))
	hc.MaxSourceBytes = int64(envInt("IXOSS_MAX_SOURCE_BYTES", int(hc.MaxSourceBytes)))
	hc.ProcessTimeout = time.Duration(envInt("IXOSS_PROCESS_TIMEOUT_MS", int(hc.ProcessTimeout/time.Millisecond))) * time.Millisecond
	hc.CacheControl = env("IXOSS_CACHE_CONTROL", hc.CacheControl)
	hc.Verifier = checker
	hc.HeaderChecker = checker
	hc.Evict = cache.EvictConfig{
		MaxBytes: int64(envInt("IXOSS_CACHE_MAX_BYTES", 0)),
		MaxAge:   time.Duration(envInt("IXOSS_CACHE_MAX_AGE_HOURS", 0)) * time.Hour,
		Interval: time.Duration(envInt("IXOSS_CACHE_EVICT_INTERVAL_MIN", 10)) * time.Minute,
	}

	if hc.CacheDir == "" {
		slog.Warn("IXOSS_CACHE_DIR is unset: every request re-encodes its image. " +
			"One AVIF encode of a 2400x1350 frame costs roughly 150ms of CPU.")
	}

	if !checker.SignatureEnabled() {
		if upstream != "" {
			// Worth saying differently in upstream mode. Unsigned local mode
			// exposes a directory; unsigned upstream mode exposes the origin,
			// to anyone who can reach this port, with a transform chain of their
			// choosing attached — which is a way to spend the origin's bandwidth
			// and this box's CPU at the same time.
			slog.Warn("IXOSS_KEY / IXOSS_SALT are unset: requests are not signed, so anyone who " +
				"can reach this port can drive arbitrary transforms of any object on the upstream origin.")
		} else {
			slog.Warn("IXOSS_KEY / IXOSS_SALT are unset: requests are not signed, " +
				"so anyone who can reach this port can ask for any transform of any object under IXOSS_ROOT.")
		}
	}

	srv, err := httpserver.New(hc, func(wm auximageprovider.Provider) (*processing.Processor, error) {
		return processing.New(&pc, checker, wm)
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	addr := env("IXOSS_BIND", ":8080")
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening",
			"addr", addr,
			"source", srv.SourceDescription(),
			"mode", sourceMode(upstream),
			"cache", orNone(hc.CacheDir),
			"concurrency", hc.Concurrency)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-stop:
		slog.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(ctx)
	}
}

// formatNames spells a list of types the way a request would, so what the log
// prints is what a caller would put after `format,`.
func formatNames(types []imagetype.Type) []string {
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = ossprocess.OSSFormatName(t)
	}
	return out
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv(name); ok {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
		slog.Warn("ignoring unparseable value", "name", name, "value", v)
	}
	return def
}

func envBool(name string, def bool) bool {
	if v, ok := os.LookupEnv(name); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		slog.Warn("ignoring unparseable value", "name", name, "value", v)
	}
	return def
}

func logLevel() slog.Level {
	switch env("IXOSS_LOG_LEVEL", "info") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func sourceMode(upstream string) string {
	if upstream != "" {
		return "upstream"
	}
	return "local"
}

func orNone(s string) string {
	if s == "" {
		return "(disabled)"
	}
	return s
}

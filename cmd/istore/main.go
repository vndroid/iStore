// Command istore serves images from a local directory, transcoding on demand
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

	"github.com/kane/istore/internal/auximageprovider"
	"github.com/kane/istore/internal/cache"
	"github.com/kane/istore/internal/httpserver"
	"github.com/kane/istore/internal/processing"
	"github.com/kane/istore/internal/security"
	"github.com/kane/istore/internal/vips"
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
	root := env("ISTORE_ROOT", "")
	if root == "" {
		return errors.New("ISTORE_ROOT is required: the directory images are served from")
	}

	vc := vips.NewDefaultConfig()
	if _, err := vips.LoadConfigFromEnv(&vc); err != nil {
		return err
	}
	if err := vips.Init(&vc); err != nil {
		return err
	}
	defer vips.Shutdown()

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
	hc.CacheDir = env("ISTORE_CACHE_DIR", "")
	hc.Concurrency = envInt("ISTORE_CONCURRENCY", runtime.GOMAXPROCS(0))
	hc.MaxSourceBytes = int64(envInt("ISTORE_MAX_SOURCE_BYTES", int(hc.MaxSourceBytes)))
	hc.ProcessTimeout = time.Duration(envInt("ISTORE_PROCESS_TIMEOUT_MS", int(hc.ProcessTimeout/time.Millisecond))) * time.Millisecond
	hc.CacheControl = env("ISTORE_CACHE_CONTROL", hc.CacheControl)
	hc.Verifier = checker
	hc.Evict = cache.EvictConfig{
		MaxBytes: int64(envInt("ISTORE_CACHE_MAX_BYTES", 0)),
		MaxAge:   time.Duration(envInt("ISTORE_CACHE_MAX_AGE_HOURS", 0)) * time.Hour,
		Interval: time.Duration(envInt("ISTORE_CACHE_EVICT_INTERVAL_MIN", 10)) * time.Minute,
	}

	if hc.CacheDir == "" {
		slog.Warn("ISTORE_CACHE_DIR is unset: every request re-encodes its image. " +
			"One AVIF encode of a 2400x1350 frame costs roughly 150ms of CPU.")
	}

	if !checker.SignatureEnabled() {
		slog.Warn("ISTORE_KEY / ISTORE_SALT are unset: requests are not signed, " +
			"so anyone who can reach this port can ask for any transform of any object under ISTORE_ROOT.")
	}

	srv, err := httpserver.New(hc, func(wm auximageprovider.Provider) (*processing.Processor, error) {
		return processing.New(&pc, checker, wm)
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	addr := env("ISTORE_BIND", ":8080")
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
			"root", root,
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

func logLevel() slog.Level {
	switch env("ISTORE_LOG_LEVEL", "info") {
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

func orNone(s string) string {
	if s == "" {
		return "(disabled)"
	}
	return s
}

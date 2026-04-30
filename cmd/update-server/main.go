// Command update-server runs the OTA update server: HTTP + CoAP transports,
// signed-manifest generation, bounded delta generation, fsnotify auto-reload
// and bearer-protected /admin/* control plane. All behavior is driven by a
// YAML config — see configs/server.yaml for the full schema.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	coapnet "github.com/plgd-dev/go-coap/v3/net"
	"github.com/plgd-dev/go-coap/v3/options"
	"github.com/plgd-dev/go-coap/v3/udp"

	"github.com/amplia/ota-updater/pkg/crypto"
	"github.com/amplia/ota-updater/internal/server"
)

func main() {
	cfgPath := flag.String("config", "./configs/server.yaml", "path to server YAML config")
	flag.Parse()

	if err := run(*cfgPath); err != nil {
		slog.Error("update-server startup failed", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := server.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logging, err := server.NewLogging(cfg.Logging)
	if err != nil {
		return fmt.Errorf("init logging: %w", err)
	}
	logger := logging.Logger()
	slog.SetDefault(logger)

	priv, err := crypto.LoadPrivateKey(cfg.Crypto.PrivateKey)
	if err != nil {
		return fmt.Errorf("load private key: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metrics := server.NewMetrics()

	store, err := server.Open(ctx, server.StoreOptions{
		BinariesDir:          cfg.Store.BinariesDir,
		DeltasDir:            cfg.Store.DeltasDir,
		TargetPath:           cfg.Target.Binary,
		TargetMaxMemoryBytes: int64(cfg.Store.TargetMaxMemoryMB) << 20,
		HotDeltaCacheBytes:   int64(cfg.Store.HotDeltaCacheMB) << 20,
		DeltaConcurrency:     cfg.Store.DeltaConcurrency,
		DiskSpaceMinFreePct:  cfg.Store.DiskSpaceMinFreePct,
		DiskSpaceMinFreeMB:   cfg.Store.DiskSpaceMinFreeMB,
		Metrics:              metrics,
	}, logger)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	manifester := server.NewManifester(store, priv, server.ManifesterConfig{
		ChunkSize:     cfg.Manifest.ChunkSize,
		RetryAfter:    cfg.Manifest.RetryAfter,
		TargetVersion: cfg.Target.Version,
		CacheSize:     cfg.Manifest.CacheSize,
		Metrics:       metrics,
	}, logger)

	// fsnotify-based auto-reload of the target binary. Tracked so the main
	// shutdown sequence can wait for it.
	watcher := server.NewWatcher(cfg.Target.Binary, server.DefaultWatcherDebounce, func() {
		reloadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := store.Reload(reloadCtx); err != nil {
			logger.Error("auto-reload failed", "op", "watcher_reload", "err", err)
			return
		}
		manifester.Invalidate()
		logger.Info("auto-reload applied",
			"op", "watcher_reload", "target_hash", store.TargetHash(),
		)
	}, logger)
	var goroutines sync.WaitGroup
	goroutines.Add(1)
	go func() {
		defer goroutines.Done()
		if err := watcher.Run(ctx); err != nil {
			logger.Error("watcher exited", "op", "watcher", "err", err)
		}
	}()

	// HTTP: main API + admin under the same listener.
	rootMux := http.NewServeMux()
	server.RegisterAdminHandlers(rootMux, server.AdminDeps{
		Token:           cfg.Admin.Token,
		Store:           store,
		Manifester:      manifester,
		Logging:         logging,
		Logger:          logger,
		Metrics:         metrics,
		RateLimitPerSec: cfg.Admin.RateLimitPerSec,
		RateLimitBurst:  cfg.Admin.RateLimitBurst,
	})
	apiHandler := server.NewHTTPHandler(server.HTTPConfig{
		Store: store, Manifester: manifester, Logger: logger, Metrics: metrics,
		BinariesDir: cfg.Store.BinariesDir,
	})
	// Catch-all: anything not matched by /admin/* goes through the API mux
	// (which has its own method+path patterns and panic recovery).
	rootMux.Handle("/", apiHandler)

	httpServer := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           rootMux,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	httpErrCh := make(chan error, 1)
	go func() {
		logger.Info("http listening", "op", "startup", "addr", cfg.HTTP.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrCh <- err
		}
		close(httpErrCh)
	}()

	// CoAP server on UDP.
	coapRouter, err := server.NewCoAPRouter(server.CoAPConfig{
		Store: store, Manifester: manifester, Logger: logger, Metrics: metrics,
	})
	if err != nil {
		return fmt.Errorf("coap router: %w", err)
	}
	coapListener, err := coapnet.NewListenUDP("udp", cfg.CoAP.Addr)
	if err != nil {
		return fmt.Errorf("coap listen: %w", err)
	}
	coapServer := udp.NewServer(options.WithMux(coapRouter))
	coapErrCh := make(chan error, 1)
	go func() {
		logger.Info("coap listening", "op", "startup", "addr", cfg.CoAP.Addr)
		if err := coapServer.Serve(coapListener); err != nil {
			coapErrCh <- err
		}
		close(coapErrCh)
	}()

	// Optional observability listener (Prometheus /metrics + /debug/pprof).
	// Bound to a separate address — expected to be loopback or private net.
	var metricsServer *http.Server
	if cfg.Metrics.Addr != "" {
		obsMux := http.NewServeMux()
		obsMux.Handle("/metrics", metrics.Handler())
		if cfg.Metrics.PprofEnabled {
			server.RegisterPprof(obsMux)
			logger.Warn("pprof endpoints enabled on observability listener",
				"op", "startup", "addr", cfg.Metrics.Addr,
				"note", "expose only on loopback or private net",
			)
		}
		metricsServer = &http.Server{
			Addr:              cfg.Metrics.Addr,
			Handler:           obsMux,
			ReadHeaderTimeout: 5 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		}
		goroutines.Add(1)
		go func() {
			defer goroutines.Done()
			logger.Info("observability listening", "op", "startup", "addr", cfg.Metrics.Addr, "pprof", cfg.Metrics.PprofEnabled)
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("observability listener exited", "op", "shutdown", "err", err)
			}
		}()
	}

	// Wait for either a signal or a server failure.
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested", "op", "shutdown")
	case err := <-httpErrCh:
		if err != nil {
			logger.Error("http server exited", "op", "shutdown", "err", err)
		}
	case err := <-coapErrCh:
		if err != nil {
			logger.Error("coap server exited", "op", "shutdown", "err", err)
		}
	}

	// Ordered shutdown, all bounded by the same timeout:
	//   1. stop HTTP + CoAP (no more incoming requests / delta generations)
	//   2. store.Close waits for in-flight bsdiff goroutines
	//   3. watcher.Run observes ctx.Done and returns; wait on its goroutine
	// bsdiff is NOT ctx-cancellable, so (2) may log "close timed out" if a
	// generation is still running when the deadline expires. That just means
	// one .tmp-* file may be orphaned in deltasDir; the dir is swept on next
	// boot (planned, not yet implemented — PR-E item 7).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "op", "shutdown", "err", err)
	}
	coapServer.Stop()
	_ = coapListener.Close()
	if metricsServer != nil {
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("metrics shutdown", "op", "shutdown", "err", err)
		}
	}
	if err := store.Close(shutdownCtx); err != nil {
		logger.Error("store close", "op", "shutdown", "err", err)
	}
	// Wait for the watcher goroutine. Run returns as soon as ctx is done
	// because we cancelled the root ctx above; if for some reason it doesn't,
	// the shutdown timeout kicks in at the main() level.
	waitCh := make(chan struct{})
	go func() {
		goroutines.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-shutdownCtx.Done():
		logger.Warn("shutdown timed out waiting for helper goroutines", "op", "shutdown")
	}
	logger.Info("shutdown complete", "op", "shutdown")
	return nil
}

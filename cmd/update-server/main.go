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

	"github.com/carlosprados/ota-updater/pkg/crypto"
	"github.com/carlosprados/ota-updater/pkg/protocol"
	"github.com/carlosprados/ota-updater/pkg/server"
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
		BinariesDir:         cfg.Store.BinariesDir,
		DeltasDir:           cfg.Store.DeltasDir,
		TargetCacheBytes:    int64(cfg.Store.TargetCacheMB) << 20,
		HotDeltaCacheBytes:  int64(cfg.Store.HotDeltaCacheMB) << 20,
		DeltaConcurrency:    cfg.Store.DeltaConcurrency,
		DiskSpaceMinFreePct: cfg.Store.DiskSpaceMinFreePct,
		DiskSpaceMinFreeMB:  cfg.Store.DiskSpaceMinFreeMB,
		Metrics:             metrics,
	}, logger)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// The registry needs to notify the manifester, and the manifester needs
	// to query the registry. The cycle is broken by capturing the manifester
	// by reference: OnChange only ever fires from Publish, which happens
	// after both are constructed.
	var manifester *server.Manifester
	registry, err := server.NewRegistry(store, server.RegistryOptions{
		StatePath:       cfg.Store.StateFile,
		HistoryDepth:    cfg.Retention.HistoryDepth,
		DefaultArtifact: cfg.DefaultArtifact,
		Metrics:         metrics,
		OnChange: func(key protocol.ArtifactKey) {
			if manifester != nil {
				manifester.InvalidateArtifact(key)
			}
		},
	}, logger)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}

	manifester = server.NewManifester(store, registry, priv, server.ManifesterConfig{
		ChunkSize:         cfg.Manifest.ChunkSize,
		RetryAfter:        cfg.Manifest.RetryAfter,
		CacheSize:         cfg.Manifest.CacheSize,
		AllowFullDownload: cfg.AllowFullDownload(),
		Metrics:           metrics,
	}, logger)

	// Config is authoritative for the artifacts it declares: re-publish them
	// on every boot so an operator edit always wins over persisted state.
	if err := registry.ReconcileConfig(ctx, cfg.Artifacts); err != nil {
		return fmt.Errorf("reconcile artifacts: %w", err)
	}

	var goroutines sync.WaitGroup

	// One fsnotify watcher per file-backed artifact. Tracked so the main
	// shutdown sequence can wait for them.
	for key, path := range registry.WatchedSources() {
		key, path := key, path
		watcher := server.NewWatcher(path, server.DefaultWatcherDebounce, func() {
			if _, err := registry.Republish(key); err != nil {
				logger.Error("auto-reload failed",
					"op", "watcher_reload", "artifact", key.String(), "err", err)
				return
			}
			logger.Info("auto-reload applied",
				"op", "watcher_reload", "artifact", key.String())
		}, logger)
		goroutines.Add(1)
		go func() {
			defer goroutines.Done()
			if err := watcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("watcher exited",
					"op", "watcher", "artifact", key.String(), "err", err)
			}
		}()
	}

	// Retention sweeper. Off unless explicitly enabled — reclaiming disk by
	// deleting an operator's binaries is not a sensible default.
	var retention *server.Retention
	if cfg.Retention.Enabled {
		retention = server.NewRetention(store, registry, server.RetentionOptions{
			Interval:              cfg.Retention.Interval,
			DeltaMaxAge:           cfg.Retention.DeltaMaxAge,
			DeltasMaxTotalBytes:   int64(cfg.Retention.DeltasMaxTotalMB) << 20,
			CollectOrphanBinaries: cfg.Retention.CollectOrphanBinaries,
			OrphanBinaryMinAge:    cfg.Retention.OrphanBinaryMinAge,
			Metrics:               metrics,
		}, logger)
		goroutines.Add(1)
		go func() {
			defer goroutines.Done()
			if err := retention.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("retention exited", "op", "retention", "err", err)
			}
		}()
	} else {
		logger.Info("retention sweeper disabled; store grows without bound",
			"op", "startup", "hint", "set retention.enabled to reclaim disk")
	}

	// HTTP: main API + admin under the same listener.
	rootMux := http.NewServeMux()
	server.RegisterAdminHandlers(rootMux, server.AdminDeps{
		Token:           cfg.Admin.Token,
		Store:           store,
		Registry:        registry,
		Manifester:      manifester,
		Retention:       retention,
		Logging:         logging,
		Logger:          logger,
		Metrics:         metrics,
		RateLimitPerSec: cfg.Admin.RateLimitPerSec,
		RateLimitBurst:  cfg.Admin.RateLimitBurst,
	})
	apiHandler := server.NewHTTPHandler(server.HTTPConfig{
		Store: store, Registry: registry, Manifester: manifester,
		Logger: logger, Metrics: metrics,
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
		Store: store, Registry: registry, Manifester: manifester,
		Logger: logger, Metrics: metrics,
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

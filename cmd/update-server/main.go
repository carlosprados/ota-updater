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

	store, err := server.Open(ctx, cfg.Store.BinariesDir, cfg.Store.DeltasDir, cfg.Target.Binary, logger)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	manifester := server.NewManifester(store, priv, server.ManifesterConfig{
		ChunkSize:     cfg.Manifest.ChunkSize,
		RetryAfter:    cfg.Manifest.RetryAfter,
		TargetVersion: cfg.Target.Version,
	}, logger)

	// fsnotify-based auto-reload of the target binary.
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
	go func() {
		if err := watcher.Run(ctx); err != nil {
			logger.Error("watcher exited", "op", "watcher", "err", err)
		}
	}()

	// HTTP: main API + admin under the same listener.
	rootMux := http.NewServeMux()
	server.RegisterAdminHandlers(rootMux, server.AdminDeps{
		Token:      cfg.Admin.Token,
		Store:      store,
		Manifester: manifester,
		Logging:    logging,
		Logger:     logger,
	})
	apiHandler := server.NewHTTPHandler(server.HTTPConfig{
		Store: store, Manifester: manifester, Logger: logger,
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
		Store: store, Manifester: manifester, Logger: logger,
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "op", "shutdown", "err", err)
	}
	coapServer.Stop()
	_ = coapListener.Close()
	logger.Info("shutdown complete", "op", "shutdown")
	return nil
}

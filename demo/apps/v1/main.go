// Demo app v1 — baseline for the OTA updater demo.
//
// Every demo binary (v1, v2, v3) embeds pkg/agent as a library: the HTTP
// server on :7000 is the "product", the agent runs in a goroutine and
// self-replaces the process via syscall.Exec when a new version lands on
// the update-server. This file is intentionally self-contained so the
// audience can read all of it in one screen during the demo.
package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/amplia/ota-updater/pkg/agent"
	"github.com/amplia/ota-updater/pkg/crypto"
	"github.com/amplia/ota-updater/pkg/protocol"
)

const (
	version        = "1.0.0"
	color          = "#1e3a5f" // blue
	title          = "Initial release"
	bannerHTTPAddr = "127.0.0.1:7000"
)

var started = time.Now()

var page = template.Must(template.New("").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="2">
<title>OTA demo · v{{.Version}}</title>
<style>
  html,body{margin:0;height:100%}
  body{background:{{.Color}};color:#fff;font-family:system-ui,sans-serif;
       display:flex;flex-direction:column;align-items:center;justify-content:center;text-align:center}
  h1{font-size:6em;margin:0;letter-spacing:.05em}
  p{font-size:1.4em;margin:.3em 0}
  code{background:rgba(0,0,0,.25);padding:.1em .4em;border-radius:.25em}
</style></head>
<body>
  <h1>v{{.Version}}</h1>
  <p>{{.Title}}</p>
  <p><code>pid={{.PID}}</code> · <code>uptime={{.Uptime}}</code></p>
</body></html>`))

func main() {
	cfgPath := flag.String("config", "./configs/agent.yaml", "agent config path")
	flag.Parse()

	cfg, err := agent.LoadConfig(*cfgPath)
	if err != nil {
		fatal("load config: %v", err)
	}
	logging, err := agent.NewLogger(cfg.Logging)
	if err != nil {
		fatal("init logging: %v", err)
	}
	logger := logging.Logger()
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	bannerSrv := &http.Server{
		Addr:              bannerHTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("demo banner HTTP listening",
			"op", "demo", "version", version, "addr", bannerHTTPAddr,
		)
		if err := bannerSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Warn("banner HTTP ended", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	updater := buildUpdater(cfg, logger)
	logger.Info("demo app ready",
		"op", "startup", "version", version,
		"device_id", cfg.Device.ID, "http_addr", bannerHTTPAddr,
	)
	if err := updater.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("updater run", "err", err)
		_ = bannerSrv.Shutdown(context.Background())
		os.Exit(1)
	}
	_ = bannerSrv.Shutdown(context.Background())
	logger.Info("demo app stopped", "op", "shutdown", "version", version)
}

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, map[string]any{
		"Version": version,
		"Color":   color,
		"Title":   title,
		"PID":     os.Getpid(),
		"Uptime":  time.Since(started).Truncate(time.Second),
	})
}

// buildUpdater wires pkg/agent the same way cmd/edge-agent does. Kept inline
// so the demo shows exactly what a third-party binary needs to embed the
// auto-update loop: load config, build slot manager + boot counter +
// watchdog + updater, run.
func buildUpdater(cfg *agent.Config, logger *slog.Logger) *agent.Updater {
	pubKey, err := crypto.LoadPublicKey(cfg.Crypto.PublicKey)
	if err != nil {
		fatal("load public key: %v", err)
	}
	slots, err := agent.NewSlotManager(cfg.Device.SlotsDir, cfg.Device.ActiveSymlink, logger)
	if err != nil {
		fatal("slot manager: %v", err)
	}
	bootCounter, err := agent.NewBootCounter(filepath.Join(cfg.Device.SlotsDir, agent.BootCountFileName))
	if err != nil {
		fatal("boot counter: %v", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        2,
			IdleConnTimeout:     60 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
		},
	}
	primary, err := agent.NewClientPair(
		agent.NewHTTPClient(cfg.Server.HTTPURL, httpClient),
		agent.NewHTTPTransport(httpClient),
	)
	if err != nil {
		fatal("client pair: %v", err)
	}
	checker := &agent.DefaultHealthChecker{
		Heartbeat: func(ctx context.Context) error {
			_, hash, _, err := slots.ActiveSlot()
			if err != nil {
				return err
			}
			_, err = primary.Client.Heartbeat(ctx, &protocol.Heartbeat{
				DeviceID:    cfg.Device.ID,
				VersionHash: hash,
				Version:     version,
				HWInfo:      protocol.HWInfo{Arch: runtime.GOARCH, OS: runtime.GOOS},
				Timestamp:   time.Now().Unix(),
			})
			return err
		},
	}
	wd, err := agent.NewWatchdog(bootCounter, checker, agent.WatchdogConfig{
		Timeout:  cfg.Update.WatchdogTimeout,
		Retries:  agent.DefaultWatchdogRetries,
		MaxBoots: agent.DefaultWatchdogMaxBoots,
	}, logger)
	if err != nil {
		fatal("watchdog: %v", err)
	}
	maxBump, _ := agent.ParseMaxBump(cfg.Update.MaxBump)
	unknownPolicy, _ := agent.ParseUnknownVersionPolicy(cfg.Update.UnknownVersionPolicy)
	autoUpdate := true
	if cfg.Update.AutoUpdate != nil {
		autoUpdate = *cfg.Update.AutoUpdate
	}
	jitter := 0.3
	if cfg.Update.Jitter != nil {
		jitter = *cfg.Update.Jitter
	}
	u, err := agent.NewUpdater(agent.UpdaterDeps{
		Config: agent.UpdaterConfig{
			DeviceID: cfg.Device.ID,
			// Version is intentionally empty for the demo: server.yaml
			// keeps target.version: "1.0.0" static, so a non-empty Version
			// on the agent would make ComputeBump(local,remote) equal
			// BumpNone and the policy would block every cycle. The real
			// "semver gate" is already covered by unit tests + README §
			// "Update policy"; the demo showcases the delta + swap + exec
			// flow, not the gate. To exercise the gate live, edit
			// server.yaml:target.version between publish-version.sh calls
			// and restart the server.
			Version:                "",
			CheckInterval:          cfg.Update.CheckInterval,
			Jitter:                 jitter,
			MaxRetries:             cfg.Update.MaxRetries,
			RetryBackoff:           cfg.Update.RetryBackoff,
			StateDir:               cfg.Device.SlotsDir,
			AutoUpdate:             autoUpdate,
			MaxBump:                maxBump,
			UnknownVersionPolicy:   unknownPolicy,
			DiskSpaceMinFreePct:    cfg.Device.DiskSpaceMinFreePct,
			DiskSpaceMinFreeMB:     cfg.Device.DiskSpaceMinFreeMB,
			RestartFailureCooldown: cfg.Update.RestartFailureCooldown,
		},
		Primary:   primary,
		Slots:     slots,
		PublicKey: pubKey,
		Watchdog:  wd,
		Restart:   agent.ExecRestart{},
		Logger:    logger,
	})
	if err != nil {
		fatal("updater: %v", err)
	}
	return u
}

func fatal(format string, args ...any) {
	slog.Error("demo app startup failed", "err", fmt.Sprintf(format, args...))
	os.Exit(1)
}

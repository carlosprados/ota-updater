// Command edge-agent runs the OTA edge agent: heartbeats, signed-manifest
// verification, delta download, A/B swap, watchdog and self-restart. All
// behavior is driven by a YAML config — see configs/agent.yaml for the full
// schema. The same logic is available as an embeddable library through the
// agent package; this binary is a thin wrapper around agent.Updater.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/amplia/ota-updater/internal/agent"
	"github.com/amplia/ota-updater/internal/crypto"
	"github.com/amplia/ota-updater/internal/protocol"
)

func main() {
	cfgPath := flag.String("config", "./configs/agent.yaml", "path to agent YAML config")
	flag.Parse()

	if err := run(*cfgPath); err != nil {
		slog.Error("edge-agent startup failed", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := agent.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logging, err := agent.NewLogger(cfg.Logging)
	if err != nil {
		return fmt.Errorf("init logging: %w", err)
	}
	logger := logging.Logger()
	slog.SetDefault(logger)

	pubKey, err := crypto.LoadPublicKey(cfg.Crypto.PublicKey)
	if err != nil {
		return fmt.Errorf("load public key: %w", err)
	}

	slots, err := agent.NewSlotManager(cfg.Device.SlotsDir, cfg.Device.ActiveSymlink, logger)
	if err != nil {
		return fmt.Errorf("slot manager: %w", err)
	}

	bootCounter, err := agent.NewBootCounter(filepath.Join(cfg.Device.SlotsDir, agent.BootCountFileName))
	if err != nil {
		return fmt.Errorf("boot counter: %w", err)
	}

	primary, fallback, err := buildPairs(cfg)
	if err != nil {
		return fmt.Errorf("build transports: %w", err)
	}

	hwInfo := func() protocol.HWInfo {
		// Free RAM/disk reporting is intentionally left out: the syscalls vary
		// per OS and would drag in x/sys. Library consumers that need richer
		// HW info can pass their own HWInfoFunc to UpdaterDeps.
		return protocol.HWInfo{Arch: runtime.GOARCH, OS: runtime.GOOS}
	}

	// HealthChecker: the post-swap watchdog probes "the server is reachable
	// AND accepts a heartbeat from this version". We bind it to the primary
	// client only (no one-shot fallback inside the watchdog window) so a
	// flapping primary doesn't get masked by a healthy fallback — the
	// watchdog window's job is to verify *primary* connectivity.
	heartbeatProbe := func(ctx context.Context) error {
		_, hash, _, err := slots.ActiveSlot()
		if err != nil {
			return fmt.Errorf("read active slot: %w", err)
		}
		_, err = primary.Client.Heartbeat(ctx, &protocol.Heartbeat{
			DeviceID:    cfg.Device.ID,
			VersionHash: hash,
			HWInfo:      hwInfo(),
			Timestamp:   time.Now().Unix(),
		})
		return err
	}
	checker := &agent.DefaultHealthChecker{Heartbeat: heartbeatProbe}

	watchdog, err := agent.NewWatchdog(bootCounter, checker, agent.WatchdogConfig{
		Timeout:  cfg.Update.WatchdogTimeout,
		Retries:  agent.DefaultWatchdogRetries,
		MaxBoots: agent.DefaultWatchdogMaxBoots,
	}, logger)
	if err != nil {
		return fmt.Errorf("watchdog: %w", err)
	}

	updater, err := agent.NewUpdater(agent.UpdaterDeps{
		Config: agent.UpdaterConfig{
			DeviceID:      cfg.Device.ID,
			CheckInterval: cfg.Update.CheckInterval,
			MaxRetries:    cfg.Update.MaxRetries,
			RetryBackoff:  cfg.Update.RetryBackoff,
			StateDir:      cfg.Device.SlotsDir,
		},
		Primary:   primary,
		Fallback:  fallback,
		Slots:     slots,
		PublicKey: pubKey,
		Watchdog:  watchdog,
		Restart:   agent.ExecRestart{},
		Logger:    logger,
		HWInfo:    hwInfo,
	})
	if err != nil {
		return fmt.Errorf("updater: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("edge-agent starting",
		"op", "startup",
		"device_id", cfg.Device.ID,
		"primary", primary.Client.Name(),
		"fallback_enabled", fallback != nil,
		"check_interval", cfg.Update.CheckInterval.String(),
		"watchdog_timeout", cfg.Update.WatchdogTimeout.String(),
	)

	err = updater.Run(ctx)
	if err != nil && err != context.Canceled {
		return fmt.Errorf("updater run: %w", err)
	}
	logger.Info("edge-agent stopped", "op", "shutdown")
	return nil
}

// buildPairs constructs the primary ClientPair (and an optional fallback)
// from the agent config. The pair selection mirrors the policy decided in
// CLAUDE.md: when fallback is enabled, the *other* transport is tried once
// per cycle on failure (the Updater enforces "no sticky").
func buildPairs(cfg *agent.Config) (agent.ClientPair, *agent.ClientPair, error) {
	httpClient := &http.Client{
		// No client-wide timeout: per-request timeouts come from the Updater's
		// context, which itself respects WatchdogTimeout / CheckInterval.
	}
	httpPair, errHTTP := agent.NewClientPair(
		agent.NewHTTPClient(cfg.Server.HTTPURL, httpClient),
		agent.NewHTTPTransport(httpClient),
	)
	coapPair, errCoAP := agent.NewClientPair(
		agent.NewCoAPClient(cfg.Server.CoAPURL),
		agent.NewCoAPTransport(cfg.Server.CoAP.ACKTimeout),
	)

	switch cfg.Server.Transport {
	case agent.TransportHTTP:
		if errHTTP != nil {
			return agent.ClientPair{}, nil, fmt.Errorf("http pair: %w", errHTTP)
		}
		if !cfg.Server.Fallback {
			return httpPair, nil, nil
		}
		if errCoAP != nil {
			return agent.ClientPair{}, nil, fmt.Errorf("coap fallback pair: %w", errCoAP)
		}
		return httpPair, &coapPair, nil
	case agent.TransportCoAP:
		if errCoAP != nil {
			return agent.ClientPair{}, nil, fmt.Errorf("coap pair: %w", errCoAP)
		}
		if !cfg.Server.Fallback {
			return coapPair, nil, nil
		}
		if errHTTP != nil {
			return agent.ClientPair{}, nil, fmt.Errorf("http fallback pair: %w", errHTTP)
		}
		return coapPair, &httpPair, nil
	default:
		return agent.ClientPair{}, nil, fmt.Errorf("unknown transport %q", cfg.Server.Transport)
	}
}

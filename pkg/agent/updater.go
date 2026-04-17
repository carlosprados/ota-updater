package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/amplia/ota-updater/pkg/crypto"
	"github.com/amplia/ota-updater/pkg/delta"
	"github.com/amplia/ota-updater/pkg/protocol"
)

// pendingUpdateFile is the basename of the marker file the Updater writes
// just before swap+restart. Its presence on the next boot tells the Updater
// to enter the watchdog verification phase.
const pendingUpdateFile = ".pending_update"

// stagingDeltaFile is the basename of the temporary file where the
// Downloader stores the verified-but-not-yet-applied delta.
const stagingDeltaFile = ".staging.delta"

// downloadStateFile is where the Downloader persists its resume state.
const downloadStateFile = ".download.json"

// HWInfoFunc lets library consumers plug in real hardware probes
// (memory, disk free, etc.). The default reports GOARCH/GOOS only.
type HWInfoFunc func() protocol.HWInfo

// UpdaterConfig parameterizes an Updater. Sensible defaults are filled in
// at NewUpdater time so library consumers only set what they care about.
type UpdaterConfig struct {
	// DeviceID identifies this device in heartbeats and update reports.
	DeviceID string
	// CheckInterval is the cadence between RunOnce iterations in Run.
	// Defaults to 1 hour.
	CheckInterval time.Duration
	// MaxRetries / RetryBackoff are forwarded to the per-download Downloader.
	MaxRetries   int
	RetryBackoff time.Duration
	// StateDir holds the Updater's persistent files: .pending_update,
	// .staging.delta, .download.json. Recommended: same dir as the slots,
	// so the BootCounter (.boot_count) sits next to them.
	StateDir string
	// SelfArgv is the argv the new binary is exec'd with after a swap.
	// Defaults to os.Args (the agent's current invocation).
	SelfArgv []string
}

// UpdaterDeps groups the collaborators the Updater needs. Construct each
// independently and hand them in here. Each non-nil dependency is required.
type UpdaterDeps struct {
	Config    UpdaterConfig
	Primary   ClientPair
	Fallback  *ClientPair // optional; one-shot per cycle
	Slots     *SlotManager
	PublicKey ed25519.PublicKey
	Watchdog  *Watchdog
	Restart   RestartStrategy
	Logger    *slog.Logger
	HWInfo    HWInfoFunc // optional; defaults to GOARCH/GOOS only
}

// Updater orchestrates one full OTA cycle: heartbeat, signature verification,
// delta download, patch+verify, A/B swap, exec. It also owns the post-swap
// boot phase: when the binary that took over comes up, it verifies its own
// health and either confirms the swap or rolls back.
//
// The Updater is library-shaped: all dependencies are injected, no globals,
// logger pluggable, restart strategy pluggable. cmd/edge-agent at step 15
// is just a wrapper around NewUpdater + Run.
type Updater struct {
	cfg       UpdaterConfig
	primary   ClientPair
	fallback  *ClientPair
	slots     *SlotManager
	publicKey ed25519.PublicKey
	watchdog  *Watchdog
	restart   RestartStrategy
	logger    *slog.Logger
	hwInfo    HWInfoFunc

	pendingPath   string
	deltaStaging  string
	downloadState string

	now func() time.Time
}

// NewUpdater wires an Updater after validating UpdaterDeps and applying
// defaults to UpdaterConfig.
func NewUpdater(deps UpdaterDeps) (*Updater, error) {
	if deps.Slots == nil {
		return nil, errors.New("updater: slots is required")
	}
	if deps.Watchdog == nil {
		return nil, errors.New("updater: watchdog is required")
	}
	if deps.Restart == nil {
		return nil, errors.New("updater: restart strategy is required")
	}
	if deps.Primary.Client == nil || deps.Primary.Transport == nil {
		return nil, errors.New("updater: primary client pair is required")
	}
	if len(deps.PublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("updater: invalid public key size %d", len(deps.PublicKey))
	}
	cfg := deps.Config
	if cfg.DeviceID == "" {
		return nil, errors.New("updater: device id is required")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("updater: state dir is required")
	}
	if info, err := os.Stat(cfg.StateDir); err != nil {
		return nil, fmt.Errorf("updater: stat state dir %q: %w", cfg.StateDir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("updater: state dir %q is not a directory", cfg.StateDir)
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = time.Hour
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 30 * time.Second
	}
	if len(cfg.SelfArgv) == 0 {
		cfg.SelfArgv = append([]string{}, os.Args...)
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	hw := deps.HWInfo
	if hw == nil {
		hw = defaultHWInfo
	}
	return &Updater{
		cfg:           cfg,
		primary:       deps.Primary,
		fallback:      deps.Fallback,
		slots:         deps.Slots,
		publicKey:     deps.PublicKey,
		watchdog:      deps.Watchdog,
		restart:       deps.Restart,
		logger:        logger,
		hwInfo:        hw,
		pendingPath:   filepath.Join(cfg.StateDir, pendingUpdateFile),
		deltaStaging:  filepath.Join(cfg.StateDir, stagingDeltaFile),
		downloadState: filepath.Join(cfg.StateDir, downloadStateFile),
		now:           time.Now,
	}, nil
}

// Run executes the boot phase, then loops RunOnce on CheckInterval until ctx
// is cancelled. It returns ctx.Err() on shutdown, or any unrecoverable error
// from the boot phase.
//
// Errors from individual RunOnce iterations are logged and swallowed; the
// loop continues so transient network failures don't stop the agent.
func (u *Updater) Run(ctx context.Context) error {
	if err := u.BootPhase(ctx); err != nil {
		return err
	}
	for {
		if err := u.RunOnce(ctx); err != nil {
			u.logger.Warn("update cycle failed",
				"op", "update_cycle", "device_id", u.cfg.DeviceID, "err", err,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(u.cfg.CheckInterval):
		}
	}
}

// BootPhase inspects .pending_update. If absent, the agent simply enters
// steady state. If present, the agent runs the watchdog window:
//
//   - Boot count exceeded → rollback, report failure, exec previous binary.
//   - Health check fails  → rollback, report failure, exec previous binary.
//   - Health check passes → Confirm, report success, clear pending.
//
// Exposed publicly so library consumers can integrate the boot phase into
// their own startup orchestration if they don't use Run directly.
func (u *Updater) BootPhase(ctx context.Context) error {
	pending, err := u.readPending()
	if err != nil {
		u.logger.Warn("read pending update; ignoring",
			"op", "boot", "device_id", u.cfg.DeviceID, "err", err,
		)
		return nil
	}
	if pending == nil {
		u.logger.Info("no pending update; entering steady state",
			"op", "boot", "device_id", u.cfg.DeviceID,
		)
		return nil
	}
	_, activeHash, _, err := u.slots.ActiveSlot()
	if err != nil {
		return fmt.Errorf("boot: read active slot: %w", err)
	}
	if activeHash != pending.NewHash {
		u.logger.Warn("pending update mismatches active slot; clearing marker",
			"op", "boot", "device_id", u.cfg.DeviceID,
			"active_hash", activeHash, "pending_new", pending.NewHash,
		)
		u.clearPending()
		return nil
	}
	u.logger.Info("post-swap boot detected; entering watchdog window",
		"op", "boot", "device_id", u.cfg.DeviceID,
		"previous_hash", pending.PreviousHash, "new_hash", pending.NewHash,
	)

	count, checkErr := u.watchdog.CheckBoot(activeHash)
	if errors.Is(checkErr, ErrBootCountExceeded) {
		reason := fmt.Sprintf("boot count exceeded (count=%d)", count)
		u.logger.Error("boot count exceeded; rolling back permanently",
			"op", "boot_rollback", "device_id", u.cfg.DeviceID,
			"version_hash", activeHash, "count", count,
		)
		return u.rollbackAndExec(ctx, pending, reason)
	}
	if checkErr != nil {
		return fmt.Errorf("boot: check boot count: %w", checkErr)
	}

	if err := u.watchdog.WaitForHealth(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		reason := fmt.Sprintf("health check failed: %v", err)
		u.logger.Warn("health check failed; rolling back",
			"op", "boot_rollback", "device_id", u.cfg.DeviceID, "err", err,
		)
		return u.rollbackAndExec(ctx, pending, reason)
	}

	if err := u.watchdog.Confirm(); err != nil {
		u.logger.Warn("watchdog confirm failed (non-fatal)",
			"op", "boot", "device_id", u.cfg.DeviceID, "err", err,
		)
	}
	if err := u.reportUpdate(ctx, pending.PreviousHash, activeHash, true, ""); err != nil {
		u.logger.Warn("update report failed (non-fatal)",
			"op", "boot", "device_id", u.cfg.DeviceID, "err", err,
		)
	}
	u.clearPending()
	u.logger.Info("update confirmed; steady state engaged",
		"op", "boot", "device_id", u.cfg.DeviceID,
		"previous_hash", pending.PreviousHash, "new_hash", activeHash,
	)
	return nil
}

// rollbackAndExec performs a permanent rollback: flips the symlink back,
// resets the boot counter (so the rolled-back binary doesn't inherit the
// bad count), reports the failure, then exec's the rolled-back binary.
// On a successful Restart this never returns.
func (u *Updater) rollbackAndExec(ctx context.Context, pending *pendingUpdate, reason string) error {
	if err := u.slots.Rollback(); err != nil {
		return fmt.Errorf("boot: rollback: %w", err)
	}
	if err := u.watchdog.counter.Reset(); err != nil {
		u.logger.Warn("reset boot counter after rollback (non-fatal)",
			"op", "boot_rollback", "device_id", u.cfg.DeviceID, "err", err,
		)
	}
	rollbackPath, rollbackHash, _, err := u.slots.ActiveSlot()
	if err != nil {
		return fmt.Errorf("boot: read active after rollback: %w", err)
	}
	if err := u.reportUpdate(ctx, pending.NewHash, rollbackHash, false, reason); err != nil {
		u.logger.Warn("rollback report failed (non-fatal)",
			"op", "boot_rollback", "device_id", u.cfg.DeviceID, "err", err,
		)
	}
	u.clearPending()
	u.logger.Warn("restarting into rolled-back binary",
		"op", "boot_rollback", "device_id", u.cfg.DeviceID,
		"path", rollbackPath, "version_hash", rollbackHash,
	)
	if err := u.restart.Restart(ctx, rollbackPath, u.cfg.SelfArgv); err != nil {
		return fmt.Errorf("boot: rollback restart: %w", err)
	}
	return nil // unreachable on successful exec
}

// RunOnce performs one update cycle: heartbeat → verify signature → download
// → patch → verify → swap → restart. Returns nil when the cycle completes
// without an actionable update. On a successful swap+restart this does not
// return (the process image is replaced).
func (u *Updater) RunOnce(ctx context.Context) error {
	activePath, activeHash, _, err := u.slots.ActiveSlot()
	if err != nil {
		return fmt.Errorf("read active slot: %w", err)
	}
	hb := &protocol.Heartbeat{
		DeviceID:    u.cfg.DeviceID,
		VersionHash: activeHash,
		HWInfo:      u.hwInfo(),
		Timestamp:   u.now().Unix(),
	}
	resp, pair, err := u.heartbeatWithFallback(ctx, hb)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if !resp.UpdateAvailable {
		u.logger.Debug("no update available",
			"op", "update_cycle", "device_id", u.cfg.DeviceID, "version_hash", activeHash,
		)
		return nil
	}
	if resp.RetryAfter > 0 {
		u.logger.Info("delta not yet ready",
			"op", "update_cycle", "device_id", u.cfg.DeviceID,
			"retry_after", resp.RetryAfter,
		)
		return nil
	}
	if resp.Signature == "" || resp.TargetHash == "" || resp.DeltaHash == "" {
		return errors.New("manifest missing signature, target_hash or delta_hash")
	}

	// Verify signature BEFORE downloading (docs/signing.md §5 step 2).
	payload, err := protocol.ManifestSigningPayload(resp.TargetHash, resp.DeltaHash)
	if err != nil {
		return fmt.Errorf("build signing payload: %w", err)
	}
	sig, err := hex.DecodeString(resp.Signature)
	if err != nil {
		return fmt.Errorf("decode signature hex: %w", err)
	}
	if err := crypto.Verify(u.publicKey, payload, sig); err != nil {
		return fmt.Errorf("verify manifest signature: %w", err)
	}

	deltaURL := pair.Client.DeltaURL(resp.DeltaEndpoint)
	if deltaURL == "" {
		return errors.New("manifest missing delta_endpoint")
	}
	dl := NewDownloader(pair.Transport, DownloaderConfig{
		StatePath:    u.downloadState,
		MaxRetries:   u.cfg.MaxRetries,
		RetryBackoff: u.cfg.RetryBackoff,
	}, u.logger)
	target := FetchTarget{
		URL:       deltaURL,
		DeltaHash: resp.DeltaHash,
		TotalSize: resp.DeltaSize,
		OutPath:   u.deltaStaging,
	}
	if err := dl.Download(ctx, target); err != nil {
		return fmt.Errorf("download delta: %w", err)
	}

	// Patch + verify reconstruction.
	activeBin, err := os.ReadFile(activePath)
	if err != nil {
		return fmt.Errorf("read active binary: %w", err)
	}
	deltaBin, err := os.ReadFile(u.deltaStaging)
	if err != nil {
		return fmt.Errorf("read staged delta: %w", err)
	}
	newBin, err := delta.Apply(activeBin, deltaBin)
	if err != nil {
		return fmt.Errorf("apply delta: %w", err)
	}
	actualHash := sha256HexBytes(newBin)
	if actualHash != resp.TargetHash {
		return fmt.Errorf("reconstructed binary hash mismatch: got %s want %s",
			actualHash, resp.TargetHash)
	}

	// Stage in the inactive slot.
	if err := u.slots.WriteToInactive(bytes.NewReader(newBin)); err != nil {
		return fmt.Errorf("write inactive slot: %w", err)
	}

	// Mark the swap as pending BEFORE flipping the symlink so a crash between
	// swap and exec is still recovered: the next boot sees .pending_update
	// and runs the watchdog window. If we crash between writePending and
	// Swap, the next boot will see active_hash != pending.new_hash and clear
	// the marker without performing any rollback.
	pending := &pendingUpdate{
		PreviousHash: activeHash,
		NewHash:      resp.TargetHash,
		SwappedUnix:  u.now().Unix(),
	}
	if err := u.writePending(pending); err != nil {
		return fmt.Errorf("write pending update marker: %w", err)
	}

	if err := u.slots.Swap(); err != nil {
		u.clearPending()
		return fmt.Errorf("swap: %w", err)
	}

	// Cleanup: the staged delta is no longer needed.
	_ = os.Remove(u.deltaStaging)

	newActivePath, _, _, err := u.slots.ActiveSlot()
	if err != nil {
		return fmt.Errorf("read active slot after swap: %w", err)
	}
	u.logger.Info("update applied; restarting",
		"op", "update_cycle", "device_id", u.cfg.DeviceID,
		"previous_hash", activeHash, "new_hash", resp.TargetHash, "exec", newActivePath,
	)
	if err := u.restart.Restart(ctx, newActivePath, u.cfg.SelfArgv); err != nil {
		// Restart failed: undo the swap so we don't leave the device pointing
		// at a binary we couldn't actually launch. The pending marker is
		// cleared so the next BootPhase doesn't try to verify a swap that
		// effectively never happened.
		u.logger.Error("restart failed; rolling back",
			"op", "update_cycle", "device_id", u.cfg.DeviceID, "err", err,
		)
		u.clearPending()
		if rbErr := u.slots.Rollback(); rbErr != nil {
			u.logger.Error("rollback after failed restart also failed",
				"op", "update_cycle", "device_id", u.cfg.DeviceID, "err", rbErr,
			)
		}
		return fmt.Errorf("restart: %w", err)
	}
	return nil // unreachable on successful exec
}

// heartbeatWithFallback runs the primary client; if it errors and a fallback
// is configured, retries once with the fallback. Returns whichever pair
// produced the response so the same transport is used for the delta.
func (u *Updater) heartbeatWithFallback(ctx context.Context, hb *protocol.Heartbeat) (*protocol.ManifestResponse, ClientPair, error) {
	resp, err := u.primary.Client.Heartbeat(ctx, hb)
	if err == nil {
		return resp, u.primary, nil
	}
	if u.fallback == nil {
		return nil, ClientPair{}, fmt.Errorf("primary %s: %w", u.primary.Client.Name(), err)
	}
	u.logger.Warn("primary heartbeat failed; trying fallback",
		"op", "heartbeat", "device_id", u.cfg.DeviceID,
		"primary", u.primary.Client.Name(), "fallback", u.fallback.Client.Name(), "err", err,
	)
	respFb, errFb := u.fallback.Client.Heartbeat(ctx, hb)
	if errFb != nil {
		return nil, ClientPair{}, fmt.Errorf("primary %s: %w; fallback %s: %v",
			u.primary.Client.Name(), err, u.fallback.Client.Name(), errFb)
	}
	return respFb, *u.fallback, nil
}

// reportUpdate sends an UpdateReport. Tries primary first, falls back once.
// Returns an error only if both transports fail (and only one was tried when
// no fallback is configured).
func (u *Updater) reportUpdate(ctx context.Context, prevHash, newHash string, success bool, reason string) error {
	rep := &protocol.UpdateReport{
		DeviceID:       u.cfg.DeviceID,
		PreviousHash:   prevHash,
		NewHash:        newHash,
		Success:        success,
		RollbackReason: reason,
		Timestamp:      u.now().Unix(),
	}
	err := u.primary.Client.Report(ctx, rep)
	if err == nil {
		return nil
	}
	if u.fallback == nil {
		return fmt.Errorf("report primary %s: %w", u.primary.Client.Name(), err)
	}
	if errFb := u.fallback.Client.Report(ctx, rep); errFb != nil {
		return fmt.Errorf("report primary %s: %w; fallback %s: %v",
			u.primary.Client.Name(), err, u.fallback.Client.Name(), errFb)
	}
	return nil
}

// pendingUpdate records an in-progress A/B swap that the next boot must verify.
// Persisted as JSON inside StateDir.
type pendingUpdate struct {
	PreviousHash string `json:"previous_hash"`
	NewHash      string `json:"new_hash"`
	SwappedUnix  int64  `json:"swapped_unix"`
}

func (u *Updater) readPending() (*pendingUpdate, error) {
	data, err := os.ReadFile(u.pendingPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pendingUpdate
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pending update: %w", err)
	}
	if p.NewHash == "" {
		return nil, errors.New("pending update missing new_hash")
	}
	return &p, nil
}

func (u *Updater) writePending(p *pendingUpdate) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFileAtomic(u.pendingPath, data, 0o644)
}

func (u *Updater) clearPending() {
	if err := os.Remove(u.pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		u.logger.Warn("clear pending update", "op", "update_cycle", "err", err)
	}
}

// sha256HexBytes returns the SHA-256 hex of an in-memory byte slice. Mirrors
// hashFile for the file-backed case.
func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic writes data to path via a temp file in the same directory
// then renames. Mirrors the slot writer but accepts a non-exec mode.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// defaultHWInfo returns just GOARCH/GOOS. Library consumers that want to
// report free RAM/disk should inject their own HWInfoFunc — the syscalls
// for that are OS-specific and we'd rather not pull in cgo or x/sys here.
func defaultHWInfo() protocol.HWInfo {
	return protocol.HWInfo{
		Arch: runtime.GOARCH,
		OS:   runtime.GOOS,
	}
}

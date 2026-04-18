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
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/amplia/ota-updater/pkg/atomicio"
	"github.com/amplia/ota-updater/pkg/crypto"
	"github.com/amplia/ota-updater/pkg/delta"
	"github.com/amplia/ota-updater/pkg/protocol"
)

// orphanSweepMaxAge is the threshold past which stray temp/partial files in
// StateDir are considered abandoned by a prior crashed process. Well above
// any legitimate download duration even on NB-IoT.
const orphanSweepMaxAge = 24 * time.Hour

// pendingUpdateFile is the basename of the marker file the Updater writes
// just before swap+restart. Its presence on the next boot tells the Updater
// to enter the watchdog verification phase.
const pendingUpdateFile = ".pending_update"

// stagingDeltaFile is the basename of the temporary file where the
// Downloader stores the verified-but-not-yet-applied delta.
const stagingDeltaFile = ".staging.delta"

// downloadStateFile is where the Downloader persists its resume state.
const downloadStateFile = ".download.json"

// UpdateNowFile is the basename of the sidecar the operator creates inside
// StateDir to force a single update cycle, ignoring AutoUpdate and the
// MaxBump policy. Consumed (deleted) on the first cycle that sees it,
// whether or not that cycle actually applies an update. Exported so ops
// tooling can reference the name from outside the package.
const UpdateNowFile = ".update_now"

// HWInfoFunc lets library consumers plug in real hardware probes
// (memory, disk free, etc.). The default reports GOARCH/GOOS only.
type HWInfoFunc func() protocol.HWInfo

// UpdaterConfig parameterizes an Updater. Sensible defaults are filled in
// at NewUpdater time so library consumers only set what they care about.
type UpdaterConfig struct {
	// DeviceID identifies this device in heartbeats and update reports.
	DeviceID string
	// Version is the human-readable semver of the currently running binary.
	// Injected by the caller (typically via `-ldflags "-X main.version=..."`
	// in cmd/edge-agent; library embedders do the same from their own main).
	// Used both as Heartbeat.Version (advisory, for server logging) and as
	// the local side of the policy comparison against ManifestResponse.TargetVersion.
	// An empty string disables the policy gate (all updates allowed).
	Version string
	// CheckInterval is the cadence between RunOnce iterations in Run.
	// Defaults to 1 hour.
	CheckInterval time.Duration
	// Jitter spreads each sleep by ±Jitter*CheckInterval to avoid fleet
	// lock-step. Range [0..1]. 0 disables, 0.3 is the recommended default.
	Jitter float64
	// MaxRetries / RetryBackoff are forwarded to the per-download Downloader.
	MaxRetries   int
	RetryBackoff time.Duration
	// StateDir holds the Updater's persistent files: .pending_update,
	// .staging.delta, .download.json, .update_now (manual trigger sidecar).
	// Recommended: same dir as the slots, so the BootCounter (.boot_count)
	// sits next to them.
	StateDir string
	// SelfArgv is the argv the new binary is exec'd with after a swap.
	// Defaults to os.Args (the agent's current invocation).
	SelfArgv []string

	// AutoUpdate is the master auto-update switch. When false, an update
	// seen on the heartbeat is logged but not applied unless a manual
	// trigger (TriggerUpdate / .update_now sidecar) is present.
	AutoUpdate bool
	// MaxBump is the policy cap (resolved from UpdateConfig.MaxBump).
	MaxBump MaxBump
	// UnknownVersionPolicy decides what to do when TargetVersion isn't valid
	// semver (resolved from UpdateConfig.UnknownVersionPolicy).
	UnknownVersionPolicy UnknownVersionPolicy
	// DiskSpaceMinFreePct and DiskSpaceMinFreeMB drive a startup warning
	// on the filesystem containing StateDir. 0 on either disables just
	// that threshold.
	DiskSpaceMinFreePct int
	DiskSpaceMinFreeMB  int
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
	rand      *rand.Rand // jittered sleep source, seeded at NewUpdater

	pendingPath   string
	deltaStaging  string
	downloadState string
	updateNowPath string

	// forceOnce, when true, makes the next RunOnce skip the AutoUpdate /
	// MaxBump policy gate. Set by TriggerUpdate or by detecting the
	// .update_now sidecar file; reset after the cycle consumes it.
	forceMu   sync.Mutex
	forceOnce bool

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
	// Sweep stale temp/partial files left over from a prior crash in the
	// StateDir. Anything with a matching prefix older than 24h is reclaimed.
	// Covers both atomicio's generic ".tmp-*" temps and the Downloader's
	// per-target ".staging.delta.partial" leftovers.
	atomicio.SweepStaleTemp(cfg.StateDir,
		[]string{".tmp-", stagingDeltaFile + ".partial", stagingDeltaFile + "."},
		orphanSweepMaxAge, logger)

	// One-shot disk-space visibility for the StateDir. Warning only — an
	// agent that boots with a nearly-full device should flag the operator
	// but continue (updates may still arrive and succeed if the delta fits).
	checkAgentDiskSpace(cfg.StateDir, cfg.DiskSpaceMinFreePct, cfg.DiskSpaceMinFreeMB, logger)

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
		rand:          rand.New(rand.NewSource(time.Now().UnixNano())),
		pendingPath:   filepath.Join(cfg.StateDir, pendingUpdateFile),
		deltaStaging:  filepath.Join(cfg.StateDir, stagingDeltaFile),
		downloadState: filepath.Join(cfg.StateDir, downloadStateFile),
		updateNowPath: filepath.Join(cfg.StateDir, UpdateNowFile),
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
		case <-time.After(u.nextSleep()):
		}
	}
}

// nextSleep returns CheckInterval jittered by ±(Jitter*CheckInterval).
// With Jitter=0 the sleep is exactly CheckInterval (lock-step). Typical
// Jitter=0.3 yields a uniform sample in [0.7*CheckInterval, 1.3*CheckInterval].
// The jitter is re-sampled every cycle so any transient synchronisation
// between agents (e.g. after a fleet-wide outage) decays in a few cycles.
func (u *Updater) nextSleep() time.Duration {
	base := u.cfg.CheckInterval
	if u.cfg.Jitter <= 0 {
		return base
	}
	span := float64(base) * u.cfg.Jitter
	delta := (u.rand.Float64()*2 - 1) * span // uniform [-span, +span]
	d := time.Duration(float64(base) + delta)
	if d <= 0 {
		return base
	}
	return d
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
		Version:     u.cfg.Version,
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

	// Policy gate: when AutoUpdate is off or the bump exceeds MaxBump, we
	// log the availability but do not apply — unless the operator set the
	// .update_now sidecar or called TriggerUpdate, in which case we consume
	// the one-shot override and proceed.
	forced := u.consumeForceOnce()
	if !forced && !u.policyAllows(resp.TargetVersion) {
		return nil
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

// TriggerUpdate marks the next RunOnce cycle as "forced": the AutoUpdate
// switch and the MaxBump policy are bypassed for that single cycle. Safe to
// call concurrently with Run; the flag is consumed the first time RunOnce
// observes it and immediately reset.
//
// Typical use: library embedders that orchestrate their own update policy
// (e.g. "auto-update off by default, but when my control plane says so,
// force it now") call this instead of relying on config flags.
func (u *Updater) TriggerUpdate() {
	u.forceMu.Lock()
	u.forceOnce = true
	u.forceMu.Unlock()
}

// consumeForceOnce atomically reads and clears the force flag. It also
// observes the .update_now sidecar file in StateDir: if present, it is
// consumed (removed) and the cycle is forced. Sidecar and programmatic
// trigger are equivalent — either one is enough to override policy.
func (u *Updater) consumeForceOnce() bool {
	u.forceMu.Lock()
	fromCall := u.forceOnce
	u.forceOnce = false
	u.forceMu.Unlock()

	fromFile := false
	if _, err := os.Stat(u.updateNowPath); err == nil {
		fromFile = true
		if rerr := os.Remove(u.updateNowPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			u.logger.Warn("remove update-now sidecar",
				"op", "update_cycle", "path", u.updateNowPath, "err", rerr,
			)
		}
	}
	if fromCall || fromFile {
		u.logger.Info("manual update trigger consumed",
			"op", "update_cycle", "device_id", u.cfg.DeviceID,
			"from_call", fromCall, "from_sidecar", fromFile,
		)
		return true
	}
	return false
}

// policyAllows returns true when this cycle may apply the update with the
// given remote TargetVersion. Logs the decision on block so operators see
// exactly why an otherwise-actionable update was skipped.
func (u *Updater) policyAllows(remoteVersion string) bool {
	if !u.cfg.AutoUpdate {
		u.logger.Info("update available but auto_update is disabled",
			"op", "update_cycle", "device_id", u.cfg.DeviceID,
			"local_version", u.cfg.Version, "remote_version", remoteVersion,
		)
		return false
	}
	// Empty local version disables the semver gate — the binary doesn't
	// know its own version so policy cannot meaningfully compare. We allow
	// the update so an unversioned agent still receives them; operators who
	// want strict gating inject a version via ldflags.
	if u.cfg.Version == "" {
		return true
	}
	bump := ComputeBump(u.cfg.Version, remoteVersion)
	if bump == BumpUnknown {
		if u.cfg.UnknownVersionPolicy == UnknownAllow {
			u.logger.Warn("remote TargetVersion not semver; applying per unknown_version_policy=allow",
				"op", "update_cycle", "device_id", u.cfg.DeviceID,
				"local_version", u.cfg.Version, "remote_version", remoteVersion,
			)
			return true
		}
		u.logger.Warn("remote TargetVersion not semver; blocked per unknown_version_policy=deny",
			"op", "update_cycle", "device_id", u.cfg.DeviceID,
			"local_version", u.cfg.Version, "remote_version", remoteVersion,
		)
		return false
	}
	if bump == BumpNone {
		// Server said UpdateAvailable=true but local is >= remote — a
		// server-config mismatch; refuse to apply.
		u.logger.Warn("manifest update_available=true but local version not older; skipping",
			"op", "update_cycle", "device_id", u.cfg.DeviceID,
			"local_version", u.cfg.Version, "remote_version", remoteVersion,
		)
		return false
	}
	if !AllowedByPolicy(bump, u.cfg.MaxBump) {
		u.logger.Info("update available but bump exceeds max_bump policy",
			"op", "update_cycle", "device_id", u.cfg.DeviceID,
			"local_version", u.cfg.Version, "remote_version", remoteVersion,
			"bump", int(bump), "max_bump", u.cfg.MaxBump.String(),
		)
		return false
	}
	return true
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
	return atomicio.WriteFile(u.pendingPath, data, 0o644, u.logger)
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

// checkAgentDiskSpace logs a warning if StateDir's filesystem is below
// either threshold. 0 on either threshold disables just that check. On
// non-Unix or unsupported filesystems (e.g. overlayfs variants with no
// Statfs) the helper degrades to a DEBUG line.
func checkAgentDiskSpace(dir string, minPct, minMB int, logger *slog.Logger) {
	free, total, err := atomicio.Free(dir)
	if err != nil {
		logger.Debug("disk-space probe unsupported; skipping warning",
			"op", "disk_space", "path", dir, "err", err,
		)
		return
	}
	var warnPct, warnMB bool
	if minPct > 0 && total > 0 {
		if (free*100)/total < uint64(minPct) {
			warnPct = true
		}
	}
	if minMB > 0 && free < uint64(minMB)<<20 {
		warnMB = true
	}
	if warnPct || warnMB {
		logger.Warn("disk space running low on agent StateDir",
			"op", "disk_space",
			"path", dir,
			"free_mb", free>>20, "total_mb", total>>20,
			"min_free_pct", minPct, "min_free_mb", minMB,
			"breach_pct", warnPct, "breach_mb", warnMB,
		)
	} else {
		logger.Info("disk space ok",
			"op", "disk_space",
			"path", dir,
			"free_mb", free>>20, "total_mb", total>>20,
		)
	}
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

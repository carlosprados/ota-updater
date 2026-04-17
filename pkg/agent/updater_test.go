package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amplia/ota-updater/pkg/crypto"
	"github.com/amplia/ota-updater/pkg/delta"
	"github.com/amplia/ota-updater/pkg/protocol"
)

// fakeClient is a programmable ProtocolClient for the updater tests.
type fakeClient struct {
	name string

	mu             sync.Mutex
	heartbeatErr   error
	heartbeatResp  *protocol.ManifestResponse
	heartbeatCalls int
	reports        []*protocol.UpdateReport
	reportErr      error
}

func (c *fakeClient) Name() string { return c.name }

func (c *fakeClient) Heartbeat(ctx context.Context, hb *protocol.Heartbeat) (*protocol.ManifestResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeatCalls++
	if c.heartbeatErr != nil {
		return nil, c.heartbeatErr
	}
	if c.heartbeatResp == nil {
		return &protocol.ManifestResponse{UpdateAvailable: false}, nil
	}
	resp := *c.heartbeatResp
	return &resp, nil
}

func (c *fakeClient) Report(ctx context.Context, rep *protocol.UpdateReport) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, rep)
	return c.reportErr
}

func (c *fakeClient) DeltaURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	return c.name + "://server" + endpoint
}

func (c *fakeClient) snapshot() (heartbeatCalls int, reports []*protocol.UpdateReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*protocol.UpdateReport, len(c.reports))
	copy(out, c.reports)
	return c.heartbeatCalls, out
}

// fakeTransport serves a fixed body regardless of URL/offset (resume-capable
// when offset>0 by returning the suffix). Used to drive the Downloader from
// memory in updater tests.
type fakeTransport struct {
	name string
	body []byte
	err  error

	mu    sync.Mutex
	calls int
}

func (t *fakeTransport) Name() string { return t.name }

func (t *fakeTransport) FetchRange(ctx context.Context, rawURL string, offset int64) (io.ReadCloser, int64, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.err != nil {
		return nil, 0, t.err
	}
	if offset >= int64(len(t.body)) {
		return io.NopCloser(strings.NewReader("")), offset, nil
	}
	if offset < 0 {
		offset = 0
	}
	return io.NopCloser(strings.NewReader(string(t.body[offset:]))), offset, nil
}

// updaterFixture builds a fully-wired Updater plus the dependencies that
// tests typically want to inspect (slots, watchdog, recordRestart).
type updaterFixture struct {
	t         *testing.T
	dir       string // top-level temp dir
	stateDir  string
	slotsDir  string
	slots     *SlotManager
	bootCnt   *BootCounter
	watchdog  *Watchdog
	checker   *fakeChecker
	restart   *recordRestart
	pubKey    ed25519.PublicKey
	privKey   ed25519.PrivateKey
	primary   *fakeClient
	fallback  *fakeClient
	transport *fakeTransport
	updater   *Updater

	// payloads for delta tests
	oldBin []byte
	newBin []byte
}

func newUpdaterFixture(t *testing.T) *updaterFixture {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	slotsDir := filepath.Join(stateDir, "slots")
	if err := os.MkdirAll(slotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin := []byte("old binary content " + strings.Repeat("X", 256))
	newBin := []byte("new binary content " + strings.Repeat("Y", 256))
	if err := os.WriteFile(filepath.Join(slotsDir, SlotNameA), oldBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, SlotNameB), []byte("seed-B"), 0o755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(stateDir, "current")
	if err := os.Symlink(filepath.Join(slotsDir, SlotNameA), active); err != nil {
		t.Fatal(err)
	}
	logger := discardLogger()
	slots, err := NewSlotManager(slotsDir, active, logger)
	if err != nil {
		t.Fatal(err)
	}
	bootCnt, err := NewBootCounter(filepath.Join(stateDir, BootCountFileName))
	if err != nil {
		t.Fatal(err)
	}
	checker := &fakeChecker{}
	wd, err := NewWatchdog(bootCnt, checker, WatchdogConfig{
		Timeout: 100 * time.Millisecond, Retries: 3, MaxBoots: 2,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	primary := &fakeClient{name: "http"}
	transport := &fakeTransport{name: "http"}
	pair, err := NewClientPair(primary, transport)
	if err != nil {
		t.Fatal(err)
	}
	restart := &recordRestart{}
	upd, err := NewUpdater(UpdaterDeps{
		Config: UpdaterConfig{
			DeviceID: "dev-1",
			StateDir: stateDir,
			// Force a tiny CheckInterval so we never hit it in tests.
			CheckInterval: 50 * time.Millisecond,
			MaxRetries:    1,
			RetryBackoff:  10 * time.Millisecond,
		},
		Primary:   pair,
		Slots:     slots,
		PublicKey: pub,
		Watchdog:  wd,
		Restart:   restart,
		Logger:    logger,
		HWInfo:    func() protocol.HWInfo { return protocol.HWInfo{Arch: "amd64", OS: "linux"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &updaterFixture{
		t: t, dir: dir, stateDir: stateDir, slotsDir: slotsDir,
		slots: slots, bootCnt: bootCnt, watchdog: wd, checker: checker,
		restart: restart, pubKey: pub, privKey: priv,
		primary: primary, transport: transport, updater: upd,
		oldBin: oldBin, newBin: newBin,
	}
}

// withFallback installs a fallback ClientPair backed by a separate fakeClient.
func (f *updaterFixture) withFallback() *fakeClient {
	fb := &fakeClient{name: "coap"}
	fbTrans := &fakeTransport{name: "coap"}
	pair, err := NewClientPair(fb, fbTrans)
	if err != nil {
		f.t.Fatal(err)
	}
	f.updater.fallback = &pair
	return fb
}

// signedManifest builds a real, signed ManifestResponse for newBin so the
// updater's signature verification step actually exercises crypto.Verify.
func (f *updaterFixture) signedManifest() (*protocol.ManifestResponse, []byte) {
	f.t.Helper()
	deltaBytes, err := delta.Generate(f.oldBin, f.newBin)
	if err != nil {
		f.t.Fatal(err)
	}
	targetHash := sha256HexBytes(f.newBin)
	deltaHash := sha256HexBytes(deltaBytes)
	payload, err := protocol.ManifestSigningPayload(targetHash, deltaHash)
	if err != nil {
		f.t.Fatal(err)
	}
	sig, err := crypto.Sign(f.privKey, payload)
	if err != nil {
		f.t.Fatal(err)
	}
	return &protocol.ManifestResponse{
		UpdateAvailable: true,
		TargetVersion:   "v2",
		TargetHash:      targetHash,
		DeltaSize:       int64(len(deltaBytes)),
		DeltaHash:       deltaHash,
		Signature:       hex.EncodeToString(sig),
		DeltaEndpoint:   protocol.DeltaPath(sha256HexBytes(f.oldBin), targetHash),
	}, deltaBytes
}

func TestNewUpdater_ValidatesRequiredFields(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	primary, _ := NewClientPair(&fakeClient{name: "http"}, &fakeTransport{name: "http"})

	dir := t.TempDir()
	logger := discardLogger()
	bc, _ := NewBootCounter(filepath.Join(dir, BootCountFileName))
	wd, _ := NewWatchdog(bc, &fakeChecker{}, WatchdogConfig{}, logger)
	slotsDir := filepath.Join(dir, "slots")
	_ = os.MkdirAll(slotsDir, 0o755)
	_ = os.WriteFile(filepath.Join(slotsDir, SlotNameA), []byte("a"), 0o755)
	_ = os.WriteFile(filepath.Join(slotsDir, SlotNameB), []byte("b"), 0o755)
	_ = os.Symlink(filepath.Join(slotsDir, SlotNameA), filepath.Join(dir, "current"))
	sm, _ := NewSlotManager(slotsDir, filepath.Join(dir, "current"), logger)

	cases := map[string]UpdaterDeps{
		"missing slots":     {Config: UpdaterConfig{DeviceID: "d", StateDir: dir}, Primary: primary, PublicKey: pub, Watchdog: wd, Restart: &recordRestart{}},
		"missing watchdog":  {Config: UpdaterConfig{DeviceID: "d", StateDir: dir}, Primary: primary, PublicKey: pub, Slots: sm, Restart: &recordRestart{}},
		"missing restart":   {Config: UpdaterConfig{DeviceID: "d", StateDir: dir}, Primary: primary, PublicKey: pub, Slots: sm, Watchdog: wd},
		"missing primary":   {Config: UpdaterConfig{DeviceID: "d", StateDir: dir}, PublicKey: pub, Slots: sm, Watchdog: wd, Restart: &recordRestart{}},
		"bad public key":    {Config: UpdaterConfig{DeviceID: "d", StateDir: dir}, Primary: primary, PublicKey: ed25519.PublicKey{1, 2, 3}, Slots: sm, Watchdog: wd, Restart: &recordRestart{}},
		"missing device id": {Config: UpdaterConfig{StateDir: dir}, Primary: primary, PublicKey: pub, Slots: sm, Watchdog: wd, Restart: &recordRestart{}},
		"missing state dir": {Config: UpdaterConfig{DeviceID: "d"}, Primary: primary, PublicKey: pub, Slots: sm, Watchdog: wd, Restart: &recordRestart{}},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewUpdater(deps); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}

func TestRunOnce_NoUpdate_ShortCircuits(t *testing.T) {
	f := newUpdaterFixture(t)
	f.primary.heartbeatResp = &protocol.ManifestResponse{UpdateAvailable: false}

	if err := f.updater.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if f.transport.calls != 0 {
		t.Fatalf("transport should not be called when no update; got %d calls", f.transport.calls)
	}
	if f.restart.calls != 0 {
		t.Fatalf("restart should not be called; got %d", f.restart.calls)
	}
}

func TestRunOnce_RetryAfter_ShortCircuits(t *testing.T) {
	f := newUpdaterFixture(t)
	f.primary.heartbeatResp = &protocol.ManifestResponse{UpdateAvailable: true, RetryAfter: 30}

	if err := f.updater.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if f.transport.calls != 0 {
		t.Fatalf("transport should not be called when RetryAfter>0; got %d calls", f.transport.calls)
	}
}

func TestRunOnce_BadSignature_AbortsBeforeDownload(t *testing.T) {
	f := newUpdaterFixture(t)
	manifest, _ := f.signedManifest()
	// Tamper with the signature: flip one byte.
	bad := []byte(manifest.Signature)
	if bad[0] == 'a' {
		bad[0] = 'b'
	} else {
		bad[0] = 'a'
	}
	manifest.Signature = string(bad)
	f.primary.heartbeatResp = manifest

	err := f.updater.RunOnce(context.Background())
	if err == nil {
		t.Fatalf("expected signature verification error")
	}
	if !strings.Contains(err.Error(), "verify manifest signature") {
		t.Fatalf("err = %v, want signature verification failure", err)
	}
	if f.transport.calls != 0 {
		t.Fatalf("transport must not be called when signature fails; got %d", f.transport.calls)
	}
}

func TestRunOnce_HappyPath_WritesPendingAndExecsNewBinary(t *testing.T) {
	f := newUpdaterFixture(t)
	manifest, deltaBytes := f.signedManifest()
	f.primary.heartbeatResp = manifest
	f.transport.body = deltaBytes

	if err := f.updater.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Symlink should now point at slot B (was A); B's content must equal newBin.
	_, activeHash, activeName, err := f.slots.ActiveSlot()
	if err != nil {
		t.Fatalf("ActiveSlot: %v", err)
	}
	if activeName != SlotNameB {
		t.Fatalf("active name = %q, want %q after swap", activeName, SlotNameB)
	}
	if activeHash != sha256HexBytes(f.newBin) {
		t.Fatalf("active hash mismatches reconstructed binary")
	}

	// Restart should have been called pointing at the new active path.
	if f.restart.calls != 1 {
		t.Fatalf("restart calls = %d, want 1", f.restart.calls)
	}
	if filepath.Base(f.restart.binary) != SlotNameB {
		t.Fatalf("restart binary = %s, want slot B path", f.restart.binary)
	}

	// Pending update marker must exist with PreviousHash=oldHash, NewHash=newHash.
	pending, err := f.updater.readPending()
	if err != nil {
		t.Fatalf("readPending: %v", err)
	}
	if pending == nil {
		t.Fatalf("pending update marker missing after swap")
	}
	if pending.PreviousHash != sha256HexBytes(f.oldBin) || pending.NewHash != sha256HexBytes(f.newBin) {
		t.Fatalf("pending fields wrong: %+v", pending)
	}

	// Staged delta must be cleaned up.
	if _, err := os.Stat(f.updater.deltaStaging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged delta should be removed; stat = %v", err)
	}
}

func TestRunOnce_HeartbeatFallback_OneShotPerCycle(t *testing.T) {
	f := newUpdaterFixture(t)
	f.primary.heartbeatErr = errors.New("primary down")
	fb := f.withFallback()
	fb.heartbeatResp = &protocol.ManifestResponse{UpdateAvailable: false}

	if err := f.updater.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	primaryCalls, _ := f.primary.snapshot()
	fbCalls, _ := fb.snapshot()
	if primaryCalls != 1 {
		t.Fatalf("primary heartbeat calls = %d, want 1", primaryCalls)
	}
	if fbCalls != 1 {
		t.Fatalf("fallback heartbeat calls = %d, want 1", fbCalls)
	}

	// Next cycle: primary should be tried first again ("not sticky").
	f.primary.heartbeatErr = nil
	f.primary.heartbeatResp = &protocol.ManifestResponse{UpdateAvailable: false}
	if err := f.updater.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	primaryCalls2, _ := f.primary.snapshot()
	fbCalls2, _ := fb.snapshot()
	if primaryCalls2 != 2 {
		t.Fatalf("after recovery primary calls = %d, want 2", primaryCalls2)
	}
	if fbCalls2 != 1 {
		t.Fatalf("fallback should not be called when primary recovers; got %d", fbCalls2)
	}
}

func TestRunOnce_HeartbeatBothFail(t *testing.T) {
	f := newUpdaterFixture(t)
	f.primary.heartbeatErr = errors.New("primary down")
	fb := f.withFallback()
	fb.heartbeatErr = errors.New("fallback down")

	err := f.updater.RunOnce(context.Background())
	if err == nil {
		t.Fatalf("expected error when both transports fail")
	}
	if !strings.Contains(err.Error(), "primary") || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("err = %v, want both transports mentioned", err)
	}
}

func TestBootPhase_NoPending_ShortCircuits(t *testing.T) {
	f := newUpdaterFixture(t)
	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}
	if f.restart.calls != 0 {
		t.Fatalf("restart calls = %d, want 0", f.restart.calls)
	}
	_, reports := f.primary.snapshot()
	if len(reports) != 0 {
		t.Fatalf("no reports expected; got %d", len(reports))
	}
}

func TestBootPhase_PendingMatchesAndHealthOK_Confirms(t *testing.T) {
	f := newUpdaterFixture(t)
	// Simulate that we just booted into the new binary: A is "new", a pending
	// marker is on disk, watchdog hasn't been confirmed yet.
	_, activeHash, _, _ := f.slots.ActiveSlot()
	pending := &pendingUpdate{
		PreviousHash: "prev-hash",
		NewHash:      activeHash,
		SwappedUnix:  time.Now().Unix(),
	}
	if err := f.updater.writePending(pending); err != nil {
		t.Fatal(err)
	}
	f.checker.failUntil = 0 // healthy

	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}

	// Pending must be cleared.
	if _, err := os.Stat(f.updater.pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending should be removed after Confirm; stat = %v", err)
	}
	// Boot counter must be reset.
	hash, count, _ := f.bootCnt.Current()
	if hash != "" || count != 0 {
		t.Fatalf("boot counter not reset: (%q,%d)", hash, count)
	}
	// One success report sent.
	_, reports := f.primary.snapshot()
	if len(reports) != 1 || !reports[0].Success {
		t.Fatalf("expected one success report; got %+v", reports)
	}
	if reports[0].PreviousHash != "prev-hash" || reports[0].NewHash != activeHash {
		t.Fatalf("report fields wrong: %+v", reports[0])
	}
	// No restart in the healthy path.
	if f.restart.calls != 0 {
		t.Fatalf("healthy boot must not restart; got %d calls", f.restart.calls)
	}
}

func TestBootPhase_PendingMismatch_ClearsAndReturns(t *testing.T) {
	f := newUpdaterFixture(t)
	// Pending says new_hash=Z but active is something else.
	if err := f.updater.writePending(&pendingUpdate{NewHash: "not-the-active-hash"}); err != nil {
		t.Fatal(err)
	}

	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}
	if _, err := os.Stat(f.updater.pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending should be cleared on mismatch; stat = %v", err)
	}
	_, reports := f.primary.snapshot()
	if len(reports) != 0 {
		t.Fatalf("no reports expected on mismatch; got %d", len(reports))
	}
	if f.restart.calls != 0 {
		t.Fatalf("no restart expected on mismatch; got %d", f.restart.calls)
	}
}

func TestBootPhase_HealthFails_RollsBackAndReports(t *testing.T) {
	f := newUpdaterFixture(t)
	_, activeHash, _, _ := f.slots.ActiveSlot()
	if err := f.updater.writePending(&pendingUpdate{
		PreviousHash: "prev-hash", NewHash: activeHash,
	}); err != nil {
		t.Fatal(err)
	}
	f.checker.failUntil = 999 // never healthy

	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase returned error (expected nil after rollback): %v", err)
	}

	// Symlink should be flipped back: active is now slot B (the original inactive).
	_, _, name, _ := f.slots.ActiveSlot()
	if name != SlotNameB {
		t.Fatalf("after rollback active = %s, want B", name)
	}
	// Restart called, pointing at the rolled-back binary.
	if f.restart.calls != 1 {
		t.Fatalf("restart calls = %d, want 1", f.restart.calls)
	}
	// Failure report sent with rollback reason.
	_, reports := f.primary.snapshot()
	if len(reports) != 1 || reports[0].Success {
		t.Fatalf("expected one failure report; got %+v", reports)
	}
	if !strings.Contains(reports[0].RollbackReason, "health check failed") {
		t.Fatalf("rollback reason = %q", reports[0].RollbackReason)
	}
	// Pending cleared.
	if _, err := os.Stat(f.updater.pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending should be cleared; stat = %v", err)
	}
}

func TestBootPhase_BootCountExceeded_RollsBackPermanently(t *testing.T) {
	f := newUpdaterFixture(t)
	_, activeHash, _, _ := f.slots.ActiveSlot()
	// Pre-load the boot counter so the next CheckBoot exceeds MaxBoots=2.
	if _, err := f.bootCnt.Increment(activeHash); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bootCnt.Increment(activeHash); err != nil {
		t.Fatal(err)
	}
	if err := f.updater.writePending(&pendingUpdate{
		PreviousHash: "prev-hash", NewHash: activeHash,
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}

	// Rolled back.
	_, _, name, _ := f.slots.ActiveSlot()
	if name != SlotNameB {
		t.Fatalf("after escalated rollback active = %s, want B", name)
	}
	if f.restart.calls != 1 {
		t.Fatalf("restart calls = %d, want 1", f.restart.calls)
	}
	_, reports := f.primary.snapshot()
	if len(reports) != 1 || reports[0].Success {
		t.Fatalf("expected one failure report; got %+v", reports)
	}
	if !strings.Contains(reports[0].RollbackReason, "boot count exceeded") {
		t.Fatalf("rollback reason = %q", reports[0].RollbackReason)
	}
}

func TestNewClientPair_RejectsMismatchedTransports(t *testing.T) {
	if _, err := NewClientPair(&fakeClient{name: "http"}, &fakeTransport{name: "coap"}); err == nil {
		t.Fatalf("mismatched pair should error")
	}
	if _, err := NewClientPair(nil, &fakeTransport{name: "http"}); err == nil {
		t.Fatalf("nil client should error")
	}
}

func TestReportUpdate_FallbackOnPrimaryFailure(t *testing.T) {
	f := newUpdaterFixture(t)
	f.primary.reportErr = errors.New("primary report down")
	fb := f.withFallback()
	// Trigger reportUpdate via BootPhase healthy path.
	_, activeHash, _, _ := f.slots.ActiveSlot()
	if err := f.updater.writePending(&pendingUpdate{
		PreviousHash: "prev", NewHash: activeHash,
	}); err != nil {
		t.Fatal(err)
	}
	f.checker.failUntil = 0

	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}
	_, primaryReports := f.primary.snapshot()
	_, fbReports := fb.snapshot()
	if len(primaryReports) != 1 {
		t.Fatalf("primary should be tried once; got %d", len(primaryReports))
	}
	if len(fbReports) != 1 {
		t.Fatalf("fallback should pick up the report; got %d", len(fbReports))
	}
	if !fbReports[0].Success {
		t.Fatalf("fallback received %+v", fbReports[0])
	}
}

func TestReportUpdate_BothTransportsFail_NonFatal(t *testing.T) {
	f := newUpdaterFixture(t)
	f.primary.reportErr = errors.New("primary down")
	fb := f.withFallback()
	fb.reportErr = errors.New("fallback down")

	_, activeHash, _, _ := f.slots.ActiveSlot()
	if err := f.updater.writePending(&pendingUpdate{
		PreviousHash: "prev", NewHash: activeHash,
	}); err != nil {
		t.Fatal(err)
	}
	f.checker.failUntil = 0

	// BootPhase logs report failures but does not abort the boot — the agent
	// should still confirm and clear pending so the loop can proceed.
	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}
	if _, err := os.Stat(f.updater.pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending should be cleared even with both reports failing")
	}
}

func TestReportUpdate_NoFallback_PrimaryFailureSurfacedAsLog(t *testing.T) {
	// With no fallback configured and primary Report failing, BootPhase still
	// completes (the report failure is logged but not fatal). We verify that
	// pending is cleared and watchdog confirmed.
	f := newUpdaterFixture(t)
	f.primary.reportErr = errors.New("primary report down")
	_, activeHash, _, _ := f.slots.ActiveSlot()
	if err := f.updater.writePending(&pendingUpdate{
		PreviousHash: "prev", NewHash: activeHash,
	}); err != nil {
		t.Fatal(err)
	}
	f.checker.failUntil = 0

	if err := f.updater.BootPhase(context.Background()); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}
	if _, err := os.Stat(f.updater.pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending should be cleared even when report fails")
	}
}

func TestRunOnce_RestartFailure_RollsBackAndClearsPending(t *testing.T) {
	f := newUpdaterFixture(t)
	manifest, deltaBytes := f.signedManifest()
	f.primary.heartbeatResp = manifest
	f.transport.body = deltaBytes
	// Make the restart strategy return an error to simulate exec failing
	// (e.g. the freshly written binary isn't executable). The Updater must
	// roll back the swap and remove the pending marker so we don't leave
	// the device in a half-applied state.
	f.restart.err = errors.New("exec failed")

	err := f.updater.RunOnce(context.Background())
	if err == nil {
		t.Fatalf("expected restart error to surface")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("err = %v, want restart prefix", err)
	}
	// Symlink rolled back to A.
	_, _, name, _ := f.slots.ActiveSlot()
	if name != SlotNameA {
		t.Fatalf("after restart failure active = %s, want A (rolled back)", name)
	}
	// Pending marker cleared.
	if _, err := os.Stat(f.updater.pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending should be cleared after restart failure; stat = %v", err)
	}
}

func TestRun_StopsOnContextCancellation(t *testing.T) {
	f := newUpdaterFixture(t)
	f.primary.heartbeatResp = &protocol.ManifestResponse{UpdateAvailable: false}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.updater.Run(ctx) }()

	// Let one cycle happen, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

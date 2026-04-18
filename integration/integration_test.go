//go:build integration

// Package integration runs end-to-end tests that boot the real update-server
// in-process and drive a real edge-agent updater against it. These tests are
// gated behind the `integration` build tag so they don't run on every
// `task ci`; invoke with `task test-integration` (or
// `go test -tags integration ./integration/...`).
package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amplia/ota-updater/pkg/agent"
	"github.com/amplia/ota-updater/pkg/protocol"
	"github.com/amplia/ota-updater/internal/server"
)

// recordingRestart captures Restart invocations without actually exec'ing.
// Mirrors the unit-test helper but lives here to keep the integration test
// self-contained.
type recordingRestart struct {
	calls   atomic.Int32
	binary  atomic.Value // string
	argv    atomic.Value // []string
	failErr error
}

func (r *recordingRestart) Restart(_ context.Context, binary string, argv []string) error {
	r.calls.Add(1)
	r.binary.Store(binary)
	r.argv.Store(argv)
	return r.failErr
}

// e2eFixture wires the real update-server with the real edge-agent updater
// against a single httptest server. The server-side report handler is
// intercepted to capture UpdateReport payloads for end-of-test assertions.
type e2eFixture struct {
	t *testing.T

	// Server-side
	httpSrv      *httptest.Server
	store        *server.Store
	manifester   *server.Manifester
	reportsLock  atomic.Pointer[[]protocol.UpdateReport]
	reportCount  atomic.Int32
	heartbeatCnt atomic.Int32

	// Agent-side
	updater     *agent.Updater
	slots       *agent.SlotManager
	bootCounter *agent.BootCounter
	restart     *recordingRestart
	stateDir    string
	pubKey      ed25519.PublicKey

	// Payloads
	oldBin     []byte
	newBin     []byte
	targetHash string
	oldHash    string
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func setupE2E(t *testing.T) *e2eFixture {
	t.Helper()
	root := t.TempDir()

	// --- Generate keypair (the real server signs, the real agent verifies).
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// --- Server filesystem layout.
	srvDir := filepath.Join(root, "server")
	binariesDir := filepath.Join(srvDir, "binaries")
	deltasDir := filepath.Join(srvDir, "deltas")
	for _, d := range []string{binariesDir, deltasDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// New binary that the server will roll out as the target.
	newBin := bytes.Repeat([]byte("NEW-CONTENT-"), 1024) // ~13 KiB, easily diffed
	targetPath := filepath.Join(srvDir, "target.bin")
	if err := os.WriteFile(targetPath, newBin, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	// Old binary that the agent currently runs; the server must know about
	// it so it can build a delta from it.
	oldBin := bytes.Repeat([]byte("OLD-CONTENT-"), 1024) // ~12 KiB

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := server.Open(context.Background(), server.StoreOptions{
		BinariesDir: binariesDir, DeltasDir: deltasDir, TargetPath: targetPath,
	}, logger)
	if err != nil {
		t.Fatalf("server.Open: %v", err)
	}
	if _, err := store.RegisterBinary(oldBin); err != nil {
		t.Fatalf("RegisterBinary(old): %v", err)
	}

	manifester := server.NewManifester(store, priv, server.ManifesterConfig{
		ChunkSize:     4096,
		RetryAfter:    1, // keep tests fast: 1s instead of 30s
		TargetVersion: "v2",
	}, logger)

	apiHandler := server.NewHTTPHandler(server.HTTPConfig{
		Store: store, Manifester: manifester, Logger: logger,
	})

	// Middleware: count heartbeats and capture report bodies so the test can
	// assert what the server actually received without modifying production.
	f := &e2eFixture{
		t:          t,
		store:      store,
		manifester: manifester,
		oldBin:     oldBin,
		newBin:     newBin,
		targetHash: sha256Hex(newBin),
		oldHash:    sha256Hex(oldBin),
		pubKey:     pub,
	}
	empty := []protocol.UpdateReport(nil)
	f.reportsLock.Store(&empty)

	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == protocol.PathHeartbeat:
			f.heartbeatCnt.Add(1)
		case r.Method == http.MethodPost && r.URL.Path == protocol.PathReport:
			body, err := io.ReadAll(r.Body)
			if err == nil {
				var rep protocol.UpdateReport
				if json.Unmarshal(body, &rep) == nil {
					prev := *f.reportsLock.Load()
					updated := append(prev, rep)
					f.reportsLock.Store(&updated)
					f.reportCount.Add(1)
				}
				// Replace the body so the downstream handler can re-decode.
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		apiHandler.ServeHTTP(w, r)
	})
	f.httpSrv = httptest.NewServer(wrapped)
	t.Cleanup(f.httpSrv.Close)

	// --- Agent filesystem layout.
	agentDir := filepath.Join(root, "agent")
	slotsDir := filepath.Join(agentDir, "slots")
	if err := os.MkdirAll(slotsDir, 0o755); err != nil {
		t.Fatalf("mkdir slots: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, agent.SlotNameA), oldBin, 0o755); err != nil {
		t.Fatalf("write slot A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, agent.SlotNameB), []byte("seed-B"), 0o755); err != nil {
		t.Fatalf("write slot B: %v", err)
	}
	activeSymlink := filepath.Join(agentDir, "current")
	if err := os.Symlink(filepath.Join(slotsDir, agent.SlotNameA), activeSymlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	f.stateDir = agentDir

	slots, err := agent.NewSlotManager(slotsDir, activeSymlink, logger)
	if err != nil {
		t.Fatalf("slot manager: %v", err)
	}
	f.slots = slots

	bc, err := agent.NewBootCounter(filepath.Join(agentDir, agent.BootCountFileName))
	if err != nil {
		t.Fatalf("boot counter: %v", err)
	}
	f.bootCounter = bc

	// --- Wire the agent's primary client at the httptest server.
	httpClient := f.httpSrv.Client()
	client := agent.NewHTTPClient(f.httpSrv.URL, httpClient)
	transport := agent.NewHTTPTransport(httpClient)
	primary, err := agent.NewClientPair(client, transport)
	if err != nil {
		t.Fatalf("client pair: %v", err)
	}

	// HealthChecker: real heartbeat against our server. Once the new binary
	// is active (slot B), the server should answer UpdateAvailable=false →
	// the heartbeat returns nil → the watchdog window passes.
	checker := &agent.DefaultHealthChecker{
		Heartbeat: func(ctx context.Context) error {
			_, hash, _, err := slots.ActiveSlot()
			if err != nil {
				return err
			}
			_, err = client.Heartbeat(ctx, &protocol.Heartbeat{
				DeviceID: "e2e-device", VersionHash: hash, Timestamp: time.Now().Unix(),
			})
			return err
		},
	}
	wd, err := agent.NewWatchdog(bc, checker, agent.WatchdogConfig{
		Timeout: 1 * time.Second, Retries: 3, MaxBoots: 2,
	}, logger)
	if err != nil {
		t.Fatalf("watchdog: %v", err)
	}

	f.restart = &recordingRestart{}
	f.updater, err = agent.NewUpdater(agent.UpdaterDeps{
		Config: agent.UpdaterConfig{
			DeviceID:      "e2e-device",
			StateDir:      agentDir,
			CheckInterval: 50 * time.Millisecond,
			MaxRetries:    3,
			RetryBackoff:  10 * time.Millisecond,
			// Mirror YAML defaults: gate off (empty Version) + auto_update=true.
			AutoUpdate: true,
			MaxBump:    agent.MaxBumpMajor,
		},
		Primary:   primary,
		Slots:     slots,
		PublicKey: pub,
		Watchdog:  wd,
		Restart:   f.restart,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("updater: %v", err)
	}

	return f
}

// reports returns a snapshot of the captured UpdateReports.
func (f *e2eFixture) reports() []protocol.UpdateReport {
	out := *f.reportsLock.Load()
	cp := make([]protocol.UpdateReport, len(out))
	copy(cp, out)
	return cp
}

// runUntilSwap drives RunOnce in a tight loop until the agent triggers a
// restart (proxy for "swap completed and exec attempted"), with a hard
// timeout. The first heartbeat will likely return RetryAfter > 0 because
// the server generates the delta asynchronously; the loop retries until
// the manifest is signed and the download proceeds.
func (f *e2eFixture) runUntilSwap(ctx context.Context, timeout time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := f.updater.RunOnce(ctx); err != nil {
			f.t.Logf("RunOnce err (will retry): %v", err)
		}
		if f.restart.calls.Load() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatalf("agent never triggered restart within %v (heartbeats=%d, reports=%d)",
		timeout, f.heartbeatCnt.Load(), f.reportCount.Load())
}

// TestE2E_FullCycle exercises the end-to-end happy path: the agent runs at
// oldBin, the server advertises newBin, the agent downloads the signed
// delta, patches the inactive slot, swaps the symlink, writes the pending
// marker and "restarts". The test then plays the role of the freshly-booted
// new binary by invoking BootPhase, which should run the watchdog window,
// confirm health, clear the marker and report success to the server.
func TestE2E_FullCycle(t *testing.T) {
	f := setupE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// --- Drive the update.
	f.runUntilSwap(ctx, 15*time.Second)

	// --- Mid-cycle assertions: swap happened, pending marker present.
	_, activeHash, activeName, err := f.slots.ActiveSlot()
	if err != nil {
		t.Fatalf("ActiveSlot post-swap: %v", err)
	}
	if activeName != agent.SlotNameB {
		t.Fatalf("post-swap active = %q, want %q", activeName, agent.SlotNameB)
	}
	if activeHash != f.targetHash {
		t.Fatalf("active hash = %s, want target %s", activeHash, f.targetHash)
	}
	// Slot B's bytes must equal newBin exactly — the whole point of the
	// integration test is to prove bsdiff+zstd round-trip end-to-end.
	slotBPath := filepath.Join(f.stateDir, "slots", agent.SlotNameB)
	got, err := os.ReadFile(slotBPath)
	if err != nil {
		t.Fatalf("read slot B: %v", err)
	}
	if !bytes.Equal(got, f.newBin) {
		t.Fatalf("reconstructed slot B differs from newBin (%d vs %d bytes)", len(got), len(f.newBin))
	}

	pendingPath := filepath.Join(f.stateDir, ".pending_update")
	pendingData, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("pending marker missing: %v", err)
	}
	if !strings.Contains(string(pendingData), f.targetHash) {
		t.Fatalf("pending marker doesn't reference target hash: %s", pendingData)
	}

	if f.restart.calls.Load() != 1 {
		t.Fatalf("restart called %d times, want 1", f.restart.calls.Load())
	}

	// --- Play the role of the freshly-booted new binary: BootPhase reads
	// the pending marker, runs the watchdog window (real heartbeat against
	// the server), confirms, and reports success.
	if err := f.updater.BootPhase(ctx); err != nil {
		t.Fatalf("BootPhase: %v", err)
	}

	// --- Final assertions: pending cleared, boot counter reset, report sent.
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending marker should be cleared after Confirm; stat err = %v", err)
	}
	hash, count, _ := f.bootCounter.Current()
	if hash != "" || count != 0 {
		t.Fatalf("boot counter not reset post-confirm: (%q,%d)", hash, count)
	}

	// Wait briefly for the report POST to flush. RunOnce + BootPhase are
	// synchronous, but the test middleware races slightly with the response
	// write — a short bounded poll avoids flakiness without sleeping blindly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.reportCount.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	reports := f.reports()
	if len(reports) != 1 {
		t.Fatalf("expected exactly 1 update report, got %d", len(reports))
	}
	rep := reports[0]
	if !rep.Success {
		t.Fatalf("expected success report, got %+v", rep)
	}
	if rep.NewHash != f.targetHash {
		t.Fatalf("report.NewHash = %s, want %s", rep.NewHash, f.targetHash)
	}
	if rep.PreviousHash != f.oldHash {
		t.Fatalf("report.PreviousHash = %s, want %s", rep.PreviousHash, f.oldHash)
	}
	if rep.DeviceID != "e2e-device" {
		t.Fatalf("report.DeviceID = %s", rep.DeviceID)
	}
}

package server

import (
	"context"
	"errors"
	"testing"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// budgetFixture returns the standard stack with a delta size budget applied.
// The cap is read at call time, so setting it on the built store is enough
// and avoids rebuilding the registry and keypair.
func budgetFixture(t *testing.T, budget int64) *serverFixture {
	t.Helper()
	f := newServerFixture(t)
	f.Store.opts.DeltaMaxSourceBytes = budget
	return f
}

// The whole point of the cap: an oversized pair must still produce a usable
// manifest. Telling the device "retry later" forever would be the stranding
// failure the full-download path exists to remove.
func TestBudget_OversizedPairFallsBackToFullDownload(t *testing.T) {
	f := budgetFixture(t, 1024) // 1 KiB: both fixtures are far larger

	resp, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !resp.UpdateAvailable {
		t.Fatalf("an oversized pair must still yield an update")
	}
	if resp.RetryAfter != 0 {
		t.Fatalf("RetryAfter=%d: the device would poll forever instead of updating",
			resp.RetryAfter)
	}
	if resp.DeltaEndpoint != "" {
		t.Fatalf("a delta was offered despite the budget: %q", resp.DeltaEndpoint)
	}
	if resp.BinaryEndpoint != protocol.BinaryPath(f.TargetHash) {
		t.Fatalf("BinaryEndpoint=%q, want the full target", resp.BinaryEndpoint)
	}
	if resp.Signature == "" {
		t.Fatalf("the fallback manifest must still be signed")
	}

	// And no generation was dispatched behind the scenes.
	if f.Store.HasDelta(f.OldHash, f.TargetHash) {
		t.Fatalf("a delta was generated despite exceeding the budget")
	}
}

// Under the budget, nothing changes.
func TestBudget_UnderTheCapStillDiffs(t *testing.T) {
	f := budgetFixture(t, 1<<30) // 1 GiB: everything fits
	if _, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash); err != nil {
		t.Fatalf("EnsureDelta under the cap: %v", err)
	}
	resp, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatal(err)
	}
	if resp.DeltaEndpoint == "" {
		t.Fatalf("a pair under the budget must still be diffed")
	}
}

// The store enforces independently of the manifester, because pkg/server is
// public surface: a direct consumer must not be able to allocate the process
// to death.
func TestBudget_StoreRefusesDirectCallers(t *testing.T) {
	f := budgetFixture(t, 1024)

	_, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash)
	if err == nil {
		t.Fatalf("EnsureDelta ignored the budget")
	}
	if !errors.Is(err, ErrDeltaTooLarge) {
		t.Fatalf("err = %v, want ErrDeltaTooLarge so callers can fall back", err)
	}
	if f.Store.StartDeltaGeneration(f.OldHash, f.TargetHash) {
		t.Fatalf("StartDeltaGeneration dispatched work over the budget")
	}
}

// Lowering the cap must not invalidate deltas that already exist: the memory
// was spent when they were generated, and serving them costs nothing.
func TestBudget_AlreadyCachedDeltaIsStillServed(t *testing.T) {
	f := newServerFixture(t) // no cap
	if _, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash); err != nil {
		t.Fatal(err)
	}
	// Now apply a cap that the pair violates.
	f.Store.opts.DeltaMaxSourceBytes = 1024

	if ok, _ := f.Store.CanDiff(f.OldHash, f.TargetHash); !ok {
		t.Fatalf("an already-cached delta must remain diffable")
	}
	data, found, err := f.Store.GetDeltaBytes(context.Background(), f.OldHash, f.TargetHash)
	if err != nil || !found || len(data) == 0 {
		t.Fatalf("cached delta no longer served: err=%v found=%v", err, found)
	}
}

func TestBudget_ZeroDisablesTheCap(t *testing.T) {
	f := budgetFixture(t, 0)
	if ok, reason := f.Store.CanDiff(f.OldHash, f.TargetHash); !ok {
		t.Fatalf("a zero budget must disable the cap, got %q", reason)
	}
}

// An unknown binary is not a budget decision - reporting it as one would send
// callers down the wrong recovery path.
func TestBudget_UnknownBinaryIsNotABudgetFailure(t *testing.T) {
	f := budgetFixture(t, 1<<30)
	if ok, _ := f.Store.CanDiff(sha256HexOf([]byte("never registered")), f.TargetHash); !ok {
		t.Fatalf("an absent binary must not be reported as over budget")
	}
}

func TestConfig_DeltaMaxSourceDefaultsOn(t *testing.T) {
	cfg, err := LoadConfig(writeYAML(t, `
crypto:
  private_key: "k"
admin:
  token: "a-strong-enough-token-of-32-chars"
artifacts:
  - name: "agent"
    binary: "/srv/agent"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.DeltaMaxSourceMB <= 0 {
		t.Fatalf("the delta budget must default ON; a default-off cap protects nobody")
	}
}

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// slotsFixture creates a fresh A/B layout with A active.
func slotsFixture(t *testing.T) (*SlotManager, string) {
	t.Helper()
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	if err := os.MkdirAll(slotsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, SlotNameA), []byte("binary-A-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, SlotNameB), []byte("binary-B-v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "current")
	if err := os.Symlink(filepath.Join(slotsDir, SlotNameA), active); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewSlotManager(slotsDir, active, logger)
	if err != nil {
		t.Fatalf("NewSlotManager: %v", err)
	}
	return m, dir
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestSlotManager_ActiveAndInactive(t *testing.T) {
	m, _ := slotsFixture(t)

	path, hash, name, err := m.ActiveSlot()
	if err != nil {
		t.Fatalf("ActiveSlot: %v", err)
	}
	if name != SlotNameA {
		t.Fatalf("active name = %q, want %q", name, SlotNameA)
	}
	if hash != sha256Hex([]byte("binary-A-v1")) {
		t.Fatalf("active hash mismatch")
	}
	if filepath.Base(path) != SlotNameA {
		t.Fatalf("active path = %s", path)
	}

	inactivePath, inactiveName, err := m.InactiveSlot()
	if err != nil {
		t.Fatalf("InactiveSlot: %v", err)
	}
	if inactiveName != SlotNameB {
		t.Fatalf("inactive name = %q", inactiveName)
	}
	if filepath.Base(inactivePath) != SlotNameB {
		t.Fatalf("inactive path = %s", inactivePath)
	}
}

func TestSlotManager_WriteToInactive_AtomicOverwrite(t *testing.T) {
	m, _ := slotsFixture(t)

	newContent := bytes.Repeat([]byte("X"), 4096)
	if err := m.WriteToInactive(bytes.NewReader(newContent)); err != nil {
		t.Fatalf("WriteToInactive: %v", err)
	}
	inactivePath, _, _ := m.InactiveSlot()
	got, err := os.ReadFile(inactivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newContent) {
		t.Fatalf("inactive content mismatch (%d bytes)", len(got))
	}

	// Active slot unchanged.
	_, activeHash, activeName, _ := m.ActiveSlot()
	if activeName != SlotNameA || activeHash != sha256Hex([]byte("binary-A-v1")) {
		t.Fatalf("active slot was unexpectedly touched: name=%s hash=%s", activeName, activeHash)
	}
}

func TestSlotManager_SwapAndRollback(t *testing.T) {
	m, _ := slotsFixture(t)

	// Write new binary to B then swap to it.
	payload := []byte("binary-B-v2")
	if err := m.WriteToInactive(bytes.NewReader(payload)); err != nil {
		t.Fatalf("WriteToInactive: %v", err)
	}
	if err := m.Swap(); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	_, hash, name, err := m.ActiveSlot()
	if err != nil {
		t.Fatalf("ActiveSlot after swap: %v", err)
	}
	if name != SlotNameB {
		t.Fatalf("post-swap active name = %q", name)
	}
	if hash != sha256Hex(payload) {
		t.Fatalf("post-swap active hash mismatch")
	}

	// Rollback → A active again.
	if err := m.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	_, _, name, _ = m.ActiveSlot()
	if name != SlotNameA {
		t.Fatalf("post-rollback active name = %q", name)
	}
}

func TestSlotManager_Swap_LeavesInactiveBinaryIntact(t *testing.T) {
	m, _ := slotsFixture(t)
	payload := []byte("binary-B-v3")
	_ = m.WriteToInactive(bytes.NewReader(payload))
	_ = m.Swap()

	// B was active; A is now inactive and must still have its original content.
	inactivePath, inactiveName, _ := m.InactiveSlot()
	if inactiveName != SlotNameA {
		t.Fatalf("expected inactive = A, got %q", inactiveName)
	}
	got, _ := os.ReadFile(inactivePath)
	if string(got) != "binary-A-v1" {
		t.Fatalf("A slot was modified: %q", got)
	}
}

func TestSlotManager_RejectsInvalidSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	slotsDir := filepath.Join(dir, "slots")
	_ = os.MkdirAll(slotsDir, 0o755)
	_ = os.WriteFile(filepath.Join(slotsDir, SlotNameA), []byte("a"), 0o755)
	_ = os.WriteFile(filepath.Join(slotsDir, SlotNameB), []byte("b"), 0o755)
	// Symlink points at something that isn't A/B.
	bogus := filepath.Join(slotsDir, "stray")
	_ = os.WriteFile(bogus, []byte("stray"), 0o755)
	active := filepath.Join(dir, "current")
	_ = os.Symlink(bogus, active)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewSlotManager(slotsDir, active, logger)
	if err != nil {
		t.Fatalf("NewSlotManager: %v", err)
	}
	if _, _, _, err := m.ActiveSlot(); err == nil {
		t.Fatalf("expected error on malformed symlink target")
	}
}

func TestSlotManager_ConstructorValidation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewSlotManager("", "/x", logger); err == nil {
		t.Fatalf("empty slots_dir should error")
	}
	if _, err := NewSlotManager("/x", "", logger); err == nil {
		t.Fatalf("empty active_symlink should error")
	}
	if _, err := NewSlotManager("/nonexistent-path-for-slots", "/x", logger); err == nil {
		t.Fatalf("missing slots_dir should error")
	}
}

// The checked variants exist because Swap/Rollback are toggles: on public
// library surface, a consumer calling Rollback twice would flip forward into
// the binary that just failed.
func TestSlotManager_SwapTo_RefusesUnexpectedContent(t *testing.T) {
	sm, dir := slotsFixture(t)
	_ = dir

	_, inactiveName, err := sm.InactiveSlot()
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.WriteToInactive(bytes.NewReader([]byte("the new binary"))); err != nil {
		t.Fatal(err)
	}
	want := sha256Hex([]byte("the new binary"))

	// Wrong expectation: must refuse and leave the symlink alone.
	_, activeBefore, _, _ := sm.ActiveSlot()
	if err := sm.SwapTo(sha256Hex([]byte("something else"))); err == nil {
		t.Fatalf("SwapTo accepted a slot whose content it was not told to expect")
	}
	_, activeAfter, _, _ := sm.ActiveSlot()
	if activeBefore != activeAfter {
		t.Fatalf("a refused SwapTo still moved the symlink")
	}

	// Empty expectation is a caller bug, not "skip the check".
	if err := sm.SwapTo(""); err == nil {
		t.Fatalf("SwapTo(\"\") must be rejected rather than treated as unchecked")
	}

	// Correct expectation: swaps.
	if err := sm.SwapTo(want); err != nil {
		t.Fatalf("SwapTo with the right hash: %v", err)
	}
	_, activeHash, activeName, err := sm.ActiveSlot()
	if err != nil {
		t.Fatal(err)
	}
	if activeName != inactiveName || activeHash != want {
		t.Fatalf("after SwapTo active=%s/%s, want %s/%s", activeName, activeHash, inactiveName, want)
	}

	// And it is no longer a blind toggle: a second call refuses, because the
	// now-inactive slot holds the old binary.
	if err := sm.SwapTo(want); err == nil {
		t.Fatalf("a second SwapTo with the same hash must refuse, not flip back")
	}
}

func TestSlotManager_RollbackTo_RefusesUnexpectedContent(t *testing.T) {
	sm, _ := slotsFixture(t)
	if err := sm.RollbackTo(sha256Hex([]byte("not what is there"))); err == nil {
		t.Fatalf("RollbackTo accepted an unexpected destination")
	}
}

// Provisioning faults must fail at construction, not hours later on the first
// update cycle.
func TestNewSlotManager_RejectsIncompleteLayout(t *testing.T) {
	logger := discardLogger()

	t.Run("missing slot file", func(t *testing.T) {
		dir := t.TempDir()
		slots := filepath.Join(dir, "slots")
		if err := os.MkdirAll(slots, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(slots, SlotNameA), []byte("a"), 0o755); err != nil {
			t.Fatal(err)
		}
		// B is missing.
		link := filepath.Join(dir, "current")
		if err := os.Symlink(filepath.Join(slots, SlotNameA), link); err != nil {
			t.Fatal(err)
		}
		if _, err := NewSlotManager(slots, link, logger); err == nil {
			t.Fatalf("expected construction to fail with slot B missing")
		}
	})

	t.Run("missing symlink", func(t *testing.T) {
		dir := t.TempDir()
		slots := filepath.Join(dir, "slots")
		if err := os.MkdirAll(slots, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{SlotNameA, SlotNameB} {
			if err := os.WriteFile(filepath.Join(slots, n), []byte(n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := NewSlotManager(slots, filepath.Join(dir, "current"), logger); err == nil {
			t.Fatalf("expected construction to fail with no active symlink")
		}
	})
}

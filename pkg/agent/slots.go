package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/carlosprados/ota-updater/pkg/atomicio"
)

// Canonical slot names. An A/B layout is fixed to keep the on-disk contract
// trivial and debuggable.
const (
	SlotNameA = "A"
	SlotNameB = "B"
)

// SlotManager owns the on-disk A/B binary layout:
//
//	<slotsDir>/A                — binary for slot A
//	<slotsDir>/B                — binary for slot B
//	<activeSymlink>             — symlink → one of the above
//
// The manager never deletes a slot. Swap only flips the symlink; the inactive
// slot is overwritten by WriteToInactive before being activated.
//
// This type is part of the public library surface (see CLAUDE.md). Construct
// it with NewSlotManager or embed SlotManagerConfig directly.
type SlotManager struct {
	slotsDir      string
	activeSymlink string
	logger        *slog.Logger
}

// NewSlotManager returns a SlotManager rooted at slotsDir with its symlink at
// activeSymlink. The caller is responsible for initial layout: both slot
// files and the symlink must already exist (typically seeded by the device
// provisioning step, not by the agent).
//
// That layout is verified here rather than on first use. A missing slot or a
// dangling symlink is a provisioning fault, and surfacing it at construction
// turns it into a clear startup error instead of a confusing failure on the
// first update cycle - potentially hours later, on a device in the field.
func NewSlotManager(slotsDir, activeSymlink string, logger *slog.Logger) (*SlotManager, error) {
	if slotsDir == "" {
		return nil, errors.New("slots_dir is required")
	}
	if activeSymlink == "" {
		return nil, errors.New("active_symlink is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	info, err := os.Stat(slotsDir)
	if err != nil {
		return nil, fmt.Errorf("stat slots_dir %q: %w", slotsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("slots_dir %q is not a directory", slotsDir)
	}
	for _, name := range []string{SlotNameA, SlotNameB} {
		p := filepath.Join(slotsDir, name)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("slot %s missing at %q (provisioning must seed both slots): %w",
				name, p, err)
		}
	}
	// Lstat, not Stat: we are asserting the symlink exists, and Stat would
	// follow it and conflate "no symlink" with "dangling symlink".
	if _, err := os.Lstat(activeSymlink); err != nil {
		return nil, fmt.Errorf("active symlink missing at %q (provisioning must create it): %w",
			activeSymlink, err)
	}
	return &SlotManager{
		slotsDir:      slotsDir,
		activeSymlink: activeSymlink,
		logger:        logger,
	}, nil
}

// ActiveSlot returns the path, SHA-256 hex, and slot name (A or B) of the
// binary currently targeted by the active symlink.
func (s *SlotManager) ActiveSlot() (path, hash, name string, err error) {
	path, name, err = s.resolveActive()
	if err != nil {
		return "", "", "", err
	}
	hash, err = hashFile(path)
	if err != nil {
		return "", "", "", fmt.Errorf("hash active slot: %w", err)
	}
	return path, hash, name, nil
}

// InactiveSlot returns the path and name (A or B) of the slot NOT currently
// active. WriteToInactive writes here; Swap flips activity to it.
func (s *SlotManager) InactiveSlot() (path, name string, err error) {
	_, activeName, err := s.resolveActive()
	if err != nil {
		return "", "", err
	}
	other := SlotNameB
	if activeName == SlotNameB {
		other = SlotNameA
	}
	return filepath.Join(s.slotsDir, other), other, nil
}

// WriteToInactive streams r into the inactive slot using a temp-file + rename
// so a crash mid-write never leaves a partially-written binary at the slot's
// final path. The whole body is written before activation.
func (s *SlotManager) WriteToInactive(r io.Reader) error {
	dst, name, err := s.InactiveSlot()
	if err != nil {
		return err
	}
	if err := atomicio.WriteReader(dst, r, 0o755, s.logger); err != nil {
		return fmt.Errorf("write inactive slot %s: %w", name, err)
	}
	s.logger.Info("inactive slot written",
		"op", "slot_write", "slot", name, "path", dst,
	)
	return nil
}

// Swap flips the active symlink to point at the inactive slot, making it the
// new active. Implemented as a temp-symlink + rename over the destination so
// readers never observe the symlink in an intermediate/broken state.
//
// # Contract
//
// Swap is a TOGGLE, not an idempotent operation: it activates whichever slot
// is currently inactive, without checking what that slot contains. Calling it
// twice returns to where you started.
//
// It does not verify the destination hash. The Updater verifies the
// reconstructed binary before calling, and pairs the call with the
// .pending_update marker so a crash mid-sequence is recoverable — callers
// driving SlotManager directly must provide the equivalent, or use SwapTo,
// which refuses to activate a slot whose content is not what the caller
// expects.
func (s *SlotManager) Swap() error {
	inactivePath, inactiveName, err := s.InactiveSlot()
	if err != nil {
		return err
	}
	if err := atomicio.ReplaceSymlink(inactivePath, s.activeSymlink, s.logger); err != nil {
		return fmt.Errorf("swap symlink: %w", err)
	}
	s.logger.Info("active slot swapped",
		"op", "slot_swap", "new_active", inactiveName, "target", inactivePath,
	)
	return nil
}

// Rollback swaps the active symlink back to the previously-inactive slot.
// Functionally identical to Swap(), but kept as a separate entry point so
// operational traces distinguish deliberate rollbacks from forward upgrades.
//
// The same contract as Swap applies: this is a toggle and asserts nothing
// about the destination. Called twice it flips forward again, into the binary
// that just failed. Use RollbackTo when the caller knows which version it
// expects to land on.
func (s *SlotManager) Rollback() error {
	inactivePath, inactiveName, err := s.InactiveSlot()
	if err != nil {
		return err
	}
	if err := atomicio.ReplaceSymlink(inactivePath, s.activeSymlink, s.logger); err != nil {
		return fmt.Errorf("rollback symlink: %w", err)
	}
	s.logger.Warn("active slot rolled back",
		"op", "slot_rollback", "new_active", inactiveName, "target", inactivePath,
	)
	return nil
}

// SwapTo activates the inactive slot only if its contents hash to
// expectedHash, and returns an error without touching the symlink otherwise.
//
// This is the safe entry point for library consumers: it converts the toggle
// semantics of Swap into an assertion about where the device ends up, so a
// double call or a mismatched slot fails loudly instead of silently
// activating the wrong binary.
func (s *SlotManager) SwapTo(expectedHash string) error {
	if err := s.assertInactiveHash(expectedHash, "swap"); err != nil {
		return err
	}
	return s.Swap()
}

// RollbackTo is SwapTo with rollback logging semantics: it activates the
// inactive slot only if it hashes to expectedHash, which is normally the
// version the device was running before the update being reverted.
func (s *SlotManager) RollbackTo(expectedHash string) error {
	if err := s.assertInactiveHash(expectedHash, "rollback"); err != nil {
		return err
	}
	return s.Rollback()
}

// assertInactiveHash checks the destination slot before a checked swap.
func (s *SlotManager) assertInactiveHash(expectedHash, op string) error {
	if expectedHash == "" {
		return fmt.Errorf("%s: expected hash is required", op)
	}
	path, name, err := s.InactiveSlot()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	actual, err := hashFile(path)
	if err != nil {
		return fmt.Errorf("%s: hash inactive slot %s: %w", op, name, err)
	}
	if actual != expectedHash {
		return fmt.Errorf("%s refused: inactive slot %s hashes to %s, expected %s",
			op, name, actual, expectedHash)
	}
	return nil
}

// resolveActive reads the symlink and returns its absolute target plus the
// canonical slot name (A or B) derived from the basename.
func (s *SlotManager) resolveActive() (path, name string, err error) {
	dst, err := os.Readlink(s.activeSymlink)
	if err != nil {
		return "", "", fmt.Errorf("readlink %q: %w", s.activeSymlink, err)
	}
	if !filepath.IsAbs(dst) {
		dst = filepath.Join(filepath.Dir(s.activeSymlink), dst)
	}
	dst = filepath.Clean(dst)
	base := filepath.Base(dst)
	if base != SlotNameA && base != SlotNameB {
		return "", "", fmt.Errorf("active symlink points at %q, expected %q or %q",
			base, SlotNameA, SlotNameB)
	}
	return dst, base, nil
}

// hashFile returns the SHA-256 hex of the entire file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

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

	"github.com/amplia/ota-updater/pkg/atomicio"
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

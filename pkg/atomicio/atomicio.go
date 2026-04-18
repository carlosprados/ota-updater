// Package atomicio implements durable atomic writes on POSIX filesystems:
// temp file in the destination directory, fsync of the file, rename over
// the destination, fsync of the directory. Each step matters:
//
//   - The temp-file-then-rename dance guarantees readers never observe a
//     partial write. rename(2) is atomic within the same filesystem.
//   - fsync(file) flushes the file contents to stable storage. Without it,
//     a power loss after rename can leave the dirent pointing at a file
//     whose blocks were never written — the fresh-named file appears empty
//     (or worse, full of stale data) on reboot.
//   - fsync(dir) flushes the dirent change itself. Without it, a power loss
//     after rename but before the FS's next journal commit can leave the
//     dirent pointing at the OLD name, i.e. the rename never happened.
//
// For OTA use cases (slot binaries, boot counters, pending-update markers,
// download resume state) this is not optional: the device fleet is assumed
// to power-cycle at any moment, and state that survives the crash must be
// indistinguishable from state that was never attempted.
//
// All functions are safe to call concurrently over DIFFERENT paths. For the
// same path, callers must serialize externally (singleflight, mutex, etc.).
package atomicio

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// WriteFile writes data to path atomically and durably. It returns on
// success only after the file's bytes AND the directory's new dirent have
// been fsynced to stable storage. The resulting file has the given mode.
//
// If the caller passes a nil logger, slog.Default() is used. The logger is
// only consulted when fsync of the *directory* fails on a system that does
// not support it (notably Windows); the operation is considered successful
// in that case because the file body fsync already succeeded.
func WriteFile(path string, data []byte, mode fs.FileMode, logger *slog.Logger) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicio: create temp in %q: %w", dir, err)
	}
	tmp := f.Name()
	if err := writeAndSync(f, data, mode); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: close temp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: rename %q -> %q: %w", tmp, path, err)
	}
	if err := fsyncDir(dir); err != nil {
		loggerOrDefault(logger).Warn("atomicio: directory fsync failed (continuing)",
			"op", "atomicio_fsync_dir", "dir", dir, "err", err,
		)
	}
	return nil
}

// WriteReader is WriteFile with a streaming input. Useful for slot binaries
// that the caller does not want to hold in RAM.
func WriteReader(path string, r io.Reader, mode fs.FileMode, logger *slog.Logger) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicio: create temp in %q: %w", dir, err)
	}
	tmp := f.Name()
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: copy into %q: %w", tmp, err)
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: chmod %q: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: fsync %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: close temp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: rename %q -> %q: %w", tmp, path, err)
	}
	if err := fsyncDir(dir); err != nil {
		loggerOrDefault(logger).Warn("atomicio: directory fsync failed (continuing)",
			"op", "atomicio_fsync_dir", "dir", dir, "err", err,
		)
	}
	return nil
}

// ReplaceSymlink atomically flips linkPath to point at target. The
// replacement uses symlink+rename so readers always see either the old or
// the new link, never a missing one. The directory is fsynced afterwards
// so the new link survives power-loss.
//
// Both target and linkPath may be absolute or relative; they are not
// interpreted here.
func ReplaceSymlink(target, linkPath string, logger *slog.Logger) error {
	dir := filepath.Dir(linkPath)
	tmp := linkPath + ".tmp"
	// A previous crash could have left the tmp symlink behind; best-effort
	// cleanup before trying to create a new one.
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("atomicio: create temp symlink %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomicio: rename symlink %q -> %q: %w", tmp, linkPath, err)
	}
	if err := fsyncDir(dir); err != nil {
		loggerOrDefault(logger).Warn("atomicio: directory fsync failed (continuing)",
			"op", "atomicio_fsync_dir", "dir", dir, "err", err,
		)
	}
	return nil
}

// writeAndSync writes data to an open file, chmods it, and fsyncs the
// contents. Shared helper for the byte-slice path of WriteFile.
func writeAndSync(f *os.File, data []byte, mode fs.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("atomicio: write %q: %w", f.Name(), err)
	}
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("atomicio: chmod %q: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("atomicio: fsync %q: %w", f.Name(), err)
	}
	return nil
}

// fsyncDir opens dir read-only and Sync()s it. Not all filesystems/OSes
// support fsync on a directory; callers are expected to treat a failure
// here as a warning, not an error. The file body was already fsynced, so
// the data itself is durable — only the dirent change is at risk.
func fsyncDir(dir string) error {
	df, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}
	return nil
}

func loggerOrDefault(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}

// SweepStaleTemp removes files in dir whose basename matches any of the
// given prefixes AND whose modification time is older than maxAge. Returns
// the count removed and the count that failed. Errors listing dir or
// stating files are logged (warn) and the sweep continues.
//
// Use at startup to clean up temp files left by a prior crashed process:
// atomic writes create "<tmp>" next to the destination, rename over it,
// then fsync. A process kill between create and rename leaves the tmp
// behind. The rename path ("dest") is always safe to keep; only the
// "<tmp>" files are candidates for sweeping.
//
// Default safety: maxAge should be well above any legitimate in-flight
// write duration (24h is recommended) so a sweep running during a fresh
// bootstrap cannot clobber a concurrent operation.
func SweepStaleTemp(dir string, prefixes []string, maxAge time.Duration, logger *slog.Logger) (removed, failed int) {
	log := loggerOrDefault(logger)
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("atomicio: sweep: read dir",
			"op", "atomicio_sweep", "dir", dir, "err", err,
		)
		return 0, 0
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		name := entry.Name()
		if !hasAnyPrefix(name, prefixes) {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := entry.Info()
		if err != nil {
			log.Warn("atomicio: sweep: stat", "path", path, "err", err)
			continue
		}
		if info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			failed++
			log.Warn("atomicio: sweep: remove", "path", path, "err", err)
			continue
		}
		removed++
		log.Info("atomicio: swept stale temp",
			"op", "atomicio_sweep", "path", path, "age", time.Since(info.ModTime()),
		)
	}
	return removed, failed
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}

// IsDiskFull reports whether err (or any of its wrapped errors) is a
// syscall.ENOSPC "no space left on device". Cross-platform: on non-Unix
// targets this returns false because syscall.ENOSPC is undefined there.
// Callers should use it to distinguish "transient network hiccup" from
// "operator must free disk" so retry budgets don't get burned on an
// unrecoverable condition.
func IsDiskFull(err error) bool {
	return isDiskFull(err)
}

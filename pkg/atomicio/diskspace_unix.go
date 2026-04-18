//go:build unix

package atomicio

import (
	"fmt"
	"syscall"
)

// Free returns the free and total bytes of the filesystem containing path.
// Used for operator-facing disk-space checks at startup; not in the hot
// path. Errors from syscall.Statfs are propagated so the caller can
// decide to degrade gracefully.
func Free(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("atomicio: statfs %q: %w", path, err)
	}
	// Bsize is the FS's preferred allocation unit; on Linux this is the
	// logical block size. Bavail is blocks available to non-root users —
	// what a service process can actually use.
	bsize := uint64(st.Bsize)
	free = st.Bavail * bsize
	total = st.Blocks * bsize
	return free, total, nil
}

//go:build !unix

package atomicio

import "errors"

// errDiskSpaceUnsupported is returned by Free on platforms where we don't
// (yet) have a Statfs-equivalent. Callers log a warning and skip the check
// rather than fail.
var errDiskSpaceUnsupported = errors.New("atomicio: disk-space probe not supported on this platform")

// Free on non-Unix always returns an error. Production targets are Linux.
// This exists so `go build` works for developers on Windows; the signature
// matches the unix variant so callers compile everywhere.
func Free(_ string) (free, total uint64, err error) {
	return 0, 0, errDiskSpaceUnsupported
}

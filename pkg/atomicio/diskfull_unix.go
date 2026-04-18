//go:build unix

package atomicio

import (
	"errors"
	"syscall"
)

// isDiskFull on Unix checks for ENOSPC anywhere in the error chain.
func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ENOSPC)
}

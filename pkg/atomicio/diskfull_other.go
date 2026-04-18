//go:build !unix

package atomicio

// isDiskFull on non-Unix platforms always returns false. The server and
// agent target Linux; this file exists only so the package builds for
// dev workflows on macOS/Windows (which are also unix-family for the
// Windows-excluded cases — this build tag is a belt-and-suspenders
// fallback, the real path is the unix build).
func isDiskFull(_ error) bool {
	return false
}

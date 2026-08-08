package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// ArtifactKey identifies one independently-updatable artifact in a fleet.
//
// A single update-server serves N components × M versions × K architectures.
// The (Name, OS, Arch) triple is the coordinate of a *publication track*: the
// server holds exactly one current target per key, and every agent heartbeat
// names the key it wants an update for.
//
// OS and Arch are optional but coupled — either both are set or neither is.
// Architecture-independent artifacts (config bundles, ML models, asset packs)
// use the bare Name form.
//
// Canonical string form:
//
//	"keystone-agent/linux/arm64"   // platform-specific
//	"tariff-table"                 // platform-independent
//
// The string form is used verbatim in HTTP paths, CBOR payloads and log
// fields, so the charset is deliberately narrow — see Validate.
type ArtifactKey struct {
	Name string
	OS   string // GOOS value, e.g. "linux"; empty for platform-independent
	Arch string // GOARCH value, e.g. "arm64"; empty for platform-independent
}

// Limits on each component. Generous for real names, tight enough that a key
// can never bloat a log line, a CBOR heartbeat or a filesystem path.
const (
	MaxArtifactNameLen = 64
	MaxArtifactOSLen   = 32
	MaxArtifactArchLen = 32
)

// ErrEmptyArtifactKey is returned when parsing an empty string. Callers that
// treat "" as "the default artifact" must check for empty BEFORE parsing.
var ErrEmptyArtifactKey = errors.New("artifact key is empty")

// String returns the canonical form: "name" or "name/os/arch".
func (k ArtifactKey) String() string {
	if k.OS == "" && k.Arch == "" {
		return k.Name
	}
	return k.Name + "/" + k.OS + "/" + k.Arch
}

// IsZero reports whether the key is entirely unset.
func (k ArtifactKey) IsZero() bool {
	return k.Name == "" && k.OS == "" && k.Arch == ""
}

// ParseArtifactKey parses the canonical string form and validates the result.
// Accepts exactly one or three slash-separated segments.
func ParseArtifactKey(s string) (ArtifactKey, error) {
	if s == "" {
		return ArtifactKey{}, ErrEmptyArtifactKey
	}
	parts := strings.Split(s, "/")
	var k ArtifactKey
	switch len(parts) {
	case 1:
		k = ArtifactKey{Name: parts[0]}
	case 3:
		k = ArtifactKey{Name: parts[0], OS: parts[1], Arch: parts[2]}
	default:
		return ArtifactKey{}, fmt.Errorf(
			"artifact key %q: want \"name\" or \"name/os/arch\", got %d segments",
			s, len(parts))
	}
	if err := k.Validate(); err != nil {
		return ArtifactKey{}, err
	}
	return k, nil
}

// Validate enforces the charset and length limits.
//
// The charset is restrictive on purpose. Keys reach the filesystem (retention
// bookkeeping), HTTP request paths and structured log fields, so anything that
// could be read as a path segment, a traversal sequence or a log-injection
// newline is rejected at the boundary rather than escaped at each use site.
// In particular ".", ".." and any string containing a path separator, NUL or
// whitespace are invalid.
func (k ArtifactKey) Validate() error {
	if k.Name == "" {
		return errors.New("artifact name is required")
	}
	if len(k.Name) > MaxArtifactNameLen {
		return fmt.Errorf("artifact name too long: %d > %d", len(k.Name), MaxArtifactNameLen)
	}
	if !validArtifactName(k.Name) {
		return fmt.Errorf("artifact name %q: only [A-Za-z0-9._-] allowed, and not \".\" or \"..\"", k.Name)
	}
	// OS and Arch are coupled: a half-specified platform is always a bug in
	// the caller, and silently accepting it would create two distinct keys
	// that a human would read as the same artifact.
	if (k.OS == "") != (k.Arch == "") {
		return fmt.Errorf("artifact %q: os and arch must both be set or both empty (os=%q arch=%q)",
			k.Name, k.OS, k.Arch)
	}
	if k.OS != "" {
		if len(k.OS) > MaxArtifactOSLen {
			return fmt.Errorf("artifact os too long: %d > %d", len(k.OS), MaxArtifactOSLen)
		}
		if len(k.Arch) > MaxArtifactArchLen {
			return fmt.Errorf("artifact arch too long: %d > %d", len(k.Arch), MaxArtifactArchLen)
		}
		if !validPlatformToken(k.OS) {
			return fmt.Errorf("artifact os %q: only [a-z0-9] allowed", k.OS)
		}
		if !validPlatformToken(k.Arch) {
			return fmt.Errorf("artifact arch %q: only [a-z0-9] allowed", k.Arch)
		}
	}
	return nil
}

// validArtifactName accepts [A-Za-z0-9._-]+ while rejecting the two relative
// path names. Dots are allowed because real component names carry them
// ("io.amplia.gateway"), which is exactly why "." and ".." need excluding.
func validArtifactName(s string) bool {
	if s == "." || s == ".." {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// validPlatformToken accepts [a-z0-9]+. GOOS and GOARCH values are always
// lowercase alphanumeric ("linux", "arm64", "386", "riscv64").
func validPlatformToken(s string) bool {
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

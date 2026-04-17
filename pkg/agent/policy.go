package agent

import (
	"strings"

	"golang.org/x/mod/semver"
)

// BumpLevel classifies a semver transition from a local version to a remote
// target. The numeric ordering (None < Patch < Minor < Major) lets a policy
// cap be expressed as "at most bump X" via a plain ≤ comparison.
type BumpLevel int

const (
	// BumpNone — remote == local, or remote older than local. No update.
	BumpNone BumpLevel = iota
	// BumpPatch — only the patch segment grew (1.2.3 → 1.2.4).
	BumpPatch
	// BumpMinor — the minor segment grew, patch reset (1.2.3 → 1.3.0).
	BumpMinor
	// BumpMajor — the major segment grew (1.2.3 → 2.0.0).
	BumpMajor
	// BumpUnknown — one or both versions failed to parse as semver. The
	// caller must consult UnknownVersionPolicy to decide how to proceed.
	BumpUnknown
)

// MaxBump is the policy-side cap: a transition is accepted automatically
// iff its BumpLevel is <= this cap.
type MaxBump int

const (
	MaxBumpNone MaxBump = iota
	MaxBumpPatch
	MaxBumpMinor
	MaxBumpMajor
)

// String labels for logs and config round-trips.
func (m MaxBump) String() string {
	switch m {
	case MaxBumpNone:
		return "none"
	case MaxBumpPatch:
		return "patch"
	case MaxBumpMinor:
		return "minor"
	case MaxBumpMajor:
		return "major"
	}
	return "major"
}

// ParseMaxBump accepts the YAML strings ("none" | "patch" | "minor" | "major",
// case-insensitive, "" → "major"). Unknown inputs return ok=false so Validate
// can surface a clear error at config load.
func ParseMaxBump(s string) (MaxBump, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "major":
		return MaxBumpMajor, true
	case "minor":
		return MaxBumpMinor, true
	case "patch":
		return MaxBumpPatch, true
	case "none":
		return MaxBumpNone, true
	}
	return 0, false
}

// UnknownVersionPolicy controls what happens when the server advertises a
// TargetVersion that doesn't parse as semver — either because it's a legacy
// free-form label or because the string was tampered with.
type UnknownVersionPolicy int

const (
	// UnknownDeny — refuse to auto-apply when the remote version can't be
	// parsed as semver. Conservative default for fleets that care about
	// deterministic gating.
	UnknownDeny UnknownVersionPolicy = iota
	// UnknownAllow — apply anyway when the remote version can't be parsed.
	// Preserves the pre-semver behavior for legacy deployments.
	UnknownAllow
)

func (u UnknownVersionPolicy) String() string {
	if u == UnknownAllow {
		return "allow"
	}
	return "deny"
}

// ParseUnknownVersionPolicy accepts "deny" | "allow" (case-insensitive,
// "" → "deny"). Unknown inputs return ok=false.
func ParseUnknownVersionPolicy(s string) (UnknownVersionPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "deny":
		return UnknownDeny, true
	case "allow":
		return UnknownAllow, true
	}
	return 0, false
}

// ComputeBump classifies the transition from local to remote. Accepts both
// "1.2.3" and "v1.2.3" on either side; golang.org/x/mod/semver is strict
// about the "v" prefix so we normalize first.
func ComputeBump(local, remote string) BumpLevel {
	l := ensureSemverPrefix(local)
	r := ensureSemverPrefix(remote)
	if !semver.IsValid(l) || !semver.IsValid(r) {
		return BumpUnknown
	}
	cmp := semver.Compare(l, r)
	if cmp >= 0 {
		// remote equals or is older than local → no update to apply.
		return BumpNone
	}
	if semver.Major(l) != semver.Major(r) {
		return BumpMajor
	}
	if semver.MajorMinor(l) != semver.MajorMinor(r) {
		return BumpMinor
	}
	return BumpPatch
}

// AllowedByPolicy reports whether a bump transition is permitted by the
// configured cap. BumpUnknown and BumpNone always return false — the caller
// is expected to have handled them already (unknown via UnknownVersionPolicy,
// none by short-circuiting before reaching here).
func AllowedByPolicy(bump BumpLevel, max MaxBump) bool {
	if bump == BumpUnknown || bump == BumpNone {
		return false
	}
	switch bump {
	case BumpPatch:
		return max >= MaxBumpPatch
	case BumpMinor:
		return max >= MaxBumpMinor
	case BumpMajor:
		return max >= MaxBumpMajor
	}
	return false
}

// ensureSemverPrefix guarantees the leading 'v' required by x/mod/semver,
// without rejecting inputs that already carry it. An empty string stays
// empty so IsValid reports false and the caller falls through to the
// UnknownVersionPolicy branch.
func ensureSemverPrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		return "v" + strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	}
	return "v" + s
}

package agent

import "testing"

func TestComputeBump(t *testing.T) {
	cases := []struct {
		local, remote string
		want          BumpLevel
	}{
		// Same / older → BumpNone
		{"1.2.3", "1.2.3", BumpNone},
		{"1.2.3", "1.2.2", BumpNone},
		{"2.0.0", "1.9.9", BumpNone},

		// Patch bumps
		{"1.2.3", "1.2.4", BumpPatch},
		{"v1.2.3", "v1.2.9", BumpPatch},
		{"0.0.1", "0.0.2", BumpPatch},

		// Minor bumps (may jump patch too)
		{"1.2.3", "1.3.0", BumpMinor},
		{"1.2.3", "1.4.7", BumpMinor},
		{"0.0.1", "0.1.0", BumpMinor},

		// Major bumps
		{"1.2.3", "2.0.0", BumpMajor},
		{"1.9.9", "2.0.0", BumpMajor},
		{"0.9.9", "1.0.0", BumpMajor},

		// v-prefix tolerance on both sides
		{"v1.0.0", "1.0.1", BumpPatch},
		{"1.0.0", "v1.0.1", BumpPatch},

		// Short forms accepted by x/mod/semver: v1.2 ≡ v1.2.0
		{"1.2", "1.2.3", BumpPatch},
		{"1", "1.1.0", BumpMinor},

		// Invalid versions → BumpUnknown
		{"", "1.2.3", BumpUnknown},
		{"1.2.3", "", BumpUnknown},
		{"not-semver", "1.2.3", BumpUnknown},
		{"1.2.3", "garbage", BumpUnknown},
		{"1.2.3.4", "1.2.3.5", BumpUnknown}, // 4-segment is not semver
	}
	for _, tc := range cases {
		got := ComputeBump(tc.local, tc.remote)
		if got != tc.want {
			t.Errorf("ComputeBump(%q, %q) = %v, want %v", tc.local, tc.remote, got, tc.want)
		}
	}
}

func TestParseMaxBump(t *testing.T) {
	cases := map[string]MaxBump{
		"":      MaxBumpMajor, // empty → default
		"major": MaxBumpMajor,
		"Major": MaxBumpMajor,
		"MINOR": MaxBumpMinor,
		"patch": MaxBumpPatch,
		"none":  MaxBumpNone,
	}
	for in, want := range cases {
		got, ok := ParseMaxBump(in)
		if !ok {
			t.Errorf("ParseMaxBump(%q) returned ok=false", in)
			continue
		}
		if got != want {
			t.Errorf("ParseMaxBump(%q) = %v, want %v", in, got, want)
		}
	}
	if _, ok := ParseMaxBump("bogus"); ok {
		t.Errorf("ParseMaxBump(bogus) should report invalid")
	}
}

func TestParseUnknownVersionPolicy(t *testing.T) {
	cases := map[string]UnknownVersionPolicy{
		"":      UnknownDeny,
		"deny":  UnknownDeny,
		"Deny":  UnknownDeny,
		"allow": UnknownAllow,
		"ALLOW": UnknownAllow,
	}
	for in, want := range cases {
		got, ok := ParseUnknownVersionPolicy(in)
		if !ok {
			t.Errorf("ParseUnknownVersionPolicy(%q) returned ok=false", in)
			continue
		}
		if got != want {
			t.Errorf("ParseUnknownVersionPolicy(%q) = %v, want %v", in, got, want)
		}
	}
	if _, ok := ParseUnknownVersionPolicy("ignore"); ok {
		t.Errorf("ParseUnknownVersionPolicy(ignore) should report invalid")
	}
}

func TestAllowedByPolicy(t *testing.T) {
	// (bump, max) → allowed
	matrix := []struct {
		bump    BumpLevel
		max     MaxBump
		allowed bool
	}{
		// BumpNone/BumpUnknown never pass
		{BumpNone, MaxBumpMajor, false},
		{BumpUnknown, MaxBumpMajor, false},

		// MaxBumpNone blocks everything real
		{BumpPatch, MaxBumpNone, false},
		{BumpMinor, MaxBumpNone, false},
		{BumpMajor, MaxBumpNone, false},

		// MaxBumpPatch allows only patches
		{BumpPatch, MaxBumpPatch, true},
		{BumpMinor, MaxBumpPatch, false},
		{BumpMajor, MaxBumpPatch, false},

		// MaxBumpMinor allows patch + minor
		{BumpPatch, MaxBumpMinor, true},
		{BumpMinor, MaxBumpMinor, true},
		{BumpMajor, MaxBumpMinor, false},

		// MaxBumpMajor allows everything valid
		{BumpPatch, MaxBumpMajor, true},
		{BumpMinor, MaxBumpMajor, true},
		{BumpMajor, MaxBumpMajor, true},
	}
	for _, tc := range matrix {
		got := AllowedByPolicy(tc.bump, tc.max)
		if got != tc.allowed {
			t.Errorf("AllowedByPolicy(%v, %v) = %v, want %v", tc.bump, tc.max, got, tc.allowed)
		}
	}
}

func TestMaxBump_StringRoundTrip(t *testing.T) {
	// MaxBump.String() must be re-parseable by ParseMaxBump.
	for _, m := range []MaxBump{MaxBumpNone, MaxBumpPatch, MaxBumpMinor, MaxBumpMajor} {
		parsed, ok := ParseMaxBump(m.String())
		if !ok || parsed != m {
			t.Errorf("roundtrip failed for %v: got %v (ok=%v)", m, parsed, ok)
		}
	}
}

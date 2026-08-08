package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestArtifactKey_String(t *testing.T) {
	cases := []struct {
		key  ArtifactKey
		want string
	}{
		{ArtifactKey{Name: "tariff-table"}, "tariff-table"},
		{ArtifactKey{Name: "keystone-agent", OS: "linux", Arch: "arm64"}, "keystone-agent/linux/arm64"},
		{ArtifactKey{Name: "io.amplia.gw", OS: "linux", Arch: "386"}, "io.amplia.gw/linux/386"},
	}
	for _, tc := range cases {
		if got := tc.key.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestParseArtifactKey_RoundTrip(t *testing.T) {
	for _, s := range []string{"tariff-table", "keystone-agent/linux/arm64", "a_b.c/darwin/amd64"} {
		k, err := ParseArtifactKey(s)
		if err != nil {
			t.Fatalf("ParseArtifactKey(%q): %v", s, err)
		}
		if got := k.String(); got != s {
			t.Errorf("round trip of %q produced %q", s, got)
		}
	}
}

func TestParseArtifactKey_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"two segments", "name/linux"},
		{"four segments", "a/b/c/d"},
		{"traversal name", ".."},
		{"single dot", "."},
		{"traversal segment", "../../etc/passwd"},
		{"space in name", "my app"},
		{"slash-escaped path", "a%2Fb"},
		{"newline injection", "app\nfake=1"},
		{"nul byte", "app\x00"},
		{"uppercase arch", "app/linux/ARM64"},
		{"dot in arch", "app/linux/arm.64"},
		{"name too long", strings.Repeat("a", MaxArtifactNameLen+1)},
		{"os too long", "app/" + strings.Repeat("l", MaxArtifactOSLen+1) + "/arm64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseArtifactKey(tc.in); err == nil {
				t.Fatalf("ParseArtifactKey(%q) succeeded; want an error", tc.in)
			}
		})
	}
}

func TestParseArtifactKey_EmptyIsDistinguishable(t *testing.T) {
	// Callers treat "" as "the default artifact", so the error has to be
	// separable from a genuinely malformed key.
	_, err := ParseArtifactKey("")
	if !errors.Is(err, ErrEmptyArtifactKey) {
		t.Fatalf("empty key error = %v, want ErrEmptyArtifactKey", err)
	}
	_, err = ParseArtifactKey("bad key")
	if errors.Is(err, ErrEmptyArtifactKey) {
		t.Fatalf("malformed key must not report as empty")
	}
}

func TestArtifactKey_Validate_PlatformCoupling(t *testing.T) {
	// A half-specified platform would silently create two keys a human reads
	// as the same artifact, so it is rejected at the boundary.
	if err := (ArtifactKey{Name: "app", OS: "linux"}).Validate(); err == nil {
		t.Fatalf("os without arch should be rejected")
	}
	if err := (ArtifactKey{Name: "app", Arch: "arm64"}).Validate(); err == nil {
		t.Fatalf("arch without os should be rejected")
	}
	if err := (ArtifactKey{Name: "app"}).Validate(); err != nil {
		t.Fatalf("bare name should be valid: %v", err)
	}
}

func TestArtifactKey_IsZero(t *testing.T) {
	if !(ArtifactKey{}).IsZero() {
		t.Fatalf("zero value should report IsZero")
	}
	if (ArtifactKey{Name: "x"}).IsZero() {
		t.Fatalf("named key should not report IsZero")
	}
}

func TestBinaryPath(t *testing.T) {
	h := strings.Repeat("ab", 32)
	if got, want := BinaryPath(h), PathBinary+"/"+h; got != want {
		t.Fatalf("BinaryPath = %q, want %q", got, want)
	}
}

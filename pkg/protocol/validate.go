package protocol

import (
	"errors"
	"fmt"
)

// Bounds on the free-form string fields that reach logs, filesystem paths and
// cache keys. They are deliberately generous for real devices and tight
// enough that a hostile or buggy peer cannot bloat a log line or a CBOR
// payload. Over CoAP every message is unauthenticated and trivially spoofed,
// so these are the only limits that exist.
const (
	MaxDeviceIDLen = 128
	MaxVersionLen  = 64
	// HashLen is the length of a SHA-256 digest in lowercase hex.
	HashLen = 64
)

// ErrInvalidMessage is the sentinel wrapping every validation failure, so
// transports can map it to 400 / 4.00 without matching on strings.
var ErrInvalidMessage = errors.New("invalid message")

// IsValidHash reports whether s is exactly 64 lowercase hex characters, i.e.
// a SHA-256 digest as this protocol writes it.
//
// This is the single definition shared by the wire types, the transport
// handlers and the retention sweeper. Content addresses derived from peer
// input end up in filesystem paths (`<hash>.bin`, `<from>_<to>.delta.zst`),
// so anything that is not exactly this shape must never reach path
// construction — "../secret" is a valid Go string and a very poor hash.
func IsValidHash(s string) bool {
	if len(s) != HashLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// validPrintable rejects control characters, which are the log-injection
// vector: a newline inside a device ID lets a peer forge log records. Any
// other rune is allowed so non-ASCII device names keep working.
func validPrintable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Validate checks the fields a server will log, cache or route on.
//
// Note what it does NOT check: VersionHash. A device whose stored version
// state is corrupt still deserves an update — that is the entire reason the
// full-download fallback exists — so an unusable VersionHash is handled as
// "a version this server does not know" rather than rejected. The server
// must therefore treat VersionHash as untrusted and gate it with IsValidHash
// before letting it reach any filesystem path. See Manifester.Build.
func (h *Heartbeat) Validate() error {
	if h == nil {
		return fmt.Errorf("%w: nil heartbeat", ErrInvalidMessage)
	}
	if h.DeviceID == "" {
		return fmt.Errorf("%w: device_id is required", ErrInvalidMessage)
	}
	if len(h.DeviceID) > MaxDeviceIDLen {
		return fmt.Errorf("%w: device_id too long: %d > %d",
			ErrInvalidMessage, len(h.DeviceID), MaxDeviceIDLen)
	}
	if !validPrintable(h.DeviceID) {
		return fmt.Errorf("%w: device_id contains control characters", ErrInvalidMessage)
	}
	if len(h.Version) > MaxVersionLen {
		return fmt.Errorf("%w: version too long: %d > %d",
			ErrInvalidMessage, len(h.Version), MaxVersionLen)
	}
	if !validPrintable(h.Version) {
		return fmt.Errorf("%w: version contains control characters", ErrInvalidMessage)
	}
	// An empty Artifact means "the default track" and is valid; a non-empty
	// one must parse, which also bounds its charset and length.
	if h.Artifact != "" {
		if _, err := ParseArtifactKey(h.Artifact); err != nil {
			return fmt.Errorf("%w: artifact: %s", ErrInvalidMessage, err)
		}
	}
	return nil
}

// Validate checks an update report before it is logged. Reports are a pure
// sink — nothing is derived from them — so the only concern is that a peer
// cannot use them to forge or bloat log records.
func (r *UpdateReport) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil report", ErrInvalidMessage)
	}
	if r.DeviceID == "" {
		return fmt.Errorf("%w: device_id is required", ErrInvalidMessage)
	}
	if len(r.DeviceID) > MaxDeviceIDLen {
		return fmt.Errorf("%w: device_id too long: %d > %d",
			ErrInvalidMessage, len(r.DeviceID), MaxDeviceIDLen)
	}
	if !validPrintable(r.DeviceID) {
		return fmt.Errorf("%w: device_id contains control characters", ErrInvalidMessage)
	}
	// Hashes here are advisory (they describe what the device did, and the
	// server derives nothing from them) but they are logged, so bound them.
	for name, h := range map[string]string{
		"previous_hash": r.PreviousHash,
		"new_hash":      r.NewHash,
	} {
		if h != "" && !IsValidHash(h) {
			return fmt.Errorf("%w: %s is not a SHA-256 hex digest", ErrInvalidMessage, name)
		}
	}
	if len(r.RollbackReason) > MaxRollbackReasonLen {
		return fmt.Errorf("%w: rollback_reason too long: %d > %d",
			ErrInvalidMessage, len(r.RollbackReason), MaxRollbackReasonLen)
	}
	if !validPrintable(r.RollbackReason) {
		return fmt.Errorf("%w: rollback_reason contains control characters", ErrInvalidMessage)
	}
	return nil
}

// MaxRollbackReasonLen bounds the free-text failure description a device may
// attach to a report. Long enough for a wrapped Go error chain.
const MaxRollbackReasonLen = 512

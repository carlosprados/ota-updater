package agent

import (
	"context"
	"testing"
	"time"

	"github.com/plgd-dev/go-coap/v3/net/blockwise"
)

// Regression for the review finding: every CoAP dial used to be a bare
// udp.Dial(host), so the whole `server.coap` config block was parsed and
// discarded — and ctx could not cancel a dial, only the request after it.
func TestDialCoAP_HonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 192.0.2.0/24 is TEST-NET-1: guaranteed non-routable, so this can never
	// succeed by accident on a machine with unusual networking.
	start := time.Now()
	_, err := dialCoAP(ctx, "192.0.2.1:5683", CoAPOptions{DialTimeout: 30 * time.Second})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a cancelled context must fail the dial")
	}
	// The point is that it returns on cancellation rather than waiting out
	// the dial timeout.
	if elapsed > 5*time.Second {
		t.Fatalf("dial took %v despite a cancelled context; ctx is not reaching udp.Dial", elapsed)
	}
}

func TestCoAPOptions_ZeroValueIsValid(t *testing.T) {
	// A zero value must still produce a usable option set (just the context),
	// so callers that do not tune anything keep go-coap's defaults.
	opts := CoAPOptions{}.dialOptions(context.Background())
	if len(opts) != 1 {
		t.Fatalf("zero CoAPOptions produced %d options, want only the context", len(opts))
	}
}

func TestCoAPOptions_EachKnobAddsAnOption(t *testing.T) {
	base := len(CoAPOptions{}.dialOptions(context.Background()))
	cases := map[string]CoAPOptions{
		"dial timeout":    {DialTimeout: time.Second},
		"ack timeout":     {ACKTimeout: time.Second},
		"max retransmits": {MaxRetransmits: 7},
		"block size":      {BlockSize: 512},
		"keepalive":       {Keepalive: time.Minute},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if got := len(o.dialOptions(context.Background())); got != base+1 {
				t.Fatalf("%s produced %d options, want %d — the knob is being dropped",
					name, got, base+1)
			}
		})
	}

	// ACK timeout and max retransmits share one go-coap option.
	both := CoAPOptions{ACKTimeout: time.Second, MaxRetransmits: 7}
	if got := len(both.dialOptions(context.Background())); got != base+1 {
		t.Fatalf("ack+retransmits produced %d options, want %d", got, base+1)
	}
}

func TestBlockSZX(t *testing.T) {
	valid := map[int]blockwise.SZX{
		16: blockwise.SZX16, 32: blockwise.SZX32, 64: blockwise.SZX64,
		128: blockwise.SZX128, 256: blockwise.SZX256,
		512: blockwise.SZX512, 1024: blockwise.SZX1024,
	}
	for size, want := range valid {
		got, ok := blockSZX(size)
		if !ok || got != want {
			t.Errorf("blockSZX(%d) = (%v, %v), want (%v, true)", size, got, ok, want)
		}
	}
	// Anything else must fall back to the library default rather than fail
	// the transfer: a mistyped config should not brick updates.
	for _, size := range []int{0, -1, 15, 100, 500, 2048} {
		if _, ok := blockSZX(size); ok {
			t.Errorf("blockSZX(%d) accepted an unsupported size", size)
		}
	}
}

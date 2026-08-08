package agent

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/plgd-dev/go-coap/v3/net/blockwise"
	"github.com/plgd-dev/go-coap/v3/options"
	"github.com/plgd-dev/go-coap/v3/udp"
	udpClient "github.com/plgd-dev/go-coap/v3/udp/client"
)

// CoAPOptions carries the NB-IoT tuning applied to every CoAP connection the
// agent opens. It is shared by CoAPClient (heartbeat, report) and
// CoAPTransport (delta download) so the two halves of a ClientPair cannot end
// up tuned differently.
//
// A zero value is valid and means "go-coap's defaults", which are tuned for
// ordinary networks: a 3 s dial, a 2 s ACK window and 4 retransmits. On a link
// with multi-second round trips those defaults give up early, which is the
// reason these knobs exist.
type CoAPOptions struct {
	// DialTimeout bounds the socket setup, which for UDP is really name
	// resolution — there is no handshake. 0 keeps go-coap's 3 s default.
	DialTimeout time.Duration
	// ACKTimeout is the window to wait for the ACK of a confirmable message
	// before retransmitting (RFC 7252 §4.8 ACK_TIMEOUT). 0 keeps the default.
	ACKTimeout time.Duration
	// MaxRetransmits caps retransmissions of a confirmable message
	// (RFC 7252 §4.8 MAX_RETRANSMIT, default 4). 0 keeps the default.
	MaxRetransmits int
	// BlockSize is the Block2 payload size in bytes: a power of two in
	// [16, 1024]. Smaller blocks survive lossy links at the cost of more
	// round trips. 0 keeps go-coap's default.
	BlockSize int
	// Keepalive pings an idle connection to keep NAT bindings alive. 0
	// disables. Rarely useful here: the agent opens a connection per
	// operation and closes it immediately.
	Keepalive time.Duration
}

// transmissionNStart is RFC 7252's NSTART: how many confirmable messages may
// be outstanding at once. The RFC default of 1 is what a constrained link
// wants, and go-coap rejects 0, so it is pinned rather than exposed.
const transmissionNStart = 1

// dialOptions renders the tuning into go-coap options. ctx is threaded into
// the dial so a cancelled operation does not leave a dial in flight —
// go-coap's Dial resolves through cfg.Ctx, which otherwise defaults to
// context.Background().
func (o CoAPOptions) dialOptions(ctx context.Context) []udp.Option {
	opts := []udp.Option{options.WithContext(ctx)}

	if o.DialTimeout > 0 {
		opts = append(opts, options.WithDialer(&net.Dialer{Timeout: o.DialTimeout}))
	}
	if o.ACKTimeout > 0 || o.MaxRetransmits > 0 {
		ack := o.ACKTimeout
		if ack <= 0 {
			ack = 2 * time.Second // go-coap's default, kept explicit
		}
		retransmits := o.MaxRetransmits
		if retransmits <= 0 {
			retransmits = 4 // RFC 7252 MAX_RETRANSMIT
		}
		opts = append(opts, options.WithTransmission(
			transmissionNStart, ack, uint32(retransmits)))
	}
	if szx, ok := blockSZX(o.BlockSize); ok {
		// transferTimeout bounds the whole blockwise transfer. Derived from
		// the ACK window rather than exposed as yet another knob: a transfer
		// is many ACK round trips, so a generous multiple is the only sane
		// relationship between the two.
		transfer := 10 * time.Minute
		opts = append(opts, options.WithBlockwise(true, szx, transfer))
	}
	if o.Keepalive > 0 {
		opts = append(opts, options.WithKeepAlive(2, o.Keepalive, func(cc *udpClient.Conn) {
			_ = cc.Close()
		}))
	}
	return opts
}

// blockSZX maps a byte size to the RFC 7959 SZX exponent. Returns false for
// anything that is not a supported power of two, so an invalid config falls
// back to go-coap's default rather than failing the transfer.
func blockSZX(size int) (blockwise.SZX, bool) {
	switch size {
	case 16:
		return blockwise.SZX16, true
	case 32:
		return blockwise.SZX32, true
	case 64:
		return blockwise.SZX64, true
	case 128:
		return blockwise.SZX128, true
	case 256:
		return blockwise.SZX256, true
	case 512:
		return blockwise.SZX512, true
	case 1024:
		return blockwise.SZX1024, true
	}
	return 0, false
}

// dialCoAP opens a tuned CoAP connection to host. Every CoAP dial in the
// agent goes through here — the previous code called udp.Dial(host) bare at
// three separate sites, which silently discarded all of the configuration
// above.
func dialCoAP(ctx context.Context, host string, o CoAPOptions) (*udpClient.Conn, error) {
	co, err := udp.Dial(host, o.dialOptions(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("coap dial %s: %w", host, err)
	}
	return co, nil
}

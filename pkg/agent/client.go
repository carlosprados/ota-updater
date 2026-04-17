package agent

import (
	"context"

	"github.com/amplia/ota-updater/pkg/protocol"
)

// ProtocolClient abstracts heartbeat and report exchanges with the
// update-server. One implementation per wire protocol (HTTP+JSON, CoAP+CBOR).
//
// Implementations must:
//   - honor ctx cancellation,
//   - return non-nil *ManifestResponse only on a successful 2xx/2.05 reply,
//   - never panic; transport errors come back as plain errors.
type ProtocolClient interface {
	// Name returns "http" or "coap"; used in logs and to pair with a
	// matching DeltaTransport.
	Name() string

	// Heartbeat POSTs a Heartbeat and returns the server's ManifestResponse.
	Heartbeat(ctx context.Context, hb *protocol.Heartbeat) (*protocol.ManifestResponse, error)

	// Report POSTs an UpdateReport. The server's body is ignored — a 2xx/2.04
	// status is sufficient confirmation.
	Report(ctx context.Context, rep *protocol.UpdateReport) error

	// DeltaURL turns the transport-relative endpoint advertised by the
	// manifest into a full URL the matching DeltaTransport can fetch.
	// Empty endpoint returns the canonical "/delta/{from}/{to}" path joined
	// with the client's base URL.
	DeltaURL(endpoint string) string
}

// ClientPair groups a ProtocolClient with the DeltaTransport that speaks
// the same wire protocol. The Updater picks one ClientPair per cycle and
// uses both halves: the client for heartbeat/report, the transport for the
// delta download.
//
// Pairs must be transport-consistent: combining an HTTP client with a CoAP
// transport (or vice versa) is a configuration bug. NewClientPair enforces
// the consistency at construction time.
type ClientPair struct {
	Client    ProtocolClient
	Transport DeltaTransport
}

// NewClientPair returns a ClientPair after verifying that client and
// transport speak the same wire protocol (matching Name()).
func NewClientPair(client ProtocolClient, transport DeltaTransport) (ClientPair, error) {
	if client == nil || transport == nil {
		return ClientPair{}, errClientPairNil
	}
	if client.Name() != transport.Name() {
		return ClientPair{}, &mismatchedPairError{client: client.Name(), transport: transport.Name()}
	}
	return ClientPair{Client: client, Transport: transport}, nil
}

type mismatchedPairError struct {
	client    string
	transport string
}

func (e *mismatchedPairError) Error() string {
	return "client/transport mismatch: client=" + e.client + " transport=" + e.transport
}

var errClientPairNil = &mismatchedPairError{client: "<nil>", transport: "<nil>"}

package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/fxamacker/cbor/v2"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"

	"github.com/amplia/ota-updater/internal/protocol"
)

// CoAPConfig bundles dependencies for the CoAP handler set.
type CoAPConfig struct {
	Store      *Store
	Manifester *Manifester
	Logger     *slog.Logger
}

// NewCoAPRouter returns a go-coap mux.Router wired with the OTA resources:
//
//	POST /heartbeat          → ManifestResponse (CBOR)
//	GET  /delta/{from}/{to}  → compressed delta (Block2 auto)
//	POST /report             → update report sink
//
// Wire it into coap.ListenAndServe("udp", addr, router). Structured messages
// are CBOR with integer-keyed fields — see the `cbor:"N,keyasint"` tags on
// internal/protocol/messages.go.
func NewCoAPRouter(cfg CoAPConfig) (*mux.Router, error) {
	c := &coapHandler{
		store:      cfg.Store,
		manifester: cfg.Manifester,
		logger:     cfg.Logger,
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	r := mux.NewRouter()
	if err := r.Handle(protocol.PathHeartbeat, mux.HandlerFunc(c.heartbeat)); err != nil {
		return nil, fmt.Errorf("register heartbeat: %w", err)
	}
	if err := r.Handle(protocol.PathReport, mux.HandlerFunc(c.report)); err != nil {
		return nil, fmt.Errorf("register report: %w", err)
	}
	if err := r.Handle(protocol.PathDelta+"/{from}/{to}", mux.HandlerFunc(c.delta)); err != nil {
		return nil, fmt.Errorf("register delta: %w", err)
	}
	return r, nil
}

type coapHandler struct {
	store      *Store
	manifester *Manifester
	logger     *slog.Logger
}

func (c *coapHandler) heartbeat(w mux.ResponseWriter, r *mux.Message) {
	if r.Code() != codes.POST {
		c.respond(w, codes.MethodNotAllowed, message.TextPlain, nil)
		return
	}
	body, err := r.ReadBody()
	if err != nil {
		c.respond(w, codes.BadRequest, message.TextPlain, readerOf("read body"))
		return
	}
	var hb protocol.Heartbeat
	if err := cbor.Unmarshal(body, &hb); err != nil {
		c.logger.Warn("invalid heartbeat payload", "err", err)
		c.respond(w, codes.BadRequest, message.TextPlain, readerOf("invalid heartbeat"))
		return
	}
	resp, err := c.manifester.Build(r.Context(), &hb)
	if err != nil {
		c.logger.Error("manifest build", "device_id", hb.DeviceID, "err", err)
		c.respond(w, codes.InternalServerError, message.TextPlain, nil)
		return
	}
	buf, err := cbor.Marshal(resp)
	if err != nil {
		c.respond(w, codes.InternalServerError, message.TextPlain, nil)
		return
	}
	c.respond(w, codes.Content, message.AppCBOR, bytes.NewReader(buf))
}

func (c *coapHandler) report(w mux.ResponseWriter, r *mux.Message) {
	if r.Code() != codes.POST {
		c.respond(w, codes.MethodNotAllowed, message.TextPlain, nil)
		return
	}
	body, err := r.ReadBody()
	if err != nil {
		c.respond(w, codes.BadRequest, message.TextPlain, nil)
		return
	}
	var rep protocol.UpdateReport
	if err := cbor.Unmarshal(body, &rep); err != nil {
		c.respond(w, codes.BadRequest, message.TextPlain, nil)
		return
	}
	c.logger.Info("update report",
		"device_id", rep.DeviceID,
		"previous_hash", rep.PreviousHash,
		"new_hash", rep.NewHash,
		"success", rep.Success,
		"rollback_reason", rep.RollbackReason,
	)
	// 2.04 Changed is the CoAP analogue of HTTP 202/204 for a sink POST.
	c.respond(w, codes.Changed, message.TextPlain, nil)
}

func (c *coapHandler) delta(w mux.ResponseWriter, r *mux.Message) {
	if r.Code() != codes.GET {
		c.respond(w, codes.MethodNotAllowed, message.TextPlain, nil)
		return
	}
	if r.RouteParams == nil {
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}
	from := r.RouteParams.Vars["from"]
	to := r.RouteParams.Vars["to"]
	if !isValidHashSegment(from) || !isValidHashSegment(to) {
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}
	path := c.store.DeltaPath(from, to)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if to == c.store.TargetHash() && c.store.HasBinary(from) {
			c.store.StartDeltaGeneration(from)
		}
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}
	if err != nil {
		c.logger.Error("open delta (coap)", "from", from, "to", to, "err", err)
		c.respond(w, codes.InternalServerError, message.TextPlain, nil)
		return
	}
	// go-coap auto-applies Block2 when the body is a sized ReadSeeker; *os.File
	// qualifies. Closing is the library's responsibility once it has read the
	// body; we hand ownership over with SetResponse.
	c.respond(w, codes.Content, message.AppOctets, f)
}

func (c *coapHandler) respond(w mux.ResponseWriter, code codes.Code, mt message.MediaType, body io.ReadSeeker) {
	if err := w.SetResponse(code, mt, body); err != nil {
		c.logger.Error("coap SetResponse", "code", code.String(), "err", err)
	}
}

func readerOf(s string) io.ReadSeeker {
	return bytes.NewReader([]byte(s))
}

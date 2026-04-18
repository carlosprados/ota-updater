package server

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"

	"github.com/amplia/ota-updater/pkg/protocol"
)

// CoAPConfig bundles dependencies for the CoAP handler set.
type CoAPConfig struct {
	Store      *Store
	Manifester *Manifester
	Logger     *slog.Logger
	Metrics    *Metrics // optional; nil disables per-request metric emission
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
		metrics:    cfg.Metrics,
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	r := mux.NewRouter()
	r.Use(recoverCoAP(c.logger))
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
	metrics    *Metrics
}

func (c *coapHandler) heartbeat(w mux.ResponseWriter, r *mux.Message) {
	start := time.Now()
	result := "none"
	defer func() {
		c.metrics.ObserveHeartbeat("coap", result, time.Since(start).Seconds())
	}()

	if r.Code() != codes.POST {
		result = "bad_request"
		c.respond(w, codes.MethodNotAllowed, message.TextPlain, nil)
		return
	}
	body, err := r.ReadBody()
	if err != nil {
		result = "bad_request"
		c.respond(w, codes.BadRequest, message.TextPlain, readerOf("read body"))
		return
	}
	var hb protocol.Heartbeat
	if err := cbor.Unmarshal(body, &hb); err != nil {
		c.logger.Warn("invalid heartbeat payload", "err", err)
		result = "bad_request"
		c.respond(w, codes.BadRequest, message.TextPlain, readerOf("invalid heartbeat"))
		return
	}
	resp, err := c.manifester.Build(r.Context(), &hb)
	if err != nil {
		c.logger.Error("manifest build",
			"op", "heartbeat", "transport", "coap", "device_id", hb.DeviceID, "err", err,
		)
		result = "error"
		c.respond(w, codes.InternalServerError, message.TextPlain, nil)
		return
	}
	buf, err := cbor.Marshal(resp)
	if err != nil {
		result = "error"
		c.respond(w, codes.InternalServerError, message.TextPlain, nil)
		return
	}
	switch {
	case !resp.UpdateAvailable:
		result = "none"
	case resp.RetryAfter > 0:
		result = "retry"
	default:
		result = "update"
	}
	c.logger.Info("heartbeat served",
		"op", "heartbeat", "transport", "coap",
		"device_id", hb.DeviceID,
		"from", hb.VersionHash,
		"to", c.store.TargetHash(),
		"update_available", resp.UpdateAvailable,
		"retry_after", resp.RetryAfter,
	)
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
	start := time.Now()
	hotHit := "miss"
	served := false
	defer func() {
		if served {
			c.metrics.ObserveDeltaServe("coap", hotHit, time.Since(start).Seconds())
		}
	}()

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
	if to != c.store.TargetHash() {
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}

	if _, ok := c.store.PeekHotDelta(from, to); ok {
		hotHit = "hit"
	}

	data, found, err := c.store.GetDeltaBytes(r.Context(), from)
	if err != nil {
		c.logger.Error("fetch delta bytes",
			"op", "delta_get", "transport", "coap", "from", from, "to", to, "err", err)
		c.respond(w, codes.InternalServerError, message.TextPlain, nil)
		return
	}
	if !found {
		c.logger.Info("delta not cached",
			"op", "delta_get", "transport", "coap", "from", from, "to", to,
		)
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}
	served = true
	c.logger.Info("delta served",
		"op", "delta_get", "transport", "coap",
		"from", from, "to", to, "size", len(data),
	)
	// bytes.Reader is an io.ReadSeeker, which go-coap uses to auto-apply
	// Block2. No file descriptor is held here — memory is the cache.
	c.respond(w, codes.Content, message.AppOctets, bytes.NewReader(data))
}

func (c *coapHandler) respond(w mux.ResponseWriter, code codes.Code, mt message.MediaType, body io.ReadSeeker) {
	if err := w.SetResponse(code, mt, body); err != nil {
		c.logger.Error("coap SetResponse", "code", code.String(), "err", err)
	}
}

func readerOf(s string) io.ReadSeeker {
	return bytes.NewReader([]byte(s))
}

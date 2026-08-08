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

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// CoAPConfig bundles dependencies for the CoAP handler set.
type CoAPConfig struct {
	Store      *Store
	Registry   *Registry
	Manifester *Manifester
	Logger     *slog.Logger
	Metrics    *Metrics // optional; nil disables per-request metric emission
}

// NewCoAPRouter returns a go-coap mux.Router wired with the OTA resources:
//
//	POST /heartbeat          → ManifestResponse (CBOR)
//	GET  /delta/{from}/{to}  → compressed delta (Block2 auto)
//	GET  /binary/{hash}      → whole compressed binary (Block2 auto)
//	POST /report             → update report sink
//
// Paths mirror the HTTP handler exactly — both come from pkg/protocol
// constants. Wire it into coap.ListenAndServe("udp", addr, router).
// Structured messages are CBOR with integer-keyed fields — see the
// `cbor:"N,keyasint"` tags on pkg/protocol/messages.go.
func NewCoAPRouter(cfg CoAPConfig) (*mux.Router, error) {
	c := &coapHandler{
		store:      cfg.Store,
		registry:   cfg.Registry,
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
	if err := r.Handle(protocol.PathBinary+"/{hash}", mux.HandlerFunc(c.binary)); err != nil {
		return nil, fmt.Errorf("register binary: %w", err)
	}
	return r, nil
}

type coapHandler struct {
	store      *Store
	registry   *Registry
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
		if IsArtifactNotFound(err) {
			c.logger.Warn("heartbeat for unknown artifact",
				"op", "heartbeat", "transport", "coap", "device_id", hb.DeviceID,
				"artifact", hb.Artifact, "err", err,
			)
			result = "unknown_artifact"
			c.respond(w, codes.NotFound, message.TextPlain, nil)
			return
		}
		c.logger.Error("manifest build",
			"op", "heartbeat", "transport", "coap", "device_id", hb.DeviceID,
			"artifact", hb.Artifact, "err", err,
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
	result = manifestResult(resp)
	c.logger.Info("heartbeat served",
		"op", "heartbeat", "transport", "coap",
		"device_id", hb.DeviceID,
		"artifact", resp.Artifact,
		"from", hb.VersionHash,
		"to", resp.TargetHash,
		"update_available", resp.UpdateAvailable,
		"mode", transferMode(resp),
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
			c.metrics.ObserveDeltaServe("coap", hotHit, "delta", time.Since(start).Seconds())
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
	// Mirrors the HTTP handler: destination must be a live target, which
	// bounds the bsdiff an unauthenticated request can trigger.
	if !c.registry.IsCurrentTarget(to) {
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}

	if _, ok := c.store.PeekHotDelta(from, to); ok {
		hotHit = "hit"
	}

	data, found, err := c.store.GetDeltaBytes(r.Context(), from, to)
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

// binary serves the whole compressed target over CoAP, mirroring the HTTP
// handler. Note the transfer is large by CoAP standards: go-coap chunks it
// with Block2, but the agent has no Block2 resume yet (see README "Known
// limitations"), so a full download over CoAP restarts from block 0 on any
// interruption. Prefer HTTP for the full-download path on flaky links.
func (c *coapHandler) binary(w mux.ResponseWriter, r *mux.Message) {
	start := time.Now()
	hotHit := "miss"
	served := false
	defer func() {
		if served {
			c.metrics.ObserveDeltaServe("coap", hotHit, "full", time.Since(start).Seconds())
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
	hash := r.RouteParams.Vars["hash"]
	if !isValidHashSegment(hash) || !c.registry.IsLive(hash) {
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}
	if _, ok := c.store.PeekHotBinary(hash); ok {
		hotHit = "hit"
	}

	data, found, err := c.store.GetBinaryBytes(r.Context(), hash)
	if err != nil {
		c.logger.Error("fetch binary bytes",
			"op", "binary_get", "transport", "coap", "hash", hash, "err", err)
		c.respond(w, codes.InternalServerError, message.TextPlain, nil)
		return
	}
	if !found {
		c.logger.Warn("binary not in store",
			"op", "binary_get", "transport", "coap", "hash", hash)
		c.respond(w, codes.NotFound, message.TextPlain, nil)
		return
	}
	served = true
	c.logger.Info("binary served",
		"op", "binary_get", "transport", "coap", "hash", hash, "size", len(data),
	)
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

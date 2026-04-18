package server

import (
	"net/http"
	"net/http/pprof"
)

// RegisterPprof wires net/http/pprof handlers onto the given mux under
// /debug/pprof/*. Intended for the separate observability listener — DO NOT
// call this on the public API mux; pprof exposes source paths, goroutine
// stacks and heap dumps and has no authentication.
//
// Callers are expected to bind the mux to a loopback-only or private-net
// address (cfg.Metrics.Addr). The helper itself is transport-agnostic.
func RegisterPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

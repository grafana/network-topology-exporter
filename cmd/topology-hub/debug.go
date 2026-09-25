package main

import (
	"net/http"
	"net/http/pprof"
)

// newDebugMux builds the dedicated ServeMux that serves net/http/pprof at
// /debug/pprof/* (issue #69), identical to internal/app.newDebugMux;
// duplicated here rather than imported so this binary does not depend on
// internal/app. Registered explicitly rather than relying on
// net/http/pprof's init() side-effect, so the debug surface stays strictly
// on the opt-in debug listener.
func newDebugMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

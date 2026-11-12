package httpserver

import (
	"net/http"
	"net/http/pprof"
)

// AdminHandler builds the operations endpoint set served on the admin port: /healthz, /readyz,
// /metrics and, when enabled, /debug/pprof. It must never be exposed publicly.
func AdminHandler(liveness, readiness, metrics http.Handler, enablePprof bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", liveness)
	mux.Handle("GET /readyz", readiness)
	mux.Handle("GET /metrics", metrics)
	if enablePprof {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	return mux
}

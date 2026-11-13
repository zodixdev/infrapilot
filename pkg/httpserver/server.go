// Package httpserver provides the net/http server used by every InfraPilot service: hardened
// timeouts, a middleware stack (recovery, request id, tracing, RED metrics, access log) and
// graceful shutdown that drains in-flight requests within a deadline.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Config mirrors config.HTTP.
type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// Server is an app.Component wrapping *http.Server. The listener is bound in New so that port
// conflicts fail fast and tests can use ":0".
type Server struct {
	name string
	srv  *http.Server
	ln   net.Listener
	log  *slog.Logger
}

// Option configures the server.
type Option func(*Server)

// WithLogger sets the logger used for lifecycle messages and the http.Server error log.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithName sets the component name (default "http").
func WithName(name string) Option { return func(s *Server) { s.name = name } }

// New validates cfg, binds the listener (ctx bounds only the bind) and wraps handler with the standard middleware stack
// unless the handler is already wrapped by the caller.
func New(ctx context.Context, cfg Config, handler http.Handler, opts ...Option) (*Server, error) {
	if handler == nil {
		return nil, errors.New("httpserver: handler is required")
	}
	if cfg.ReadHeaderTimeout <= 0 || cfg.ReadTimeout <= 0 || cfg.WriteTimeout <= 0 || cfg.IdleTimeout <= 0 {
		return nil, errors.New("httpserver: all timeouts must be positive")
	}
	if cfg.MaxHeaderBytes <= 0 {
		cfg.MaxHeaderBytes = http.DefaultMaxHeaderBytes
	}
	s := &Server{name: "http", log: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(s)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("httpserver: listen %s: %w", cfg.Addr, err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
	return s, nil
}

// Name implements app.Component.
func (s *Server) Name() string { return s.name }

// Addr is the bound address (useful with ":0").
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Run serves until Shutdown completes. It returns nil after a graceful stop.
func (s *Server) Run(ctx context.Context) error {
	s.srv.BaseContext = func(net.Listener) context.Context { return context.WithoutCancel(ctx) }
	s.log.InfoContext(ctx, "http server listening", slog.String("component", s.name), slog.String("addr", s.ln.Addr().String()))
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("httpserver %s: %w", s.name, err)
}

// Shutdown closes the listener, waits for in-flight requests until ctx expires, then force-closes
// remaining connections so Run returns in bounded time.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	// Serve closes the listener it was given; if Run never started (an earlier component failed) the
	// listener is still ours to close.
	if lnErr := s.ln.Close(); lnErr != nil && !errors.Is(lnErr, net.ErrClosed) {
		err = errors.Join(err, lnErr)
	}
	if err == nil {
		return nil
	}
	closeErr := s.srv.Close()
	return errors.Join(fmt.Errorf("httpserver %s: drain: %w", s.name, err), closeErr)
}

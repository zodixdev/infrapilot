// Package grpcserver provides the internal gRPC server used by InfraPilot services: keepalive and
// size limits, interceptors (recovery, default deadline, RED metrics, access log), OpenTelemetry
// tracing, the standard health service and graceful, deadline-bounded shutdown (ADR-0003, ADR-0016).
package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

// Config mirrors config.GRPC.
type Config struct {
	Addr                 string
	MaxRecvMsgBytes      int
	ConnectionTimeout    time.Duration
	KeepaliveTime        time.Duration
	KeepaliveTimeout     time.Duration
	MaxConnectionAge     time.Duration
	EnableReflection     bool
	MaxConcurrentStreams uint32
	// DefaultTimeout is applied to unary calls that arrive without a deadline (AGENTS.md §4: every call
	// has a deadline). Streams are exempt (agent streams are long-lived by design).
	DefaultTimeout time.Duration
	// MaxConnectionAgeGrace lets in-flight RPCs finish after MaxConnectionAge (0 → KeepaliveTimeout).
	MaxConnectionAgeGrace time.Duration
	// TLS: server certificate and key (PEM files, re-read on every handshake so cert-manager rotation needs no
	// restart). ClientCAFile additionally requires and verifies client certificates (mTLS). Empty = plaintext.
	TLSCertFile, TLSKeyFile, TLSClientCAFile string
}

// Server is an app.Component wrapping *grpc.Server.
type Server struct {
	name   string
	srv    *grpc.Server
	ln     net.Listener
	log    *slog.Logger
	health *health.Server
	tp     trace.TracerProvider
	mp     metric.MeterProvider
	unary  []grpc.UnaryServerInterceptor
	stream []grpc.StreamServerInterceptor
}

// Option configures the server.
type Option func(*Server)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithName sets the component name (default "grpc").
func WithName(name string) Option { return func(s *Server) { s.name = name } }

// WithTracerProvider enables tracing via the otelgrpc stats handler.
func WithTracerProvider(tp trace.TracerProvider) Option { return func(s *Server) { s.tp = tp } }

// WithMeterProvider enables RED metrics.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(s *Server) { s.mp = mp } }

// WithUnaryInterceptors appends service-specific interceptors (auth, tenancy) after the standard ones.
func WithUnaryInterceptors(i ...grpc.UnaryServerInterceptor) Option {
	return func(s *Server) { s.unary = append(s.unary, i...) }
}

// WithStreamInterceptors appends service-specific stream interceptors after the standard ones.
func WithStreamInterceptors(i ...grpc.StreamServerInterceptor) Option {
	return func(s *Server) { s.stream = append(s.stream, i...) }
}

// New validates cfg, binds the listener and builds the server with the standard interceptor chain.
// Services register their implementations on Registrar() before Run.
func New(ctx context.Context, cfg Config, opts ...Option) (*Server, error) {
	if cfg.MaxRecvMsgBytes <= 0 {
		return nil, errors.New("grpcserver: MaxRecvMsgBytes must be positive")
	}
	if cfg.MaxConcurrentStreams == 0 {
		return nil, errors.New("grpcserver: MaxConcurrentStreams must be positive (0 means unlimited in grpc-go)")
	}
	if cfg.MaxConnectionAgeGrace <= 0 {
		cfg.MaxConnectionAgeGrace = cfg.KeepaliveTimeout
	}
	creds, err := transportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.ConnectionTimeout <= 0 || cfg.KeepaliveTime <= 0 || cfg.KeepaliveTimeout <= 0 {
		return nil, errors.New("grpcserver: timeouts must be positive")
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}
	s := &Server{
		name:   "grpc",
		log:    slog.New(slog.DiscardHandler),
		tp:     tracenoop.NewTracerProvider(),
		mp:     metricnoop.NewMeterProvider(),
		health: health.NewServer(),
	}
	for _, o := range opts {
		o(s)
	}
	std, stdStream, err := standardInterceptors(s.log, s.mp, cfg.DefaultTimeout) //nolint:contextcheck // stream interceptors take ctx from ss.Context()
	if err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpcserver: listen %s: %w", cfg.Addr, err)
	}
	s.ln = ln
	s.srv = grpc.NewServer(
		grpc.MaxRecvMsgSize(cfg.MaxRecvMsgBytes),
		grpc.ConnectionTimeout(cfg.ConnectionTimeout),
		grpc.MaxConcurrentStreams(cfg.MaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:             cfg.KeepaliveTime,
			Timeout:          cfg.KeepaliveTimeout,
			MaxConnectionAge: cfg.MaxConnectionAge,
			// Grace keeps in-flight RPCs alive after the age limit while new streams are refused.
			MaxConnectionAgeGrace: cfg.MaxConnectionAgeGrace,
		}),
		// Clients must not ping more often than every KeepaliveTime/2 (15 s by default) or they get GOAWAY
		// too_many_pings; client keepalive settings in later milestones must respect this.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             cfg.KeepaliveTime / 2,
			PermitWithoutStream: true,
		}),
		grpc.Creds(creds),
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(s.tp),
			otelgrpc.WithMeterProvider(metricnoop.NewMeterProvider()),
		)),
		grpc.ChainUnaryInterceptor(append(std, s.unary...)...),
		grpc.ChainStreamInterceptor(append(stdStream, s.stream...)...),
	)
	healthpb.RegisterHealthServer(s.srv, s.health)
	if cfg.EnableReflection {
		reflection.Register(s.srv)
	}
	// Overall status starts NOT_SERVING; the service flips it to SERVING once ready (SetServing).
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	return s, nil
}

// Name implements app.Component.
func (s *Server) Name() string { return s.name }

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Registrar is where services register their implementations (before Run).
func (s *Server) Registrar() grpc.ServiceRegistrar { return s.srv }

// SetServing sets the overall grpc.health.v1 status. Shutdown forces NOT_SERVING.
func (s *Server) SetServing(serving bool) {
	st := healthpb.HealthCheckResponse_NOT_SERVING
	if serving {
		st = healthpb.HealthCheckResponse_SERVING
	}
	s.health.SetServingStatus("", st)
}

// Run serves until Shutdown completes. It returns nil after a graceful stop.
func (s *Server) Run(ctx context.Context) error {
	s.log.InfoContext(ctx, "grpc server listening", slog.String("component", s.name), slog.String("addr", s.ln.Addr().String()))
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("grpcserver %s: %w", s.name, err)
	}
	return nil
}

// Shutdown marks the server NOT_SERVING, stops accepting new RPCs, waits for in-flight RPCs until
// ctx expires, then force-stops remaining connections so Run returns in bounded time.
func (s *Server) Shutdown(ctx context.Context) error {
	s.health.Shutdown()
	// Close our listener in case Run never started; Serve owns it otherwise (double close is harmless).
	defer func() { _ = s.ln.Close() }()
	done := make(chan struct{})
	go func() {
		s.srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.srv.Stop()
		<-done
		return fmt.Errorf("grpcserver %s: drain: %w", s.name, ctx.Err())
	}
}

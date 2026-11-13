// Package grpcclient builds connections to internal InfraPilot gRPC services (ADR-0003, ADR-0025): mTLS with
// in-place certificate rotation and optional server identity pinning, round-robin balancing over DNS, a default
// deadline for calls without one, client RED metrics, OpenTelemetry tracing, and transparent retries with
// exponential backoff and jitter for RPCs the caller declares idempotent — and only for those.
package grpcclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/zodixdev/infrapilot/pkg/config"
	"github.com/zodixdev/infrapilot/pkg/internal/tlsreload"
	"github.com/zodixdev/infrapilot/pkg/internal/workloadid"
)

// MaxAttempts bounds transparent retries of idempotent RPCs (first attempt included).
const MaxAttempts = 3

type options struct {
	log        *slog.Logger
	tp         trace.TracerProvider
	mp         metric.MeterProvider
	idempotent []string
	upstream   string
}

// Option configures a client connection.
type Option func(*options)

// WithLogger sets the logger used for TLS reload failures.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithTracerProvider enables client spans and trace-context propagation.
func WithTracerProvider(tp trace.TracerProvider) Option { return func(o *options) { o.tp = tp } }

// WithMeterProvider enables client RED metrics.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.mp = mp } }

// WithIdempotent declares full method names ("/pkg.Service/Method") or services ("/pkg.Service/*") that are
// safe to retry on UNAVAILABLE. Mutations that are not idempotent must not be listed (AGENTS.md §4).
func WithIdempotent(methods ...string) Option {
	return func(o *options) { o.idempotent = append(o.idempotent, methods...) }
}

// WithUpstream names the upstream service in metrics (default: the target).
func WithUpstream(name string) Option { return func(o *options) { o.upstream = name } }

// New validates cfg for the environment (ADR-0025 rails) and creates a lazily connecting client connection.
// Close it on shutdown.
func New(cfg config.GRPCClient, env config.Environment, opts ...Option) (*grpc.ClientConn, error) {
	o := options{log: slog.New(slog.DiscardHandler), tp: tracenoop.NewTracerProvider(), mp: metricnoop.NewMeterProvider(), upstream: cfg.Target}
	for _, opt := range opts {
		opt(&o)
	}
	if err := cfg.ValidateFor(env); err != nil {
		return nil, fmt.Errorf("grpcclient: %w", err)
	}
	creds, err := transportCredentials(&cfg, &o)
	if err != nil {
		return nil, err
	}
	sc, err := serviceConfig(o.idempotent)
	if err != nil {
		return nil, err
	}
	unary, stream, err := interceptors(o.mp, o.upstream, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.Target,
		grpc.WithTransportCredentials(creds),
		// The service config (balancing, which RPCs may be retried) is ours alone: never accept one from DNS
		// TXT records, which could enable retries of non-idempotent methods.
		grpc.WithDisableServiceConfig(),
		grpc.WithDefaultServiceConfig(sc),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(o.tp),
			otelgrpc.WithMeterProvider(metricnoop.NewMeterProvider()),
		)),
		grpc.WithChainUnaryInterceptor(unary),
		grpc.WithChainStreamInterceptor(stream),
		// Must respect the server's enforcement policy (MinTime = server KeepaliveTime/2 = 15 s by default).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: %s: %w", cfg.Target, err)
	}
	return conn, nil
}

func transportCredentials(cfg *config.GRPCClient, o *options) (credentials.TransportCredentials, error) {
	if cfg.Insecure {
		return insecure.NewCredentials(), nil
	}
	serverName := cfg.ServerName
	if serverName == "" {
		serverName = targetHost(cfg.Target)
	}
	if serverName == "" {
		// x509 verification with an empty name skips the host check; crypto/tls refuses it, and so do we.
		return nil, fmt.Errorf("grpcclient: cannot derive a server name from target %q; set ServerName", cfg.Target)
	}
	m, err := tlsreload.Load(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSCAFile, tlsreload.OnError(func(err error) {
		o.log.Error("grpc client TLS material reload failed; keeping the previous material",
			slog.String("upstream", o.upstream), slog.Any("error", err))
	}))
	if err != nil {
		return nil, fmt.Errorf("grpcclient: %w", err)
	}
	if err := registerExpiry(o.mp, m, o.upstream); err != nil {
		return nil, err
	}
	tc := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		// Verification against the *current* CA bundle happens in VerifyConnection so CA rotation needs no
		// restart; the standard verifier would pin the bundle loaded at startup. VerifyConnection runs on every
		// handshake, also when InsecureSkipVerify is set, and the handshake still checks the server's proof of
		// possession of the key. No ClientSessionCache is set, so there is no resumption.
		InsecureSkipVerify: true, //nolint:gosec // replaced by verifyServer below, which performs full chain + name checks
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, pool := m.Current()
			return verifyServer(cs, pool, serverName, cfg.ServerIdentity)
		},
	}
	if cfg.TLSCertFile != "" {
		tc.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, _ := m.Current()
			return cert, nil
		}
	}
	return credentials.NewTLS(tc), nil
}

func registerExpiry(mp metric.MeterProvider, m *tlsreload.Material, upstream string) error {
	_, err := mp.Meter("github.com/zodixdev/infrapilot/pkg/grpcclient").Float64ObservableGauge(
		"tls_certificate_expiry_timestamp_seconds",
		metric.WithDescription("Expiry (Unix seconds) of the TLS certificate currently in use"), metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			if na := m.NotAfter(); !na.IsZero() {
				obs.Observe(float64(na.Unix()), metric.WithAttributes(attribute.String("role", "client"), attribute.String("name", upstream)))
			}
			return nil
		}))
	if err != nil {
		return fmt.Errorf("grpcclient: create expiry gauge: %w", err)
	}
	return nil
}

// verifyServer performs the checks crypto/tls would do (chain to roots, server auth usage, host name) and, when
// configured, pins the server's workload identity.
func verifyServer(cs tls.ConnectionState, roots *x509.CertPool, serverName, identity string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("grpcclient: server presented no certificate")
	}
	if roots == nil || serverName == "" {
		// x509 would fall back to the system roots / skip the name check; never verify that way.
		return errors.New("grpcclient: no CA bundle or server name to verify against")
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, DNSName: serverName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("grpcclient: verify server certificate: %w", err)
	}
	if identity == "" {
		return nil
	}
	if got, err := workloadid.FromCertificate(leaf); err != nil || got != identity {
		return fmt.Errorf("grpcclient: server identity is not %s", identity)
	}
	return nil
}

// targetHost derives the TLS server name from a gRPC target: "dns:///host:port", "dns://resolver/host:port",
// "host:port" or "host". Other schemes (unix:, passthrough with odd endpoints) yield "" and require ServerName.
func targetHost(target string) string {
	endpoint := target
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "dns" && u.Scheme != "passthrough") {
			return ""
		}
		endpoint = strings.TrimPrefix(u.Path, "/")
	} else if strings.HasPrefix(target, "unix:") {
		return ""
	}
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		return h
	}
	return endpoint
}

// serviceConfig enables round-robin balancing and a retry policy for the idempotent methods only.
func serviceConfig(idempotent []string) (string, error) {
	type name struct {
		Service string `json:"service"`
		Method  string `json:"method,omitempty"`
	}
	type retryPolicy struct {
		MaxAttempts          int      `json:"maxAttempts"`
		InitialBackoff       string   `json:"initialBackoff"`
		MaxBackoff           string   `json:"maxBackoff"`
		BackoffMultiplier    float64  `json:"backoffMultiplier"`
		RetryableStatusCodes []string `json:"retryableStatusCodes"`
	}
	type methodConfig struct {
		Name        []name       `json:"name"`
		RetryPolicy *retryPolicy `json:"retryPolicy,omitempty"`
	}
	type retryThrottling struct {
		MaxTokens  int     `json:"maxTokens"`
		TokenRatio float64 `json:"tokenRatio"`
	}
	sc := struct {
		LoadBalancingConfig []map[string]any `json:"loadBalancingConfig"`
		MethodConfig        []methodConfig   `json:"methodConfig,omitempty"`
		// Retry budget: retries stop while more than half of recent calls failed, so an outage is not amplified.
		RetryThrottling *retryThrottling `json:"retryThrottling,omitempty"`
	}{LoadBalancingConfig: []map[string]any{{"round_robin": map[string]any{}}}}
	if len(idempotent) > 0 {
		mc := methodConfig{RetryPolicy: &retryPolicy{
			// grpc-go randomizes each backoff in [0, current backoff) — full jitter.
			MaxAttempts: MaxAttempts, InitialBackoff: "0.05s", MaxBackoff: "1s", BackoffMultiplier: 2,
			RetryableStatusCodes: []string{"UNAVAILABLE"},
		}}
		for _, m := range idempotent {
			parts := strings.Split(strings.TrimPrefix(m, "/"), "/")
			if !strings.HasPrefix(m, "/") || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return "", fmt.Errorf("grpcclient: idempotent method %q must be /pkg.Service/Method or /pkg.Service/*", m)
			}
			n := name{Service: parts[0]}
			if parts[1] != "*" {
				n.Method = parts[1]
			}
			mc.Name = append(mc.Name, n)
		}
		sc.MethodConfig = []methodConfig{mc}
		sc.RetryThrottling = &retryThrottling{MaxTokens: 10, TokenRatio: 0.1}
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return "", fmt.Errorf("grpcclient: service config: %w", err)
	}
	return string(b), nil
}

// interceptors record client RED metrics. Unary calls without a deadline get the default timeout; streams keep
// the caller's context (long-lived streams are bounded by their owner, not by a per-call default).
func interceptors(mp metric.MeterProvider, upstream string, timeout time.Duration) (grpc.UnaryClientInterceptor, grpc.StreamClientInterceptor, error) {
	m := mp.Meter("github.com/zodixdev/infrapilot/pkg/grpcclient")
	handled, err := m.Int64Counter("grpc_client_handled_total", metric.WithDescription("Client RPCs completed, by code"))
	if err != nil {
		return nil, nil, fmt.Errorf("grpcclient: create counter: %w", err)
	}
	dur, err := m.Float64Histogram("grpc_client_handling_seconds",
		metric.WithDescription("Client RPC latency including retries"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	if err != nil {
		return nil, nil, fmt.Errorf("grpcclient: create histogram: %w", err)
	}
	observe := func(ctx context.Context, method string, start time.Time, err error) {
		svc, meth := splitMethod(method)
		attrs := metric.WithAttributes(
			attribute.String("grpc_upstream", upstream),
			attribute.String("grpc_service", svc),
			attribute.String("grpc_method", meth),
			attribute.String("grpc_code", status.Code(err).String()),
		)
		handled.Add(ctx, 1, attrs)
		dur.Record(ctx, time.Since(start).Seconds(), attrs)
	}
	unary := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		observe(ctx, method, start, err)
		return err
	}
	stream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		start := time.Now()
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			observe(ctx, method, start, err)
			return nil, err
		}
		return &observedStream{ClientStream: cs, done: func(err error) { observe(context.WithoutCancel(ctx), method, start, err) }}, nil
	}
	return unary, stream, nil
}

// observedStream records the stream's outcome once, when RecvMsg returns the final status.
type observedStream struct {
	grpc.ClientStream
	once sync.Once
	done func(error)
}

func (s *observedStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		final := err
		if errors.Is(err, io.EOF) {
			final = nil
		}
		s.once.Do(func() { s.done(final) })
	}
	return err
}

func splitMethod(full string) (service, method string) {
	full = strings.TrimPrefix(full, "/")
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "unknown", full
}

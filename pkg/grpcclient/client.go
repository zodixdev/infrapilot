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
	"net"
	"strings"
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
)

// MaxAttempts bounds transparent retries of idempotent RPCs (first attempt included).
const MaxAttempts = 3

type options struct {
	tp         trace.TracerProvider
	mp         metric.MeterProvider
	idempotent []string
	upstream   string
}

// Option configures a client connection.
type Option func(*options)

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

// New creates a lazily connecting client connection. Close it on shutdown.
func New(cfg config.GRPCClient, opts ...Option) (*grpc.ClientConn, error) {
	o := options{tp: tracenoop.NewTracerProvider(), mp: metricnoop.NewMeterProvider(), upstream: cfg.Target}
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("grpcclient: Timeout must be positive")
	}
	creds, err := transportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	sc, err := serviceConfig(o.idempotent)
	if err != nil {
		return nil, err
	}
	interceptor, err := unaryInterceptor(o.mp, o.upstream, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.Target,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultServiceConfig(sc),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(o.tp),
			otelgrpc.WithMeterProvider(metricnoop.NewMeterProvider()),
		)),
		grpc.WithChainUnaryInterceptor(interceptor),
		// Must respect the server's enforcement policy (MinTime = server KeepaliveTime/2 = 15 s by default).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: %s: %w", cfg.Target, err)
	}
	return conn, nil
}

func transportCredentials(cfg config.GRPCClient) (credentials.TransportCredentials, error) {
	if cfg.Insecure {
		return insecure.NewCredentials(), nil
	}
	if cfg.TLSCAFile == "" {
		return nil, errors.New("grpcclient: TLSCAFile is required unless Insecure")
	}
	m, err := tlsreload.Load(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: %w", err)
	}
	serverName := cfg.ServerName
	if serverName == "" {
		serverName = targetHost(cfg.Target)
	}
	tc := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		// Verification against the *current* CA bundle happens in VerifyConnection so CA rotation needs no
		// restart; the standard verifier would pin the bundle loaded at startup. VerifyConnection runs on every
		// handshake, also when InsecureSkipVerify is set.
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

// verifyServer performs the checks crypto/tls would do (chain to roots, server auth usage, host name) and, when
// configured, pins the server's workload identity.
func verifyServer(cs tls.ConnectionState, roots *x509.CertPool, serverName, identity string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("grpcclient: server presented no certificate")
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
	for _, u := range leaf.URIs {
		if u.String() == identity {
			return nil
		}
	}
	return fmt.Errorf("grpcclient: server identity is not %s", identity)
}

// targetHost extracts the host from "dns:///host:port", "host:port" or "host".
func targetHost(target string) string {
	if i := strings.Index(target, ":///"); i >= 0 {
		target = target[i+4:]
	}
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
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
	sc := struct {
		LoadBalancingConfig []map[string]any `json:"loadBalancingConfig"`
		MethodConfig        []methodConfig   `json:"methodConfig,omitempty"`
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
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return "", fmt.Errorf("grpcclient: service config: %w", err)
	}
	return string(b), nil
}

func unaryInterceptor(mp metric.MeterProvider, upstream string, timeout time.Duration) (grpc.UnaryClientInterceptor, error) {
	m := mp.Meter("github.com/zodixdev/infrapilot/pkg/grpcclient")
	handled, err := m.Int64Counter("grpc_client_handled_total", metric.WithDescription("Client RPCs completed, by code"))
	if err != nil {
		return nil, fmt.Errorf("grpcclient: create counter: %w", err)
	}
	dur, err := m.Float64Histogram("grpc_client_handling_seconds",
		metric.WithDescription("Client RPC latency including retries"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	if err != nil {
		return nil, fmt.Errorf("grpcclient: create histogram: %w", err)
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		svc, meth := splitMethod(method)
		attrs := metric.WithAttributes(
			attribute.String("grpc_upstream", upstream),
			attribute.String("grpc_service", svc),
			attribute.String("grpc_method", meth),
			attribute.String("grpc_code", status.Code(err).String()),
		)
		handled.Add(ctx, 1, attrs)
		dur.Record(ctx, time.Since(start).Seconds(), attrs)
		return err
	}, nil
}

func splitMethod(full string) (service, method string) {
	full = strings.TrimPrefix(full, "/")
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "unknown", full
}

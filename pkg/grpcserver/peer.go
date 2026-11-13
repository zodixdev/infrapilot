package grpcserver

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/zodixdev/infrapilot/pkg/internal/workloadid"
)

// PeerPolicy is the per-RPC allow-list of calling workload identities (ADR-0025). Identities are the single
// spiffe:// URI SAN of a verified client certificate, compared as canonical strings.
type PeerPolicy struct {
	// Rules maps a full method ("/pkg.Service/Method") or a whole service ("/pkg.Service/*") to the identities
	// allowed to call it. A method rule takes precedence over its service rule. Methods without a rule are
	// denied, except the gRPC health and reflection services (reflection is forbidden in staging/prod by config).
	Rules map[string][]string
	// InsecureIdentity is the identity assumed for plaintext connections. Only for local development and tests
	// (config rails forbid plaintext gRPC in staging and prod); empty means plaintext callers have no identity.
	InsecureIdentity string
}

// WithPeerPolicy enforces p on every unary and stream RPC. Without this option the server does not authorize
// peers (the conformance workload); every InfraPilot service sets it. The rules are copied.
func WithPeerPolicy(p PeerPolicy) Option {
	rules := make(map[string][]string, len(p.Rules))
	for k, v := range maps.All(p.Rules) {
		rules[k] = append([]string(nil), v...)
	}
	cp := &peerPolicy{rules: rules, insecureIdentity: p.InsecureIdentity}
	return func(s *Server) { s.peers = cp }
}

type peerIdentityKey struct{}

// PeerIdentity returns the verified workload identity of the caller, set by the peer-policy interceptor.
func PeerIdentity(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(peerIdentityKey{}).(string)
	return id, ok && id != ""
}

type peerPolicy struct {
	rules            map[string][]string
	insecureIdentity string

	denied  metric.Int64Counter
	logMu   sync.Mutex
	lastLog map[string]time.Time
}

func (p *peerPolicy) validate() error {
	for k, ids := range p.rules {
		parts := strings.Split(strings.TrimPrefix(k, "/"), "/")
		if !strings.HasPrefix(k, "/") || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("grpcserver: peer rule %q must be /pkg.Service/Method or /pkg.Service/*", k)
		}
		if len(ids) == 0 {
			return fmt.Errorf("grpcserver: peer rule %q allows nobody; remove it instead", k)
		}
		for _, id := range ids {
			if err := workloadid.Validate(id); err != nil {
				return fmt.Errorf("grpcserver: peer rule %q: %w", k, err)
			}
		}
	}
	if p.insecureIdentity != "" {
		if err := workloadid.Validate(p.insecureIdentity); err != nil {
			return fmt.Errorf("grpcserver: insecure identity: %w", err)
		}
	}
	return nil
}

// exempt RPCs are infrastructure endpoints every caller (kubelet probes, grpcurl in dev) may use.
func exempt(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

func (p *peerPolicy) allowed(fullMethod, identity string) bool {
	if identity == "" {
		return false
	}
	ids, ok := p.rules[fullMethod]
	if !ok {
		i := strings.LastIndex(fullMethod, "/")
		if i <= 0 {
			return false
		}
		ids, ok = p.rules[fullMethod[:i]+"/*"]
	}
	return ok && containsString(ids, identity)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// identityOf returns the identity of the verified client certificate; plaintext connections get the
// configured insecure identity. Certificates without exactly one valid spiffe URI SAN have no identity.
func (p *peerPolicy) identityOf(ctx context.Context) string {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr.AuthInfo == nil {
		return p.insecureIdentity
	}
	tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return p.insecureIdentity
	}
	// VerifiedChains is populated only when the server verified the client certificate (mTLS).
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return ""
	}
	id, err := workloadid.FromCertificate(tlsInfo.State.VerifiedChains[0][0])
	if err != nil {
		return ""
	}
	return id
}

func (p *peerPolicy) check(ctx context.Context, logger *slog.Logger, fullMethod string) (context.Context, error) {
	if exempt(fullMethod) {
		return ctx, nil
	}
	id := p.identityOf(ctx)
	if !p.allowed(fullMethod, id) {
		p.denied.Add(ctx, 1, metric.WithAttributes(attribute.String("grpc_method", fullMethod)))
		if p.shouldLog(fullMethod) {
			logger.WarnContext(ctx, "grpc peer not allowed (logged at most once per second per method)",
				slog.String("grpc_method", fullMethod), slog.String("peer_identity", id))
		}
		return ctx, status.Error(codes.PermissionDenied, "caller is not allowed to call this method")
	}
	return context.WithValue(ctx, peerIdentityKey{}, id), nil
}

// shouldLog rate-limits denial logs so one misconfigured caller cannot flood them; the counter keeps the total.
func (p *peerPolicy) shouldLog(method string) bool {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	now := time.Now()
	if now.Sub(p.lastLog[method]) < time.Second {
		return false
	}
	if len(p.lastLog) > 1024 { // bounded: methods are a finite set, but never grow without limit
		clear(p.lastLog)
	}
	p.lastLog[method] = now
	return true
}

func (p *peerPolicy) interceptors(logger *slog.Logger, mp metric.MeterProvider) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	denied, err := mp.Meter("github.com/zodixdev/infrapilot/pkg/grpcserver").Int64Counter("grpc_server_peer_denied_total",
		metric.WithDescription("RPCs rejected by the peer allow-list"))
	if err != nil {
		return nil, nil, fmt.Errorf("grpcserver: create counter: %w", err)
	}
	p.denied, p.lastLog = denied, map[string]time.Time{}
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := p.check(ctx, logger, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := p.check(ss.Context(), logger, info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &identityStream{ServerStream: ss, ctx: ctx})
	}
	return unary, stream, nil
}

type identityStream struct {
	grpc.ServerStream
	ctx context.Context //nolint:containedctx // carries the verified identity to the stream handler
}

func (s *identityStream) Context() context.Context { return s.ctx }

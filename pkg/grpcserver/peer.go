package grpcserver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// PeerPolicy is the per-RPC allow-list of calling workload identities (ADR-0025). Identities are the
// spiffe:// URI SANs of verified client certificates.
type PeerPolicy struct {
	// Rules maps a full method ("/pkg.Service/Method") or a whole service ("/pkg.Service/*") to the identities
	// allowed to call it. A method rule takes precedence over its service rule. Methods without a rule are
	// denied, except the gRPC health and reflection services.
	Rules map[string][]string
	// InsecureIdentity is the identity assumed for plaintext connections. Only for local development and tests
	// (config rails forbid plaintext gRPC in staging and prod); empty means plaintext callers have no identity.
	InsecureIdentity string
}

// WithPeerPolicy enforces p on every unary and stream RPC. Without this option the server does not authorize
// peers (the conformance workload); every InfraPilot service sets it.
func WithPeerPolicy(p PeerPolicy) Option { return func(s *Server) { s.peers = &p } }

type peerIdentityKey struct{}

// PeerIdentity returns the verified workload identity of the caller, set by the peer-policy interceptor.
func PeerIdentity(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(peerIdentityKey{}).(string)
	return id, ok && id != ""
}

func (p *PeerPolicy) validate() error {
	for k, ids := range p.Rules {
		if !strings.HasPrefix(k, "/") || strings.Count(k, "/") != 2 {
			return fmt.Errorf("grpcserver: peer rule %q must be /pkg.Service/Method or /pkg.Service/*", k)
		}
		for _, id := range ids {
			if !strings.HasPrefix(id, "spiffe://") {
				return fmt.Errorf("grpcserver: peer rule %q: identity %q must be a spiffe:// URI", k, id)
			}
		}
	}
	if p.InsecureIdentity != "" && !strings.HasPrefix(p.InsecureIdentity, "spiffe://") {
		return fmt.Errorf("grpcserver: insecure identity %q must be a spiffe:// URI", p.InsecureIdentity)
	}
	return nil
}

// exempt RPCs are infrastructure endpoints every caller (kubelet probes, grpcurl in dev) may use.
func exempt(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

func (p *PeerPolicy) allowed(fullMethod, identity string) bool {
	ids, ok := p.Rules[fullMethod]
	if !ok {
		svc := fullMethod[:strings.LastIndex(fullMethod, "/")]
		ids, ok = p.Rules[svc+"/*"]
	}
	if !ok || identity == "" {
		return false
	}
	for _, id := range ids {
		if id == identity {
			return true
		}
	}
	return false
}

// identityOf extracts the first spiffe:// URI SAN of the verified client certificate. Plaintext connections
// get the configured insecure identity.
func (p *PeerPolicy) identityOf(ctx context.Context) string {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr.AuthInfo == nil {
		return p.InsecureIdentity
	}
	tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return p.InsecureIdentity
	}
	// VerifiedChains is populated only when the server verified the client certificate (mTLS).
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return ""
	}
	for _, u := range tlsInfo.State.VerifiedChains[0][0].URIs {
		if u.Scheme == "spiffe" {
			return u.String()
		}
	}
	return ""
}

func (p *PeerPolicy) check(ctx context.Context, logger *slog.Logger, fullMethod string) (context.Context, error) {
	if exempt(fullMethod) {
		return ctx, nil
	}
	id := p.identityOf(ctx)
	if !p.allowed(fullMethod, id) {
		logger.WarnContext(ctx, "grpc peer not allowed", slog.String("grpc_method", fullMethod), slog.String("peer_identity", id))
		return ctx, status.Error(codes.PermissionDenied, "caller is not allowed to call this method")
	}
	return context.WithValue(ctx, peerIdentityKey{}, id), nil
}

func (p *PeerPolicy) interceptors(logger *slog.Logger) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
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
	return unary, stream
}

type identityStream struct {
	grpc.ServerStream
	ctx context.Context //nolint:containedctx // carries the verified identity to the stream handler
}

func (s *identityStream) Context() context.Context { return s.ctx }

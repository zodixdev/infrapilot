package grpc

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/pkg/grpcmeta"
	"github.com/zodixdev/infrapilot/pkg/grpcserver"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
)

// withoutPrincipal lists RPCs that run before a principal exists (the edge resolving an API key).
var withoutPrincipal = []string{cpv1.AuthorizationService_AuthenticateApiKey_FullMethodName}

type actorKey struct{}

func actorFrom(ctx context.Context) service.Actor {
	a, _ := ctx.Value(actorKey{}).(service.Actor)
	return a
}

// PrincipalInterceptor authenticates the end-user principal carried in metadata (ADR-0025). The principal is
// accepted only when the verified peer is one of edgeIdentities; health/reflection never reach here with a
// principal requirement because the peer policy exempts them and they are not control-plane methods.
func PrincipalInterceptor(svc *service.Service, edgeIdentities []string, logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, "/infrapilot.controlplane.v1.") {
			return handler(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		meta := service.RequestMeta{RequestID: first(md, grpcmeta.RequestID), IdempotencyKey: first(md, grpcmeta.IdempotencyKey)}
		ctx = service.WithRequestMeta(ctx, meta)
		if slices.Contains(withoutPrincipal, info.FullMethod) {
			return handler(ctx, req)
		}
		raw := md.Get(grpcmeta.Principal)
		if len(raw) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing principal")
		}
		if peer, _ := grpcserver.PeerIdentity(ctx); !slices.Contains(edgeIdentities, peer) {
			// Only the edge vouches for end users; any other workload sending a principal is refused.
			logger.WarnContext(ctx, "principal metadata from a non-edge peer", slog.String("peer_identity", peer))
			return nil, status.Error(codes.PermissionDenied, "caller may not act for end users")
		}
		var pb cpv1.Principal
		if err := proto.Unmarshal([]byte(raw[0]), &pb); err != nil {
			return nil, status.Error(codes.Unauthenticated, "malformed principal")
		}
		p := service.Principal{Subject: pb.GetSubject(), Email: pb.GetEmail(), EmailVerified: pb.GetEmailVerified(), DisplayName: pb.GetDisplayName()}
		switch pb.GetKind() {
		case cpv1.PrincipalKind_PRINCIPAL_KIND_USER:
			p.Kind = domain.ActorUser
		case cpv1.PrincipalKind_PRINCIPAL_KIND_API_KEY:
			p.Kind = domain.ActorAPIKey
			id, err := uuid.Parse(pb.GetApiKeyId())
			if err != nil {
				return nil, status.Error(codes.Unauthenticated, "malformed principal")
			}
			p.APIKeyID = id
		default:
			return nil, status.Error(codes.Unauthenticated, "malformed principal")
		}
		actor, err := svc.ResolveActor(ctx, p)
		if err != nil {
			return nil, toStatus(ctx, logger, err)
		}
		return handler(context.WithValue(ctx, actorKey{}, actor), req)
	}
}

func first(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 && len(v[0]) <= 256 {
		return v[0]
	}
	return ""
}

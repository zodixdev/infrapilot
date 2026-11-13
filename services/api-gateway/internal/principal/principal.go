// Package principal is the authenticated caller as the gateway knows it, and how it travels to internal services
// (ADR-0025: serialized infrapilot.controlplane.v1.Principal in gRPC metadata, with the request id and the
// Idempotency-Key).
package principal

import (
	"context"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/pkg/grpcmeta"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
)

// Kind of caller.
type Kind int

// Kinds.
const (
	User Kind = iota + 1
	APIKey
)

// Principal is an authenticated caller.
type Principal struct {
	Kind Kind
	// Users.
	Subject, Email, Name string
	EmailVerified        bool
	// API keys.
	KeyID, OrgID, ProjectID string
	Scopes                  []cpv1.ApiKeyScope
}

// Key identifies the principal in cache and rate-limit keys (no secrets).
func (p *Principal) Key() string {
	if p.Kind == APIKey {
		return "k:" + p.KeyID
	}
	return "u:" + p.Subject
}

// Proto is the wire form sent to internal services.
func (p *Principal) Proto() *cpv1.Principal {
	if p.Kind == APIKey {
		return &cpv1.Principal{Kind: cpv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ApiKeyId: p.KeyID, OrganizationId: p.OrgID,
			ProjectId: p.ProjectID, Scopes: p.Scopes}
	}
	return &cpv1.Principal{Kind: cpv1.PrincipalKind_PRINCIPAL_KIND_USER, Subject: p.Subject, Email: p.Email,
		EmailVerified: p.EmailVerified, DisplayName: p.Name}
}

type ctxKey struct{}

// With attaches p to ctx.
func With(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the principal of the request, if authenticated.
func From(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok && p != nil
}

type idemKey struct{}

// WithIdempotencyKey records the request's Idempotency-Key.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idemKey{}, key)
}

// Detach returns a context that is not cancelled with ctx but keeps its values and its deadline: work shared by
// several waiters (singleflight) must not fail because the first waiter went away, yet stays bounded by the
// request deadline.
func Detach(ctx context.Context) (context.Context, context.CancelFunc) {
	d := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok {
		return context.WithDeadline(d, dl)
	}
	return context.WithCancel(d)
}

// Outgoing returns ctx with the request's principal, request id and Idempotency-Key as gRPC metadata.
func Outgoing(ctx context.Context) (context.Context, error) {
	kv := []string{grpcmeta.RequestID, httpserver.RequestIDFromContext(ctx)}
	if p, ok := From(ctx); ok {
		b, err := proto.Marshal(p.Proto())
		if err != nil {
			return nil, err
		}
		kv = append(kv, grpcmeta.Principal, string(b))
	}
	if k, _ := ctx.Value(idemKey{}).(string); k != "" {
		kv = append(kv, grpcmeta.IdempotencyKey, k)
	}
	return metadata.AppendToOutgoingContext(ctx, kv...), nil
}

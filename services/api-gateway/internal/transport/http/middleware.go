package http

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/authz"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/transport/http/api"
)

// maxCredentialBytes bounds Authorization / X-API-Key header values before any parsing.
const maxCredentialBytes = 8 << 10

// cors answers preflights and decorates responses for the configured browser origins only.
func cors(origins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			// Responses differ by origin whether or not it is allowed; shared caches must key on it.
			h := w.Header()
			h.Add("Vary", "Origin")
			if !slices.Contains(origins, origin) {
				next.ServeHTTP(w, r)
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", "RateLimit-Limit, RateLimit-Remaining, Retry-After, X-Request-Id")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-API-Key")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authenticate resolves presented credentials into a principal. Requests without credentials pass through
// anonymous; whether an operation requires a credential is decided by the contract (validator). Every failed
// authentication is charged to a per-client-IP budget, and a client that exhausted it is refused before any
// credential work (signature checks, control-plane lookups) is done.
func authenticate(v *auth.Verifier, keys *auth.KeyResolver, lim *limiter, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bearer, apiKey := r.Header.Get("Authorization"), r.Header.Get("X-API-Key")
			if bearer == "" && apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}
			failBucket := "authfail:ip:" + clientIP(r, lim.limits.TrustedProxies)
			if !lim.admitAuthAttempt(w, r, failBucket) {
				return
			}
			if (bearer != "" && apiKey != "") || len(bearer) > maxCredentialBytes || len(apiKey) > maxCredentialBytes {
				lim.chargeAuthFailure(r, failBucket)
				unauthorized(w, r)
				return
			}
			var p *principal.Principal
			var err error
			if apiKey != "" {
				p, err = keys.Resolve(r.Context(), apiKey)
			} else {
				scheme, token, ok := strings.Cut(bearer, " ")
				if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
					lim.chargeAuthFailure(r, failBucket)
					unauthorized(w, r)
					return
				}
				var id auth.Identity
				id, err = v.Verify(r.Context(), strings.TrimSpace(token))
				p = &principal.Principal{Kind: principal.User, Subject: id.Subject, Email: id.Email, EmailVerified: id.EmailVerified, Name: id.Name}
			}
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
			case r.Context().Err() != nil:
				// The client went away while we were resolving its credential; nothing to report.
				problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "request cancelled")
			case errors.Is(err, auth.ErrKeysUnavailable), errors.Is(err, auth.ErrBackendUnavailable):
				// F02: the identity provider (or control-plane) cannot vouch right now; this is not the caller's fault.
				logger.WarnContext(r.Context(), "authentication backend unavailable", slog.Any("error", err))
				problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "authentication temporarily unavailable")
			default:
				// The reason only (never the credential): diagnoses clock skew, wrong client or issuer.
				logger.DebugContext(r.Context(), "credential rejected", slog.Any("reason", err))
				lim.chargeAuthFailure(r, failBucket)
				unauthorized(w, r)
			}
		})
	}
}

func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="infrapilot", error="invalid_token"`)
	problem(w, r, http.StatusUnauthorized, codeUnauth, "invalid credentials")
}

// RateRule is a GCRA limit: a steady rate per second and a burst.
type RateRule struct {
	Rate  float64
	Burst int
}

// Limits configures rate limiting (ADR-0026).
type Limits struct {
	User, APIKey, Anonymous RateRule
	// AuthFailure is the per-client-IP budget of failed authentications (credential guessing).
	AuthFailure RateRule
	// Operations add a stricter per-principal bucket for sensitive operations (by operationId), e.g. member
	// changes and organization creation.
	Operations map[string]RateRule
	// TrustedProxies are the networks whose X-Forwarded-For is believed (ingress controllers).
	TrustedProxies []netip.Prefix
}

type limiter struct {
	store    *redisx.Store
	limits   Limits
	bypassed metric.Int64Counter
	limited  metric.Int64Counter
	log      *slog.Logger
}

func newLimiter(store *redisx.Store, limits Limits, meter metric.Meter, logger *slog.Logger) (*limiter, error) {
	l := &limiter{store: store, limits: limits, log: logger}
	var err error
	if l.bypassed, err = meter.Int64Counter("gateway_ratelimit_bypassed_total",
		metric.WithDescription("Requests admitted without a rate-limit decision because Redis was unavailable (fail open, F03)")); err != nil {
		return nil, err
	}
	if l.limited, err = meter.Int64Counter("gateway_ratelimit_limited_total", metric.WithDescription("Requests rejected with 429, by bucket")); err != nil {
		return nil, err
	}
	return l, nil
}

// allow applies one bucket; it writes 429 and returns false when the request must stop.
func (l *limiter) allow(w http.ResponseWriter, r *http.Request, bucket, kind string, rule RateRule) bool {
	d, err := l.store.Allow(r.Context(), bucket, rule.Rate, rule.Burst)
	if err != nil {
		// Fail open (F03): availability over limiting; the counter drives GatewayRateLimitFailOpen.
		l.bypassed.Add(r.Context(), 1, metric.WithAttributes(attribute.String("bucket", kind)))
		return true
	}
	w.Header().Set("RateLimit-Limit", strconv.Itoa(rule.Burst))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
	if d.Allowed {
		return true
	}
	l.limited.Add(r.Context(), 1, metric.WithAttributes(attribute.String("bucket", kind)))
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
	httpProblemTooMany(w, r)
	return false
}

// admitAuthAttempt refuses (429) a client whose failed-authentication budget is exhausted, without consuming it.
// Redis unavailable → admit (F03).
func (l *limiter) admitAuthAttempt(w http.ResponseWriter, r *http.Request, bucket string) bool {
	d, err := l.store.Peek(r.Context(), bucket, l.limits.AuthFailure.Rate, l.limits.AuthFailure.Burst)
	if err != nil || d.Allowed {
		return true
	}
	l.limited.Add(r.Context(), 1, metric.WithAttributes(attribute.String("bucket", "auth_failure")))
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
	httpProblemTooMany(w, r)
	return false
}

// chargeAuthFailure consumes one failed authentication from the client's budget (best effort).
func (l *limiter) chargeAuthFailure(r *http.Request, bucket string) {
	_, _ = l.store.Allow(r.Context(), bucket, l.limits.AuthFailure.Rate, l.limits.AuthFailure.Burst)
}

func httpProblemTooMany(w http.ResponseWriter, r *http.Request) {
	problem(w, r, http.StatusTooManyRequests, codeRateLimited, "too many requests")
}

// middleware limits every request by principal (or client IP when anonymous).
func (l *limiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket, kind, rule := "ip:"+clientIP(r, l.limits.TrustedProxies), "anonymous", l.limits.Anonymous
		if p, ok := principal.From(r.Context()); ok {
			bucket = p.Key()
			kind, rule = "user", l.limits.User
			if p.Kind == principal.APIKey {
				kind, rule = "api_key", l.limits.APIKey
			}
		}
		if l.allow(w, r, bucket, kind, rule) {
			next.ServeHTTP(w, r)
		}
	})
}

// clientIP is the peer address, or the right-most X-Forwarded-For entry not added by a trusted proxy when the
// peer itself is trusted. Clients cannot pick their own rate-limit bucket by spoofing the header.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !inAny(peer, trusted) {
		return host
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !inAny(a, trusted) {
			return a.String()
		}
	}
	return host
}

func inAny(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// securityCheck is the validator's AuthenticationFunc: an operation's security requirement is met only by the
// matching kind of credential (OIDC → user token, ApiKey → API key).
func securityCheck(ctx context.Context, in *openapi3filter.AuthenticationInput) error {
	p, ok := principal.From(ctx) // the validator passes the request context
	if !ok {
		return errNotAuthenticated
	}
	switch in.SecuritySchemeName {
	case "OIDC":
		if p.Kind == principal.User {
			return nil
		}
	case "ApiKey":
		if p.Kind == principal.APIKey {
			return nil
		}
	}
	return errWrongCredential
}

// policy is the edge authorization of one operation: which permission, and whether a project path parameter
// scopes it. Organization-less operations (my organizations, create organization) have none. Keys are the
// operation names the generated strict handler passes to middleware: the OpenAPI operationId with its first
// letter upper-cased (TestPolicyCoversEveryOperation keeps the table and the contract in sync).
type policy struct {
	perm    cpv1.Permission
	project bool
}

var policies = map[string]policy{
	"GetOrganization":    {perm: cpv1.Permission_PERMISSION_ORG_READ},
	"UpdateOrganization": {perm: cpv1.Permission_PERMISSION_ORG_UPDATE},
	"DeleteOrganization": {perm: cpv1.Permission_PERMISSION_ORG_DELETE},
	"ListMembers":        {perm: cpv1.Permission_PERMISSION_MEMBER_READ},
	"GetMember":          {perm: cpv1.Permission_PERMISSION_MEMBER_READ},
	"AddMember":          {perm: cpv1.Permission_PERMISSION_MEMBER_MANAGE},
	"UpdateMemberRole":   {perm: cpv1.Permission_PERMISSION_MEMBER_MANAGE},
	// Members may remove themselves, which the edge cannot tell apart; control-plane decides (ADR-0022).
	"RemoveMember":    {perm: cpv1.Permission_PERMISSION_MEMBER_READ},
	"ListProjects":    {perm: cpv1.Permission_PERMISSION_ORG_READ}, // keys list only their own project
	"GetProject":      {perm: cpv1.Permission_PERMISSION_PROJECT_READ, project: true},
	"CreateProject":   {perm: cpv1.Permission_PERMISSION_PROJECT_WRITE},
	"UpdateProject":   {perm: cpv1.Permission_PERMISSION_PROJECT_WRITE},
	"DeleteProject":   {perm: cpv1.Permission_PERMISSION_PROJECT_WRITE},
	"ListResources":   {perm: cpv1.Permission_PERMISSION_RESOURCE_READ, project: true},
	"GetResource":     {perm: cpv1.Permission_PERMISSION_RESOURCE_READ, project: true},
	"CreateResource":  {perm: cpv1.Permission_PERMISSION_RESOURCE_WRITE, project: true},
	"UpdateResource":  {perm: cpv1.Permission_PERMISSION_RESOURCE_WRITE, project: true},
	"DeleteResource":  {perm: cpv1.Permission_PERMISSION_RESOURCE_WRITE, project: true},
	"ListApiKeys":     {perm: cpv1.Permission_PERMISSION_APIKEY_READ},
	"GetApiKey":       {perm: cpv1.Permission_PERMISSION_APIKEY_READ},
	"CreateApiKey":    {perm: cpv1.Permission_PERMISSION_APIKEY_MANAGE},
	"RevokeApiKey":    {perm: cpv1.Permission_PERMISSION_APIKEY_MANAGE},
	"ListAuditEvents": {perm: cpv1.Permission_PERMISSION_AUDIT_READ},
}

// operationPolicy runs inside the generated strict handler (it knows the operationId): the per-operation rate
// limits, then the edge pre-authorization of ADR-0022. Denials of outsiders are 404 so ids reveal nothing.
func operationPolicy(az *authz.Authorizer, l *limiter, timeout time.Duration) api.StrictMiddlewareFunc {
	return func(f api.StrictHandlerFunc, operationID string) api.StrictHandlerFunc {
		pol, guarded := policies[operationID]
		rule, limited := l.limits.Operations[operationID]
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			p, authed := principal.From(ctx)
			if limited && authed && !l.allow(w, r, "op:"+operationID+":"+p.Key(), "operation", rule) {
				return nil, errResponded
			}
			if !guarded || !authed {
				return f(ctx, w, r, request)
			}
			// Canonical UUID text, so case variants neither split the decision cache nor fail the key-org match.
			orgID, err := uuid.Parse(r.PathValue("org_id"))
			if err != nil {
				problem(w, r, http.StatusBadRequest, codeValidation, "invalid organization id")
				return nil, errResponded
			}
			org, project := orgID.String(), ""
			if pol.project {
				projectID, perr := uuid.Parse(r.PathValue("project_id"))
				if perr != nil {
					problem(w, r, http.StatusBadRequest, codeValidation, "invalid project id")
					return nil, errResponded
				}
				project = projectID.String()
			}
			if p.Kind == principal.APIKey && p.OrgID != org {
				problem(w, r, http.StatusNotFound, codeNotFound, "not found")
				return nil, errResponded
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			d, err := az.Check(cctx, p, org, pol.perm, project)
			if err != nil {
				return nil, err // mapped like any upstream error (503 when control-plane is unreachable)
			}
			if !d.Allowed {
				if p.Kind == principal.User && !d.Member {
					problem(w, r, http.StatusNotFound, codeNotFound, "not found")
				} else {
					problem(w, r, http.StatusForbidden, codeDenied, "insufficient permissions")
				}
				return nil, errResponded
			}
			return f(ctx, w, r, request)
		}
	}
}

// errResponded tells the response error handler that the middleware already wrote the response.
var errResponded = errors.New("response already written")

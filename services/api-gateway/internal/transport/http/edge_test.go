package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zodixdev/infrapilot/services/api-gateway/internal/principal"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/transport/http/api"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// unguarded operations are deliberately not pre-authorized at the edge: they are not organization-scoped.
var unguarded = []string{"GetHealth", "ListMyOrganizations", "CreateOrganization"}

// TestPolicyCoversEveryOperation fails when an operation is added to the contract without deciding its edge
// authorization: a missing entry would otherwise silently skip pre-authorization.
func TestPolicyCoversEveryOperation(t *testing.T) {
	t.Parallel()
	spec, err := api.GetSpec()
	require.NoError(t, err)
	seen := map[string]bool{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			id := generatedName(op.OperationID)
			seen[id] = true
			_, guarded := policies[id]
			exempt := false
			for _, u := range unguarded {
				exempt = exempt || u == id
			}
			require.NotEqual(t, guarded, exempt, "%s %s (%s) must be either in policies or explicitly unguarded", method, path, id)
		}
	}
	for id := range policies {
		require.True(t, seen[id], "policy for unknown operation %s", id)
	}
}

// generatedName is how oapi-codegen names an operation (and what it passes to strict middleware).
func generatedName(operationID string) string {
	return strings.ToUpper(operationID[:1]) + operationID[1:]
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}
	tests := []struct {
		name, remote, xff, want string
	}{
		{"direct client, header ignored", "203.0.113.7:5000", "198.51.100.1", "203.0.113.7"},
		{"trusted proxy, right-most untrusted hop", "10.1.2.3:443", "198.51.100.9, 203.0.113.5", "203.0.113.5"},
		{"spoofed left entries are skipped", "10.1.2.3:443", "1.2.3.4, 203.0.113.5, 10.1.9.9", "203.0.113.5"},
		{"only trusted hops → proxy address", "10.1.2.3:443", "10.1.4.4", "10.1.2.3"},
		{"garbage header → proxy address", "10.1.2.3:443", "not-an-ip", "10.1.2.3"},
		{"ipv4-mapped proxy is trusted", "[::ffff:10.1.2.3]:443", "203.0.113.5", "203.0.113.5"},
		{"no header", "10.1.2.3:443", "", "10.1.2.3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			require.Equal(t, tc.want, clientIP(r, trusted))
		})
	}
}

func TestCORS(t *testing.T) {
	t.Parallel()
	h := cors([]string{"https://app.example"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	do := func(origin, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, "/v1/organizations", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if method == http.MethodOptions {
			r.Header.Set("Access-Control-Request-Method", "POST")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ok := do("https://app.example", http.MethodOptions)
	require.Equal(t, http.StatusNoContent, ok.Code)
	require.Equal(t, "https://app.example", ok.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, ok.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")
	evil := do("https://evil.example", http.MethodGet)
	require.Empty(t, evil.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, "Origin", evil.Header().Get("Vary"), "caches must key on Origin either way")
	require.Empty(t, do("", http.MethodGet).Header().Get("Vary"))
}

func TestSecurityCheck(t *testing.T) {
	t.Parallel()
	user := principal.With(t.Context(), &principal.Principal{Kind: principal.User, Subject: "s"})
	key := principal.With(t.Context(), &principal.Principal{Kind: principal.APIKey, KeyID: "k"})
	in := func(scheme string) *openapi3filter.AuthenticationInput {
		return &openapi3filter.AuthenticationInput{SecuritySchemeName: scheme}
	}
	require.NoError(t, securityCheck(user, in("OIDC")))
	require.NoError(t, securityCheck(key, in("ApiKey")))
	require.ErrorIs(t, securityCheck(key, in("OIDC")), errWrongCredential)
	require.ErrorIs(t, securityCheck(user, in("ApiKey")), errWrongCredential)
	require.ErrorIs(t, securityCheck(t.Context(), in("OIDC")), errNotAuthenticated)
}

func TestUpstreamErrorMapping(t *testing.T) {
	t.Parallel()
	badReq, _ := status.New(codes.InvalidArgument, "bad").WithDetails(&errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "slug", Description: "invalid"}}})
	reused, _ := status.New(codes.AlreadyExists, "x").WithDetails(&errdetails.ErrorInfo{Reason: idemReasonUpstrm})
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{badReq.Err(), 400, codeValidation},
		{status.Error(codes.NotFound, "x"), 404, codeNotFound},
		{status.Error(codes.PermissionDenied, "x"), 403, codeDenied},
		{status.Error(codes.Unauthenticated, "x"), 401, codeUnauth},
		{reused.Err(), 422, codeIdemReused},
		{status.Error(codes.AlreadyExists, "slug taken"), 409, codeConflict},
		{status.Error(codes.FailedPrecondition, "last owner"), 409, codeConflict},
		{status.Error(codes.ResourceExhausted, "x"), 429, codeRateLimited},
		{status.Error(codes.Unavailable, "x"), 503, codeUnavailable},
		{status.Error(codes.DeadlineExceeded, "x"), 503, codeUnavailable},
		{context.DeadlineExceeded, 503, codeUnavailable},
		{context.Canceled, 503, codeUnavailable},
		{status.Error(codes.Internal, "secret detail"), 500, codeInternal},
		{errors.New("plain"), 500, codeInternal},
	}
	h := upstreamError(discardLogger())
	for _, tc := range tests {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/x", nil)
		w := httptest.NewRecorder()
		h(w, r, tc.err)
		require.Equal(t, tc.status, w.Code, "%v", tc.err)
		require.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
		require.NotContains(t, w.Body.String(), "secret detail", "internal messages never reach clients")
		if tc.status == 503 {
			require.Equal(t, "1", w.Header().Get("Retry-After"))
		}
	}
}

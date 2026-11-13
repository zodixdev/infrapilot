package grpcserver_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/zodixdev/infrapilot/pkg/grpcserver"
	"github.com/zodixdev/infrapilot/pkg/testutil/certtest"
)

// identityService exposes two methods that return the caller's verified identity, so tests can tell
// method rules from service-wide rules:
//
//	/infrapilot.test.Identity/Edge   (method rule)
//	/infrapilot.test.Identity/Any    (service wildcard rule)
const identityService = "infrapilot.test.Identity"

var identityDesc = grpc.ServiceDesc{
	ServiceName: identityService,
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Edge", Handler: whoami("Edge")},
		{MethodName: "Any", Handler: whoami("Any")},
	},
	Metadata: "peer_test.go",
}

func whoami(name string) func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(emptypb.Empty)
		if err := dec(in); err != nil {
			return nil, err
		}
		h := func(ctx context.Context, _ any) (any, error) {
			id, _ := grpcserver.PeerIdentity(ctx)
			return wrapperspb.String(id), nil
		}
		return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + identityService + "/" + name}, h)
	}
}

var (
	gatewayID = certtest.SPIFFE("infrapilot", "api-gateway")
	workerID  = certtest.SPIFFE("infrapilot", "check-runner")
)

func peerPolicy(insecureID string) grpcserver.PeerPolicy {
	return grpcserver.PeerPolicy{
		Rules: map[string][]string{
			"/" + identityService + "/Edge": {gatewayID},
			"/" + identityService + "/*":    {gatewayID, workerID},
		},
		InsecureIdentity: insecureID,
	}
}

func startWithPolicy(t *testing.T, cfg grpcserver.Config, p grpcserver.PeerPolicy) string {
	t.Helper()
	s, err := grpcserver.New(t.Context(), cfg, grpcserver.WithPeerPolicy(p))
	require.NoError(t, err)
	s.Registrar().RegisterService(&identityDesc, struct{}{})
	s.SetServing(true)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.WithoutCancel(t.Context())) }()
	t.Cleanup(func() {
		require.NoError(t, s.Shutdown(context.WithoutCancel(t.Context())))
		require.NoError(t, <-runErr)
	})
	return s.Addr().String()
}

func call(t *testing.T, addr string, creds credentials.TransportCredentials, method string) (string, error) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	out := new(wrapperspb.StringValue)
	err = conn.Invoke(t.Context(), "/"+identityService+"/"+method, &emptypb.Empty{}, out)
	return out.GetValue(), err
}

func mtlsCreds(t *testing.T, ca *certtest.CA, f *certtest.Files) credentials.TransportCredentials {
	t.Helper()
	pem, err := os.ReadFile(ca.File)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(pem))
	tc := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if f != nil {
		cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
		require.NoError(t, err)
		tc.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(tc)
}

func TestPeerPolicy_MutualTLS(t *testing.T) {
	t.Parallel()
	ca := certtest.NewCA(t)
	srv := ca.Issue("server", certtest.SPIFFE("infrapilot", "control-plane"))
	cfg := testConfig()
	cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile = srv.Cert, srv.Key, ca.File
	addr := startWithPolicy(t, cfg, peerPolicy(""))

	gw := ca.Issue("gateway", gatewayID)
	worker := ca.Issue("worker", workerID)
	anon := ca.Issue("no-uri")
	other := ca.Issue("other", "spiffe://infrapilot.local/ns/other/sa/api-gateway")

	tests := []struct {
		name   string
		client certtest.Files
		method string
		want   codes.Code
	}{
		{"edge identity on method rule", gw, "Edge", codes.OK},
		{"method rule takes precedence over service wildcard", worker, "Edge", codes.PermissionDenied},
		{"service wildcard admits listed identity", worker, "Any", codes.OK},
		{"certificate without URI SAN has no identity", anon, "Any", codes.PermissionDenied},
		{"same service account in another namespace is a different identity", other, "Edge", codes.PermissionDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := call(t, addr, mtlsCreds(t, ca, &tc.client), tc.method)
			require.Equal(t, tc.want, status.Code(err), "error: %v", err)
			if tc.want == codes.OK {
				require.NotEmpty(t, got, "handler sees the verified identity")
			}
		})
	}

	t.Run("health is exempt from the allow-list", func(t *testing.T) {
		t.Parallel()
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(mtlsCreds(t, ca, &anon)))
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		_, err = healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
		require.NoError(t, err)
	})

	t.Run("method without a rule is denied by default", func(t *testing.T) {
		t.Parallel()
		onlyEdge := grpcserver.PeerPolicy{Rules: map[string][]string{"/" + identityService + "/Edge": {gatewayID}}}
		addr := startWithPolicy(t, cfg, onlyEdge)
		_, err := call(t, addr, mtlsCreds(t, ca, &gw), "Any")
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})
}

func TestPeerPolicy_PlaintextUsesInsecureIdentity(t *testing.T) {
	t.Parallel()
	addr := startWithPolicy(t, testConfig(), peerPolicy(gatewayID))
	got, err := call(t, addr, insecure.NewCredentials(), "Edge")
	require.NoError(t, err)
	require.Equal(t, gatewayID, got)

	addr = startWithPolicy(t, testConfig(), peerPolicy(""))
	_, err = call(t, addr, insecure.NewCredentials(), "Any")
	require.Equal(t, codes.PermissionDenied, status.Code(err), "plaintext without a configured identity is anonymous")
}

func TestPeerPolicy_Validation(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]grpcserver.PeerPolicy{
		"rule without leading slash": {Rules: map[string][]string{"svc/Method": {gatewayID}}},
		"identity not spiffe":        {Rules: map[string][]string{"/svc/Method": {"api-gateway"}}},
		"insecure identity":          {InsecureIdentity: "gateway"},
	} {
		_, err := grpcserver.New(t.Context(), testConfig(), grpcserver.WithPeerPolicy(p))
		require.Error(t, err, name)
	}
}

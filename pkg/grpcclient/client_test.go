package grpcclient_test

import (
	"context"
	"crypto/x509"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/zodixdev/infrapilot/pkg/config"
	"github.com/zodixdev/infrapilot/pkg/grpcclient"
	"github.com/zodixdev/infrapilot/pkg/grpcserver"
	"github.com/zodixdev/infrapilot/pkg/testutil/certtest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// probe is a hand-written service:
//
//	Whoami    → caller identity seen by the server
//	Remaining → time left on the server-side deadline
//	Flaky     → UNAVAILABLE until the third attempt, counting attempts (declared idempotent in tests)
//	Mutate    → always UNAVAILABLE, counting attempts (never declared idempotent)
const svc = "infrapilot.test.Probe"

type probe struct{ flaky, mutate atomic.Int32 }

func (p *probe) desc() *grpc.ServiceDesc {
	h := func(name string, fn func(ctx context.Context) (any, error)) grpc.MethodDesc {
		return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(new(emptypb.Empty)); err != nil {
				return nil, err
			}
			return ic(ctx, &emptypb.Empty{}, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + svc + "/" + name},
				func(ctx context.Context, _ any) (any, error) { return fn(ctx) })
		}}
	}
	return &grpc.ServiceDesc{
		ServiceName: svc, HandlerType: (*any)(nil), Metadata: "client_test.go",
		Methods: []grpc.MethodDesc{
			h("Whoami", func(ctx context.Context) (any, error) {
				id, _ := grpcserver.PeerIdentity(ctx)
				return wrapperspb.String(id), nil
			}),
			h("Remaining", func(ctx context.Context) (any, error) {
				dl, ok := ctx.Deadline()
				if !ok {
					return durationpb.New(0), nil
				}
				return durationpb.New(time.Until(dl)), nil
			}),
			h("Flaky", func(context.Context) (any, error) {
				if p.flaky.Add(1) < 3 {
					return nil, status.Error(codes.Unavailable, "try again")
				}
				return &emptypb.Empty{}, nil
			}),
			h("Mutate", func(context.Context) (any, error) {
				p.mutate.Add(1)
				return nil, status.Error(codes.Unavailable, "down")
			}),
		},
	}
}

var (
	serverID = certtest.SPIFFE("infrapilot", "control-plane")
	clientID = certtest.SPIFFE("infrapilot", "api-gateway")
)

func startServer(t *testing.T, ca *certtest.CA, insecureID string) (string, *probe) {
	t.Helper()
	cfg := serverConfig()
	if ca != nil {
		f := ca.Issue("server", serverID)
		cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile = f.Cert, f.Key, ca.File
	}
	return runServer(t, cfg, insecureID)
}

func startServerWithFiles(t *testing.T, ca *certtest.CA, f certtest.Files) string {
	t.Helper()
	cfg := serverConfig()
	cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile = f.Cert, f.Key, ca.File
	addr, _ := runServer(t, cfg, "")
	return addr
}

func serverConfig() grpcserver.Config {
	return grpcserver.Config{
		Addr: "127.0.0.1:0", MaxRecvMsgBytes: 1 << 20, ConnectionTimeout: 5 * time.Second,
		KeepaliveTime: 30 * time.Second, KeepaliveTimeout: 10 * time.Second, MaxConcurrentStreams: 16,
	}
}

func runServer(t *testing.T, cfg grpcserver.Config, insecureID string) (string, *probe) {
	t.Helper()
	s, err := grpcserver.New(t.Context(), cfg, grpcserver.WithPeerPolicy(grpcserver.PeerPolicy{
		Rules: map[string][]string{"/" + svc + "/*": {clientID}}, InsecureIdentity: insecureID,
	}))
	require.NoError(t, err)
	p := &probe{}
	s.Registrar().RegisterService(p.desc(), p)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.WithoutCancel(t.Context())) }()
	t.Cleanup(func() {
		require.NoError(t, s.Shutdown(context.WithoutCancel(t.Context())))
		require.NoError(t, <-runErr)
	})
	return s.Addr().String(), p
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, b, 0o600))
}

func dial(t *testing.T, cfg config.GRPCClient, opts ...grpcclient.Option) *grpc.ClientConn {
	t.Helper()
	conn, err := grpcclient.New(cfg, config.EnvTest, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestClient_MutualTLSCarriesWorkloadIdentity(t *testing.T) {
	t.Parallel()
	ca := certtest.NewCA(t)
	addr, _ := startServer(t, ca, "")
	cli := ca.Issue("client", clientID)
	conn := dial(t, config.GRPCClient{Target: addr, ServerName: "localhost", ServerIdentity: serverID,
		TLSCertFile: cli.Cert, TLSKeyFile: cli.Key, TLSCAFile: ca.File, Timeout: 2 * time.Second})
	out := new(wrapperspb.StringValue)
	require.NoError(t, conn.Invoke(t.Context(), "/"+svc+"/Whoami", &emptypb.Empty{}, out))
	require.Equal(t, clientID, out.GetValue())
}

func TestClient_RejectsUntrustedOrWrongServer(t *testing.T) {
	t.Parallel()
	ca := certtest.NewCA(t)
	addr, _ := startServer(t, ca, "")
	cli := ca.Issue("client", clientID)
	other := certtest.NewCA(t)

	tests := map[string]struct {
		cfg  config.GRPCClient
		want string
	}{
		"server identity mismatch": {config.GRPCClient{ServerIdentity: certtest.SPIFFE("infrapilot", "impostor"), TLSCAFile: ca.File}, "server identity is not"},
		"server name mismatch":     {config.GRPCClient{ServerName: "control-plane.infrapilot.svc", TLSCAFile: ca.File}, "verify server certificate"},
		"untrusted CA":             {config.GRPCClient{TLSCAFile: other.File}, "verify server certificate"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := tc.cfg
			c.Target, c.Timeout, c.TLSCertFile, c.TLSKeyFile = addr, time.Second, cli.Cert, cli.Key
			if c.ServerName == "" {
				c.ServerName = "localhost"
			}
			err := dial(t, c).Invoke(t.Context(), "/"+svc+"/Whoami", &emptypb.Empty{}, new(wrapperspb.StringValue))
			require.Equal(t, codes.Unavailable, status.Code(err), "handshake must fail: %v", err)
			require.ErrorContains(t, err, tc.want, "the failure is the verification, not something else")
		})
	}
}

func TestClient_RequiresServerAuthUsage(t *testing.T) {
	t.Parallel()
	ca := certtest.NewCA(t)
	// A certificate usable only as a client must not be accepted as a server certificate.
	srv := ca.IssueWithUsage("client-only-server", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, serverID)
	addr := startServerWithFiles(t, ca, srv)
	cli := ca.Issue("client", clientID)
	err := dial(t, config.GRPCClient{Target: addr, ServerName: "localhost", TLSCertFile: cli.Cert, TLSKeyFile: cli.Key,
		TLSCAFile: ca.File, Timeout: time.Second}).Invoke(t.Context(), "/"+svc+"/Whoami", &emptypb.Empty{}, new(wrapperspb.StringValue))
	require.ErrorContains(t, err, "verify server certificate")
}

func TestClient_FollowsCARotation(t *testing.T) {
	t.Parallel()
	oldCA, newCA := certtest.NewCA(t), certtest.NewCA(t)
	cli := newCA.Issue("client", clientID)
	// The server is issued by the new CA; the client starts out trusting only the old one.
	addr := startServerWithFiles(t, newCA, newCA.Issue("server", serverID))
	dir := t.TempDir()
	bundle := dir + "/ca.crt"
	copyFile(t, oldCA.File, bundle)
	conn := dial(t, config.GRPCClient{Target: addr, ServerName: "localhost", ServerIdentity: serverID,
		TLSCertFile: cli.Cert, TLSKeyFile: cli.Key, TLSCAFile: bundle, Timeout: time.Second})
	call := func() error {
		return conn.Invoke(t.Context(), "/"+svc+"/Whoami", &emptypb.Empty{}, new(wrapperspb.StringValue))
	}
	require.ErrorContains(t, call(), "verify server certificate")

	copyFile(t, newCA.File, bundle)
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(bundle, future, future))
	require.Eventually(t, func() bool { return call() == nil }, 10*time.Second, 200*time.Millisecond,
		"a rotated CA bundle is used without restarting the client")
}

func TestClient_DerivesServerNameFromTarget(t *testing.T) {
	t.Parallel()
	ca := certtest.NewCA(t)
	addr, _ := startServer(t, ca, "")
	cli := ca.Issue("client", clientID)
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	conn := dial(t, config.GRPCClient{Target: "dns:///localhost:" + port, ServerIdentity: serverID,
		TLSCertFile: cli.Cert, TLSKeyFile: cli.Key, TLSCAFile: ca.File, Timeout: 2 * time.Second})
	require.NoError(t, conn.Invoke(t.Context(), "/"+svc+"/Whoami", &emptypb.Empty{}, new(wrapperspb.StringValue)))

	_, err = grpcclient.New(config.GRPCClient{Target: "unix:///run/cp.sock", TLSCAFile: ca.File, Timeout: time.Second}, config.EnvTest)
	require.ErrorContains(t, err, "set ServerName")
}

func TestClient_DefaultDeadline(t *testing.T) {
	t.Parallel()
	addr, _ := startServer(t, nil, clientID)
	conn := dial(t, config.GRPCClient{Target: addr, Insecure: true, Timeout: 1500 * time.Millisecond})

	got := new(durationpb.Duration)
	require.NoError(t, conn.Invoke(t.Context(), "/"+svc+"/Remaining", &emptypb.Empty{}, got))
	require.InDelta(t, 1.5, got.AsDuration().Seconds(), 0.3, "calls without a deadline get the configured one")

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, conn.Invoke(ctx, "/"+svc+"/Remaining", &emptypb.Empty{}, got))
	require.Less(t, got.AsDuration(), 400*time.Millisecond, "a caller deadline is kept")
}

func TestClient_RetriesOnlyIdempotentMethods(t *testing.T) {
	t.Parallel()
	addr, p := startServer(t, nil, clientID)
	conn := dial(t, config.GRPCClient{Target: addr, Insecure: true, Timeout: 3 * time.Second},
		grpcclient.WithIdempotent("/"+svc+"/Flaky"))

	require.NoError(t, conn.Invoke(t.Context(), "/"+svc+"/Flaky", &emptypb.Empty{}, &emptypb.Empty{}))
	require.EqualValues(t, 3, p.flaky.Load(), "UNAVAILABLE retried up to MaxAttempts")

	err := conn.Invoke(t.Context(), "/"+svc+"/Mutate", &emptypb.Empty{}, &emptypb.Empty{})
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.EqualValues(t, 1, p.mutate.Load(), "non-idempotent RPCs are never retried")
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	_, err := grpcclient.New(config.GRPCClient{Target: "x:1", Insecure: true}, config.EnvTest)
	require.ErrorContains(t, err, "TIMEOUT")
	_, err = grpcclient.New(config.GRPCClient{Target: "x:1", Timeout: time.Second}, config.EnvTest)
	require.ErrorContains(t, err, "TLS_CA_FILE")
	_, err = grpcclient.New(config.GRPCClient{Target: "x:1", Insecure: true, Timeout: time.Second}, config.EnvProd)
	require.ErrorContains(t, err, "INSECURE is not allowed in prod", "environment rails are enforced by New")
	_, err = grpcclient.New(config.GRPCClient{Target: "x:1", Insecure: true, Timeout: time.Second}, config.EnvTest,
		grpcclient.WithIdempotent("pkg.Service/Method"))
	require.ErrorContains(t, err, "idempotent method")
}

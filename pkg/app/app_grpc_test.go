package app_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/zodixdev/infrapilot/pkg/app"
	"github.com/zodixdev/infrapilot/pkg/grpcserver"
	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
)

// blockingService is a hand-written gRPC service (no generated code) with two unary methods:
//
//	Block(Empty) → Empty   signals started, then waits for release (or the server-side ctx)
//	Ping(Empty)  → Empty   returns immediately
type blockingService struct {
	started chan struct{} // receives one value per Block call that reached the handler
	release chan struct{}
}

const blockingName = "infrapilot.test.AppDrain"

func (b *blockingService) desc() *grpc.ServiceDesc {
	handler := func(name string, fn func(ctx context.Context) error) grpc.MethodDesc {
		return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, _ any) (any, error) {
				if err := fn(ctx); err != nil {
					return nil, err
				}
				return &emptypb.Empty{}, nil
			}
			if ic == nil {
				return h(ctx, in)
			}
			return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + blockingName + "/" + name}, h)
		}}
	}
	return &grpc.ServiceDesc{
		ServiceName: blockingName,
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			handler("Block", func(ctx context.Context) error {
				b.started <- struct{}{}
				select {
				case <-b.release:
					return nil
				case <-ctx.Done():
					return status.FromContextError(ctx.Err()).Err()
				}
			}),
			handler("Ping", func(context.Context) error { return nil }),
		},
		Metadata: "app_grpc_test.go",
	}
}

func invoke(ctx context.Context, conn *grpc.ClientConn, method string) error {
	return conn.Invoke(ctx, "/"+blockingName+"/"+method, &emptypb.Empty{}, &emptypb.Empty{})
}

type drainFixture struct {
	svc     *blockingService
	reg     *health.Registry
	grpcSrv *grpcserver.Server
	conn    *grpc.ClientConn
	readyz  string
	cancel  context.CancelFunc
	done    chan error
}

// startDrainFixture wires the real health registry, HTTP server (/readyz) and gRPC server into an App,
// exactly as a service's cmd/ does, and waits until the service reports ready.
func startDrainFixture(t *testing.T, cfg app.Config) *drainFixture {
	t.Helper()
	f := &drainFixture{
		svc: &blockingService{started: make(chan struct{}, 4), release: make(chan struct{})},
		reg: health.NewRegistry(),
	}
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", f.reg.ReadinessHandler())
	httpSrv, err := httpserver.New(t.Context(), httpserver.Config{
		Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second,
	}, mux)
	require.NoError(t, err)
	f.grpcSrv, err = grpcserver.New(t.Context(), grpcserver.Config{
		Addr: "127.0.0.1:0", MaxRecvMsgBytes: 1 << 20, ConnectionTimeout: 5 * time.Second,
		KeepaliveTime: 30 * time.Second, KeepaliveTimeout: 10 * time.Second, MaxConcurrentStreams: 16,
		DefaultTimeout: 30 * time.Second,
	})
	require.NoError(t, err)
	f.grpcSrv.Registrar().RegisterService(f.svc.desc(), f.svc)

	a, err := app.New(cfg, app.WithReadinessGate(f.reg), app.WithSignals())
	require.NoError(t, err)
	a.Add(f.grpcSrv, httpSrv)

	f.conn, err = grpc.NewClient(f.grpcSrv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.conn.Close() })

	var ctx context.Context
	ctx, f.cancel = context.WithCancel(t.Context())
	t.Cleanup(f.cancel)
	f.done = make(chan error, 1)
	go func() { f.done <- a.Run(ctx) }()
	f.readyz = "http://" + httpSrv.Addr().String() + "/readyz"
	require.Eventually(t, func() bool { return statusOf(t.Context(), f.readyz) == http.StatusOK }, 5*time.Second, 10*time.Millisecond)
	return f
}

func (f *drainFixture) waitRun(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-f.done:
		return err
	case <-time.After(within):
		t.Fatalf("app.Run did not return within %s", within)
		return nil
	}
}

// TestApp_GRPCInFlightUnaryCompletesDuringAppShutdown proves the acceptance item "graceful shutdown drains
// in-flight gRPC work within the deadline" through the App, not only through grpcserver.Shutdown: readiness
// is withdrawn first, the server keeps serving new RPCs during the endpoint-drain delay, the in-flight unary
// call completes successfully, and Run returns cleanly. Afterwards the server refuses RPCs.
func TestApp_GRPCInFlightUnaryCompletesDuringAppShutdown(t *testing.T) {
	t.Parallel()
	f := startDrainFixture(t, app.Config{ShutdownTimeout: 10 * time.Second, ShutdownDelay: time.Second})

	callCtx, cancelCall := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelCall()
	callErr := make(chan error, 1)
	go func() { callErr <- invoke(callCtx, f.conn, "Block") }()
	<-f.svc.started // the RPC is in the handler

	f.cancel() // app-driven shutdown while the RPC is in flight
	require.Eventually(t, func() bool { return statusOf(t.Context(), f.readyz) == http.StatusServiceUnavailable },
		time.Second, 5*time.Millisecond, "readiness withdrawn as the first shutdown step")
	// Still inside the 1s ShutdownDelay: traffic routed before the endpoint was withdrawn is served.
	require.NoError(t, invoke(callCtx, f.conn, "Ping"), "new RPCs are served while the endpoint drains")

	select {
	case err := <-callErr:
		t.Fatalf("in-flight RPC returned before it was released: %v", err)
	default:
	}
	close(f.svc.release)
	require.NoError(t, <-callErr, "in-flight unary RPC completed during app shutdown")
	require.NoError(t, f.waitRun(t, 10*time.Second), "clean drain within the deadline")

	err := invoke(callCtx, f.conn, "Ping")
	require.Equal(t, codes.Unavailable, status.Code(err), "server is gone after shutdown: %v", err)
}

// TestApp_GRPCStuckUnaryIsCutAtTheDrainDeadline: an RPC that would outlive ShutdownTimeout does not hold
// the process; the App force-stops the gRPC server at the deadline, reports it, and Run returns promptly.
func TestApp_GRPCStuckUnaryIsCutAtTheDrainDeadline(t *testing.T) {
	t.Parallel()
	const drain = 300 * time.Millisecond
	f := startDrainFixture(t, app.Config{ShutdownTimeout: drain})

	callCtx, cancelCall := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelCall()
	callErr := make(chan error, 1)
	go func() { callErr <- invoke(callCtx, f.conn, "Block") }() // never released
	<-f.svc.started

	start := time.Now()
	f.cancel()
	err := f.waitRun(t, 5*time.Second)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the forced stop is reported: %v", err)
	require.ErrorContains(t, err, "shutdown grpc")
	require.GreaterOrEqual(t, elapsed, drain, "the drain waited for the in-flight RPC up to the deadline")
	require.Less(t, elapsed, drain+2*time.Second, "Run returned promptly after the deadline")

	rpcErr := <-callErr
	require.Error(t, rpcErr, "the stuck RPC was cut off")
	require.Equal(t, codes.Unavailable, status.Code(rpcErr), "cut by the server's forced stop, not by the client timeout: %v", rpcErr)
	require.NoError(t, callCtx.Err())
}

package grpcserver_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/zodixdev/infrapilot/pkg/grpcserver"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// A hand-written service descriptor keeps the test independent of generated code:
//
//	Sleep(Duration) → Empty          blocks for the duration (or until ctx is done)
//	Panic(Empty) → Empty             panics
//	Remaining(Empty) → Duration      returns the time left on the server-side deadline
const testService = "infrapilot.test.Lifecycle"

type lifecycle struct{}

var testDesc = grpc.ServiceDesc{
	ServiceName: testService,
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Sleep", Handler: unaryHandler("Sleep", func(ctx context.Context, in *durationpb.Duration) (any, error) {
			select {
			case <-time.After(in.AsDuration()):
				return &emptypb.Empty{}, nil
			case <-ctx.Done():
				return nil, status.FromContextError(ctx.Err()).Err()
			}
		})},
		{MethodName: "Panic", Handler: unaryHandler("Panic", func(context.Context, *durationpb.Duration) (any, error) { panic("boom") })},
		{MethodName: "Remaining", Handler: unaryHandler("Remaining", func(ctx context.Context, _ *durationpb.Duration) (any, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				return durationpb.New(0), nil
			}
			return durationpb.New(time.Until(dl)), nil
		})},
	},
	Metadata: "lifecycle_test.go",
}

func unaryHandler(name string, fn func(context.Context, *durationpb.Duration) (any, error)) func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(durationpb.Duration)
		if err := dec(in); err != nil {
			return nil, err
		}
		h := func(ctx context.Context, req any) (any, error) {
			d, _ := req.(*durationpb.Duration)
			return fn(ctx, d)
		}
		if interceptor == nil {
			return h(ctx, in)
		}
		return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + testService + "/" + name}, h)
	}
}

func testConfig() grpcserver.Config {
	return grpcserver.Config{
		Addr: "127.0.0.1:0", MaxRecvMsgBytes: 1 << 20, ConnectionTimeout: 5 * time.Second,
		KeepaliveTime: 30 * time.Second, KeepaliveTimeout: 10 * time.Second, MaxConcurrentStreams: 16,
		DefaultTimeout: 3 * time.Second,
	}
}

func start(t *testing.T, opts ...grpcserver.Option) (*grpcserver.Server, *grpc.ClientConn, <-chan error) {
	t.Helper()
	s, err := grpcserver.New(t.Context(), testConfig(), opts...)
	require.NoError(t, err)
	s.Registrar().RegisterService(&testDesc, lifecycle{})
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(t.Context()) }()
	conn, err := grpc.NewClient(s.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return s, conn, runErr
}

func sleep(ctx context.Context, conn *grpc.ClientConn, d time.Duration) error {
	return conn.Invoke(ctx, "/"+testService+"/Sleep", durationpb.New(d), &emptypb.Empty{})
}

// inFlightMeter returns a meter provider whose grpc_server_in_flight gauge tests poll instead of sleeping.
func inFlightMeter(t *testing.T) (*sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.WithoutCancel(t.Context())) })
	return reader, mp
}

// waitInFlight polls until exactly want RPCs are inside handlers.
func waitInFlight(t *testing.T, reader *sdkmetric.ManualReader, want int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &rm); err != nil {
			return false
		}
		var n int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "grpc_server_in_flight" {
					for _, dp := range sum.DataPoints {
						n += dp.Value
					}
				}
			}
		}
		return n == want
	}, 5*time.Second, 2*time.Millisecond, "expected %d in-flight RPC(s)", want)
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.MaxRecvMsgBytes = 0
	_, err := grpcserver.New(t.Context(), cfg)
	require.Error(t, err)
	cfg = testConfig()
	cfg.KeepaliveTime = 0
	_, err = grpcserver.New(t.Context(), cfg)
	require.Error(t, err)
}

func TestServer_GracefulStopDrainsInFlight(t *testing.T) {
	t.Parallel()
	reader, mp := inFlightMeter(t)
	s, conn, runErr := start(t, grpcserver.WithMeterProvider(mp))
	callErr := make(chan error, 1)
	rpcStart := time.Now()
	go func() { callErr <- sleep(t.Context(), conn, 400*time.Millisecond) }()
	waitInFlight(t, reader, 1) // the RPC reached the handler

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.Shutdown(ctx))
	require.GreaterOrEqual(t, time.Since(rpcStart), 400*time.Millisecond, "Shutdown waited for the in-flight RPC")
	require.NoError(t, <-callErr, "in-flight RPC completed during drain")
	require.NoError(t, <-runErr)

	err := sleep(t.Context(), conn, 0)
	require.Equal(t, codes.Unavailable, status.Code(err), "server is gone: %v", err)
}

func TestServer_ShutdownDeadlineForcesStop(t *testing.T) {
	t.Parallel()
	reader, mp := inFlightMeter(t)
	s, conn, runErr := start(t, grpcserver.WithMeterProvider(mp))
	callErr := make(chan error, 1)
	go func() { callErr <- sleep(t.Context(), conn, 10*time.Second) }()
	waitInFlight(t, reader, 1)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Shutdown(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 3*time.Second)
	require.NoError(t, <-runErr)
	require.Error(t, <-callErr, "the stuck RPC was cut off")
}

func TestServer_HealthRecoveryAndDefaultDeadline(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.WithoutCancel(t.Context())) })
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.WithoutCancel(t.Context())) })

	s, conn, runErr := start(t, grpcserver.WithMeterProvider(mp), grpcserver.WithTracerProvider(tp))
	hc := healthpb.NewHealthClient(conn)

	resp, err := hc.Check(t.Context(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, resp.GetStatus(), "not serving until the service says so")
	s.SetServing(true)
	resp, err = hc.Check(t.Context(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())

	err = conn.Invoke(t.Context(), "/"+testService+"/Panic", &durationpb.Duration{}, &emptypb.Empty{})
	require.Equal(t, codes.Internal, status.Code(err))
	require.NotContains(t, err.Error(), "boom", "panic details stay server-side")

	var remaining durationpb.Duration
	require.NoError(t, conn.Invoke(context.Background(), "/"+testService+"/Remaining", &durationpb.Duration{}, &remaining)) //nolint:usetesting // deliberately no deadline
	require.Positive(t, remaining.AsDuration(), "default deadline injected when the client sets none")
	require.LessOrEqual(t, remaining.AsDuration(), 3*time.Second)

	require.NoError(t, sleep(t.Context(), conn, 0))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	codesSeen := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "grpc_server_handled_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				method, _ := dp.Attributes.Value(attribute.Key("grpc_method"))
				code, _ := dp.Attributes.Value(attribute.Key("grpc_code"))
				codesSeen[method.AsString()+"/"+code.AsString()] += dp.Value
			}
		}
	}
	require.Equal(t, int64(1), codesSeen["Panic/Internal"], "%v", codesSeen)
	require.Equal(t, int64(1), codesSeen["Sleep/OK"])
	require.NotEmpty(t, exporter.GetSpans())

	require.NoError(t, s.Shutdown(t.Context()))
	require.NoError(t, <-runErr)
}

func TestServer_ShutdownMarksNotServingFirst(t *testing.T) {
	t.Parallel()
	s, conn, runErr := start(t)
	s.SetServing(true)
	hc := healthpb.NewHealthClient(conn)
	stream, err := hc.Watch(t.Context(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	first, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, first.GetStatus())

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- s.Shutdown(ctx) }()

	second, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, second.GetStatus(), "watchers learn about the drain")
	// The watch stream is long-lived, so graceful stop cannot finish; the deadline must cut it.
	err = <-shutdownErr
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, <-runErr)
}

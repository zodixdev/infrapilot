package grpcserver

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func standardInterceptors(logger *slog.Logger, mp metric.MeterProvider, defaultTimeout time.Duration) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor, error) {
	m := mp.Meter("github.com/zodixdev/infrapilot/pkg/grpcserver")
	handled, err := m.Int64Counter("grpc_server_handled_total", metric.WithDescription("RPCs completed, by code"))
	if err != nil {
		return nil, nil, fmt.Errorf("grpcserver: create counter: %w", err)
	}
	dur, err := m.Float64Histogram("grpc_server_handling_seconds",
		metric.WithDescription("RPC latency"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	if err != nil {
		return nil, nil, fmt.Errorf("grpcserver: create histogram: %w", err)
	}
	inflight, err := m.Int64UpDownCounter("grpc_server_in_flight", metric.WithDescription("In-flight RPCs"))
	if err != nil {
		return nil, nil, fmt.Errorf("grpcserver: create gauge: %w", err)
	}

	observe := func(ctx context.Context, fullMethod string, start time.Time, err error) {
		svc, method := splitMethod(fullMethod)
		code := status.Code(err)
		attrs := metric.WithAttributes(
			attribute.String("grpc_service", svc),
			attribute.String("grpc_method", method),
			attribute.String("grpc_code", code.String()),
		)
		handled.Add(ctx, 1, attrs)
		dur.Record(ctx, time.Since(start).Seconds(), attrs)
		level := slog.LevelInfo
		switch {
		case code == codes.Internal || code == codes.Unknown || code == codes.DataLoss || code == codes.Unavailable:
			level = slog.LevelWarn
		case svc == "grpc.health.v1.Health":
			level = slog.LevelDebug
		}
		logger.LogAttrs(ctx, level, "grpc request",
			slog.String("grpc_service", svc), slog.String("grpc_method", method),
			slog.String("grpc_code", code.String()), slog.Duration("duration", time.Since(start)))
	}

	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		start := time.Now()
		inflight.Add(ctx, 1)
		defer inflight.Add(ctx, -1)
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
			defer cancel()
		}
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ctx, "panic in grpc handler", slog.String("grpc_method", info.FullMethod),
					slog.Any("panic", fmt.Sprint(r)), slog.String("stack", string(debug.Stack())))
				err = status.Error(codes.Internal, "internal error")
				resp = nil
			}
			observe(ctx, info.FullMethod, start, err)
		}()
		return handler(ctx, req)
	}

	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		start := time.Now()
		ctx := ss.Context()
		inflight.Add(ctx, 1)
		defer inflight.Add(ctx, -1)
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ctx, "panic in grpc stream handler", slog.String("grpc_method", info.FullMethod),
					slog.Any("panic", fmt.Sprint(r)), slog.String("stack", string(debug.Stack())))
				err = status.Error(codes.Internal, "internal error")
			}
			observe(ctx, info.FullMethod, start, err)
		}()
		return handler(srv, ss)
	}
	return []grpc.UnaryServerInterceptor{unary}, []grpc.StreamServerInterceptor{stream}, nil
}

func splitMethod(full string) (service, method string) {
	full = strings.TrimPrefix(full, "/")
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "unknown", full
}

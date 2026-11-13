package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
)

// TestToStatus maps service-layer errors to gRPC codes. Statement timeouts (57014) become DEADLINE_EXCEEDED, not
// UNAVAILABLE: clients retry UNAVAILABLE, and retrying statements that time out on a slow database only adds load.
func TestToStatus(t *testing.T) {
	defer goleak.VerifyNone(t)
	wrap := func(err error) error { return fmt.Errorf("create api key: %w", err) }
	tests := []struct {
		name string
		err  error
		code codes.Code
		msg  string
	}{
		{"nil", nil, codes.OK, ""},
		{"statement timeout", wrap(&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}), codes.DeadlineExceeded, "request took too long, retry later"},
		{"lock timeout", wrap(&pgconn.PgError{Code: "55P03"}), codes.Unavailable, "temporarily unavailable, retry later"},
		{"serialization failure", wrap(&pgconn.PgError{Code: "40001"}), codes.Unavailable, "temporarily unavailable, retry later"},
		{"deadlock", wrap(&pgconn.PgError{Code: "40P01"}), codes.Unavailable, "temporarily unavailable, retry later"},
		{"admin shutdown", wrap(&pgconn.PgError{Code: "57P01"}), codes.Unavailable, "temporarily unavailable, retry later"},
		{"connection exception", wrap(&pgconn.PgError{Code: "08006"}), codes.Unavailable, "temporarily unavailable, retry later"},
		{"closed connection", wrap(net.ErrClosed), codes.Unavailable, "temporarily unavailable, retry later"},
		{"other database error", wrap(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"secret\""}), codes.Internal, "internal error"},
		{"context deadline", wrap(context.DeadlineExceeded), codes.DeadlineExceeded, "deadline exceeded"},
		{"context canceled", wrap(context.Canceled), codes.Canceled, "request canceled"},
		{"not found", domain.NotFoundf("project %s", "p1"), codes.NotFound, "project p1"},
		{"permission denied hides details", domain.Deniedf("role viewer lacks apikey.manage"), codes.PermissionDenied, "insufficient permissions"},
		{"conflict", domain.Conflictf("slug taken"), codes.AlreadyExists, "slug taken"},
		{"idempotency key reused", wrap(domain.ErrIdempotencyKeyReused), codes.AlreadyExists, "the Idempotency-Key was already used with a different request"},
		{"existing status passes through", status.Error(codes.ResourceExhausted, "slow down"), codes.ResourceExhausted, "slow down"},
		{"unknown error hides internals", errors.New("pq: password=hunter2"), codes.Internal, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, ok := status.FromError(toStatus(t.Context(), slog.New(slog.DiscardHandler), tt.err))
			require.True(t, ok)
			require.Equal(t, tt.code, st.Code())
			require.Equal(t, tt.msg, st.Message())
		})
	}
}

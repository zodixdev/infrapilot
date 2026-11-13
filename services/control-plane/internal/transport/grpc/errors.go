// Package grpc exposes the control-plane use cases over gRPC (ADR-0003): it authenticates the edge principal
// (ADR-0025), converts between protobuf and domain types and maps domain errors to status codes. It holds no
// business rules.
package grpc

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
)

// ErrorReasonIdempotencyKeyReused is the google.rpc.ErrorInfo reason for ADR-0024 mismatches.
const ErrorReasonIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"

// ErrorDomain qualifies ErrorInfo reasons.
const ErrorDomain = "controlplane.infrapilot.io"

// toStatus maps an error from the service layer to a gRPC status. Domain errors carry caller-safe messages;
// everything else is logged and returned without internals. Database unavailability (F01) becomes UNAVAILABLE
// so the edge answers 503 quickly and clients may retry.
func toStatus(ctx context.Context, logger *slog.Logger, err error) error {
	if err == nil {
		return nil
	}
	var ve *domain.ValidationError
	switch {
	case errors.As(err, &ve):
		st := status.New(codes.InvalidArgument, "request validation failed")
		br := &errdetails.BadRequest{}
		for _, v := range ve.Violations {
			br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{Field: v.Field, Description: v.Message})
		}
		if d, dErr := st.WithDetails(br); dErr == nil {
			st = d
		}
		return st.Err()
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		st := status.New(codes.AlreadyExists, "the Idempotency-Key was already used with a different request")
		if d, dErr := st.WithDetails(&errdetails.ErrorInfo{Reason: ErrorReasonIdempotencyKeyReused, Domain: ErrorDomain}); dErr == nil {
			st = d
		}
		return st.Err()
	case errors.Is(err, domain.ErrNotFound):
		return status.Error(codes.NotFound, safeMessage(err, domain.ErrNotFound))
	case errors.Is(err, domain.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, safeMessage(err, domain.ErrPermissionDenied))
	case errors.Is(err, domain.ErrUnauthenticated):
		return status.Error(codes.Unauthenticated, "authentication required")
	case errors.Is(err, domain.ErrConflict):
		return status.Error(codes.AlreadyExists, safeMessage(err, domain.ErrConflict))
	case errors.Is(err, domain.ErrFailedPrecondition):
		return status.Error(codes.FailedPrecondition, safeMessage(err, domain.ErrFailedPrecondition))
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case unavailable(err):
		logger.WarnContext(ctx, "database unavailable", slog.Any("error", err))
		return status.Error(codes.Unavailable, "temporarily unavailable, retry later")
	}
	if s, ok := status.FromError(err); ok {
		return s.Err()
	}
	logger.ErrorContext(ctx, "internal error", slog.Any("error", err))
	return status.Error(codes.Internal, "internal error")
}

// safeMessage returns the domain message without the sentinel suffix ("project x: not found" → "project x").
func safeMessage(err, sentinel error) string {
	msg := err.Error()
	suffix := ": " + sentinel.Error()
	if len(msg) > len(suffix) && msg[len(msg)-len(suffix):] == suffix {
		return msg[:len(msg)-len(suffix)]
	}
	return sentinel.Error()
}

// unavailable reports transient database failures: connection errors, server shutdown/startup, lock or
// serialization conflicts. Retrying the whole request later is safe for these.
func unavailable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case len(pgErr.Code) == 5 && pgErr.Code[:2] == "08": // connection exceptions
			return true
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03": // admin/crash shutdown, cannot connect now
			return true
		case pgErr.Code == "55P03", pgErr.Code == "40001", pgErr.Code == "40P01": // lock timeout, serialization, deadlock
			return true
		case pgErr.Code == "57014": // statement timeout
			return true
		}
		return false
	}
	var connErr *pgconn.ConnectError
	var netErr net.Error
	return errors.As(err, &connErr) || errors.As(err, &netErr) || pgconn.SafeToRetry(err) || pgconn.Timeout(err) ||
		errors.Is(err, net.ErrClosed)
}

package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zodixdev/infrapilot/pkg/httpserver"
)

// Stable problem codes (contracts/openapi/public.yaml, Problem.code).
const (
	codeValidation   = "validation_failed"
	codeUnauth       = "unauthenticated"
	codeDenied       = "permission_denied"
	codeNotFound     = "not_found"
	codeConflict     = "conflict"
	codeIdemReused   = "idempotency_key_reused"
	codeRateLimited  = "rate_limited"
	codeInternal     = "internal"
	codeUnavailable  = "unavailable"
	idemReasonUpstrm = "IDEMPOTENCY_KEY_REUSED" // google.rpc.ErrorInfo reason set by control-plane
)

func problem(w http.ResponseWriter, r *http.Request, httpStatus int, code, title string) {
	if httpStatus == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	httpserver.WriteProblemBody(w, r, httpserver.Problem{Status: httpStatus, Code: code, Title: title})
}

// upstreamError maps a gRPC error from an internal service to problem+json (the only place status codes are
// translated, AGENTS.md §5). Messages of client errors are the service's caller-safe text; internals are logged.
func upstreamError(logger *slog.Logger) func(w http.ResponseWriter, r *http.Request, err error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		st, ok := status.FromError(err)
		if !ok {
			st = status.FromContextError(err)
			if st.Code() == codes.Unknown {
				logger.ErrorContext(r.Context(), "handler error", slog.Any("error", err))
				problem(w, r, http.StatusInternalServerError, codeInternal, "internal error")
				return
			}
		}
		switch st.Code() {
		case codes.InvalidArgument:
			p := httpserver.Problem{Status: http.StatusBadRequest, Code: codeValidation, Title: "request validation failed"}
			for _, d := range st.Details() {
				if br, ok := d.(*errdetails.BadRequest); ok {
					for _, v := range br.GetFieldViolations() {
						p.Errors = append(p.Errors, httpserver.FieldError{Field: v.GetField(), Message: v.GetDescription()})
					}
				}
			}
			httpserver.WriteProblemBody(w, r, p)
		case codes.NotFound:
			problem(w, r, http.StatusNotFound, codeNotFound, "not found")
		case codes.PermissionDenied:
			problem(w, r, http.StatusForbidden, codeDenied, st.Message())
		case codes.Unauthenticated:
			problem(w, r, http.StatusUnauthorized, codeUnauth, "authentication required")
		case codes.AlreadyExists:
			for _, d := range st.Details() {
				if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetReason() == idemReasonUpstrm {
					problem(w, r, http.StatusUnprocessableEntity, codeIdemReused, "the Idempotency-Key was already used with a different request")
					return
				}
			}
			problem(w, r, http.StatusConflict, codeConflict, st.Message())
		case codes.FailedPrecondition:
			problem(w, r, http.StatusConflict, codeConflict, st.Message())
		case codes.ResourceExhausted:
			problem(w, r, http.StatusTooManyRequests, codeRateLimited, "too many requests")
		case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted, codes.Canceled:
			logger.WarnContext(r.Context(), "upstream unavailable", slog.String("grpc_code", st.Code().String()))
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "temporarily unavailable, retry later")
		default:
			logger.ErrorContext(r.Context(), "upstream error", slog.String("grpc_code", st.Code().String()), slog.String("message", st.Message()))
			problem(w, r, http.StatusInternalServerError, codeInternal, "internal error")
		}
	}
}

// errNotAuthenticated / errWrongCredential are returned by the validator's AuthenticationFunc.
var (
	errNotAuthenticated = errors.New("authentication required")
	errWrongCredential  = errors.New("operation does not accept this kind of credential")
)

// validationError maps request-validation failures (unknown route, schema violations, missing credentials) to
// problem+json with field-level details.
func validationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, routers.ErrPathNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no such endpoint")
		return
	case errors.Is(err, routers.ErrMethodNotAllowed):
		problem(w, r, http.StatusMethodNotAllowed, codeNotFound, "method not allowed")
		return
	}
	var secErr *openapi3filter.SecurityRequirementsError
	if errors.As(err, &secErr) {
		for _, e := range secErr.Errors {
			if errors.Is(e, errWrongCredential) {
				problem(w, r, http.StatusForbidden, codeDenied, "this operation does not accept this kind of credential")
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="infrapilot"`)
		problem(w, r, http.StatusUnauthorized, codeUnauth, "authentication required")
		return
	}
	p := httpserver.Problem{Status: http.StatusBadRequest, Code: codeValidation, Title: "request validation failed"}
	var reqErr *openapi3filter.RequestError
	if errors.As(err, &reqErr) {
		field := "body"
		if reqErr.Parameter != nil {
			field = reqErr.Parameter.Name
		}
		msg := reqErr.Reason
		var schemaErr *openapi3.SchemaError
		if errors.As(reqErr.Err, &schemaErr) {
			if ptr := schemaErr.JSONPointer(); len(ptr) > 0 && reqErr.Parameter == nil {
				field = strings.Join(ptr, ".")
			}
			msg = schemaErr.Reason
		}
		if msg == "" {
			msg = "is invalid"
		}
		p.Errors = []httpserver.FieldError{{Field: field, Message: msg}}
	} else {
		p.Detail = "request does not match the API contract"
	}
	httpserver.WriteProblemBody(w, r, p)
}

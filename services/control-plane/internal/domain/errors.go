// Package domain holds control-plane business rules without I/O: roles and permissions (ADR-0022), membership
// invariants, validation, API key material and pagination tokens.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Error kinds. Transports map them to gRPC/HTTP status at the edge only (AGENTS.md §5).
var (
	ErrNotFound           = errors.New("not found")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrUnauthenticated    = errors.New("unauthenticated")
	ErrConflict           = errors.New("conflict")
	ErrFailedPrecondition = errors.New("failed precondition")
	// ErrIdempotencyKeyReused: the Idempotency-Key was used before with a different request (ADR-0024).
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different request")
)

// FieldViolation describes one invalid input field.
type FieldViolation struct {
	Field   string
	Message string
}

// ValidationError lists every invalid field of a request.
type ValidationError struct {
	Violations []FieldViolation
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, v.Field+": "+v.Message)
	}
	return "invalid argument: " + strings.Join(parts, "; ")
}

// Validator accumulates violations; Err returns nil when there are none.
type Validator struct {
	violations []FieldViolation
}

// Add records a violation.
func (v *Validator) Add(field, format string, args ...any) {
	v.violations = append(v.violations, FieldViolation{Field: field, Message: fmt.Sprintf(format, args...)})
}

// Check records a violation when ok is false.
func (v *Validator) Check(ok bool, field, format string, args ...any) {
	if !ok {
		v.Add(field, format, args...)
	}
}

// Err returns a *ValidationError or nil.
func (v *Validator) Err() error {
	if len(v.violations) == 0 {
		return nil
	}
	return &ValidationError{Violations: v.violations}
}

// NotFoundf wraps ErrNotFound with a message safe to show to the caller.
func NotFoundf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrNotFound)
}

// Preconditionf wraps ErrFailedPrecondition.
func Preconditionf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrFailedPrecondition)
}

// Deniedf wraps ErrPermissionDenied.
func Deniedf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrPermissionDenied)
}

// Conflictf wraps ErrConflict.
func Conflictf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrConflict)
}

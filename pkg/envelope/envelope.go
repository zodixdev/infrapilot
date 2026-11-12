// Package envelope builds and validates the infrapilot.events.v1.Envelope that wraps every
// message on Kafka and RabbitMQ (ADR-0006, contracts/events/README.md).
package envelope

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/events/v1"
)

// ErrInvalid is wrapped by every validation error returned from this package.
var ErrInvalid = errors.New("invalid envelope")

// eventTypePattern: "<domain>.<entity>.<past-tense-verb>" for events, "<domain>.<verb>" for tasks.
var eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*){1,3}$`)

const traceParentKey = "traceparent"

// Params describes the message to wrap.
type Params struct {
	// Type is the event or task type, e.g. "incident.incident.opened" or "notifications.deliver".
	Type string
	// SchemaVersion of the payload message; must be >= 1.
	SchemaVersion uint32
	// OrganizationID of the owning tenant (UUID). Empty only for platform-level messages.
	OrganizationID string
	// SubjectID identifies the entity the message is about.
	SubjectID string
	// OccurredAt is when the fact happened; zero means "now".
	OccurredAt time.Time
	// Producer is the producing service name.
	Producer string
	// Payload is the typed message.
	Payload proto.Message
}

// Option customizes New. Options exist for tests and for replaying historical events.
type Option func(*options)

type options struct {
	now func() time.Time
	id  string
}

// WithClock overrides the clock used for produced_at (and occurred_at when unset).
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// WithID sets an explicit event id instead of generating a UUIDv7.
func WithID(id string) Option { return func(o *options) { o.id = id } }

// New builds a validated Envelope: UUIDv7 event id, produced_at from the clock, payload packed as Any,
// and trace_parent taken from the span in ctx (empty when there is none).
func New(ctx context.Context, p Params, opts ...Option) (*eventsv1.Envelope, error) {
	o := options{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	if p.Payload == nil {
		return nil, fmt.Errorf("%w: payload is required", ErrInvalid)
	}
	id := o.id
	if id == "" {
		u, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("generate event id: %w", err)
		}
		id = u.String()
	}
	payload, err := anypb.New(p.Payload)
	if err != nil {
		return nil, fmt.Errorf("pack payload: %w", err)
	}
	now := o.now()
	occurred := p.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	env := &eventsv1.Envelope{
		EventId:        id,
		EventType:      p.Type,
		SchemaVersion:  p.SchemaVersion,
		OrganizationId: p.OrganizationID,
		SubjectId:      p.SubjectID,
		OccurredAt:     timestamppb.New(occurred),
		ProducedAt:     timestamppb.New(now),
		Producer:       p.Producer,
		TraceParent:    traceParentFromContext(ctx),
		Payload:        payload,
	}
	if err := Validate(env); err != nil {
		return nil, err
	}
	return env, nil
}

// Validate checks the invariants every consumer may rely on.
func Validate(e *eventsv1.Envelope) error {
	if e == nil {
		return fmt.Errorf("%w: nil", ErrInvalid)
	}
	if _, err := uuid.Parse(e.GetEventId()); err != nil {
		return fmt.Errorf("%w: event_id %q is not a UUID", ErrInvalid, e.GetEventId())
	}
	if !eventTypePattern.MatchString(e.GetEventType()) {
		return fmt.Errorf("%w: event_type %q does not match %s", ErrInvalid, e.GetEventType(), eventTypePattern)
	}
	if e.GetSchemaVersion() == 0 {
		return fmt.Errorf("%w: schema_version must be >= 1", ErrInvalid)
	}
	if org := e.GetOrganizationId(); org != "" {
		if _, err := uuid.Parse(org); err != nil {
			return fmt.Errorf("%w: organization_id %q is not a UUID", ErrInvalid, org)
		}
	}
	if e.GetProducer() == "" {
		return fmt.Errorf("%w: producer is required", ErrInvalid)
	}
	if e.GetOccurredAt() == nil || e.GetProducedAt() == nil {
		return fmt.Errorf("%w: occurred_at and produced_at are required", ErrInvalid)
	}
	if e.GetPayload() == nil || e.GetPayload().GetTypeUrl() == "" {
		return fmt.Errorf("%w: payload is required", ErrInvalid)
	}
	return nil
}

// Unpack decodes the payload into dst and fails when the Any type does not match.
func Unpack(e *eventsv1.Envelope, dst proto.Message) error {
	if e.GetPayload() == nil {
		return fmt.Errorf("%w: payload is required", ErrInvalid)
	}
	if err := e.GetPayload().UnmarshalTo(dst); err != nil {
		return fmt.Errorf("%w: unpack payload: %w", ErrInvalid, err)
	}
	return nil
}

// ContextWithTrace returns ctx carrying the remote span context from trace_parent, so a consumer's
// spans become children of the producer's span. ctx is returned unchanged when trace_parent is empty.
func ContextWithTrace(ctx context.Context, e *eventsv1.Envelope) context.Context {
	if e.GetTraceParent() == "" {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{traceParentKey: e.GetTraceParent()})
}

func traceParentFromContext(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier[traceParentKey]
}

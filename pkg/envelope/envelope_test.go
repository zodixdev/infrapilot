package envelope_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	eventsv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/events/v1"
	"github.com/zodixdev/infrapilot/pkg/envelope"
)

func validParams() envelope.Params {
	return envelope.Params{
		Type:           "incident.incident.opened",
		SchemaVersion:  1,
		OrganizationID: "0192f7a0-3c1e-7c9a-9d41-9a1f0f3b2c11",
		SubjectID:      "incident-1",
		Producer:       "incident-engine",
		Payload:        wrapperspb.String("hello"),
	}
}

func TestNew_BuildsValidEnvelope(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	env, err := envelope.New(t.Context(), validParams(), envelope.WithClock(func() time.Time { return now }))
	require.NoError(t, err)

	id, err := uuid.Parse(env.GetEventId())
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), id.Version(), "event ids are UUIDv7")
	require.Equal(t, now, env.GetProducedAt().AsTime())
	require.Equal(t, now, env.GetOccurredAt().AsTime(), "occurred_at defaults to the clock")
	require.Empty(t, env.GetTraceParent(), "no span in context → no trace parent")
	require.NoError(t, envelope.Validate(env))

	var got wrapperspb.StringValue
	require.NoError(t, envelope.Unpack(env, &got))
	require.Equal(t, "hello", got.GetValue())
}

func TestNew_IDsAreTimeOrdered(t *testing.T) {
	t.Parallel()
	a, err := envelope.New(t.Context(), validParams())
	require.NoError(t, err)
	b, err := envelope.New(t.Context(), validParams())
	require.NoError(t, err)
	require.Less(t, a.GetEventId(), b.GetEventId())
}

func TestNew_PropagatesTraceParent(t *testing.T) {
	t.Parallel()
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(t.Context(), sc)

	env, err := envelope.New(ctx, validParams())
	require.NoError(t, err)
	require.Equal(t, "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01", env.GetTraceParent())

	consumerCtx := envelope.ContextWithTrace(t.Context(), env)
	got := trace.SpanContextFromContext(consumerCtx)
	require.True(t, got.IsRemote())
	require.Equal(t, sc.TraceID(), got.TraceID())
	require.Equal(t, sc.SpanID(), got.SpanID())
}

func TestNew_Rejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*envelope.Params)
		opts   []envelope.Option
		want   string
	}{
		{"nil payload", func(p *envelope.Params) { p.Payload = nil }, nil, "payload is required"},
		{"bad type", func(p *envelope.Params) { p.Type = "Incident.Opened" }, nil, "event_type"},
		{"single segment type", func(p *envelope.Params) { p.Type = "incident" }, nil, "event_type"},
		{"zero schema version", func(p *envelope.Params) { p.SchemaVersion = 0 }, nil, "schema_version"},
		{"bad org id", func(p *envelope.Params) { p.OrganizationID = "acme" }, nil, "organization_id"},
		{"missing producer", func(p *envelope.Params) { p.Producer = "" }, nil, "producer"},
		{"bad explicit id", func(*envelope.Params) {}, []envelope.Option{envelope.WithID("not-a-uuid")}, "event_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := validParams()
			tt.mutate(&p)
			_, err := envelope.New(t.Context(), p, tt.opts...)
			require.ErrorIs(t, err, envelope.ErrInvalid)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestNew_AllowsPlatformEventsWithoutOrganization(t *testing.T) {
	t.Parallel()
	p := validParams()
	p.OrganizationID = ""
	p.Type = "platform.maintenance.started"
	_, err := envelope.New(t.Context(), p)
	require.NoError(t, err)
}

func TestValidate_Rejects(t *testing.T) {
	t.Parallel()
	require.ErrorIs(t, envelope.Validate(nil), envelope.ErrInvalid)

	env, err := envelope.New(t.Context(), validParams())
	require.NoError(t, err)
	env.Payload = nil
	require.ErrorContains(t, envelope.Validate(env), "payload")

	env, err = envelope.New(t.Context(), validParams())
	require.NoError(t, err)
	env.OccurredAt = nil
	require.ErrorContains(t, envelope.Validate(env), "occurred_at")
}

func TestUnpack_TypeMismatch(t *testing.T) {
	t.Parallel()
	env, err := envelope.New(t.Context(), validParams())
	require.NoError(t, err)
	err = envelope.Unpack(env, &timestamppb.Timestamp{})
	require.ErrorIs(t, err, envelope.ErrInvalid)

	require.ErrorIs(t, envelope.Unpack(&eventsv1.Envelope{}, &wrapperspb.StringValue{}), envelope.ErrInvalid)
}

package postgres

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// registerMetrics exposes pgxpool.Stat as gauges/counters (names follow the pgxpool collector convention).
func (p *Pool) registerMetrics(mp metric.MeterProvider) error {
	m := mp.Meter("github.com/zodixdev/infrapilot/pkg/postgres")
	var (
		total, idle, acquired, constructing, maxConns metric.Int64ObservableGauge
		acquireCount, emptyAcquire, canceled          metric.Int64ObservableCounter
		acquireSeconds                                metric.Float64ObservableCounter
		err                                           error
	)
	mk := func(g *metric.Int64ObservableGauge, name, desc string) {
		if err != nil {
			return
		}
		*g, err = m.Int64ObservableGauge(name, metric.WithDescription(desc))
	}
	mk(&total, "pgxpool_total_conns", "Total connections in the pool")
	mk(&idle, "pgxpool_idle_conns", "Idle connections")
	mk(&acquired, "pgxpool_acquired_conns", "Connections currently acquired")
	mk(&constructing, "pgxpool_constructing_conns", "Connections being established")
	mk(&maxConns, "pgxpool_max_conns", "Configured maximum connections")
	if err == nil {
		acquireCount, err = m.Int64ObservableCounter("pgxpool_acquire_total", metric.WithDescription("Successful acquires"))
	}
	if err == nil {
		emptyAcquire, err = m.Int64ObservableCounter("pgxpool_empty_acquire_total", metric.WithDescription("Acquires that had to wait for a connection"))
	}
	if err == nil {
		canceled, err = m.Int64ObservableCounter("pgxpool_canceled_acquire_total", metric.WithDescription("Acquires canceled by context"))
	}
	if err == nil {
		acquireSeconds, err = m.Float64ObservableCounter("pgxpool_acquire_duration_seconds_total", metric.WithDescription("Total time spent acquiring"), metric.WithUnit("s"))
	}
	if err != nil {
		return fmt.Errorf("postgres: create pool metrics: %w", err)
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := p.Stat()
		o.ObserveInt64(total, int64(s.TotalConns()))
		o.ObserveInt64(idle, int64(s.IdleConns()))
		o.ObserveInt64(acquired, int64(s.AcquiredConns()))
		o.ObserveInt64(constructing, int64(s.ConstructingConns()))
		o.ObserveInt64(maxConns, int64(s.MaxConns()))
		o.ObserveInt64(acquireCount, s.AcquireCount())
		o.ObserveInt64(emptyAcquire, s.EmptyAcquireCount())
		o.ObserveInt64(canceled, s.CanceledAcquireCount())
		o.ObserveFloat64(acquireSeconds, s.AcquireDuration().Seconds())
		return nil
	}, total, idle, acquired, constructing, maxConns, acquireCount, emptyAcquire, canceled, acquireSeconds)
	if err != nil {
		return fmt.Errorf("postgres: register pool metrics callback: %w", err)
	}
	return nil
}

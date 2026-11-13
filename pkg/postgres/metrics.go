package postgres

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// registerMetrics exposes pgxpool.Stat as gauges/counters labelled with the pool name (application
// name, falling back to the role), so app and batch pools in one process stay distinguishable.
func (p *Pool) registerMetrics(mp metric.MeterProvider) error {
	m := mp.Meter("github.com/zodixdev/infrapilot/pkg/postgres")
	name := p.cfg.ApplicationName
	if name == "" {
		name = p.pool.Config().ConnConfig.User
	}
	attrs := metric.WithAttributes(attribute.String("pool", name))
	var (
		total, idle, acquired, constructing, maxConns metric.Int64ObservableGauge
		acquireCount, emptyAcquire, canceled          metric.Int64ObservableCounter
		newConns, lifetimeDestroy, idleDestroy        metric.Int64ObservableCounter
		acquireSeconds                                metric.Float64ObservableCounter
		err                                           error
	)
	gauge := func(g *metric.Int64ObservableGauge, n, desc string) {
		if err == nil {
			*g, err = m.Int64ObservableGauge(n, metric.WithDescription(desc))
		}
	}
	counter := func(c *metric.Int64ObservableCounter, n, desc string) {
		if err == nil {
			*c, err = m.Int64ObservableCounter(n, metric.WithDescription(desc))
		}
	}
	gauge(&total, "pgxpool_total_conns", "Total connections in the pool")
	gauge(&idle, "pgxpool_idle_conns", "Idle connections")
	gauge(&acquired, "pgxpool_acquired_conns", "Connections currently acquired")
	gauge(&constructing, "pgxpool_constructing_conns", "Connections being established")
	gauge(&maxConns, "pgxpool_max_conns", "Configured maximum connections")
	counter(&acquireCount, "pgxpool_acquire_total", "Successful acquires")
	counter(&emptyAcquire, "pgxpool_empty_acquire_total", "Acquires that had to wait for a connection")
	counter(&canceled, "pgxpool_canceled_acquire_total", "Acquires canceled by context")
	counter(&newConns, "pgxpool_new_conns_total", "Connections opened")
	counter(&lifetimeDestroy, "pgxpool_max_lifetime_destroy_total", "Connections closed for exceeding MaxConnLifetime")
	counter(&idleDestroy, "pgxpool_max_idle_destroy_total", "Connections closed for exceeding MaxConnIdleTime")
	if err == nil {
		acquireSeconds, err = m.Float64ObservableCounter("pgxpool_acquire_duration_seconds_total",
			metric.WithDescription("Total time spent acquiring"), metric.WithUnit("s"))
	}
	if err != nil {
		return fmt.Errorf("postgres: create pool metrics: %w", err)
	}
	p.metrics, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := p.pool.Stat()
		o.ObserveInt64(total, int64(s.TotalConns()), attrs)
		o.ObserveInt64(idle, int64(s.IdleConns()), attrs)
		o.ObserveInt64(acquired, int64(s.AcquiredConns()), attrs)
		o.ObserveInt64(constructing, int64(s.ConstructingConns()), attrs)
		o.ObserveInt64(maxConns, int64(s.MaxConns()), attrs)
		o.ObserveInt64(acquireCount, s.AcquireCount(), attrs)
		o.ObserveInt64(emptyAcquire, s.EmptyAcquireCount(), attrs)
		o.ObserveInt64(canceled, s.CanceledAcquireCount(), attrs)
		o.ObserveInt64(newConns, s.NewConnsCount(), attrs)
		o.ObserveInt64(lifetimeDestroy, s.MaxLifetimeDestroyCount(), attrs)
		o.ObserveInt64(idleDestroy, s.MaxIdleDestroyCount(), attrs)
		o.ObserveFloat64(acquireSeconds, s.AcquireDuration().Seconds(), attrs)
		return nil
	}, total, idle, acquired, constructing, maxConns, acquireCount, emptyAcquire, canceled,
		newConns, lifetimeDestroy, idleDestroy, acquireSeconds)
	if err != nil {
		return fmt.Errorf("postgres: register pool metrics callback: %w", err)
	}
	return nil
}

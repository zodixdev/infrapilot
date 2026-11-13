// Package app runs a service's components with ordered startup and graceful, deadline-bounded shutdown
// (AGENTS.md §5: readiness → false, stop intake, drain with deadline, close clients in reverse order).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

// Component is a long-running part of a service (listener, consumer, background loop, resource).
//
// Run blocks until ctx is cancelled, Shutdown has completed, or a fatal error occurs; it must return nil
// after a clean stop. Shutdown asks the component to stop accepting work and drain within ctx's deadline.
// After Shutdown the App cancels the component's Run context and waits for Run to return before stopping
// the next component, so a component never outlives the resources added before it.
type Component interface {
	Name() string
	Run(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// ReadinessGate is flipped by the App: true once every component has started, false as the very first step
// of shutdown. health.Registry satisfies it.
type ReadinessGate interface {
	SetReady(bool)
}

// Config controls shutdown timing.
type Config struct {
	// ShutdownDelay keeps serving after readiness turns false so that load balancers and kube-proxy stop
	// routing new traffic before listeners close. Applied only on signal-initiated shutdown.
	ShutdownDelay time.Duration
	// ShutdownTimeout bounds the drain of all serving components combined.
	ShutdownTimeout time.Duration
	// CloseTimeout is a separate budget for resources added with Closer (pools, telemetry flush), so a drain
	// that used its whole deadline cannot starve them (default 5s; the Helm grace period reserves it).
	CloseTimeout time.Duration
}

// App orchestrates components.
type App struct {
	cfg        Config
	log        *slog.Logger
	gate       ReadinessGate
	components []Component
	signals    []os.Signal
}

// Option configures the App.
type Option func(*App)

// WithLogger sets the logger (default: discard).
func WithLogger(l *slog.Logger) Option { return func(a *App) { a.log = l } }

// WithReadinessGate sets the gate flipped around startup/shutdown.
func WithReadinessGate(g ReadinessGate) Option { return func(a *App) { a.gate = g } }

// WithSignals overrides the termination signals (default SIGINT, SIGTERM). Empty disables signal handling.
func WithSignals(sig ...os.Signal) Option { return func(a *App) { a.signals = sig } }

// New creates an App. Components are started in the order added and stopped in reverse.
func New(cfg Config, opts ...Option) (*App, error) {
	if cfg.ShutdownTimeout <= 0 {
		return nil, errors.New("app: ShutdownTimeout must be positive")
	}
	if cfg.ShutdownDelay < 0 || cfg.CloseTimeout < 0 {
		return nil, errors.New("app: ShutdownDelay and CloseTimeout must not be negative")
	}
	if cfg.CloseTimeout == 0 {
		cfg.CloseTimeout = 5 * time.Second
	}
	a := &App{
		cfg:     cfg,
		log:     slog.New(slog.DiscardHandler),
		signals: []os.Signal{syscall.SIGINT, syscall.SIGTERM},
	}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Add registers components in start order.
func (a *App) Add(c ...Component) { a.components = append(a.components, c...) }

// ErrShutdownTimeout is returned when a component did not stop within its budget.
var ErrShutdownTimeout = errors.New("app: shutdown deadline exceeded")

type running struct {
	c      Component
	cancel context.CancelFunc
	done   chan struct{}
	err    error // written before done is closed
}

// Run starts every component, marks the service ready, then blocks until ctx is cancelled, a termination
// signal arrives, or a component fails. It then performs the graceful shutdown sequence and returns the
// errors encountered (nil on a clean, signal-initiated stop).
func (a *App) Run(ctx context.Context) error {
	sigCtx, stopSignals := ctx, context.CancelFunc(func() {})
	if len(a.signals) > 0 {
		sigCtx, stopSignals = signal.NotifyContext(ctx, a.signals...)
	}
	defer stopSignals()

	exited := make(chan *running, len(a.components))
	rs := make([]*running, len(a.components))
	for i, c := range a.components {
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r := &running{c: c, cancel: cancel, done: make(chan struct{})}
		rs[i] = r
		go func() {
			r.err = recoverRun(runCtx, c)
			close(r.done)
			exited <- r
		}()
		a.log.InfoContext(ctx, "component started", slog.String("component", c.Name()))
	}
	if a.gate != nil {
		a.gate.SetReady(true)
	}
	a.log.InfoContext(ctx, "service ready", slog.Int("components", len(a.components)))

	var errs []error
	failed := func(r *running) {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("component %s failed: %w", r.c.Name(), r.err))
			a.log.ErrorContext(ctx, "component failed, shutting down", slog.String("component", r.c.Name()), slog.Any("error", r.err))
		} else {
			errs = append(errs, fmt.Errorf("component %s exited before shutdown", r.c.Name()))
			a.log.ErrorContext(ctx, "component exited unexpectedly", slog.String("component", r.c.Name()))
		}
	}

	signalled := false
	select {
	case <-sigCtx.Done():
		signalled = true
		a.log.InfoContext(ctx, "shutdown requested", slog.String("reason", context.Cause(sigCtx).Error()))
	case r := <-exited:
		failed(r)
	}
	// Restore default signal handling: a second SIGTERM/SIGINT now terminates the process immediately.
	stopSignals()

	// 1. Stop advertising readiness so new traffic is routed elsewhere.
	if a.gate != nil {
		a.gate.SetReady(false)
	}
	// 2. Keep serving while the endpoint is withdrawn — unless a component fails meanwhile.
	if signalled && a.cfg.ShutdownDelay > 0 {
		a.log.InfoContext(ctx, "draining endpoint", slog.Duration("delay", a.cfg.ShutdownDelay))
		timer := time.NewTimer(a.cfg.ShutdownDelay)
		select {
		case <-timer.C:
		case r := <-exited:
			timer.Stop()
			failed(r)
		}
	}
	// 3. Stop components in reverse order. Serving components share the drain deadline; resources added with
	// Closer share a separate close budget that starts when the first of them is reached.
	drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
	defer cancelDrain()
	closeBudget := sync.OnceValues(func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), a.cfg.CloseTimeout)
	})
	defer func() { _, cancel := closeBudget(); cancel() }()
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		stepCtx := drainCtx //nolint:contextcheck // both budgets derive from ctx via WithoutCancel (shutdown must outlive cancellation)
		if isResource(r.c) {
			stepCtx, _ = closeBudget()
		}
		start := time.Now()
		if err := safeShutdown(stepCtx, r.c); err != nil {
			errs = append(errs, fmt.Errorf("shutdown %s: %w", r.c.Name(), err))
			a.log.WarnContext(ctx, "component shutdown error", slog.String("component", r.c.Name()), slog.Any("error", err))
		}
		r.cancel()
		if !waitDone(stepCtx, r.done) {
			errs = append(errs, fmt.Errorf("component %s: %w", r.c.Name(), ErrShutdownTimeout))
			a.log.ErrorContext(ctx, "component did not stop in time", slog.String("component", r.c.Name()))
			continue
		}
		if r.err != nil && !errors.Is(errors.Join(errs...), r.err) {
			errs = append(errs, fmt.Errorf("component %s: %w", r.c.Name(), r.err))
		}
		a.log.InfoContext(ctx, "component stopped", slog.String("component", r.c.Name()), slog.Duration("took", time.Since(start)))
	}
	return errors.Join(errs...)
}

// waitDone reports whether done closed before ctx expired, preferring done when both are ready.
func waitDone(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

func recoverRun(ctx context.Context, c Component) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return c.Run(ctx)
}

func safeShutdown(ctx context.Context, c Component) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic during shutdown: %v\n%s", r, debug.Stack())
		}
	}()
	return c.Shutdown(ctx)
}

// Func adapts start/stop functions into a Component. run must block until ctx is done; stop may be nil.
func Func(name string, run func(ctx context.Context) error, stop func(ctx context.Context) error) Component {
	return &funcComponent{name: name, run: run, stop: stop}
}

// Closer wraps a resource that only needs closing on shutdown (DB pool, telemetry provider). Closers are
// stopped in reverse order like everything else, but with the separate CloseTimeout budget.
func Closer(name string, closeFn func(ctx context.Context) error) Component {
	return &funcComponent{name: name, run: func(ctx context.Context) error { <-ctx.Done(); return nil }, stop: closeFn, resource: true}
}

func isResource(c Component) bool {
	f, ok := c.(*funcComponent)
	return ok && f.resource
}

type funcComponent struct {
	name     string
	run      func(ctx context.Context) error
	stop     func(ctx context.Context) error
	resource bool
}

func (f *funcComponent) Name() string { return f.name }

func (f *funcComponent) Run(ctx context.Context) error { return f.run(ctx) }

func (f *funcComponent) Shutdown(ctx context.Context) error {
	if f.stop == nil {
		return nil
	}
	return f.stop(ctx)
}

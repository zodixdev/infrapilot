// Package app runs a service's components with ordered startup and graceful, deadline-bounded
// shutdown (AGENTS.md §5: readiness → false, stop intake, drain with deadline, close in reverse order).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Component is a long-running part of a service (listener, consumer, background loop, resource).
//
// Run blocks until ctx is cancelled, Shutdown has completed, or a fatal error occurs; it must return
// nil after a clean stop. Shutdown asks the component to stop accepting work and drain within ctx's
// deadline; after it returns (or ctx expires) the component must hold no goroutines.
type Component interface {
	Name() string
	Run(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// ReadinessGate is flipped by the App: true once every component has started, false as the very
// first step of shutdown. health.Registry satisfies it.
type ReadinessGate interface {
	SetReady(bool)
}

// Config controls shutdown timing.
type Config struct {
	// ShutdownDelay keeps serving after readiness turns false so that load balancers and kube-proxy
	// stop routing new traffic before listeners close. Applied only on signal-initiated shutdown.
	ShutdownDelay time.Duration
	// ShutdownTimeout bounds the drain of all components combined.
	ShutdownTimeout time.Duration
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
	if cfg.ShutdownDelay < 0 {
		return nil, errors.New("app: ShutdownDelay must not be negative")
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

// ErrShutdownTimeout is returned when components did not drain within ShutdownTimeout.
var ErrShutdownTimeout = errors.New("app: shutdown deadline exceeded")

type runResult struct {
	name string
	err  error
}

// Run starts every component, marks the service ready, then blocks until ctx is cancelled, a termination
// signal arrives, or a component fails. It then performs the graceful shutdown sequence and returns the
// first error encountered (nil on a clean, signal-initiated stop).
func (a *App) Run(ctx context.Context) error {
	if len(a.signals) > 0 {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, a.signals...)
		defer stop()
	}
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()

	results := make(chan runResult, len(a.components))
	var wg sync.WaitGroup
	for _, c := range a.components {
		wg.Add(1)
		go func(c Component) {
			defer wg.Done()
			err := recoverRun(runCtx, c)
			results <- runResult{name: c.Name(), err: err}
		}(c)
		a.log.InfoContext(ctx, "component started", slog.String("component", c.Name()))
	}
	if a.gate != nil {
		a.gate.SetReady(true)
	}
	a.log.InfoContext(ctx, "service ready", slog.Int("components", len(a.components)))

	// Wait for a stop trigger.
	var cause error
	signalled := false
	select {
	case <-ctx.Done():
		signalled = true
		a.log.InfoContext(ctx, "shutdown requested", slog.String("reason", context.Cause(ctx).Error()))
	case r := <-results:
		if r.err != nil {
			cause = fmt.Errorf("component %s failed: %w", r.name, r.err)
			a.log.ErrorContext(ctx, "component failed, shutting down", slog.String("component", r.name), slog.Any("error", r.err))
		} else {
			cause = fmt.Errorf("component %s exited before shutdown", r.name)
			a.log.ErrorContext(ctx, "component exited unexpectedly", slog.String("component", r.name))
		}
	}

	// 1. Stop advertising readiness so new traffic is routed elsewhere.
	if a.gate != nil {
		a.gate.SetReady(false)
	}
	// 2. Keep serving while the endpoint is withdrawn (only when we are healthy and were asked to stop).
	if signalled && a.cfg.ShutdownDelay > 0 {
		a.log.InfoContext(ctx, "draining endpoint", slog.Duration("delay", a.cfg.ShutdownDelay))
		timer := time.NewTimer(a.cfg.ShutdownDelay)
		<-timer.C
	}
	// 3. Drain components in reverse order within one shared deadline.
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
	defer cancelStop()
	var errs []error
	if cause != nil {
		errs = append(errs, cause)
	}
	for i := len(a.components) - 1; i >= 0; i-- {
		c := a.components[i]
		start := time.Now()
		if err := c.Shutdown(stopCtx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown %s: %w", c.Name(), err))
			a.log.WarnContext(ctx, "component shutdown error", slog.String("component", c.Name()), slog.Any("error", err))
		} else {
			a.log.InfoContext(ctx, "component stopped", slog.String("component", c.Name()), slog.Duration("took", time.Since(start)))
		}
	}
	// 4. Make Run return for components that only watch their context, then wait with the same deadline.
	cancelRun()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-stopCtx.Done():
		errs = append(errs, ErrShutdownTimeout)
		a.log.ErrorContext(ctx, "components did not drain in time", slog.Duration("timeout", a.cfg.ShutdownTimeout))
	}
	// Collect Run errors from components that finished (non-blocking drain of the channel).
	for {
		select {
		case r := <-results:
			if r.err != nil && (cause == nil || !errors.Is(cause, r.err)) {
				errs = append(errs, fmt.Errorf("component %s: %w", r.name, r.err))
			}
			continue
		default:
		}
		break
	}
	return errors.Join(errs...)
}

func recoverRun(ctx context.Context, c Component) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return c.Run(ctx)
}

// Func adapts start/stop functions into a Component. run must block until ctx is done; stop may be nil.
func Func(name string, run func(ctx context.Context) error, stop func(ctx context.Context) error) Component {
	return &funcComponent{name: name, run: run, stop: stop}
}

// Closer wraps a resource that only needs closing on shutdown (DB pool, telemetry provider).
// Closing happens at the component's position in the reverse order, after everything added later.
func Closer(name string, closeFn func(ctx context.Context) error) Component {
	return Func(name, func(ctx context.Context) error { <-ctx.Done(); return nil }, closeFn)
}

type funcComponent struct {
	name string
	run  func(ctx context.Context) error
	stop func(ctx context.Context) error
}

func (f *funcComponent) Name() string { return f.name }

func (f *funcComponent) Run(ctx context.Context) error { return f.run(ctx) }

func (f *funcComponent) Shutdown(ctx context.Context) error {
	if f.stop == nil {
		return nil
	}
	return f.stop(ctx)
}

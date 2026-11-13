// Package health implements the liveness/readiness registry behind /healthz and /readyz
// (AGENTS.md §9). Liveness only says the process is running; readiness aggregates dependency
// checks and a global gate that graceful shutdown switches off first.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/zodixdev/infrapilot/pkg/log"
)

// Status of a check or of the whole service.
type Status string

// Possible statuses.
const (
	StatusUp   Status = "up"
	StatusDown Status = "down"
)

// Check probes one dependency. It must respect ctx (a per-check timeout is applied).
type Check func(ctx context.Context) error

// Result of one check.
type Result struct {
	Status    Status        `json:"status"`
	Error     string        `json:"error,omitempty"`
	Duration  time.Duration `json:"-"`
	CheckedAt time.Time     `json:"checked_at"`
}

// MarshalJSON renders the duration in milliseconds (a time.Duration would serialize as nanoseconds).
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	return json.Marshal(struct {
		plain
		DurationMS float64 `json:"duration_ms"`
	}{plain(r), float64(r.Duration) / float64(time.Millisecond)})
}

// Report is the readiness document.
type Report struct {
	Status Status            `json:"status"`
	Checks map[string]Result `json:"checks"`
}

// Option configures the registry.
type Option func(*Registry)

// WithTimeout bounds every single check evaluation (default 2s).
func WithTimeout(d time.Duration) Option { return func(r *Registry) { r.timeout = d } }

// WithLogger reports check transitions.
func WithLogger(l *slog.Logger) Option { return func(r *Registry) { r.log = l } }

// WithMeter exports `health_check_up{check}` and `health_ready` gauges.
func WithMeter(m metric.Meter) Option { return func(r *Registry) { r.meter = m } }

// CheckOption configures one registered check.
type CheckOption func(*entry)

// Periodic evaluates the check in the background every interval (started by Run) and serves the
// cached result; use it for dependencies that must not be probed on every kubelet request.
func Periodic(interval time.Duration) CheckOption {
	return func(e *entry) { e.interval = interval }
}

type entry struct {
	name     string
	check    Check
	interval time.Duration
	// inflight is set while a check goroutine runs. A check that ignores its context therefore occupies at
	// most one goroutine: later evaluations report "still running" instead of starting another.
	inflight atomic.Bool

	mu   sync.Mutex
	last Result
	seen bool
}

// Registry is safe for concurrent use.
type Registry struct {
	timeout time.Duration
	log     *slog.Logger
	meter   metric.Meter

	mu      sync.RWMutex
	entries map[string]*entry
	ready   bool
	upGauge metric.Int64Gauge
	rdGauge metric.Int64Gauge
}

// NewRegistry creates a registry whose global gate starts closed; call SetReady(true) once startup is done.
func NewRegistry(opts ...Option) *Registry {
	r := &Registry{
		timeout: 2 * time.Second,
		log:     slog.New(slog.DiscardHandler),
		entries: make(map[string]*entry),
	}
	for _, o := range opts {
		o(r)
	}
	if r.meter != nil {
		// Failures to create instruments only disable metrics; health must keep working.
		r.upGauge, _ = r.meter.Int64Gauge("health_check_up", metric.WithDescription("1 when the dependency check passes"))
		r.rdGauge, _ = r.meter.Int64Gauge("health_ready", metric.WithDescription("1 when the service reports ready"))
	}
	return r
}

// Register adds a named check. Registering the same name twice replaces the previous check. Periodic checks
// must be registered before Run is called; Run only drives the checks it sees when it starts.
func (r *Registry) Register(name string, c Check, opts ...CheckOption) {
	e := &entry{name: name, check: c}
	for _, o := range opts {
		o(e)
	}
	r.mu.Lock()
	r.entries[name] = e
	r.mu.Unlock()
}

// SetReady opens or closes the global gate. Shutdown closes it before draining.
func (r *Registry) SetReady(ready bool) {
	r.mu.Lock()
	prev := r.ready
	r.ready = ready
	r.mu.Unlock()
	if prev != ready {
		r.log.Info("readiness gate changed", slog.Bool("ready", ready))
	}
	r.recordReady(ready)
}

// Ready reports the global gate only (not the dependency checks).
func (r *Registry) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready
}

// Readiness evaluates all checks (periodic ones from cache) and combines them with the gate.
func (r *Registry) Readiness(ctx context.Context) Report {
	r.mu.RLock()
	gate := r.ready
	entries := make([]*entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	r.mu.RUnlock()

	rep := Report{Status: StatusUp, Checks: make(map[string]Result, len(entries))}
	if !gate {
		rep.Status = StatusDown
	}
	type outcome struct {
		name string
		res  Result
	}
	results := make(chan outcome, len(entries))
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			results <- outcome{e.name, r.resultFor(ctx, e)}
		}(e)
	}
	wg.Wait()
	close(results)
	for o := range results {
		rep.Checks[o.name] = o.res
		if o.res.Status != StatusUp {
			rep.Status = StatusDown
		}
	}
	return rep
}

func (r *Registry) resultFor(ctx context.Context, e *entry) Result {
	if e.interval > 0 {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.seen {
			return e.last
		}
		// First evaluation before Run has probed: evaluate inline so readiness is never wrongly "up". The
		// request context is detached so a disconnecting client cannot cache "down: context canceled".
		res := r.evaluate(context.WithoutCancel(ctx), e)
		e.last, e.seen = res, true
		return res
	}
	return r.evaluate(ctx, e)
}

func (r *Registry) evaluate(ctx context.Context, e *entry) Result {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	start := time.Now()
	err := safeCheck(ctx, e)
	res := Result{Status: StatusUp, Duration: time.Since(start), CheckedAt: start}
	if err != nil {
		res.Status = StatusDown
		// Errors reach /readyz and logs: scrub credentials and cap the length.
		res.Error = log.Scrub(err.Error())
		if len(res.Error) > 512 {
			res.Error = res.Error[:512] + "…"
		}
	}
	r.recordCheck(ctx, e.name, res.Status == StatusUp)
	return res
}

// Run drives periodic checks until ctx is cancelled. It owns no goroutines after it returns.
// Transitions are logged; the first probe happens immediately.
func (r *Registry) Run(ctx context.Context) error {
	r.mu.RLock()
	periodic := make([]*entry, 0, len(r.entries))
	for _, e := range r.entries {
		if e.interval > 0 {
			periodic = append(periodic, e)
		}
	}
	r.mu.RUnlock()
	if len(periodic) == 0 {
		<-ctx.Done()
		return nil
	}
	var wg sync.WaitGroup
	for _, e := range periodic {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			r.probeLoop(ctx, e)
		}(e)
	}
	wg.Wait()
	return nil
}

func (r *Registry) probeLoop(ctx context.Context, e *entry) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		res := r.evaluate(ctx, e)
		e.mu.Lock()
		prev, seen := e.last, e.seen
		e.last, e.seen = res, true
		e.mu.Unlock()
		if !seen || prev.Status != res.Status {
			r.log.LogAttrs(ctx, levelFor(res.Status), "health check transition",
				slog.String("check", e.name), slog.String("status", string(res.Status)), slog.String("error", res.Error))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func levelFor(s Status) slog.Level {
	if s == StatusUp {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// errStillRunning is reported while a previous evaluation of the same check has not returned yet.
var errStillRunning = errors.New("previous evaluation still running")

func safeCheck(ctx context.Context, e *entry) error {
	if !e.inflight.CompareAndSwap(false, true) {
		return errStillRunning
	}
	done := make(chan error, 1)
	go func() {
		defer e.inflight.Store(false)
		defer func() {
			if rec := recover(); rec != nil {
				done <- errors.New("health check panicked")
			}
		}()
		done <- e.check(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// The check goroutine finishes on its own once it honours ctx; we do not wait for it so a
		// stuck dependency cannot stall readiness reporting.
		return ctx.Err()
	}
}

// LivenessHandler always returns 200 while the process serves requests.
func (r *Registry) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]Status{"status": StatusUp})
	})
}

// ReadinessHandler returns 200 when ready, 503 otherwise, with the full report as JSON.
func (r *Registry) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rep := r.Readiness(req.Context())
		code := http.StatusOK
		if rep.Status != StatusUp {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, rep)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // best effort: the status code already carries the answer
}

func (r *Registry) recordCheck(ctx context.Context, name string, up bool) {
	if r.upGauge == nil {
		return
	}
	var v int64
	if up {
		v = 1
	}
	r.upGauge.Record(context.WithoutCancel(ctx), v, metric.WithAttributes(attribute.String("check", name)))
}

func (r *Registry) recordReady(ready bool) {
	if r.rdGauge == nil {
		return
	}
	var v int64
	if ready {
		v = 1
	}
	r.rdGauge.Record(context.Background(), v)
}

// Names returns registered check names, sorted. For diagnostics and tests.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

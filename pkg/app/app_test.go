package app_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/zodixdev/infrapilot/pkg/app"
	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// recorder notes the order of lifecycle events across components.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) SetReady(ready bool) {
	if ready {
		r.add("ready=true")
	} else {
		r.add("ready=false")
	}
}

// fake is a Component whose Run blocks until Shutdown or ctx; optional failure/panic/stuck modes.
type fake struct {
	name    string
	rec     *recorder
	stopped chan struct{}
	once    sync.Once
	failAt  time.Duration
	panics  bool
	// stuck components ignore cancellation until release is closed.
	stuck   bool
	release chan struct{}
}

func newFake(name string, rec *recorder) *fake {
	return &fake{name: name, rec: rec, stopped: make(chan struct{})}
}

func (f *fake) Name() string { return f.name }

func (f *fake) Run(ctx context.Context) error {
	f.rec.add("run:" + f.name)
	if f.panics {
		panic("component exploded")
	}
	if f.failAt > 0 {
		time.Sleep(f.failAt)
		return errors.New("broken pipe")
	}
	if f.stuck {
		<-ctx.Done()
		<-f.release // ignores the drain deadline until the test releases it
		return nil
	}
	select {
	case <-f.stopped:
	case <-ctx.Done():
	}
	return nil
}

func (f *fake) Shutdown(context.Context) error {
	f.rec.add("stop:" + f.name)
	f.once.Do(func() { close(f.stopped) })
	return nil
}

func newApp(t *testing.T, rec *recorder, cfg app.Config) *app.App {
	t.Helper()
	a, err := app.New(cfg, app.WithReadinessGate(rec), app.WithSignals())
	require.NoError(t, err)
	return a
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	_, err := app.New(app.Config{})
	require.Error(t, err)
	_, err = app.New(app.Config{ShutdownTimeout: time.Second, ShutdownDelay: -1})
	require.Error(t, err)
}

func TestApp_StartsInOrderStopsInReverse(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second})
	a.Add(newFake("db", rec), newFake("grpc", rec), newFake("http", rec))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, func() bool {
		for _, e := range rec.list() {
			if e == "ready=true" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)

	events := rec.list()
	// Runs are concurrent goroutines, so only their relative position to ready matters.
	idx := func(e string) int {
		for i, x := range events {
			if x == e {
				return i
			}
		}
		return -1
	}
	require.Contains(t, events, "run:db")
	require.Contains(t, events, "run:http")
	require.Less(t, idx("ready=true"), idx("ready=false"))
	require.Less(t, idx("ready=false"), idx("stop:http"))
	require.Less(t, idx("stop:http"), idx("stop:grpc"))
	require.Less(t, idx("stop:grpc"), idx("stop:db"))
}

func TestApp_ComponentFailureTriggersShutdown(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second, ShutdownDelay: 500 * time.Millisecond})
	bad := newFake("consumer", rec)
	bad.failAt = 20 * time.Millisecond
	a.Add(newFake("http", rec), bad)

	start := time.Now()
	err := a.Run(t.Context())
	require.ErrorContains(t, err, "component consumer failed: broken pipe")
	require.Less(t, time.Since(start), 400*time.Millisecond, "no endpoint-drain delay on failure")
	require.Contains(t, rec.list(), "stop:http")
	require.Contains(t, rec.list(), "ready=false")
}

func TestApp_PanicInRunBecomesError(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second})
	p := newFake("p", rec)
	p.panics = true
	a.Add(p)
	err := a.Run(t.Context())
	require.ErrorContains(t, err, "panic: component exploded")
	require.ErrorContains(t, err, "goroutine", "the stack is kept")
}

func TestApp_ShutdownDelayAppliedOnSignal(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second, ShutdownDelay: 150 * time.Millisecond})
	a.Add(newFake("http", rec))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	start := time.Now()
	require.NoError(t, <-done)
	require.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond)
}

func TestApp_StopOrderAndDeadline(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: 200 * time.Millisecond})
	s := newFake("stuck", rec)
	s.stuck = true
	s.release = make(chan struct{})
	defer close(s.release) // let the goroutine finish so goleak stays clean
	a.Add(newFake("db", rec), s)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	start := time.Now()
	err := <-done
	require.ErrorIs(t, err, app.ErrShutdownTimeout)
	require.Less(t, time.Since(start), 2*time.Second, "Run returns at the deadline even though a component is stuck")
}

func TestApp_ExitingComponentIsAnError(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second})
	a.Add(app.Func("oneshot", func(context.Context) error { return nil }, nil))
	err := a.Run(t.Context())
	require.ErrorContains(t, err, "exited before shutdown")
}

func TestApp_CloserRunsInReverseOrder(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second})
	a.Add(
		app.Closer("telemetry", func(context.Context) error { rec.add("close:telemetry"); return nil }),
		app.Closer("pool", func(context.Context) error { rec.add("close:pool"); return errors.New("already closed") }),
		newFake("http", rec),
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	err := <-done
	require.ErrorContains(t, err, "shutdown pool: already closed")
	events := rec.list()
	require.Equal(t, []string{"stop:http", "close:pool", "close:telemetry"}, events[len(events)-3:])
}

// TestApp_SIGTERM is not parallel: it signals the test process.
func TestApp_SIGTERM(t *testing.T) {
	rec := &recorder{}
	a, err := app.New(app.Config{ShutdownTimeout: time.Second}, app.WithReadinessGate(rec), app.WithSignals(syscall.SIGTERM))
	require.NoError(t, err)
	a.Add(newFake("http", rec))
	done := make(chan error, 1)
	go func() { done <- a.Run(t.Context()) }()
	time.Sleep(50 * time.Millisecond)
	p, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, p.Signal(syscall.SIGTERM))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("app did not stop on SIGTERM")
	}
}

// TestApp_ReadinessFalseBeforeDrain wires a real health registry and HTTP server: once shutdown
// starts, /readyz answers 503 while an in-flight request is still allowed to finish.
func TestApp_ReadinessFalseBeforeDrain(t *testing.T) {
	t.Parallel()
	reg := health.NewRegistry()
	inflight := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", reg.ReadinessHandler())
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(inflight)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	srv, err := httpserver.New(t.Context(), httpserver.Config{
		Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second,
	}, mux)
	require.NoError(t, err)

	a, err := app.New(app.Config{ShutdownTimeout: 5 * time.Second, ShutdownDelay: 300 * time.Millisecond},
		app.WithReadinessGate(reg), app.WithSignals())
	require.NoError(t, err)
	a.Add(srv)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	base := "http://" + srv.Addr().String()
	require.Eventually(t, func() bool { return statusOf(t.Context(), base+"/readyz") == http.StatusOK }, 2*time.Second, 10*time.Millisecond)

	slowCode := make(chan int, 1)
	go func() { slowCode <- statusOf(t.Context(), base+"/slow") }()
	<-inflight

	cancel() // shutdown requested while /slow is in flight
	require.Eventually(t, func() bool { return statusOf(t.Context(), base+"/readyz") == http.StatusServiceUnavailable },
		time.Second, 10*time.Millisecond, "readiness withdrawn before listeners close")

	close(release)
	require.Equal(t, http.StatusOK, <-slowCode, "in-flight request completed")
	require.NoError(t, <-done)
}

func statusOf(ctx context.Context, url string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestApp_ContextOnlyComponentStopsBeforeEarlierResources: a background loop (no stop func) added after the
// pool must have returned from Run before the pool is closed (go-review M4).
func TestApp_ContextOnlyComponentStopsBeforeEarlierResources(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second})
	a.Add(
		app.Closer("pool", func(context.Context) error { rec.add("close:pool"); return nil }),
		app.Func("prober", func(ctx context.Context) error { <-ctx.Done(); rec.add("exit:prober"); return nil }, nil),
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, func() bool { return len(rec.list()) > 0 }, time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	events := rec.list()
	require.Equal(t, []string{"ready=true", "ready=false", "exit:prober", "close:pool"}, events)
}

// TestApp_ClosersGetTheirOwnBudget: a drain that consumes its whole deadline does not leave the telemetry
// flush with an expired context (go-review M5).
func TestApp_ClosersGetTheirOwnBudget(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: 100 * time.Millisecond, CloseTimeout: time.Second})
	var closeCtxErr error
	a.Add(
		app.Closer("telemetry", func(ctx context.Context) error { closeCtxErr = ctx.Err(); return nil }),
		app.Func("slow-drain", func(ctx context.Context) error { <-ctx.Done(); return nil },
			func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }),
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	err := <-done
	require.ErrorIs(t, err, context.DeadlineExceeded, "the slow drain is reported")
	require.NoError(t, closeCtxErr, "the closer still had a live budget")
}

func TestApp_ShutdownPanicIsReported(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a := newApp(t, rec, app.Config{ShutdownTimeout: time.Second})
	a.Add(app.Closer("db", func(context.Context) error { rec.add("close:db"); return nil }),
		app.Func("bad", func(ctx context.Context) error { <-ctx.Done(); return nil }, func(context.Context) error { panic("oops") }))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	err := <-done
	require.ErrorContains(t, err, "panic during shutdown: oops")
	require.Contains(t, rec.list(), "close:db", "the sequence continues after a panicking Shutdown")
}

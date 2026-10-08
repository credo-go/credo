package credo_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// journal records lifecycle events from concurrent calls.
type journal struct {
	mu     sync.Mutex
	events []string
}

func (j *journal) add(event string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
}

func (j *journal) list() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.events)
}

func (j *journal) count(event string) int {
	n := 0
	for _, e := range j.list() {
		if e == event {
			n++
		}
	}
	return n
}

// part is a configurable component: Start and Shutdown record themselves
// and can fail, panic or block.
type part struct {
	name string
	j    *journal

	startErr      error
	startPanic    bool
	startBlock    chan struct{} // Start waits for it, ignoring its context
	startEntered  chan struct{}
	shutdownErr   error
	shutdownPanic bool
	shutdownBlock chan struct{} // Shutdown waits for it, ignoring its context
	readyErr      error

	closed atomic.Bool
}

func (p *part) Start(ctx context.Context) error {
	p.j.add("start:" + p.name)
	if p.startEntered != nil {
		close(p.startEntered)
	}
	if p.startPanic {
		panic("start " + p.name)
	}
	if p.startBlock != nil {
		<-p.startBlock
	}
	return p.startErr
}

func (p *part) Shutdown(context.Context) error {
	p.j.add("stop:" + p.name)
	p.closed.Store(true)
	if p.shutdownPanic {
		panic("shutdown " + p.name)
	}
	if p.shutdownBlock != nil {
		<-p.shutdownBlock
	}
	return p.shutdownErr
}

func (p *part) Ready(context.Context) error { return p.readyErr }

// Distinct binding types over part; each promotes Start, Ready and Shutdown.
type (
	partA struct{ *part }
	partB struct{ *part }
	partC struct{ *part }
	partD struct{ *part }
)

// stopOnly is a component without Start.
type stopOnly struct {
	j    *journal
	name string
}

func (s *stopOnly) Shutdown(context.Context) error {
	s.j.add("stop:" + s.name)
	return nil
}

func startComponents(t *testing.T, app *credo.App) {
	t.Helper()
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
}

func shutdownApp(t *testing.T, app *credo.App, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), timeout)
	defer cancel()
	return app.Shutdown(ctx)
}

func lifecycleEntry(t *testing.T, err error, name string) credo.LifecycleEntry {
	t.Helper()
	le, ok := errors.AsType[*credo.LifecycleError](err)
	if !ok {
		t.Fatalf("error %v is not a *credo.LifecycleError", err)
	}
	for _, e := range le.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %v", name, err)
	return credo.LifecycleEntry{}
}

// TestComponents_TiersStartAndStopInOrder pins the walk: internal tier in
// dependency order, its start hooks, then the ingress tier and its hooks;
// the drain stops ingress (stop hooks first) before internal, each in
// reverse dependency order.
func TestComponents_TiersStartAndStopInOrder(t *testing.T) {
	j := &journal{}
	app := mustNew(t)
	app.Provide[*partA](func() *partA { return &partA{&part{name: "store", j: j}} })
	app.Provide[*partB](func(a *partA) *partB { return &partB{&part{name: "service", j: j}} })
	app.Provide[*partC](func(b *partB) *partC { return &partC{&part{name: "consumer", j: j}} },
		credo.Ingress())
	app.OnStart(func(context.Context) error { j.add("hook:start-internal"); return nil })
	app.OnStart(func(context.Context) error { j.add("hook:start-ingress"); return nil }, credo.Ingress())
	app.OnStop(func(context.Context) error { j.add("hook:stop-internal"); return nil })
	app.OnStop(func(context.Context) error { j.add("hook:stop-ingress"); return nil }, credo.Ingress())

	startComponents(t, app)
	if err := shutdownApp(t, app, 5*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	want := []string{
		"start:store", "start:service", "hook:start-internal",
		"start:consumer", "hook:start-ingress",
		"hook:stop-ingress", "stop:consumer",
		"hook:stop-internal", "stop:service", "stop:store",
	}
	if got := j.list(); !slices.Equal(got, want) {
		t.Errorf("events = %v\nwant     %v", got, want)
	}
}

// TestComponents_IngressTierStopsConcurrently: two ingress components that
// each wait for the other inside Shutdown complete only when the tier stops
// them concurrently.
func TestComponents_IngressTierStopsConcurrently(t *testing.T) {
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	barrier := func(ctx context.Context) error {
		arrived.Done()
		select {
		case <-both:
			return nil
		case <-ctx.Done():
			return errors.New("the ingress tier stopped its components one at a time")
		}
	}
	app := mustNew(t)
	app.Manage(componentFunc(barrier), credo.Ingress(), credo.Named("first"))
	app.Manage(componentFunc(barrier), credo.Ingress(), credo.Named("second"))
	startComponents(t, app)
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

// componentFunc adapts a function to a component.
type componentFunc func(ctx context.Context) error

func (f componentFunc) Shutdown(ctx context.Context) error { return f(ctx) }

// TestComponents_StartContextEndsWithTheCall: the context handed to Start is
// live during the call and cancelled once it returns.
func TestComponents_StartContextEndsWithTheCall(t *testing.T) {
	var startCtx context.Context
	var live bool
	app := mustNew(t)
	app.ProvideValue[*ctxStarter](&ctxStarter{fn: func(ctx context.Context) {
		startCtx, live = ctx, ctx.Err() == nil
	}})
	startComponents(t, app)
	t.Cleanup(func() { _ = shutdownApp(t, app, 2*time.Second) })
	if !live {
		t.Error("the Start context should be live during the call")
	}
	if startCtx.Err() == nil {
		t.Error("the Start context should be cancelled once Start returned")
	}
}

type ctxStarter struct{ fn func(context.Context) }

func (s *ctxStarter) Start(ctx context.Context) error { s.fn(ctx); return nil }
func (s *ctxStarter) Shutdown(context.Context) error  { return nil }

// TestComponents_StartIsCalledOnce: a component bound under two types — a
// pointer and an interface alias over it — starts and stops once.
func TestComponents_StartIsCalledOnce(t *testing.T) {
	j := &journal{}
	app := mustNew(t)
	p := &partA{&part{name: "shared", j: j}}
	app.ProvideValue[*partA](p)
	app.ProvideValue[credo.Component](p)
	startComponents(t, app)
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if n := j.count("start:shared"); n != 1 {
		t.Errorf("Start ran %d times, want 1", n)
	}
	if n := j.count("stop:shared"); n != 1 {
		t.Errorf("Shutdown ran %d times, want 1 (one resource, one teardown)", n)
	}
}

// TestComponents_FailedStartRollsBack: a failed Start rolls back what was
// built — not the component that failed — runs the stop hooks, leaves the
// App stopped, refuses a second start and answers 503.
func TestComponents_FailedStartRollsBack(t *testing.T) {
	j := &journal{}
	errBoom := errors.New("boom")
	app := mustNew(t)
	app.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(200, "pong") })
	app.Provide[*partA](func() *partA { return &partA{&part{name: "store", j: j}} })
	app.Provide[*partB](func(*partA) *partB { return &partB{&part{name: "service", j: j, startErr: errBoom}} })
	app.OnStop(func(context.Context) error { j.add("hook:stop"); return nil })

	err := app.Start(t.Context())
	if !errors.Is(err, errBoom) {
		t.Fatalf("App.Start() = %v, want it to wrap the Start error", err)
	}
	e := lifecycleEntry(t, err, "*credo_test.partB")
	if e.Phase != credo.PhaseStart || e.Outcome != credo.OutcomeFailed || e.Tier != credo.TierInternal {
		t.Errorf("entry = %+v, want an internal start failure", e)
	}
	want := []string{"start:store", "start:service", "hook:stop", "stop:store"}
	if got := j.list(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if got := app.State(); got != "stopped" {
		t.Errorf("State() = %q, want stopped", got)
	}
	if err := app.Start(t.Context()); err == nil {
		t.Error("a second App.Start should be refused")
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ServeHTTP after a failed start = %d, want 503", rec.Code)
	}
}

// TestComponents_ShutdownInterruptsStart: Shutdown during the start phase
// cancels the running Start, starts nothing further and rolls back.
func TestComponents_ShutdownInterruptsStart(t *testing.T) {
	j := &journal{}
	entered := make(chan struct{})
	app := mustNew(t)
	app.ProvideValue[*partA](&partA{&part{name: "store", j: j}})
	app.ProvideValue[*cooperativeStarter](&cooperativeStarter{entered: entered})
	app.ProvideValue[*partC](&partC{&part{name: "consumer", j: j}}, credo.Ingress())

	startErr := make(chan error, 1)
	go func() { startErr <- app.Start(t.Context()) }()
	<-entered
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() during start = %v", err)
	}
	err := <-startErr
	if err == nil || !strings.Contains(err.Error(), "shut down while starting") {
		t.Fatalf("App.Start() = %v, want the interruption", err)
	}
	if j.count("start:consumer") != 0 {
		t.Error("an ingress component started after the interrupt")
	}
	if j.count("stop:store") != 1 {
		t.Error("the built store was not rolled back")
	}
	if got := app.State(); got != "stopped" {
		t.Errorf("State() = %q, want stopped", got)
	}
}

// cooperativeStarter blocks in Start until its context ends.
type cooperativeStarter struct{ entered chan struct{} }

func (s *cooperativeStarter) Start(ctx context.Context) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}
func (s *cooperativeStarter) Shutdown(context.Context) error { return nil }

// TestComponents_StartIgnoringCancellationIsAbandoned: a Start that ignores
// the interrupt is abandoned at the rollback deadline and keeps the
// components it depends on open.
func TestComponents_StartIgnoringCancellationIsAbandoned(t *testing.T) {
	j := &journal{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	entered := make(chan struct{})
	app := mustNew(t, credo.WithShutdownTimeout(100*time.Millisecond))
	app.ProvideValue[*partA](&partA{&part{name: "store", j: j}})
	app.Provide[*partB](func(*partA) *partB {
		return &partB{&part{name: "stubborn", j: j, startBlock: release, startEntered: entered}}
	})

	ctx, cancel := context.WithCancel(t.Context())
	startErr := make(chan error, 1)
	go func() { startErr <- app.Start(ctx) }()
	<-entered
	cancel()
	err := <-startErr
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("App.Start() = %v, want the context's error", err)
	}
	if e := lifecycleEntry(t, err, "*credo_test.partB"); e.Outcome != credo.OutcomeAbandoned || e.Phase != credo.PhaseStart {
		t.Errorf("stubborn entry = %+v, want an abandoned start", e)
	}
	e := lifecycleEntry(t, err, "*credo_test.partA")
	if e.Outcome != credo.OutcomeKeptOpen || !slices.Equal(e.KeptOpenBy, []string{"*credo_test.partB"}) {
		t.Errorf("store entry = %+v, want kept open by the stubborn component", e)
	}
	if j.count("stop:store") != 0 {
		t.Error("a dependency of a running Start was shut down")
	}
}

// TestComponents_AbandonedShutdownKeepsDependenciesOpen: a Shutdown still
// running at the deadline is abandoned, and the components it uses stay
// open and are named.
func TestComponents_AbandonedShutdownKeepsDependenciesOpen(t *testing.T) {
	j := &journal{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	app := mustNew(t)
	store := &partA{&part{name: "store", j: j}}
	app.ProvideValue[*partA](store)
	app.Provide[*partB](func(*partA) *partB { return &partB{&part{name: "service", j: j, shutdownBlock: release}} })
	startComponents(t, app)

	err := shutdownApp(t, app, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() = %v, want the deadline", err)
	}
	if e := lifecycleEntry(t, err, "*credo_test.partB"); e.Outcome != credo.OutcomeAbandoned || e.Phase != credo.PhaseShutdown {
		t.Errorf("service entry = %+v, want an abandoned shutdown", e)
	}
	e := lifecycleEntry(t, err, "*credo_test.partA")
	if e.Outcome != credo.OutcomeKeptOpen || !slices.Equal(e.KeptOpenBy, []string{"*credo_test.partB"}) {
		t.Errorf("store entry = %+v, want kept open by the service", e)
	}
	if store.closed.Load() {
		t.Error("the store was shut down under a running consumer")
	}
}

// TestComponents_PanicsAreTheComponentsErrors: a panicking Start fails the
// start phase and a panicking Shutdown is reported, both as *DIPanicError.
func TestComponents_PanicsAreTheComponentsErrors(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue[*partA](&partA{&part{name: "a", j: &journal{}, startPanic: true}})
		err := callRecovering(t, "App.Start", func() error { return app.Start(t.Context()) })
		e := lifecycleEntry(t, err, "*credo_test.partA")
		pe, ok := errors.AsType[*credo.DIPanicError](e.Err)
		if e.Outcome != credo.OutcomePanicked || !ok || pe.Phase != credo.DIPanicStart || pe.Stack == "" {
			t.Errorf("entry = %+v, want a start DIPanicError with a stack", e)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		j := &journal{}
		app := mustNew(t)
		app.ProvideValue[*partA](&partA{&part{name: "a", j: j}})
		app.ProvideValue[*partB](&partB{&part{name: "b", j: j, shutdownPanic: true}})
		startComponents(t, app)
		err := callRecovering(t, "Shutdown", func() error { return shutdownApp(t, app, 2*time.Second) })
		e := lifecycleEntry(t, err, "*credo_test.partB")
		pe, ok := errors.AsType[*credo.DIPanicError](e.Err)
		if e.Outcome != credo.OutcomePanicked || !ok || pe.Phase != credo.DIPanicShutdown {
			t.Errorf("entry = %+v, want a shutdown DIPanicError", e)
		}
		if j.count("stop:a") != 1 {
			t.Error("a panicking Shutdown skipped the next component")
		}
	})
}

// TestComponents_UnresolvedComponentIsNeverBuilt: a constructor-bound
// component without Start or Ready is built only when something resolves it.
func TestComponents_UnresolvedComponentIsNeverBuilt(t *testing.T) {
	j := &journal{}
	var built atomic.Int32
	app := mustNew(t)
	app.Provide[*stopOnly](func() *stopOnly { built.Add(1); return &stopOnly{j: j, name: "lazy"} })
	startComponents(t, app)
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if built.Load() != 0 || len(j.list()) != 0 {
		t.Errorf("built %d, events %v; want an unresolved component left alone", built.Load(), j.list())
	}
}

// TestComponents_PlanFollowsTheBindingType: Start and Ready are planned from
// the binding's type; an interface that hides them hides the capabilities,
// while a Shutdown only the value shows still makes it an internal component.
func TestComponents_PlanFollowsTheBindingType(t *testing.T) {
	j := &journal{}
	hidden := &partA{&part{name: "hidden", j: j, readyErr: errors.New("never asked")}}
	app := mustNew(t)
	app.ProvideValue[namedThing](hidden)
	app.UseHealth()
	startComponents(t, app)

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/ready = %d %s; a Ready hidden by the binding's type must not be asked", rec.Code, rec.Body)
	}
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if want := []string{"stop:hidden"}; !slices.Equal(j.list(), want) {
		t.Errorf("events = %v, want %v (no Start, Shutdown found on the value)", j.list(), want)
	}
}

// namedThing hides Start, Ready and Shutdown.
type namedThing interface{ isNamedThing() }

func (*partA) isNamedThing() {}

// TestComponents_BorrowedIsNeverShutDown: a borrowed value is not shut down,
// and its Ready still contributes to readiness.
func TestComponents_BorrowedIsNeverShutDown(t *testing.T) {
	j := &journal{}
	borrowed := &partD{&part{name: "borrowed", j: j, readyErr: errors.New("upstream down")}}
	app := mustNew(t)
	app.ProvideValue[*partD](borrowed, credo.Borrowed())
	app.UseHealth()
	startComponents(t, app)

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "partD") {
		t.Errorf("/ready = %d %s, want 503 naming the borrowed value", rec.Code, rec.Body)
	}
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if borrowed.closed.Load() || j.count("start:borrowed") != 0 {
		t.Errorf("events = %v; a borrowed value is neither started nor shut down", j.list())
	}
}

// Closer shapes.
type (
	closeErr  struct{ j *journal }
	closeVoid struct{ j *journal }
	closeCtx  struct{ j *journal }
)

func (c *closeErr) Close() error                { c.j.add("close:err"); return nil }
func (c *closeVoid) Close()                     { c.j.add("close:void") }
func (c *closeCtx) Close(context.Context) error { c.j.add("close:ctx"); return nil }

// TestComponents_CloserShapes: credo.Closer() makes each Close shape the
// binding's teardown, in reverse registration order.
func TestComponents_CloserShapes(t *testing.T) {
	j := &journal{}
	app := mustNew(t)
	app.ProvideValue[*closeErr](&closeErr{j}, credo.Closer())
	app.ProvideValue[*closeVoid](&closeVoid{j}, credo.Closer())
	app.ProvideValue[*closeCtx](&closeCtx{j}, credo.Closer())
	startComponents(t, app)
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if want := []string{"close:ctx", "close:void", "close:err"}; !slices.Equal(j.list(), want) {
		t.Errorf("events = %v, want %v", j.list(), want)
	}
}

// TestComponents_Override: Override replaces a binding in place; the value
// it replaces never becomes the App's.
func TestComponents_Override(t *testing.T) {
	j := &journal{}
	var built atomic.Int32
	app := mustNew(t)
	app.Provide[*partA](func() *partA { built.Add(1); return &partA{&part{name: "real", j: j}} })
	app.ProvideValue[*partA](&partA{&part{name: "fake", j: j}}, credo.Override())
	startComponents(t, app)
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if want := []string{"start:fake", "stop:fake"}; built.Load() != 0 || !slices.Equal(j.list(), want) {
		t.Errorf("built %d, events %v; want only the override", built.Load(), j.list())
	}
}

// TestComponents_RegistrationMisusePanics pins the per-call option checks.
func TestComponents_RegistrationMisusePanics(t *testing.T) {
	cases := []struct {
		name, want string
		fn         func(app *credo.App)
	}{
		{"override without binding", "has none", func(app *credo.App) {
			app.ProvideValue[*partA](&partA{&part{j: &journal{}}}, credo.Override())
		}},
		{"borrowed constructor", "credo.Borrowed()", func(app *credo.App) {
			app.Provide[*partA](func() *partA { return nil }, credo.Borrowed())
		}},
		{"closer on manage", "does not accept", func(app *credo.App) {
			app.Manage(&stopOnly{}, credo.Closer())
		}},
		{"repeated option", "given twice", func(app *credo.App) {
			app.Manage(&stopOnly{}, credo.Ingress(), credo.Ingress())
		}},
		{"zero option", "zero RegistrationOption", func(app *credo.App) {
			app.Manage(&stopOnly{}, credo.RegistrationOption{})
		}},
		{"empty name", "non-empty name", func(app *credo.App) {
			app.Manage(&stopOnly{}, credo.Named(""))
		}},
		{"manage without shutdown", "Manage", func(app *credo.App) {
			app.Manage(struct{}{})
		}},
		{"duplicate name", "worker", func(app *credo.App) {
			app.Manage(&stopOnly{}, credo.Named("worker"))
			app.Manage(&stopOnly{}, credo.Named("worker"))
		}},
		{"manage itself", "cannot manage itself", func(app *credo.App) {
			app.Manage(app)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t)
			expectPanicContaining(t, tc.want, func() { tc.fn(app) })
		})
	}
}

// TestComponents_ManagedValueAndConstructor: Manage adds a value, stopped in
// reverse registration order, and a constructor the start walk builds and
// starts with its DI parameters.
func TestComponents_ManagedValueAndConstructor(t *testing.T) {
	j := &journal{}
	app := mustNew(t)
	app.ProvideValue[*partA](&partA{&part{name: "store", j: j}})
	app.Manage(&stopOnly{j: j, name: "first"}, credo.Named("first"))
	app.Manage(func(*partA) *partB { return &partB{&part{name: "managed", j: j}} }, credo.Named("managed"))
	app.Manage(&stopOnly{j: j, name: "last"}, credo.Named("last"))
	startComponents(t, app)
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	got := j.list()
	if j.count("start:managed") != 1 || j.count("stop:managed") != 1 {
		t.Fatalf("events = %v; the managed constructor should start and stop once", got)
	}
	if slices.Index(got, "stop:last") > slices.Index(got, "stop:first") {
		t.Errorf("events = %v; managed values stop in reverse registration order", got)
	}
	if slices.Index(got, "stop:managed") > slices.Index(got, "stop:store") {
		t.Errorf("events = %v; the managed constructor stops before its dependency", got)
	}
}

// readyOnly is a component with Ready and no Start.
type readyOnly struct{}

func (*readyOnly) Ready(context.Context) error    { return nil }
func (*readyOnly) Shutdown(context.Context) error { return nil }

// TestComponents_ServeHTTPBeforeStartPanics: an App with start work refuses
// ServeHTTP until it was started, naming the ways to start it; an App with
// nothing to start serves without App.Start.
func TestComponents_ServeHTTPBeforeStartPanics(t *testing.T) {
	cases := []struct {
		name  string
		setup func(app *credo.App)
	}{
		{"component with Start", func(app *credo.App) {
			app.ProvideValue[*partA](&partA{&part{name: "a", j: &journal{}}})
		}},
		{"component with only Ready", func(app *credo.App) {
			app.ProvideValue[*readyOnly](&readyOnly{})
		}},
		{"start hook", func(app *credo.App) {
			app.OnStart(func(context.Context) error { return nil })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(200, "pong") })
			tc.setup(app)
			expectPanicContaining(t, "call App.Start before serving it through ServeHTTP", func() {
				app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ping", nil))
			})
		})
	}
	t.Run("nothing to start", func(t *testing.T) {
		app := mustNew(t)
		app.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(200, "pong") })
		app.ProvideValue[*stopOnly](&stopOnly{j: &journal{}})
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /ping = %d, want 200 without App.Start", rec.Code)
		}
	})
}

// TestComponents_RunContextCancelledDuringStart: cancelling RunContext's
// context during the start phase rolls back, returns nil, never logs
// "server started" and never accepts.
func TestComponents_RunContextCancelledDuringStart(t *testing.T) {
	j := &journal{}
	entered := make(chan struct{})
	host, port, addr := freePort(t)
	logs := &syncBuffer{}
	app := mustNew(t, credo.WithAddr(host, port), credo.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	app.ProvideValue[*partA](&partA{&part{name: "store", j: j}})
	app.ProvideValue[*cooperativeStarter](&cooperativeStarter{entered: entered})

	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan error, 1)
	go func() { ran <- app.RunContext(ctx) }()
	<-entered
	// The listener is bound, so the kernel may complete a handshake, but no
	// request is served during the start phase.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial during the start phase: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	cancel()
	if err := <-ran; err != nil {
		t.Fatalf("RunContext() = %v, want nil after a clean rollback", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := conn.Read(make([]byte, 64)); n != 0 {
		t.Error("a request was served during the start phase")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Error("the listener still accepts after the rollback")
	}
	if j.count("stop:store") != 1 {
		t.Errorf("events = %v; the built store should be rolled back", j.list())
	}
	if strings.Contains(logs.String(), "server started") {
		t.Error(`"server started" was logged for an interrupted start`)
	}
}

// TestComponents_MountedChildWithoutManagePanics: a child App with start
// work that its parent does not manage panics on its first request; the
// parent's recovery answers 500 and logs the panic with its message.
func TestComponents_MountedChildWithoutManagePanics(t *testing.T) {
	child := mustNew(t)
	child.ProvideValue[*partA](&partA{&part{name: "child", j: &journal{}}})
	child.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(200, "child") })
	logs := &syncBuffer{}
	parent := mustNew(t, credo.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	parent.Mount("/c", child)
	startComponents(t, parent)
	t.Cleanup(func() { _ = shutdownApp(t, parent, 2*time.Second) })
	rec := httptest.NewRecorder()
	parent.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/ping", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("GET /c/ping = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "parent.Manage(child)") {
		t.Errorf("logs = %s, want the panic naming parent.Manage(child)", logs.String())
	}
}

// TestComponents_ConcurrentShutdownStopsOnce: concurrent Shutdown calls run
// one drain; the call that claimed it reports it and the others are refused
// with the state error.
func TestComponents_ConcurrentShutdownStopsOnce(t *testing.T) {
	j := &journal{}
	app := mustNew(t)
	app.ProvideValue[*partA](&partA{&part{name: "a", j: j}})
	app.OnStop(func(context.Context) error { j.add("hook:stop"); return nil })
	startComponents(t, app)
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	for range 4 {
		wg.Go(func() {
			err := shutdownApp(t, app, 2*time.Second)
			switch {
			case err == nil:
				succeeded.Add(1)
			case !strings.Contains(err.Error(), "server in state"):
				t.Errorf("Shutdown() = %v, want nil or the state error", err)
			}
		})
	}
	wg.Wait()
	if succeeded.Load() != 1 {
		t.Errorf("%d Shutdown calls claimed the drain, want 1", succeeded.Load())
	}
	if j.count("stop:a") != 1 || j.count("hook:stop") != 1 {
		t.Errorf("events = %v, want one drain", j.list())
	}
}

// TestComponents_ManagedChildApp: a mounted child App handed to
// parent.Manage starts in the parent's start phase and stops with it.
func TestComponents_ManagedChildApp(t *testing.T) {
	j := &journal{}
	child := mustNew(t)
	child.ProvideValue[*partA](&partA{&part{name: "child", j: j}})
	child.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(200, "child") })

	parent := mustNew(t)
	parent.Mount("/c", child)
	parent.Manage(child, credo.Named("child"))
	startComponents(t, parent)

	rec := httptest.NewRecorder()
	parent.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/c/ping", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "child" {
		t.Errorf("GET /c/ping = %d %q, want 200 child", rec.Code, rec.Body)
	}
	if err := shutdownApp(t, parent, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if want := []string{"start:child", "stop:child"}; !slices.Equal(j.list(), want) {
		t.Errorf("events = %v, want %v", j.list(), want)
	}
	if got := child.State(); got != "stopped" {
		t.Errorf("child State() = %q, want stopped", got)
	}
}

// TestComponents_IngressStopHooksOverlapTheHTTPDrain: an ingress stop hook
// runs while an in-flight request is still draining, and an internal
// component a draining consumer uses is still open.
func TestComponents_IngressStopHooksOverlapTheHTTPDrain(t *testing.T) {
	j := &journal{}
	hookRan := make(chan struct{})
	inFlight := make(chan struct{})
	store := &partA{&part{name: "queue", j: j}}

	app := mustNew(t)
	app.ProvideValue[*partA](store)
	app.Provide[*partC](func(q *partA) *partC {
		return &partC{&part{name: "consumer", j: j}}
	}, credo.Ingress())
	app.OnStop(func(context.Context) error {
		if store.closed.Load() {
			return errors.New("the internal queue closed before the ingress tier")
		}
		close(hookRan)
		return nil
	}, credo.Ingress())
	app.GET("/slow", func(ctx *credo.Context) error {
		close(inFlight)
		select {
		case <-hookRan:
			return ctx.Response().Text(200, "done")
		case <-time.After(5 * time.Second):
			return errors.New("the ingress stop hook waited for the HTTP drain")
		}
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- app.ServeContext(context.Background(), l) }()
	waitRunning(t, app)

	resp := make(chan int, 1)
	go func() {
		r, err := http.Get("http://" + l.Addr().String() + "/slow")
		if err != nil {
			resp <- 0
			return
		}
		r.Body.Close()
		resp <- r.StatusCode
	}()
	<-inFlight
	if err := shutdownApp(t, app, 5*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if code := <-resp; code != http.StatusOK {
		t.Errorf("in-flight request = %d, want 200", code)
	}
	if err := <-served; err != nil {
		t.Errorf("ServeContext() = %v", err)
	}
	got := j.list()
	if slices.Index(got, "stop:queue") < slices.Index(got, "stop:consumer") {
		t.Errorf("events = %v; the queue stops after its ingress consumer", got)
	}
}

// TestTier_Text pins the tier's text form.
func TestTier_Text(t *testing.T) {
	for _, tier := range []credo.Tier{credo.TierIngress, credo.TierInternal} {
		text, err := tier.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		var back credo.Tier
		if err := back.UnmarshalText(text); err != nil || back != tier {
			t.Errorf("round trip of %s = (%v, %v)", text, back, err)
		}
	}
	if credo.TierIngress.String() != "ingress" || credo.TierInternal.String() != "internal" {
		t.Error("tier names changed")
	}
	var bad credo.Tier
	if err := bad.UnmarshalText([]byte("edge")); err == nil {
		t.Error("an unknown tier should be rejected")
	}
}

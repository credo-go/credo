package worker

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// stubWorker is a continuous worker constructed by DI in these tests. The
// type parameter only mints distinct types, so each test case can bind its
// own provider.
type stubWorker[K any] struct {
	runs    atomic.Int32
	running chan struct{}
}

func newStubWorker[K any]() *stubWorker[K] {
	return &stubWorker[K]{running: make(chan struct{}, 1)}
}

func (w *stubWorker[K]) Run(ctx context.Context) error {
	w.runs.Add(1)
	select {
	case w.running <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil
}

type (
	kindA struct{}
	kindB struct{}
	kindC struct{}
	kindD struct{}
)

func blockingFunc() Func {
	return func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
}

func TestRegister_NameRules(t *testing.T) {
	rejected := []struct {
		name string
		want string
	}{
		{"", "name must not be empty"},
		{" job", "leading or trailing whitespace"},
		{"job ", "leading or trailing whitespace"},
		{"\tjob", "leading or trailing whitespace"},
		{"job\nsync", "control characters"},
		{"job\x00", "control characters"},
		{"job\u0085x", "control characters"},
	}
	for _, tt := range rejected {
		t.Run("rejects "+strings.ToValidUTF8(tt.name, "?"), func(t *testing.T) {
			app := newTestApp(t)
			requireErrContaining(t, Register(app, tt.name, blockingFunc()), tt.want)
			requireErrContaining(t, RegisterProvided[*stubWorker[kindA]](app, tt.name), tt.want)
			mustPanicContaining(t, tt.want, func() { MustRegister(app, tt.name, blockingFunc()) })
			mustPanicContaining(t, tt.want, func() { MustRegisterProvided[*stubWorker[kindA]](app, tt.name) })
		})
	}

	accepted := []string{"invoice-worker", "credo.outbox", "report:daily", "rapor-üretici", "a b"}
	for _, name := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			app := newTestApp(t)
			if err := Register(app, name, blockingFunc()); err != nil {
				t.Fatalf("Register(%q) = %v", name, err)
			}
			// The same name with readiness is a distinct registration in a
			// fresh app: "credo." stays valid because the readiness check is
			// named "worker:<name>".
			app2 := newTestApp(t)
			if err := Register(app2, name, blockingFunc(), WithReadiness(ReadinessPolicy{FailWhenFailed: true})); err != nil {
				t.Fatalf("Register(%q, WithReadiness) = %v", name, err)
			}
			finalize(t, app)
			pool, err := app.Resolve[*Pool]()
			if err != nil {
				t.Fatal(err)
			}
			if got := pool.Workers()[0].Name; got != name {
				t.Fatalf("registered name = %q, want %q (names are never normalized)", got, name)
			}
		})
	}
}

func TestRegister_RejectsNilWorkers(t *testing.T) {
	app := newTestApp(t)
	var nilFunc Func
	var nilPointer *stubWorker[kindA]
	for _, w := range []Worker{nil, nilFunc, nilPointer} {
		requireErrContaining(t, Register(app, "nil", w), `worker "nil" must not be nil`)
	}
	requireErrContaining(t, Register(nil, "x", blockingFunc()), "app must not be nil")
	requireErrContaining(t, RegisterProvided[*stubWorker[kindA]](nil, "x"), "app must not be nil")
}

func TestRegister_DuplicateNameAcrossForms(t *testing.T) {
	app := newTestApp(t)
	if err := Register(app, "dup", blockingFunc()); err != nil {
		t.Fatal(err)
	}
	requireErrContaining(t, RegisterProvided[*stubWorker[kindA]](app, "dup"), `duplicate worker name "dup"`)

	app = newTestApp(t)
	if err := RegisterProvided[*stubWorker[kindA]](app, "dup"); err != nil {
		t.Fatal(err)
	}
	requireErrContaining(t, Register(app, "dup", blockingFunc()), `duplicate worker name "dup"`)
}

func TestRegisterProvided_OrderFree(t *testing.T) {
	for _, order := range []string{"provide-first", "register-first"} {
		t.Run(order, func(t *testing.T) {
			app := newTestApp(t)
			provide := func() { app.MustProvide[*stubWorker[kindA]](newStubWorker[kindA]) }
			register := func() { MustRegisterProvided[*stubWorker[kindA]](app, "provided") }
			if order == "provide-first" {
				provide()
				register()
			} else {
				register()
				provide()
			}
			finalize(t, app)

			pool, err := app.Resolve[*Pool]()
			if err != nil {
				t.Fatal(err)
			}
			if err = pool.Start(t.Context()); err != nil {
				t.Fatalf("Start() = %v", err)
			}
			w, err := app.Resolve[*stubWorker[kindA]]()
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-w.running:
			case <-time.After(5 * time.Second):
				t.Fatal("the DI-provided instance did not run")
			}
			shutdownPool(t, pool)
			if got := w.runs.Load(); got != 1 {
				t.Fatalf("runs = %d, want 1 (the container's singleton ran once)", got)
			}
		})
	}
}

// aliasedWorker is an application-level interface that a concrete worker is
// bound to through app.Alias.
type aliasedWorker interface {
	Worker
	aliased()
}

func (*stubWorker[K]) aliased() {}

func TestRegisterProvided_InterfaceThroughAlias(t *testing.T) {
	app := newTestApp(t)
	app.MustProvide[*stubWorker[kindB]](newStubWorker[kindB])
	if err := app.Alias[aliasedWorker, *stubWorker[kindB]](); err != nil {
		t.Fatal(err)
	}
	MustRegisterProvided[aliasedWorker](app, "aliased")
	finalize(t, app)

	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}
	if err = pool.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	w, err := app.Resolve[*stubWorker[kindB]]()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.running:
	case <-time.After(5 * time.Second):
		t.Fatal("the aliased worker did not run")
	}
	shutdownPool(t, pool)
}

func TestPoolStart_ResolutionFailuresStartNothing(t *testing.T) {
	app := newTestApp(t)

	// A healthy worker registered by value: all-or-nothing means it must not
	// run either.
	var healthyRuns atomic.Int32
	MustRegister(app, "healthy", Func(func(ctx context.Context) error {
		healthyRuns.Add(1)
		<-ctx.Done()
		return nil
	}))
	// Not provided at all.
	MustRegisterProvided[*stubWorker[kindA]](app, "missing")
	// Constructor error.
	app.MustProvide[*stubWorker[kindB]](func() (*stubWorker[kindB], error) {
		return nil, errors.New("constructor failed")
	})
	MustRegisterProvided[*stubWorker[kindB]](app, "ctor-error")
	// Constructor panic.
	app.MustProvide[*stubWorker[kindC]](func() *stubWorker[kindC] { panic("constructor boom") })
	MustRegisterProvided[*stubWorker[kindC]](app, "ctor-panic")
	// Typed nil result.
	app.MustProvide[*stubWorker[kindD]](func() *stubWorker[kindD] { return nil })
	MustRegisterProvided[*stubWorker[kindD]](app, "typed-nil")
	finalize(t, app)

	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}
	err = pool.Start(t.Context())
	if err == nil {
		t.Fatal("Start() = nil, want the joined resolution errors")
	}
	for _, want := range []string{
		`worker: "missing": resolve *worker.stubWorker[github.com/credo-go/credo/worker.kindA]`,
		`worker: "ctor-error": resolve *worker.stubWorker`,
		"constructor failed",
		`worker: "ctor-panic": resolve *worker.stubWorker`,
		"constructor boom",
		`worker: "typed-nil": resolve *worker.stubWorker`,
		"resolved to nil",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Start() error = %v\nwant it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "healthy") {
		t.Errorf("Start() error names the healthy worker: %v", err)
	}
	if _, ok := errors.AsType[*credo.DIPanicError](err); !ok {
		t.Errorf("Start() error = %v, want the constructor panic kept as *credo.DIPanicError", err)
	}

	infos := pool.Workers()
	if len(infos) != 5 {
		t.Fatalf("Workers() after failed Start = %d entries, want all 5 definitions", len(infos))
	}
	for _, info := range infos {
		if info.Status != StatusIdle {
			t.Errorf("worker %q status = %q after failed Start, want idle", info.Name, info.Status)
		}
	}
	if err := pool.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Errorf("second Start = %v, want refusal", err)
	}
	shutdownPool(t, pool)
	if got := healthyRuns.Load(); got != 0 {
		t.Fatalf("healthy worker ran %d times, want 0 (Start is all-or-nothing)", got)
	}
}

func TestRegisterProvided_FailedResolutionFailsAppStartup(t *testing.T) {
	app := newTestApp(t, credo.WithAddr("127.0.0.1", 0))
	res := &resource{workerDone: new(atomic.Bool)}
	app.MustProvideValue(res)
	var shutdownHooks atomic.Int32
	app.OnShutdown(func(context.Context) error {
		shutdownHooks.Add(1)
		return nil
	})
	var served atomic.Bool
	app.GET("/", func(*credo.Context) error {
		served.Store(true)
		return nil
	})
	MustRegisterProvided[*stubWorker[kindA]](app, "orphan")

	err := app.RunContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), `worker: "orphan": resolve`) {
		t.Fatalf("RunContext() = %v, want the worker resolution error", err)
	}
	if app.State() != "stopped" {
		t.Fatalf("State() = %q, want stopped", app.State())
	}
	if !res.closed.Load() {
		t.Error("DI teardown did not run after the failed startup")
	}
	if shutdownHooks.Load() != 1 {
		t.Errorf("OnShutdown hooks ran %d times, want 1", shutdownHooks.Load())
	}
	if served.Load() {
		t.Error("the app served a request although startup failed")
	}
}

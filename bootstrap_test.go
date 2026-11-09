package credo_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// Types for the bootstrap misuse table.
type (
	bootRepo     interface{ Find() string }
	bootPgRepo   struct{}
	bootPlugin   interface{ Name() string }
	bootAudit    struct{}
	bootUnbound  struct{}
	bootTwiceVal struct{}
)

func (*bootPgRepo) Find() string { return "pg" }
func (*bootAudit) Name() string  { return "audit" }

// mustPanicMessage runs fn and returns its panic message, failing the test
// when fn does not panic.
func mustPanicMessage(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("call did not panic")
		}
		msg = fmt.Sprint(r)
	}()
	fn()
	return ""
}

// TestBootstrap_MisusePanicsAtTheCall is the registration phase of the three
// error phases: misuse known at the call site panics with a message that
// names the App method, its type arguments, the phase and the remedy.
func TestBootstrap_MisusePanicsAtTheCall(t *testing.T) {
	newApp := func(t *testing.T) *credo.App {
		app := mustNew(t)
		app.Provide(func() *bootPgRepo { return &bootPgRepo{} })
		app.Provide(func() *bootAudit { return &bootAudit{} })
		return app
	}
	finalized := func(t *testing.T) *credo.App {
		app := newApp(t)
		mustFinalize(t, app)
		return app
	}
	shutDown := func(t *testing.T) *credo.App {
		app := newApp(t)
		if err := app.Shutdown(t.Context()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		return app
	}
	const (
		afterFinalize = "called after Finalize; make every registration before Finalize, " +
			"which Run and the first ServeHTTP call implicitly"
		afterShutdown = "called after shutdown began; registration ends when the App shuts down"
		frozenApp     = "called after app was compiled or shut down"
	)
	noop := func(*credo.Context) error { return nil }

	tests := []struct {
		name  string
		setup func(*testing.T) *credo.App
		call  func(*credo.App)
		want  []string
	}{
		// Arguments the container cannot accept.
		{"nil constructor", newApp, func(app *credo.App) { app.Provide(nil) },
			[]string{"credo: App.Provide: constructor must not be nil; want func(dependencies...) T or " +
				"func(dependencies...) (T, error), which binds T"}},
		{"constructor not a function", newApp, func(app *credo.App) { app.Provide("ctor") },
			[]string{"credo: App.Provide: constructor must be a function, got string; want func"}},
		{"constructor without a result", newApp, func(app *credo.App) { app.Provide(func() {}) },
			[]string{"credo: App.Provide: constructor must return 1 or 2 values, got 0; want func(dependencies...) T"}},
		{"constructor second result not an error", newApp,
			func(app *credo.App) { app.Provide(func() (*bootUnbound, int) { return nil, 0 }) },
			[]string{"credo: App.Provide[*credo_test.bootUnbound]: second return type must implement error, got int"}},
		{"duplicate Provide", newApp,
			func(app *credo.App) { app.Provide(func() *bootPgRepo { return nil }) },
			[]string{"credo: App.Provide[*credo_test.bootPgRepo]: *credo_test.bootPgRepo is already registered; " +
				"bind a second instance under a wrapper type of its own"}},
		{"duplicate ProvideValue", newApp, func(app *credo.App) {
			app.ProvideValue(&bootTwiceVal{})
			app.ProvideValue(&bootTwiceVal{})
		}, []string{"credo: App.ProvideValue[*credo_test.bootTwiceVal]: *credo_test.bootTwiceVal is already registered"}},
		{"Alias to a non-interface", newApp, func(app *credo.App) { app.Alias[*bootAudit, *bootPgRepo]() },
			[]string{"credo: App.Alias[*credo_test.bootAudit, *credo_test.bootPgRepo]: " +
				"first type parameter must be an interface"}},
		{"Alias of an unregistered type", newApp, func(app *credo.App) { app.Alias[bootPlugin, *bootUnboundPlugin]() },
			[]string{"credo: App.Alias[credo_test.bootPlugin, *credo_test.bootUnboundPlugin]: concrete type " +
				"*credo_test.bootUnboundPlugin is not registered; provide it before aliasing it"}},
		{"Alias twice", newApp, func(app *credo.App) {
			app.Alias[bootRepo, *bootPgRepo]()
			app.Alias[bootRepo, *bootPgRepo]()
		}, []string{"credo: App.Alias[credo_test.bootRepo, *credo_test.bootPgRepo]: interface " +
			"credo_test.bootRepo already has an alias, to *credo_test.bootPgRepo; give credo.Override() to " +
			"point it at *credo_test.bootPgRepo, or drop one of the two"}},
		{"Alias over a direct binding", newApp, func(app *credo.App) {
			app.ProvideValue[bootRepo](&bootPgRepo{})
			app.Alias[bootRepo, *bootPgRepo]()
		}, []string{"credo: App.Alias[credo_test.bootRepo, *credo_test.bootPgRepo]: interface " +
			"credo_test.bootRepo already has a direct registration; give credo.Override() to replace it " +
			"with the alias"}},
		{"Alias with another option", newApp,
			func(app *credo.App) { app.Alias[bootRepo, *bootPgRepo](credo.Ingress()) },
			[]string{"credo: App.Alias does not accept credo.Ingress(); it accepts credo.Override()"}},
		{"Alias override without a binding", newApp,
			func(app *credo.App) { app.Alias[bootRepo, *bootPgRepo](credo.Override()) },
			[]string{"credo: App.Alias[credo_test.bootRepo, *credo_test.bootPgRepo]: credo.Override() replaces " +
				"an earlier binding, but credo_test.bootRepo has none; drop credo.Override()"}},
		{"ProvideValue after an alias", newApp, func(app *credo.App) {
			app.Alias[bootRepo, *bootPgRepo]()
			app.ProvideValue[bootRepo](&bootPgRepo{})
		}, []string{"credo: App.ProvideValue[credo_test.bootRepo]: credo_test.bootRepo is already bound " +
			"through app.Alias[credo_test.bootRepo, *credo_test.bootPgRepo](); give credo.Override() to replace " +
			"the alias, or drop the Alias call"}},
		{"Provide after an alias", newApp, func(app *credo.App) {
			app.Alias[bootRepo, *bootPgRepo]()
			app.Provide(func() bootRepo { return &bootPgRepo{} })
		}, []string{"credo: App.Provide[credo_test.bootRepo]: credo_test.bootRepo is already bound through " +
			"app.Alias[credo_test.bootRepo, *credo_test.bootPgRepo]()"}},
		{"BindMany of a non-implementation", newApp, func(app *credo.App) { app.BindMany[bootPlugin, *bootPgRepo]() },
			[]string{"credo: App.BindMany[credo_test.bootPlugin, *credo_test.bootPgRepo]: " +
				"*credo_test.bootPgRepo does not implement credo_test.bootPlugin"}},
		{"BindMany twice", newApp, func(app *credo.App) {
			app.BindMany[bootPlugin, *bootAudit]()
			app.BindMany[bootPlugin, *bootAudit]()
		}, []string{"credo: App.BindMany[credo_test.bootPlugin, *credo_test.bootAudit]: binding already exists"}},

		// Resolution before Finalize.
		{"Resolve before Finalize", newApp, func(app *credo.App) { _, _ = app.Resolve[*bootPgRepo]() },
			[]string{"credo: App.Resolve[*credo_test.bootPgRepo]: called before Finalize; call Finalize first " +
				"(Run and the first ServeHTTP call it implicitly)"}},
		{"MustResolve before Finalize", newApp, func(app *credo.App) { app.MustResolve[*bootPgRepo]() },
			[]string{"credo: App.Resolve[*credo_test.bootPgRepo]: called before Finalize"}},
		{"ResolveAll before Finalize", newApp, func(app *credo.App) { _, _ = app.ResolveAll[bootPlugin]() },
			[]string{"credo: App.ResolveAll[credo_test.bootPlugin]: called before Finalize"}},

		// Each DI registration after Finalize.
		{"Provide after Finalize", finalized,
			func(app *credo.App) { app.Provide(func() *bootUnbound { return nil }) },
			[]string{"credo: App.Provide[*credo_test.bootUnbound]: " + afterFinalize}},
		{"ProvideValue after Finalize", finalized, func(app *credo.App) { app.ProvideValue(&bootUnbound{}) },
			[]string{"credo: App.ProvideValue[*credo_test.bootUnbound]: " + afterFinalize}},
		{"Alias after Finalize", finalized, func(app *credo.App) { app.Alias[bootRepo, *bootPgRepo]() },
			[]string{"credo: App.Alias[credo_test.bootRepo, *credo_test.bootPgRepo]: " + afterFinalize}},
		{"BindMany after Finalize", finalized, func(app *credo.App) { app.BindMany[bootPlugin, *bootAudit]() },
			[]string{"credo: App.BindMany[credo_test.bootPlugin, *credo_test.bootAudit]: " + afterFinalize}},

		// Each registration after shutdown.
		{"Provide after shutdown", shutDown,
			func(app *credo.App) { app.Provide(func() *bootUnbound { return nil }) },
			[]string{"credo: App.Provide[*credo_test.bootUnbound]: " + afterShutdown}},
		{"ProvideValue after shutdown", shutDown, func(app *credo.App) { app.ProvideValue(&bootUnbound{}) },
			[]string{"credo: App.ProvideValue[*credo_test.bootUnbound]: " + afterShutdown}},
		{"Alias after shutdown", shutDown, func(app *credo.App) { app.Alias[bootRepo, *bootPgRepo]() },
			[]string{"credo: App.Alias[credo_test.bootRepo, *credo_test.bootPgRepo]: " + afterShutdown}},
		{"BindMany after shutdown", shutDown, func(app *credo.App) { app.BindMany[bootPlugin, *bootAudit]() },
			[]string{"credo: App.BindMany[credo_test.bootPlugin, *credo_test.bootAudit]: " + afterShutdown}},
		{"route after shutdown", shutDown, func(app *credo.App) { app.GET("/late", noop) },
			[]string{"credo: App.GET " + frozenApp}},
		{"middleware after shutdown", shutDown,
			func(app *credo.App) { app.GlobalMiddleware(func(next credo.Handler) credo.Handler { return next }) },
			[]string{"credo: App.GlobalMiddleware " + frozenApp}},
		{"hook after shutdown", shutDown,
			func(app *credo.App) { app.OnStart(func(context.Context) error { return nil }) },
			[]string{"credo: App.OnStart " + frozenApp}},
		{"feature mount after shutdown", shutDown, func(app *credo.App) { app.UseHealth() },
			[]string{"credo: App.UseHealth " + frozenApp}},
		{"renderer after shutdown", shutDown,
			func(app *credo.App) { app.UseErrorRenderer(credo.RFC9457ErrorRenderer()) },
			[]string{"credo: App.UseErrorRenderer " + frozenApp}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := tt.setup(t)
			msg := mustPanicMessage(t, func() { tt.call(app) })
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Errorf("panic = %q\nwant it to contain %q", msg, want)
				}
			}
		})
	}
}

type bootUnboundPlugin struct{}

func (*bootUnboundPlugin) Name() string { return "unbound" }

// Types for the Finalize report: an order service that reaches a missing HTTP
// client through its payment client, a report service that misses its ledger,
// and two services that need each other.
type (
	bootOrders   struct{}
	bootPayments struct{}
	bootCycleA   struct{}
	bootCycleB   struct{}
	bootReports  struct{}
	bootLedger   struct{}
)

// TestBootstrap_FinalizeReportsTheWholeGraph is the Finalize phase of the
// three error phases: two missing dependencies and a cycle come back together,
// in registration order, each missing dependency with its full path.
func TestBootstrap_FinalizeReportsTheWholeGraph(t *testing.T) {
	app := mustNew(t)
	app.Provide(func(*bootPayments) *bootOrders { return &bootOrders{} })
	app.Provide(func(*http.Client) *bootPayments { return &bootPayments{} })
	app.Provide(func(*bootCycleB) *bootCycleA { return &bootCycleA{} })
	app.Provide(func(*bootCycleA) *bootCycleB { return &bootCycleB{} })
	app.Provide(func(*bootLedger) *bootReports { return &bootReports{} })

	err := app.Finalize()
	if err == nil {
		t.Fatal("Finalize = nil, want the graph errors")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("Finalize error %T does not join its errors", err)
	}
	got := joined.Unwrap()
	want := []string{
		"di: missing dependency: *credo_test.bootOrders → *credo_test.bootPayments → *http.Client " +
			"(not registered); provide *http.Client before Finalize",
		"di: circular dependency: *credo_test.bootCycleA → *credo_test.bootCycleB → *credo_test.bootCycleA; " +
			"remove one of these constructor parameters",
		"di: missing dependency: *credo_test.bootReports → *credo_test.bootLedger (not registered); " +
			"provide *credo_test.bootLedger before Finalize",
	}
	if len(got) != len(want) {
		t.Fatalf("Finalize returned %d errors, want %d:\n%v", len(got), len(want), err)
	}
	for i := range want {
		if got[i].Error() != want[i] {
			t.Errorf("error %d =\n%s\nwant\n%s", i, got[i], want[i])
		}
	}

	// The report is stable, and a Resolve after the failed Finalize returns it.
	if again := app.Finalize(); again == nil || again.Error() != err.Error() {
		t.Fatalf("second Finalize = %v, want the same report", again)
	}
	if _, rerr := app.Resolve[*bootOrders](); rerr == nil || !errors.Is(rerr, got[0]) {
		t.Fatalf("Resolve after a failed Finalize = %v, want the Finalize error", rerr)
	}
}

package credo_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo"
)

// Types for the implicit-Finalize tests.
type (
	trigDep     struct{ n int }
	trigLate    struct{}
	trigPlugin  interface{ Name() string }
	trigPluginA struct{}
	trigNeeds   struct{}
	trigMissing struct{}
	trigManaged struct{}
)

func (*trigPluginA) Name() string                   { return "a" }
func (*trigManaged) Shutdown(context.Context) error { return nil }

// lineAfter returns the file:line of the line after its call, the line a
// trigger is made on.
func lineAfter() string {
	_, file, line, _ := runtime.Caller(1)
	return fmt.Sprintf("%s:%d", file, line+1)
}

// newTriggerApp returns an App with *trigDep, *trigPluginA in the trigPlugin
// collection, and a config key, none of them resolved yet.
func newTriggerApp(t *testing.T) *credo.App {
	t.Helper()
	app := mustNew(t)
	app.Provide(func() *trigDep { return &trigDep{n: 1} })
	app.Provide(func() *trigPluginA { return &trigPluginA{} })
	app.BindMany[trigPlugin, *trigPluginA]()
	return app
}

func TestImplicitFinalize_ResolvingCallsResolveWithoutFinalize(t *testing.T) {
	calls := map[string]func(*credo.App) error{
		"Resolve": func(app *credo.App) error {
			dep, err := app.Resolve[*trigDep]()
			if err == nil && dep.n != 1 {
				err = errors.New("wrong value")
			}
			return err
		},
		"MustResolve": func(app *credo.App) error {
			if app.MustResolve[*trigDep]().n != 1 {
				return errors.New("wrong value")
			}
			return nil
		},
		"ResolveAll": func(app *credo.App) error {
			plugins, err := app.ResolveAll[trigPlugin]()
			if err == nil && len(plugins) != 1 {
				err = fmt.Errorf("got %d plugins", len(plugins))
			}
			return err
		},
		"MustResolveAll": func(app *credo.App) error {
			if n := len(app.MustResolveAll[trigPlugin]()); n != 1 {
				return fmt.Errorf("got %d plugins", n)
			}
			return nil
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(newTriggerApp(t)); err != nil {
				t.Fatalf("%s without Finalize: %v", name, err)
			}
		})
	}
}

func TestImplicitFinalize_ProbesLeaveTheContainerOpen(t *testing.T) {
	app := newTriggerApp(t)
	_ = app.Has[*trigDep]()
	_, _ = app.GetConfig[string]("server.host")
	_ = app.ConfigExists[string]("server.host")
	app.Provide(func() *trigLate { return &trigLate{} }) // the container is still open
	if _, err := app.Resolve[*trigLate](); err != nil {
		t.Fatalf("Resolve after the probes: %v", err)
	}
}

// lateRegistrations are the registrations a frozen container refuses, with
// the call each panic names.
var lateRegistrations = []struct {
	call     string
	register func(*credo.App)
}{
	{"App.Provide[*credo_test.trigLate]", func(app *credo.App) {
		app.Provide(func() *trigLate { return &trigLate{} })
	}},
	{"App.Provide[*credo_test.trigDep]", func(app *credo.App) {
		app.Provide(func() *trigDep { return &trigDep{} }, credo.Override())
	}},
	{"App.ProvideValue[*credo_test.trigLate]", func(app *credo.App) { app.ProvideValue(&trigLate{}) }},
	{"App.ProvideValue[*credo_test.trigDep]", func(app *credo.App) {
		app.ProvideValue(&trigDep{}, credo.Override())
	}},
	{"App.Alias[credo_test.trigPlugin, *credo_test.trigPluginA]", func(app *credo.App) {
		app.Alias[trigPlugin, *trigPluginA]()
	}},
	{"App.BindMany[credo_test.trigPlugin, *credo_test.trigPluginA]", func(app *credo.App) {
		app.BindMany[trigPlugin, *trigPluginA]()
	}},
	{"App.Manage[*credo_test.trigManaged]", func(app *credo.App) { app.Manage(&trigManaged{}) }},
	{`Workers.Continuous("late")`, func(app *credo.App) {
		app.Workers().Continuous("late", func(ctx context.Context) error { <-ctx.Done(); return nil })
	}},
	{`Workers.Scheduled("late")`, func(app *credo.App) {
		app.Workers().Scheduled("late", "@every 1h", func(context.Context) error { return nil })
	}},
}

// triggers finalize an App in each way there is; each returns the trigger
// the late registration's message names, without the callers its caller adds.
var triggers = map[string]func(*testing.T, *credo.App) string{
	"Resolve": func(t *testing.T, app *credo.App) string {
		site := lineAfter()
		_, _ = app.Resolve[*trigDep]()
		return "Resolve[*credo_test.trigDep] at " + site
	},
	"MustResolve": func(t *testing.T, app *credo.App) string {
		site := lineAfter()
		_ = app.MustResolve[*trigDep]()
		return "MustResolve[*credo_test.trigDep] at " + site
	},
	"ResolveAll": func(t *testing.T, app *credo.App) string {
		site := lineAfter()
		_, _ = app.ResolveAll[trigPlugin]()
		return "ResolveAll[credo_test.trigPlugin] at " + site
	},
	"MustResolveAll": func(t *testing.T, app *credo.App) string {
		site := lineAfter()
		_ = app.MustResolveAll[trigPlugin]()
		return "MustResolveAll[credo_test.trigPlugin] at " + site
	},
	"Finalize": func(t *testing.T, app *credo.App) string {
		site := lineAfter()
		if err := app.Finalize(); err != nil {
			t.Fatal(err)
		}
		return "Finalize at " + site
	},
	"Start": func(t *testing.T, app *credo.App) string {
		t.Cleanup(func() { _ = app.Shutdown(context.WithoutCancel(t.Context())) })
		site := lineAfter()
		if err := app.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		return "Start at " + site
	},
}

func TestImplicitFinalize_LateRegistrationNamesTheTrigger(t *testing.T) {
	for name, trigger := range triggers {
		for _, reg := range lateRegistrations {
			t.Run(name+"/"+reg.call, func(t *testing.T) {
				app := newTriggerApp(t)
				caller := lineAfter()
				by := trigger(t, app)
				want := fmt.Sprintf("credo: %s: called after the container was finalized by %s (called from %s); "+
					"register before that call", reg.call, by, caller)
				if msg := mustPanicMessage(t, func() { reg.register(app) }); msg != want {
					t.Errorf("message = %q\nwant      %q", msg, want)
				}
			})
		}
	}
}

func TestImplicitFinalize_TriggerAtTheTestFunctionHasNoCallers(t *testing.T) {
	app := newTriggerApp(t)
	site := lineAfter()
	_, _ = app.Resolve[*trigDep]()
	want := "credo: App.Provide[*credo_test.trigLate]: called after the container was finalized by " +
		"Resolve[*credo_test.trigDep] at " + site + "; register before that call"
	msg := mustPanicMessage(t, func() { app.Provide(func() *trigLate { return &trigLate{} }) })
	if msg != want {
		t.Errorf("message = %q\nwant      %q", msg, want)
	}
}

func TestImplicitFinalize_FirstServeHTTPHasNoSite(t *testing.T) {
	for _, reg := range lateRegistrations {
		t.Run(reg.call, func(t *testing.T) {
			app := newTriggerApp(t)
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			want := "credo: " + reg.call + ": called after the container was finalized by the first ServeHTTP " +
				"call; register before the App serves"
			if msg := mustPanicMessage(t, func() { reg.register(app) }); msg != want {
				t.Errorf("message = %q\nwant      %q", msg, want)
			}
		})
	}
}

// resolveInHelper is a helper library's call that makes the first Resolve;
// it returns that call's file:line.
func resolveInHelper(app *credo.App) string {
	site := lineAfter()
	_, _ = app.Resolve[*trigDep]()
	return site
}

// wireThroughHelper calls resolveInHelper; it returns its own call's line.
func wireThroughHelper(app *credo.App) (helper, wiring string) {
	wiring = lineAfter()
	helper = resolveInHelper(app)
	return helper, wiring
}

func TestImplicitFinalize_HelperSiteAndItsCallers(t *testing.T) {
	app := newTriggerApp(t)
	test := lineAfter()
	helper, wiring := wireThroughHelper(app)
	want := "credo: App.Provide[*credo_test.trigLate]: called after the container was finalized by " +
		"Resolve[*credo_test.trigDep] at " + helper + " (called from " + wiring + ", " + test + "); " +
		"register before that call"
	msg := mustPanicMessage(t, func() { app.Provide(func() *trigLate { return &trigLate{} }) })
	if msg != want {
		t.Fatalf("message = %q\nwant      %q", msg, want)
	}
	if strings.Contains(msg, "testing.go") || strings.Contains(msg, "/src/runtime/") {
		t.Errorf("message lists a standard-library frame: %q", msg)
	}
}

func TestImplicitFinalize_AtMostThreeCallers(t *testing.T) {
	app := newTriggerApp(t)
	var helper, first string
	var nest func(depth int)
	nest = func(depth int) {
		if depth > 0 {
			nest(depth - 1)
			return
		}
		first = lineAfter()
		helper = resolveInHelper(app)
	}
	nest(4) // five frames of the test between the helper and testing.tRunner
	msg := mustPanicMessage(t, func() { app.Provide(func() *trigLate { return &trigLate{} }) })
	prefix := "Resolve[*credo_test.trigDep] at " + helper + " (called from " + first + ", "
	_, rest, ok := strings.Cut(msg, prefix)
	if !ok {
		t.Fatalf("message = %q\nwant it to contain %q", msg, prefix)
	}
	callers, _, _ := strings.Cut(rest, ")")
	if n := len(strings.Split(callers, ", ")) + 1; n != 3 {
		t.Fatalf("message = %q lists %d callers, want 3", msg, n)
	}
}

func TestImplicitFinalize_FailedFinalizeIsTheTriggeringCallsError(t *testing.T) {
	newApp := func(t *testing.T) *credo.App {
		app := mustNew(t)
		app.Provide(func(*trigMissing) *trigNeeds { return &trigNeeds{} })
		return app
	}
	const report = "missing dependency: *credo_test.trigNeeds → *credo_test.trigMissing (not registered)"

	app := newApp(t)
	_, first := app.Resolve[*trigNeeds]()
	if first == nil || !strings.Contains(first.Error(), report) {
		t.Fatalf("first Resolve = %v, want the Finalize report %q", first, report)
	}
	_, later := app.Resolve[*trigNeeds]()
	if later == nil || later.Error() != first.Error() {
		t.Fatalf("later Resolve = %v, want %v", later, first)
	}
	if err := app.Finalize(); err == nil || !strings.Contains(err.Error(), report) {
		t.Fatalf("Finalize after the failed implicit one = %v, want the same report", err)
	}

	msg := mustPanicMessage(t, func() { newApp(t).MustResolve[*trigNeeds]() })
	if !strings.Contains(msg, report) {
		t.Fatalf("MustResolve panic = %q, want the Finalize report %q", msg, report)
	}
}

func TestImplicitFinalize_RoutesAfterTheFirstResolveServe(t *testing.T) {
	app := newTriggerApp(t)
	dep := app.MustResolve[*trigDep]()
	app.GET("/dep", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, fmt.Sprint(dep.n))
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dep", nil))
	if w.Code != http.StatusOK || w.Body.String() != "1" {
		t.Fatalf("GET /dep = %d %q, want 200 \"1\"", w.Code, w.Body.String())
	}
}

func TestImplicitFinalize_ConcurrentFirstResolvesFinalizeOnce(t *testing.T) {
	app := mustNew(t)
	var built atomic.Int32
	app.Provide(func() *trigDep {
		built.Add(1)
		return &trigDep{n: 1}
	})
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			<-start
			if _, err := app.Resolve[*trigDep](); err != nil {
				errs <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Resolve: %v", err)
	}
	if got := built.Load(); got != 1 {
		t.Fatalf("constructor ran %d times, want 1", got)
	}
	msg := mustPanicMessage(t, func() { app.Provide(func() *trigLate { return &trigLate{} }) })
	if !strings.Contains(msg, "called after the container was finalized by Resolve[*credo_test.trigDep] at ") {
		t.Fatalf("message = %q, want one of the Resolve calls as the trigger", msg)
	}
}

func TestImplicitFinalize_EntryPointIsTheTrigger(t *testing.T) {
	app := newTriggerApp(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var msg string
	app.OnStart(func(context.Context) error {
		defer cancel()
		msg = mustPanicMessage(t, func() { app.Provide(func() *trigLate { return &trigLate{} }) })
		return nil
	})
	site := lineAfter()
	if err := app.ServeContext(ctx, ln); err != nil {
		t.Fatalf("ServeContext = %v", err)
	}
	want := "credo: App.Provide[*credo_test.trigLate]: called after the container was finalized by " +
		"ServeContext at " + site + "; register before that call"
	if msg != want {
		t.Errorf("message = %q\nwant      %q", msg, want)
	}
}

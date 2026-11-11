package credo_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// Types for the binding-type tests.
type (
	bindGreeter    interface{ Greet() string }
	bindEnglish    struct{}
	bindTurkish    struct{}
	bindEnglishErr struct{}
)

func (*bindEnglish) Greet() string    { return "hello" }
func (*bindTurkish) Greet() string    { return "merhaba" }
func (*bindEnglishErr) Greet() string { return "hello" }

// TestProvide_BindsTheConstructorResultType: Provide binds the first result
// type of func(...) T and of func(...) (T, error); a constructor declared to
// return an interface binds the interface, and nothing else.
func TestProvide_BindsTheConstructorResultType(t *testing.T) {
	app := mustNew(t)
	app.Provide(func() *bindEnglish { return &bindEnglish{} })
	app.Provide(func() (*bindEnglishErr, error) { return &bindEnglishErr{}, nil })
	app.Provide(func() bindGreeter { return &bindTurkish{} })
	if !app.Has[*bindEnglish]() || !app.Has[*bindEnglishErr]() || !app.Has[bindGreeter]() {
		t.Fatal("Has = false for a type a constructor returns")
	}
	if app.Has[*bindTurkish]() {
		t.Error("Has[*bindTurkish] = true; a constructor declared to return an interface binds the interface only")
	}
	mustFinalize(t, app)
	if got := app.MustResolve[bindGreeter]().Greet(); got != "merhaba" {
		t.Errorf("Resolve[bindGreeter] greets %q, want merhaba", got)
	}
	if _, err := app.Resolve[*bindEnglishErr](); err != nil {
		t.Errorf("Resolve[*bindEnglishErr] = %v", err)
	}
}

// quietThing hides the Shutdown of *stopOnly and nothing else.
type quietThing interface{ isQuietThing() }

func (*stopOnly) isQuietThing() {}

// TestHiddenCapability_ProvideValuePanics: a ProvideValue whose type hides
// its value's Start or Ready panics at the call, naming the binding, the
// methods it hides and the remedies; credo.Borrowed() is exempt, as is a
// view, and a type that hides only Shutdown is torn down as before.
func TestHiddenCapability_ProvideValuePanics(t *testing.T) {
	t.Run("hidden Start and Ready", func(t *testing.T) {
		app := mustNew(t)
		msg := mustPanicMessage(t, func() {
			app.ProvideValue[namedThing](&partA{&part{name: "hidden", j: &journal{}}})
		})
		for _, want := range []string{
			"credo: App.ProvideValue[credo_test.namedThing]: the binding's type credo_test.namedThing hides " +
				"Shutdown, Start and Ready of its value, a *credo_test.partA",
			"bind the value as *credo_test.partA and the interface with " +
				"app.Alias[credo_test.namedThing, *credo_test.partA](), or give credo.View() when another " +
				"registration owns the value, or credo.Borrowed() when the caller starts and stops it",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic = %q\nwant it to contain %q", msg, want)
			}
		}
	})
	t.Run("hidden Ready only", func(t *testing.T) {
		app := mustNew(t)
		expectPanicContaining(t, "credo: App.ProvideValue[credo.Component]: the binding's type "+
			"credo.Component hides Ready of its value, a *credo_test.readyOnly", func() {
			app.ProvideValue[credo.Component](&readyOnly{})
		})
	})
	t.Run("borrowed", func(t *testing.T) {
		j := &journal{}
		app := mustNew(t)
		app.ProvideValue[namedThing](&partA{&part{name: "borrowed", j: j}}, credo.Borrowed())
		startComponents(t, app)
		if err := shutdownApp(t, app, 2*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		if got := j.list(); len(got) != 0 {
			t.Errorf("events = %v; a borrowed value is neither started nor stopped", got)
		}
	})
	t.Run("hidden Shutdown only", func(t *testing.T) {
		j := &journal{}
		app := mustNew(t)
		app.ProvideValue[quietThing](&stopOnly{j: j, name: "quiet"})
		startComponents(t, app)
		if err := shutdownApp(t, app, 2*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		if want := []string{"stop:quiet"}; !slices.Equal(j.list(), want) {
			t.Errorf("events = %v, want %v (a Shutdown found on the value)", j.list(), want)
		}
	})
}

// TestHiddenCapability_ConstructorFailsTheStart: a constructor declared to
// return an interface whose value has Start or Ready the interface hides
// fails once its value is built — at the start, or at the Resolve that
// builds it — naming the binding, the methods and the remedy; a constructor
// handed to Manage follows the same rule.
func TestHiddenCapability_ConstructorFailsTheStart(t *testing.T) {
	const remedy = "declare the constructor to return *credo_test.partA and bind credo.Component with " +
		"app.Alias[credo.Component, *credo_test.partA]()"
	t.Run("at the start", func(t *testing.T) {
		j := &journal{}
		app := mustNew(t)
		app.Provide(func() credo.Component { return &partA{&part{name: "hidden", j: j}} })
		app.Provide(func(credo.Component) *partB { return &partB{&part{name: "user", j: j}} })
		err := app.Start(t.Context())
		if err == nil {
			t.Fatal("App.Start() = nil, want the hidden Start refused")
		}
		for _, want := range []string{
			"the constructor of credo.Component returns a *credo_test.partA, whose Start and Ready the " +
				"binding's type hides",
			remedy,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("App.Start() = %v\nwant it to contain %q", err, want)
			}
		}
		if got := j.list(); slices.Contains(got, "start:hidden") {
			t.Errorf("events = %v; a refused value is never started", got)
		}
	})
	t.Run("at Resolve", func(t *testing.T) {
		app := mustNew(t)
		app.Provide(func() credo.Component { return &partA{&part{name: "hidden", j: &journal{}}} })
		mustFinalize(t, app)
		if _, err := app.Resolve[credo.Component](); err == nil || !strings.Contains(err.Error(), remedy) {
			t.Errorf("Resolve() = %v, want it to contain %q", err, remedy)
		}
	})
	t.Run("hidden Ready", func(t *testing.T) {
		app := mustNew(t)
		app.Provide(func() credo.Component { return &readyOnly{} })
		mustFinalize(t, app)
		_, err := app.Resolve[credo.Component]()
		if want := "returns a *credo_test.readyOnly, whose Ready the binding's type hides"; err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("Resolve() = %v, want it to contain %q", err, want)
		}
	})
	t.Run("Manage", func(t *testing.T) {
		app := mustNew(t)
		app.Manage(func() credo.Component { return &partA{&part{name: "hidden", j: &journal{}}} })
		err := app.Start(t.Context())
		want := "the constructor handed to Manage(credo.Component) returns a *credo_test.partA, whose Start " +
			"and Ready its result type credo.Component hides, and the App plans Start, Ready and the ingress " +
			"tier only from that type; declare the constructor to return *credo_test.partA"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("App.Start() = %v\nwant it to contain %q", err, want)
		}
	})
}

// overridable is a component the guide's pattern binds through an alias.
type overridable struct{ starts, builds *atomic.Int32 }

func (o *overridable) Greet() string                  { return "real" }
func (o *overridable) Start(context.Context) error    { o.starts.Add(1); return nil }
func (o *overridable) Shutdown(context.Context) error { return nil }

type fakeGreeter struct{}

func (fakeGreeter) Greet() string { return "fake" }

// greetUser consumes the aliased interface.
type greetUser struct{ g bindGreeter }

// TestAlias_OverrideReplacesTheAlias: an override of an aliased interface
// replaces the alias; Finalize drops the type it pointed at when nothing
// else reaches it, so it is neither built nor started and resolving it
// names the override. A constructor parameter, another alias or a BindMany
// entry naming the type keeps it.
func TestAlias_OverrideReplacesTheAlias(t *testing.T) {
	setup := func(t *testing.T, extra func(app *credo.App)) (*credo.App, *atomic.Int32, *atomic.Int32) {
		var starts, builds atomic.Int32
		app := mustNew(t)
		app.Provide(func() *overridable { builds.Add(1); return &overridable{starts: &starts, builds: &builds} })
		app.Alias[bindGreeter, *overridable]()
		app.Provide(func(g bindGreeter) *greetUser { return &greetUser{g: g} })
		if extra != nil {
			extra(app)
		}
		app.ProvideValue[bindGreeter](fakeGreeter{}, credo.Override())
		startComponents(t, app)
		t.Cleanup(func() { _ = shutdownApp(t, app, 2*time.Second) })
		return app, &starts, &builds
	}

	t.Run("dropped", func(t *testing.T) {
		app, starts, builds := setup(t, nil)
		if got := app.MustResolve[*greetUser]().g.Greet(); got != "fake" {
			t.Errorf("consumer greets %q, want the override", got)
		}
		if builds.Load() != 0 || starts.Load() != 0 {
			t.Errorf("built %d, started %d; a type the override left unreached is dropped", builds.Load(),
				starts.Load())
		}
		_, err := app.Resolve[*overridable]()
		want := "di: Resolve[*credo_test.overridable]: not registered: Finalize dropped it, since " +
			"ProvideValue[credo_test.bindGreeter] with credo.Override() replaced " +
			"app.Alias[credo_test.bindGreeter, *credo_test.overridable]() and nothing else uses " +
			"*credo_test.overridable"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve[*overridable] = %v\nwant it to contain %q", err, want)
		}
	})
	type keeper struct{}
	for _, tc := range []struct {
		name  string
		extra func(app *credo.App)
	}{
		{"kept by a constructor", func(app *credo.App) {
			app.Provide(func(*overridable) *keeper { return &keeper{} })
		}},
		{"kept by another alias", func(app *credo.App) {
			app.Alias[interface{ Start(context.Context) error }, *overridable]()
		}},
		{"kept by BindMany", func(app *credo.App) { app.BindMany[bindGreeter, *overridable]() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, starts, builds := setup(t, tc.extra)
			if got := app.MustResolve[*greetUser]().g.Greet(); got != "fake" {
				t.Errorf("consumer greets %q, want the override", got)
			}
			if builds.Load() != 1 || starts.Load() != 1 {
				t.Errorf("built %d, started %d; a type something else reaches is kept", builds.Load(),
					starts.Load())
			}
		})
	}
}

// TestAlias_OverridePointsAtAnotherType: Alias with credo.Override() points
// an interface at another type, replacing its alias or its direct binding.
func TestAlias_OverridePointsAtAnotherType(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first func(app *credo.App)
	}{
		{"an alias", func(app *credo.App) {
			app.Provide(func() *bindEnglish { return &bindEnglish{} })
			app.Alias[bindGreeter, *bindEnglish]()
		}},
		{"a direct binding", func(app *credo.App) {
			app.ProvideValue[bindGreeter](&bindEnglish{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t)
			tc.first(app)
			app.Provide(func() *bindTurkish { return &bindTurkish{} })
			app.Alias[bindGreeter, *bindTurkish](credo.Override())
			mustFinalize(t, app)
			if got := app.MustResolve[bindGreeter]().Greet(); got != "merhaba" {
				t.Errorf("Resolve[bindGreeter] greets %q, want the new alias's target", got)
			}
		})
	}
}

// TestAlias_OverrideKeepsReadiness: the override's fake has no readiness of
// its own, and the dropped type's Ready is never asked.
func TestAlias_OverrideKeepsReadiness(t *testing.T) {
	j := &journal{}
	app := mustNew(t)
	app.Provide(func() *partA { return &partA{&part{name: "real", j: j, readyErr: errors.New("never asked")}} })
	app.Alias[namedThing, *partA]()
	app.ProvideValue[namedThing](fakeNamed{}, credo.Override())
	app.UseHealth()
	startComponents(t, app)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/ready = %d %s; the dropped type's Ready must not be asked", rec.Code, rec.Body)
	}
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := j.list(); len(got) != 0 {
		t.Errorf("events = %v; the dropped type is never built", got)
	}
}

type fakeNamed struct{}

func (fakeNamed) isNamedThing() {}

// TestHiddenCapability_ViewOfAHeldResource: an interface declared a view of
// a resource another registration owns is accepted, bound by ProvideValue or
// returned by a constructor, before the owner or after it, and the resource
// is started, readied and stopped once, through its owner; the type a view
// binds hides nothing the App plans. Undeclared, the interface claims the
// resource: after the owner it is refused as a second claim, naming
// app.Alias and credo.View(); bound first, ProvideValue panics at the
// hidden capabilities, naming both remedies.
func TestHiddenCapability_ViewOfAHeldResource(t *testing.T) {
	for _, tc := range []struct {
		name string
		view func(app *credo.App, p *partA)
	}{
		{"ProvideValue", func(app *credo.App, p *partA) { app.ProvideValue[namedThing](p, credo.View()) }},
		{"constructor", func(app *credo.App, p *partA) {
			app.Provide(func() namedThing { return p }, credo.View())
		}},
		{"ProvideValue, view first", func(app *credo.App, p *partA) {
			app.ProvideValue[namedThing](p, credo.View())
		}},
		{"constructor, view first", func(app *credo.App, p *partA) {
			app.Provide(func() namedThing { return p }, credo.View())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &journal{}
			p := &partA{&part{name: "held", j: j}}
			app := mustNew(t)
			if strings.HasSuffix(tc.name, "view first") {
				tc.view(app, p)
				app.ProvideValue(p)
			} else {
				app.ProvideValue(p)
				tc.view(app, p)
			}
			app.UseHealth()
			startComponents(t, app)
			if _, err := app.Resolve[namedThing](); err != nil {
				t.Fatalf("Resolve[namedThing]() = %v; the view of a held resource is accepted", err)
			}
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
			if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), `"status":"up"`) != 2 {
				t.Errorf("/ready = %d %s, want 200 with one entry for the resource", rec.Code, rec.Body)
			}
			if err := shutdownApp(t, app, 2*time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
			if want := []string{"start:held", "stop:held"}; !slices.Equal(j.list(), want) {
				t.Errorf("events = %v, want %v (one start and one stop, through the holder)", j.list(), want)
			}
		})
	}
	t.Run("undeclared, owner first", func(t *testing.T) {
		p := &partA{&part{name: "first", j: &journal{}}}
		app := mustNew(t)
		app.ProvideValue(p)
		const want = "credo: App.ProvideValue[credo_test.namedThing]: credo_test.namedThing holds the resource " +
			"that the ProvideValue binding of *credo_test.partA owns, and a resource has one owner; declare " +
			"credo_test.namedThing a view: bind it with app.Alias[credo_test.namedThing, *credo_test.partA](), " +
			"or give credo.View() to its registration"
		if got := mustPanicMessage(t, func() { app.ProvideValue[namedThing](p) }); got != want {
			t.Fatalf("panic = %q\nwant    %q", got, want)
		}
	})
	t.Run("undeclared constructor, owner first", func(t *testing.T) {
		p := &partA{&part{name: "first", j: &journal{}}}
		app := mustNew(t)
		app.ProvideValue(p)
		app.Provide(func() namedThing { return p })
		startComponents(t, app)
		_, err := app.Resolve[namedThing]()
		const want = "credo_test.namedThing holds the resource that the ProvideValue binding of " +
			"*credo_test.partA owns, and a resource has one owner; declare credo_test.namedThing a view: bind " +
			"it with app.Alias[credo_test.namedThing, *credo_test.partA](), or give credo.View() to its " +
			"registration"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Resolve() = %v\nwant it to contain %q", err, want)
		}
	})
	t.Run("undeclared, view bound first", func(t *testing.T) {
		p := &partA{&part{name: "first", j: &journal{}}}
		app := mustNew(t)
		const want = "credo: App.ProvideValue[credo_test.namedThing]: the binding's type credo_test.namedThing " +
			"hides Shutdown, Start and Ready of its value, a *credo_test.partA, and the App plans Start, Ready " +
			"and the ingress tier only from the binding's type; bind the value as *credo_test.partA and the " +
			"interface with app.Alias[credo_test.namedThing, *credo_test.partA](), or give credo.View() when " +
			"another registration owns the value, or credo.Borrowed() when the caller starts and stops it"
		if got := mustPanicMessage(t, func() { app.ProvideValue[namedThing](p) }); got != want {
			t.Fatalf("panic = %q\nwant    %q", got, want)
		}
	})
}

// startView shows Start and hides the Shutdown of what it binds.
type startView interface{ Start(context.Context) error }

// startStopper has Start and Shutdown.
type startStopper struct {
	name string
	j    *journal
}

func (s *startStopper) Start(context.Context) error    { s.j.add("start:" + s.name); return nil }
func (s *startStopper) Shutdown(context.Context) error { s.j.add("stop:" + s.name); return nil }

// startOnlyValue has Start and no Shutdown: it is not a component.
type startOnlyValue struct{ j *journal }

func (s *startOnlyValue) Start(context.Context) error { s.j.add("start:only"); return nil }

// TestHiddenCapability_HiddenShutdownStillStarts: a binding whose type shows
// Start and hides the Shutdown its value has is a component — the hidden
// Shutdown makes it one, and its teardown follows it — and is started;
// nothing is refused. A value with Start and no Shutdown is not a component
// and is never started.
func TestHiddenCapability_HiddenShutdownStillStarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		bind func(app *credo.App, v *startStopper)
	}{
		{"ProvideValue", func(app *credo.App, v *startStopper) { app.ProvideValue[startView](v) }},
		{"constructor", func(app *credo.App, v *startStopper) {
			app.Provide(func() startView { return v })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &journal{}
			app := mustNew(t)
			tc.bind(app, &startStopper{name: "view", j: j})
			startComponents(t, app)
			if err := shutdownApp(t, app, 2*time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
			if want := []string{"start:view", "stop:view"}; !slices.Equal(j.list(), want) {
				t.Errorf("events = %v, want %v", j.list(), want)
			}
		})
	}
	t.Run("no Shutdown", func(t *testing.T) {
		j := &journal{}
		app := mustNew(t)
		app.ProvideValue[startView](&startOnlyValue{j: j})
		startComponents(t, app)
		if err := shutdownApp(t, app, 2*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		if got := j.list(); len(got) != 0 {
			t.Errorf("events = %v; a value without Shutdown is not a component and is never started", got)
		}
	})
}

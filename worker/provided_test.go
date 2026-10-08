package worker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// providedStub is a worker constructed by DI in these tests. The type
// parameter only mints distinct types, so a test can bind several.
type providedStub[K any] struct {
	runs    atomic.Int32
	running chan struct{}
}

func newProvidedStub[K any]() *providedStub[K] {
	return &providedStub[K]{running: make(chan struct{}, 1)}
}

func (w *providedStub[K]) Run(ctx context.Context) error {
	w.runs.Add(1)
	select {
	case w.running <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil
}

type (
	providedKindA struct{}
	providedKindB struct{}
)

// providedRegister registers T under name as kind, with RunOnStart for a
// scheduled worker so that it runs at once.
func providedRegister[T Worker](s *Supervisor, kind Kind, name string) {
	if kind == KindScheduled {
		s.ScheduledProvided[T](name, "@every 1h", ScheduledConfig{RunOnStart: true})
		return
	}
	s.ContinuousProvided[T](name)
}

func providedAwaitRun[K any](t *testing.T, w *providedStub[K]) {
	t.Helper()
	select {
	case <-w.running:
	case <-time.After(5 * time.Second):
		t.Fatal("the DI-provided instance did not run")
	}
}

func TestProvided_ProvideBeforeOrAfterRegistration(t *testing.T) {
	for _, kind := range []Kind{KindContinuous, KindScheduled} {
		for _, order := range []string{"provide-first", "register-first"} {
			t.Run(string(kind)+"/"+order, func(t *testing.T) {
				app := newTestApp(t)
				s := Use(app)
				provide := func() { app.Provide[*providedStub[providedKindA]](newProvidedStub[providedKindA]) }
				register := func() { providedRegister[*providedStub[providedKindA]](s, kind, "provided") }
				if order == "provide-first" {
					provide()
					register()
				} else {
					register()
					provide()
				}
				startApp(t, app)

				w, err := app.Resolve[*providedStub[providedKindA]]()
				if err != nil {
					t.Fatal(err)
				}
				providedAwaitRun(t, w)
				if got := w.runs.Load(); got != 1 {
					t.Fatalf("runs = %d, want 1 (the container's singleton ran once)", got)
				}
				if info, _ := s.Lookup("provided"); info.Kind != kind {
					t.Errorf("Kind = %s, want %s", info.Kind, kind)
				}
			})
		}
	}
}

// providedAliased is an application-level interface that a concrete worker
// is bound to through app.Alias.
type providedAliased interface {
	Worker
	aliased()
}

func (*providedStub[K]) aliased() {}

func TestProvided_InterfaceBoundWithAlias(t *testing.T) {
	for _, kind := range []Kind{KindContinuous, KindScheduled} {
		t.Run(string(kind), func(t *testing.T) {
			app := newTestApp(t)
			app.Provide[*providedStub[providedKindB]](newProvidedStub[providedKindB])
			app.Alias[providedAliased, *providedStub[providedKindB]]()
			providedRegister[providedAliased](Use(app), kind, "aliased")
			startApp(t, app)

			w, err := app.Resolve[*providedStub[providedKindB]]()
			if err != nil {
				t.Fatal(err)
			}
			providedAwaitRun(t, w)
		})
	}
}

// providedOrder records lifecycle events in the order they happen.
type providedOrder struct {
	mu     sync.Mutex
	events []string
}

func (o *providedOrder) add(event string) {
	o.mu.Lock()
	o.events = append(o.events, event)
	o.mu.Unlock()
}

func (o *providedOrder) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

// providedDep is a component a provided worker depends on.
type providedDep struct{ order *providedOrder }

func (d *providedDep) Start(context.Context) error {
	d.order.add("dep start")
	return nil
}

func (d *providedDep) Shutdown(context.Context) error {
	d.order.add("dep shutdown")
	return nil
}

// providedOwner is a provided worker that is itself a component: the App
// shuts it down after the worker has stopped.
type providedOwner struct {
	order   *providedOrder
	running chan struct{}
}

func (w *providedOwner) Run(ctx context.Context) error {
	w.order.add("run")
	close(w.running)
	<-ctx.Done()
	w.order.add("run returned")
	return nil
}

func (w *providedOwner) Shutdown(context.Context) error {
	w.order.add("T shutdown")
	return nil
}

func TestProvided_BuiltAfterItsDependenciesStartAndStoppedBeforeThem(t *testing.T) {
	for _, kind := range []Kind{KindContinuous, KindScheduled} {
		t.Run(string(kind), func(t *testing.T) {
			app := newTestApp(t)
			order := &providedOrder{}
			owner := &providedOwner{order: order, running: make(chan struct{})}
			app.Provide[*providedDep](func() *providedDep { return &providedDep{order: order} })
			app.Provide[*providedOwner](func(*providedDep) *providedOwner {
				order.add("T built")
				return owner
			})
			providedRegister[*providedOwner](Use(app), kind, "owner")

			if err := app.Start(t.Context()); err != nil {
				t.Fatalf("App.Start() = %v", err)
			}
			lifecycleAwait(t, owner.running, "the worker to run")
			ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
			defer cancel()
			if err := app.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}

			want := []string{"dep start", "T built", "run", "run returned", "T shutdown", "dep shutdown"}
			if got := order.get(); !slices.Equal(got, want) {
				t.Errorf("order = %q\nwant      %q", got, want)
			}
		})
	}
}

func TestProvided_MissingBindingFailsFinalize(t *testing.T) {
	for _, kind := range []Kind{KindContinuous, KindScheduled} {
		t.Run(string(kind), func(t *testing.T) {
			app := newTestApp(t)
			providedRegister[*providedStub[providedKindA]](Use(app), kind, "orphan")
			err := app.Finalize()
			if err == nil {
				t.Fatal("Finalize() = nil, want the missing binding")
			}
			for _, want := range []string{"worker:orphan", "*worker.providedStub[", "providedKindA]"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Finalize() = %v\nwant it to contain %q", err, want)
				}
			}
		})
	}
}

// providedRecordingDep is a dependency the failing constructors take, so it
// is built — and started — before they fail.
type providedRecordingDep struct{ started, shutdowns atomic.Int32 }

func (d *providedRecordingDep) Start(context.Context) error {
	d.started.Add(1)
	return nil
}

func (d *providedRecordingDep) Shutdown(context.Context) error {
	d.shutdowns.Add(1)
	return nil
}

func TestProvided_ConstructionFailureFailsStart(t *testing.T) {
	type stub = providedStub[providedKindA]
	cases := []struct {
		name    string
		ctor    any
		outcome credo.LifecycleOutcome
		want    string
	}{
		{
			name:    "constructor error",
			ctor:    func(*providedRecordingDep) (*stub, error) { return nil, errors.New("constructor failed") },
			outcome: credo.OutcomeFailed,
			want:    "constructor failed",
		},
		{
			name:    "constructor panic",
			ctor:    func(*providedRecordingDep) *stub { panic("constructor boom") },
			outcome: credo.OutcomePanicked,
			want:    "constructor boom",
		},
		{
			name:    "nil value",
			ctor:    func(*providedRecordingDep) *stub { return nil },
			outcome: credo.OutcomeFailed,
			want:    "resolved to nil",
		},
	}
	for _, kind := range []Kind{KindContinuous, KindScheduled} {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				capture := newLogCapture()
				app := newTestApp(t, credo.WithLogger(capture.logger()))
				dep := &providedRecordingDep{}
				app.Provide[*providedRecordingDep](func() *providedRecordingDep { return dep })
				app.Provide[*stub](tc.ctor)
				s := Use(app)
				providedRegister[*stub](s, kind, "broken")

				err := app.Start(t.Context())
				lerr, ok := errors.AsType[*credo.LifecycleError](err)
				if !ok {
					t.Fatalf("App.Start() = %v, want a *credo.LifecycleError", err)
				}
				entry, n := lifecycleEntry(lerr, "worker:broken")
				if n != 1 || entry.Phase != credo.PhaseStart || entry.Outcome != tc.outcome {
					t.Fatalf("App.Start() = %v, want worker:broken start %s, once", err, tc.outcome)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("App.Start() = %v\nwant it to contain %q", err, tc.want)
				}
				if tc.outcome == credo.OutcomePanicked {
					if _, ok := errors.AsType[*credo.DIPanicError](err); !ok {
						t.Errorf("App.Start() = %v, want the constructor panic kept as *credo.DIPanicError", err)
					}
				}

				// The rollback stops what was built.
				if dep.started.Load() != 1 || dep.shutdowns.Load() != 1 {
					t.Errorf("dependency started %d and shut down %d times, want 1 and 1 (rolled back)",
						dep.started.Load(), dep.shutdowns.Load())
				}
				if got := app.State(); got != "stopped" {
					t.Errorf("State() = %q, want stopped", got)
				}
				if info, _ := s.Lookup("broken"); info.Status != StatusPending {
					t.Errorf("broken = %s, want pending (its component never started)", info.Status)
				}
				if lines := capture.withMessage("worker started"); len(lines) != 0 {
					t.Errorf("worker started logged %d times, want none", len(lines))
				}
			})
		}
	}
}

// providedIngressDep is an ingress component a provided worker depends on.
type providedIngressDep struct{}

func (*providedIngressDep) Shutdown(context.Context) error { return nil }

func TestProvided_InternalWorkerOnIngressComponentFailsFinalize(t *testing.T) {
	type stub = providedStub[providedKindA]
	newApp := func(t *testing.T) *credo.App {
		app := newTestApp(t)
		app.Provide[*providedIngressDep](func() *providedIngressDep { return &providedIngressDep{} }, credo.Ingress())
		app.Provide[*stub](func(*providedIngressDep) *stub { return newProvidedStub[providedKindA]() })
		return app
	}

	app := newApp(t)
	Use(app).ContinuousProvided[*stub]("consumer")
	err := app.Finalize()
	if err == nil {
		t.Fatal("Finalize() = nil, want the internal worker's dependency on an ingress component")
	}
	for _, want := range []string{
		"worker:consumer", // the worker
		"the ingress component *worker.providedIngressDep", // the ingress component
		"worker:consumer) → *worker.providedStub[",         // the path, through T
		"] → *worker.providedIngressDep",                   // ... to the ingress component
		"split *worker.providedIngressDep",                 // remedy: split the ingress component
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Finalize() = %v\nwant it to contain %q", err, want)
		}
	}

	// Each remedy that keeps the dependency is accepted: the worker declared
	// ingress, and a scheduled worker, ingress by default.
	t.Run("continuous worker declared ingress", func(t *testing.T) {
		app := newApp(t)
		Use(app).ContinuousProvided[*stub]("consumer", ContinuousConfig{Tier: credo.TierIngress})
		finalize(t, app)
	})
	t.Run("scheduled worker", func(t *testing.T) {
		app := newApp(t)
		Use(app).ScheduledProvided[*stub]("report", "@every 1h")
		finalize(t, app)
	})

	// The remedy names the worker's own setting, its Tier field: a worker
	// registration takes no RegistrationOption.
	t.Run("the remedy names the worker's Tier setting", func(t *testing.T) {
		if !strings.Contains(err.Error(), "Tier: credo.TierIngress") {
			t.Errorf("Finalize() = %v\nwant the remedy Tier: credo.TierIngress", err)
		}
	})
}

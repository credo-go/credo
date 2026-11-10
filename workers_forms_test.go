package credo_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// formsDep is what the run functions and constructors of these tests take:
// each run signals on ran.
type formsDep struct{ ran chan struct{} }

func newFormsDep() *formsDep { return &formsDep{ran: make(chan struct{}, 1)} }

// hit signals a run without blocking.
func (d *formsDep) hit() {
	select {
	case d.ran <- struct{}{}:
	default:
	}
}

// formsName is a second dependency of a run function.
type formsName struct{ s string }

// formsRunner is a worker over a formsDep.
type formsRunner struct{ dep *formsDep }

func (r *formsRunner) Run(ctx context.Context) error {
	r.dep.hit()
	<-ctx.Done()
	return nil
}

// formsJob is a named function type of the worker signature, with no Run
// method of its own.
type formsJob func(context.Context) error

// formsNotAWorker has no Run method.
type formsNotAWorker struct{ n int }

// formsMissing is never bound.
type formsMissing struct{}

// formsIngress is an ingress component.
type formsIngress struct{ _ byte }

func (*formsIngress) Shutdown(context.Context) error { return nil }

// formsAwait waits for a run of dep.
func formsAwait(t *testing.T, dep *formsDep) {
	t.Helper()
	select {
	case <-dep.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not run")
	}
}

// TestWorkersForms_EachFormRegistersAndRuns: a worker value, a bare
// func(context.Context) error, a named function type of that signature, a
// run function with its context first, last and between its parameters, a
// method expression, and a constructor of each shape register and run.
func TestWorkersForms_EachFormRegistersAndRuns(t *testing.T) {
	type form struct {
		name string
		w    func(dep *formsDep) any
	}
	block := func(ctx context.Context, d *formsDep) error {
		d.hit()
		<-ctx.Done()
		return nil
	}
	forms := []form{
		{"worker value", func(dep *formsDep) any { return &formsRunner{dep: dep} }},
		{"bare func(context.Context) error", func(dep *formsDep) any {
			return func(ctx context.Context) error { return block(ctx, dep) }
		}},
		{"named function type", func(dep *formsDep) any {
			return formsJob(func(ctx context.Context) error { return block(ctx, dep) })
		}},
		{"run function, context first", func(*formsDep) any {
			return func(ctx context.Context, d *formsDep) error { return block(ctx, d) }
		}},
		{"run function, context last", func(*formsDep) any {
			return func(d *formsDep, ctx context.Context) error { return block(ctx, d) }
		}},
		{"run function, context between", func(*formsDep) any {
			return func(d *formsDep, ctx context.Context, n *formsName) error {
				if n.s != "name" {
					return errors.New("wrong formsName")
				}
				return block(ctx, d)
			}
		}},
		{"method expression", func(*formsDep) any { return (*formsRunner).Run }},
		{"constructor of a Worker", func(*formsDep) any {
			return func(d *formsDep) *formsRunner { return &formsRunner{dep: d} }
		}},
		{"constructor of a Worker, with an error", func(*formsDep) any {
			return func(d *formsDep) (*formsRunner, error) { return &formsRunner{dep: d}, nil }
		}},
		{"constructor declared to return credo.Worker", func(*formsDep) any {
			return func(d *formsDep) credo.Worker { return &formsRunner{dep: d} }
		}},
		{"constructor of a function", func(*formsDep) any {
			return func(d *formsDep) func(context.Context) error {
				return func(ctx context.Context) error { return block(ctx, d) }
			}
		}},
		{"constructor of a named function type, with an error", func(*formsDep) any {
			return func(d *formsDep) (formsJob, error) {
				return func(ctx context.Context) error { return block(ctx, d) }, nil
			}
		}},
	}
	for _, kind := range []credo.WorkerKind{credo.WorkerKindContinuous, credo.WorkerKindScheduled} {
		for _, f := range forms {
			t.Run(string(kind)+"/"+f.name, func(t *testing.T) {
				app := mustNew(t)
				dep := newFormsDep()
				app.ProvideValue(dep)
				app.ProvideValue(&formsName{s: "name"})
				app.ProvideValue(&formsRunner{dep: dep})
				s := app.Workers()
				if kind == credo.WorkerKindScheduled {
					s.Scheduled("w", "@every 1h", f.w(dep), credo.ScheduledConfig{RunOnStart: true})
				} else {
					s.Continuous("w", f.w(dep))
				}
				startWorkersApp(t, app)
				formsAwait(t, dep)
				if info, ok := s.Lookup("w"); !ok || info.Kind != kind {
					t.Errorf("Lookup(w) = %+v, %v, want a %s worker", info, ok, kind)
				}
			})
		}
	}
}

// TestWorkersForms_OtherShapesPanic: a w of none of the forms panics naming
// the forms accepted, and a function that takes two contexts, or a context
// and returns anything but a lone error, names the run function's shape.
func TestWorkersForms_OtherShapesPanic(t *testing.T) {
	const forms = " is none of the forms a worker registers as; pass a worker (a credo.Worker, or a " +
		"func(context.Context) error), a run function (one that takes exactly one context.Context and returns " +
		"only an error, whose other parameters the container supplies), or a constructor of a worker " +
		"(func(deps...) W or func(deps...) (W, error), where W is a worker)"
	const runShape = "; a run function takes exactly one context.Context, in any position, and returns only an error"
	tests := []struct {
		name string
		w    any
		want string // after the call
	}{
		{"an int", 42, "int" + forms},
		{"a value without Run", formsNotAWorker{n: 1}, "credo_test.formsNotAWorker" + forms},
		{"a function without results", func() {}, "func()" + forms},
		{"a constructor of an int", func() int { return 0 }, "func() int" + forms},
		{"a constructor of an int, with an error", func() (int, error) { return 0, nil },
			"func() (int, error)" + forms},
		{"a constructor whose second result is no error", func() (*formsRunner, int) { return nil, 0 },
			"func() (*credo_test.formsRunner, int)" + forms},
		{"a function that returns a lone error without a context", func(*formsDep) error { return nil },
			"func(*credo_test.formsDep) error" + forms},
		{"three results", func() (*formsRunner, error, int) { return nil, nil, 0 },
			"func() (*credo_test.formsRunner, error, int)" + forms},
		{"two contexts", func(context.Context, context.Context) error { return nil },
			"func(context.Context, context.Context) error takes 2 context.Context parameters" + runShape},
		{"a context and a value beside the error", func(context.Context) (int, error) { return 0, nil },
			"func(context.Context) (int, error) takes a context.Context and returns (int, error)" + runShape},
		{"a context and no result", func(context.Context, *formsDep) {},
			"func(context.Context, *credo_test.formsDep) takes a context.Context and returns nothing" + runShape},
		{"a context and an int", func(context.Context) int { return 0 },
			"func(context.Context) int takes a context.Context and returns int" + runShape},
		{"a nil constructor", (func() *formsRunner)(nil), "the worker is nil; pass a value with a Run method"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("Continuous", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() { s.Continuous("x", tt.w) })
				registerWant(t, msg, `credo: Workers.Continuous("x"): `+tt.want)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
			})
			t.Run("Scheduled", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() { s.Scheduled("x", "@every 1m", tt.w) })
				registerWant(t, msg, `credo: Workers.Scheduled("x"): `+tt.want)
			})
		})
	}
}

// formsOwner is a worker with a teardown of its own.
type formsOwner struct {
	ran   chan struct{}
	once  sync.Once
	stops atomic.Int32
}

func (o *formsOwner) Run(ctx context.Context) error {
	o.once.Do(func() { close(o.ran) })
	<-ctx.Done()
	return nil
}

func (o *formsOwner) Shutdown(context.Context) error {
	o.stops.Add(1)
	return nil
}

// TestWorkersForms_ConstructorOfAMethodValue: a constructor that returns a
// method value hands over the function alone: it runs as the worker, and the
// value the method belongs to is not torn down.
func TestWorkersForms_ConstructorOfAMethodValue(t *testing.T) {
	app := mustNew(t)
	owner := &formsOwner{ran: make(chan struct{})}
	app.Workers().Continuous("method", func() func(context.Context) error { return owner.Run })
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	workersAwait(t, owner.ran, "the method value to run")
	if err := shutdownApp(t, app, 5*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := owner.stops.Load(); got != 0 {
		t.Errorf("the method's receiver was shut down %d times, want 0: a function has no teardown", got)
	}
}

// TestWorkersForms_MissingParameterFailsFinalize: a missing parameter of a
// constructor or of a run function fails Finalize with its path.
func TestWorkersForms_MissingParameterFailsFinalize(t *testing.T) {
	tests := []struct {
		name string
		w    any
		want string
	}{
		{"constructor", func(*formsMissing) *formsRunner { return &formsRunner{} },
			"di: missing dependency: the value %s builds → *credo_test.formsMissing (not registered); " +
				"provide *credo_test.formsMissing before Finalize"},
		{"run function", func(context.Context, *formsMissing) error { return nil },
			"di: missing dependency: %s → *credo_test.formsMissing (not registered); " +
				"provide *credo_test.formsMissing before Finalize"},
	}
	for _, kind := range []credo.WorkerKind{credo.WorkerKindContinuous, credo.WorkerKindScheduled} {
		for _, tt := range tests {
			t.Run(string(kind)+"/"+tt.name, func(t *testing.T) {
				app := mustNew(t)
				call := `Workers.Continuous("orphan")`
				if kind == credo.WorkerKindScheduled {
					call = `Workers.Scheduled("orphan")`
					app.Workers().Scheduled("orphan", "@every 1h", tt.w)
				} else {
					app.Workers().Continuous("orphan", tt.w)
				}
				want := fmt.Sprintf(tt.want, call)
				err := app.Finalize()
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("Finalize() = %v\nwant it to contain %q", err, want)
				}
			})
		}
	}
}

// formsRecordingDep is a dependency the failing constructors take, so it is
// built — and started — before they fail.
type formsRecordingDep struct{ started, shutdowns atomic.Int32 }

func (d *formsRecordingDep) Start(context.Context) error {
	d.started.Add(1)
	return nil
}

func (d *formsRecordingDep) Shutdown(context.Context) error {
	d.shutdowns.Add(1)
	return nil
}

// TestWorkersForms_ConstructionFailureFailsStart: a constructor error, a
// panic and a nil worker each fail the start, as the built value's start
// failure, and the rollback stops what was built.
func TestWorkersForms_ConstructionFailureFailsStart(t *testing.T) {
	cases := []struct {
		name    string
		ctor    any
		outcome credo.LifecycleOutcome
		want    func(call string) string
	}{
		{
			name:    "constructor error",
			ctor:    func(*formsRecordingDep) (*formsRunner, error) { return nil, errors.New("constructor failed") },
			outcome: credo.OutcomeFailed,
			want:    func(string) string { return "constructor failed" },
		},
		{
			name:    "constructor panic",
			ctor:    func(*formsRecordingDep) *formsRunner { panic("constructor boom") },
			outcome: credo.OutcomePanicked,
			want:    func(string) string { return "constructor boom" },
		},
		{
			name:    "nil worker",
			ctor:    func(*formsRecordingDep) *formsRunner { return nil },
			outcome: credo.OutcomeFailed,
			want: func(call string) string {
				return call + ": the constructor returned a nil *credo_test.formsRunner; return a worker, or an error"
			},
		},
		{
			name:    "nil worker behind an interface",
			ctor:    func(*formsRecordingDep) (credo.Worker, error) { return nil, nil },
			outcome: credo.OutcomeFailed,
			want: func(call string) string {
				return call + ": the constructor returned a nil credo.Worker; return a worker, or an error"
			},
		},
	}
	for _, kind := range []credo.WorkerKind{credo.WorkerKindContinuous, credo.WorkerKindScheduled} {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				capture := newLogCapture()
				app := mustNew(t, credo.WithLogger(capture.logger()))
				dep := &formsRecordingDep{}
				app.ProvideValue(dep)
				s := app.Workers()
				call := `Workers.Continuous("broken")`
				if kind == credo.WorkerKindScheduled {
					call = `Workers.Scheduled("broken")`
					s.Scheduled("broken", "@every 1h", tc.ctor)
				} else {
					s.Continuous("broken", tc.ctor)
				}

				err := app.Start(t.Context())
				lerr, ok := errors.AsType[*credo.LifecycleError](err)
				if !ok {
					t.Fatalf("App.Start() = %v, want a *credo.LifecycleError", err)
				}
				entry, n := workersLifecycleEntry(lerr, "worker-value:broken")
				if n != 1 || entry.Phase != credo.PhaseStart || entry.Outcome != tc.outcome ||
					entry.Tier != credo.TierInternal {
					t.Fatalf("App.Start() = %v, want worker-value:broken (internal) start %s, once", err, tc.outcome)
				}
				if want := tc.want(call); !strings.Contains(err.Error(), want) {
					t.Errorf("App.Start() = %v\nwant it to contain %q", err, want)
				}
				if tc.outcome == credo.OutcomePanicked {
					if _, ok := errors.AsType[*credo.PanicError](err); !ok {
						t.Errorf("App.Start() = %v, want the constructor panic kept as *credo.PanicError", err)
					}
				}
				if dep.started.Load() != 1 || dep.shutdowns.Load() != 1 {
					t.Errorf("dependency started %d and shut down %d times, want 1 and 1 (rolled back)",
						dep.started.Load(), dep.shutdowns.Load())
				}
				if info, _ := s.Lookup("broken"); info.Status != credo.WorkerStatusPending {
					t.Errorf("broken = %s, want pending (its loop never started)", info.Status)
				}
				if lines := capture.withMessage("worker started"); len(lines) != 0 {
					t.Errorf("worker started logged %d times, want none", len(lines))
				}
			})
		}
	}
}

// runDep is a component a run function takes; it counts its builds.
type runDep struct {
	j       *journal
	started atomic.Bool
}

func (d *runDep) Start(context.Context) error {
	d.started.Store(true)
	return nil
}

func (d *runDep) Shutdown(context.Context) error {
	d.j.add("dep shutdown")
	return nil
}

// TestWorkersRunFunction_ParametersResolvedOnceEachRunItsOwnContext: the
// parameters are resolved once, after their components have started; each
// run receives them with its own context; the drain stops the loop before
// them.
func TestWorkersRunFunction_ParametersResolvedOnceEachRunItsOwnContext(t *testing.T) {
	app := mustNew(t)
	j := &journal{}
	var builds atomic.Int32
	app.Provide(func() *runDep {
		builds.Add(1)
		return &runDep{j: j}
	})

	type seen struct {
		dep     *runDep
		started bool
		runID   string
	}
	var mu sync.Mutex
	var runs []seen
	second := make(chan struct{})
	app.Workers().Continuous("runner", func(d *runDep, ctx context.Context) error {
		run, _ := credo.CurrentWorkerRun(ctx)
		mu.Lock()
		runs = append(runs, seen{dep: d, started: d.started.Load(), runID: run.ID})
		n := len(runs)
		mu.Unlock()
		if n == 1 {
			return errors.New("restart me")
		}
		if n == 2 {
			close(second)
		}
		<-ctx.Done()
		j.add("run returned")
		return nil
	}, credo.ContinuousConfig{Restart: credo.RestartPolicy{MinDelay: time.Millisecond, MaxDelay: time.Millisecond}})

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	workersAwait(t, second, "the second run")
	if err := shutdownApp(t, app, 5*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if builds.Load() != 1 {
		t.Errorf("the parameter was built %d times, want once", builds.Load())
	}
	if len(runs) < 2 || runs[0].dep != runs[1].dep || runs[0].dep == nil {
		t.Fatalf("runs = %+v, want two runs with one parameter value", runs)
	}
	if !runs[0].started {
		t.Error("the first run saw its parameter unstarted, want it resolved after its component started")
	}
	if runs[0].runID == "" || runs[0].runID == runs[1].runID {
		t.Errorf("run IDs = %q and %q, want each run its own context", runs[0].runID, runs[1].runID)
	}
	if got, want := j.list(), []string{"run returned", "dep shutdown"}; !slices.Equal(got, want) {
		t.Errorf("drain = %q, want %q (the loop before its parameter)", got, want)
	}
}

// TestWorkersRunFunction_IngressParameterUnderAnInternalLoop: a parameter
// bound with credo.Ingress() fails Finalize under a continuous worker's
// internal loop, naming both and the two remedies; each remedy is accepted.
func TestWorkersRunFunction_IngressParameterUnderAnInternalLoop(t *testing.T) {
	run := func(context.Context, *formsIngress) error { return nil }
	newApp := func(t *testing.T) *credo.App {
		app := mustNew(t)
		app.Provide(func() *formsIngress { return &formsIngress{} }, credo.Ingress())
		return app
	}
	app := newApp(t)
	app.Workers().Continuous("consumer", run)
	const want = `di: internal component Workers.Continuous("consumer") depends on the ingress component ` +
		`*credo_test.formsIngress: Workers.Continuous("consumer") → *credo_test.formsIngress; declare ` +
		`Workers.Continuous("consumer") ingress with Tier: credo.TierIngress in its worker configuration, or split ` +
		"*credo_test.formsIngress so that what internal components use is an internal part it depends on"
	if err := app.Finalize(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Finalize() = %v\nwant it to contain %q", err, want)
	}

	t.Run("continuous worker declared ingress", func(t *testing.T) {
		app := newApp(t)
		app.Workers().Continuous("consumer", run, credo.ContinuousConfig{Tier: credo.TierIngress})
		mustFinalize(t, app)
	})
	t.Run("scheduled worker", func(t *testing.T) {
		app := newApp(t)
		app.Workers().Scheduled("report", "@every 1h", run)
		mustFinalize(t, app)
	})
}

// TestWorkersRunFunction_AddsNoBuiltValue: a run function adds the loop
// alone, so the name worker-value:<name> stays free; a constructor takes it.
func TestWorkersRunFunction_AddsNoBuiltValue(t *testing.T) {
	app := mustNew(t)
	app.ProvideValue(newFormsDep())
	app.Workers().Continuous("x", func(ctx context.Context, d *formsDep) error { <-ctx.Done(); return nil })
	app.Manage(&registerComponent{}, credo.ComponentName("worker-value:x"))

	app = mustNew(t)
	app.ProvideValue(newFormsDep())
	app.Workers().Continuous("x", func(d *formsDep) *formsRunner { return &formsRunner{dep: d} })
	msg := mustPanicMessage(t, func() { app.Manage(&registerComponent{}, credo.ComponentName("worker-value:x")) })
	const want = `a component named "worker-value:x" is already registered, as the value Workers.Continuous("x") builds`
	if !strings.Contains(msg, want) {
		t.Fatalf("message = %q, want the constructor's built value to hold the name", msg)
	}
}

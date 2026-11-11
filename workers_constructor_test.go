package credo_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/testutil"
)

// ctorDB is a database handle: it names itself as its resource, so a value
// that embeds it is one resource with it, and it has Start, Ready and
// Shutdown.
type ctorDB struct {
	j            *journal
	starts       atomic.Int32
	stops        atomic.Int32
	mu           sync.Mutex
	readyErr     error
	shutdownFail error
}

func newCtorDB(j *journal) *ctorDB { return &ctorDB{j: j} }

func (d *ctorDB) ResourceIdentity() any { return d }

func (d *ctorDB) Start(context.Context) error {
	d.starts.Add(1)
	d.j.add("db start")
	return nil
}

func (d *ctorDB) Ready(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.readyErr
}

func (d *ctorDB) setReady(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readyErr = err
}

func (d *ctorDB) Shutdown(context.Context) error {
	d.stops.Add(1)
	d.j.add("db shutdown")
	return d.shutdownFail
}

// ctorDBWorker is a worker that holds the database in a named field: a
// value of its own, with no lifecycle, beside the database's resource.
type ctorDBWorker struct {
	db   *ctorDB
	ran  chan struct{}
	once sync.Once
}

func (w *ctorDBWorker) Run(ctx context.Context) error {
	w.once.Do(func() { close(w.ran) })
	<-ctx.Done()
	w.db.j.add("run returned")
	return nil
}

// ctorDBEmbedder is a worker that embeds the database: it holds the
// database's resource, and its Start, Ready and Shutdown are the database's.
type ctorDBEmbedder struct{ *ctorDB }

func (*ctorDBEmbedder) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// ctorDBWrapper embeds the database and is no worker.
type ctorDBWrapper struct{ *ctorDB }

// ctorConsumer is another consumer of the database, a component of its own.
type ctorConsumer struct{ db *ctorDB }

func (*ctorConsumer) Start(context.Context) error { return nil }

func (c *ctorConsumer) Shutdown(context.Context) error {
	c.db.j.add("consumer shutdown")
	return nil
}

// ctorRelay is a worker that other code uses: bound with Provide and
// registered through its Run.
type ctorRelay struct {
	j            *journal
	starts       atomic.Int32
	stops        atomic.Int32
	startedAtRun atomic.Bool
	ran          chan struct{}
	once         sync.Once
	mu           sync.Mutex
	readyErr     error
	shutdownFail error
}

func newCtorRelay(j *journal) *ctorRelay { return &ctorRelay{j: j, ran: make(chan struct{})} }

func (r *ctorRelay) Start(context.Context) error {
	r.starts.Add(1)
	r.j.add("relay start")
	return nil
}

func (r *ctorRelay) Ready(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readyErr
}

func (r *ctorRelay) setReady(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readyErr = err
}

func (r *ctorRelay) Shutdown(context.Context) error {
	r.stops.Add(1)
	r.j.add("relay shutdown")
	return r.shutdownFail
}

func (r *ctorRelay) Run(ctx context.Context) error {
	r.startedAtRun.Store(r.starts.Load() == 1)
	r.once.Do(func() { close(r.ran) })
	<-ctx.Done()
	r.j.add("run returned")
	return nil
}

// ctorController is a handler-side user of the relay.
type ctorController struct{ r *ctorRelay }

func newCtorController(r *ctorRelay) *ctorController { return &ctorController{r: r} }

// readyNames returns the names of the /ready entries.
func readyNames(body readyBody) []string {
	names := make([]string, 0, len(body.Checks))
	for name := range body.Checks {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestWorkersShared_RelayBoundAndRegisteredByItsRunFunction: a worker that
// other code uses, bound with Provide and registered as the method
// expression of its Run: a controller and the loop share one value, started
// once before the loop and torn down once after it; /ready shows the
// binding's one entry, which credo.OptionalReadiness() on the binding keeps
// from gating; the entry and the teardown carry the binding's name in either
// registration order.
func TestWorkersShared_RelayBoundAndRegisteredByItsRunFunction(t *testing.T) {
	for _, order := range []string{"binding first", "worker first"} {
		for _, named := range []bool{false, true} {
			name := order
			if named {
				name += "/named"
			}
			t.Run(name, func(t *testing.T) {
				app := mustNew(t)
				j := &journal{}
				relay := newCtorRelay(j)
				relay.shutdownFail = errors.New("relay flush failed")
				opts := []credo.RegistrationOption{credo.OptionalReadiness()}
				entryName := "*credo_test.ctorRelay"
				if named {
					opts = append(opts, credo.ComponentName("outbox"))
					entryName = "outbox"
				}
				bind := func() { app.Provide(func() *ctorRelay { return relay }, opts...) }
				register := func() { app.Workers().Continuous("relay", (*ctorRelay).Run) }
				if order == "binding first" {
					bind()
					register()
				} else {
					register()
					bind()
				}
				app.Provide(newCtorController)
				app.UseHealth()
				if err := app.Start(t.Context()); err != nil {
					t.Fatalf("App.Start() = %v", err)
				}
				workersAwait(t, relay.ran, "the relay's loop")
				if c := app.MustResolve[*ctorController](); c.r != relay {
					t.Error("the controller and the loop hold different values")
				}
				if !relay.startedAtRun.Load() {
					t.Error("the loop ran before the relay started")
				}

				relay.setReady(errors.New("relay behind"))
				body := getReady(t, app)
				if got := readyNames(body); !slices.Equal(got, []string{entryName}) {
					t.Errorf("/ready entries = %q, want the binding's one entry %q", got, entryName)
				}
				if body.code != http.StatusOK || body.Status != "degraded" {
					t.Errorf("/ready = %+v, want 200 degraded: the binding's entry does not gate", body)
				}

				err := shutdownApp(t, app, 5*time.Second)
				entry := lifecycleEntry(t, err, entryName)
				if entry.Phase != credo.PhaseShutdown || entry.Tier != credo.TierInternal {
					t.Errorf("teardown entry = %+v, want the binding's internal shutdown", entry)
				}
				if le, _ := errors.AsType[*credo.LifecycleError](err); len(le.Entries) != 1 {
					t.Errorf("Shutdown() = %v, want the relay's one teardown entry", err)
				}
				if relay.starts.Load() != 1 || relay.stops.Load() != 1 {
					t.Errorf("relay started %d and stopped %d times, want once each",
						relay.starts.Load(), relay.stops.Load())
				}
				want := []string{"relay start", "run returned", "relay shutdown"}
				if got := j.list(); !slices.Equal(got, want) {
					t.Errorf("events = %q, want %q", got, want)
				}
			})
		}
	}
}

// TestWorkersShared_ConstructorReturningTheBindingFailsTheStart: a worker
// constructor that returns the value a binding owns — made with Provide,
// with ProvideValue, or borrowed — fails the start in either registration
// order, naming the worker's call, the built type, the owner and the method
// expression that replaces the constructor.
func TestWorkersShared_ConstructorReturningTheBindingFailsTheStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bind  func(app *credo.App, r *ctorRelay)
		owner string
	}{
		{"Provide", func(app *credo.App, r *ctorRelay) { app.Provide(func() *ctorRelay { return r }) },
			"the Provide binding of *credo_test.ctorRelay owns"},
		{"ProvideValue", func(app *credo.App, r *ctorRelay) { app.ProvideValue(r) },
			"the ProvideValue binding of *credo_test.ctorRelay owns"},
		{"borrowed", func(app *credo.App, r *ctorRelay) { app.ProvideValue(r, credo.Borrowed()) },
			"the ProvideValue binding of *credo_test.ctorRelay owns for its caller (credo.Borrowed())"},
	} {
		for _, workerFirst := range []bool{false, true} {
			name := tc.name + "/binding first"
			if workerFirst {
				name = tc.name + "/worker first"
			}
			t.Run(name, func(t *testing.T) {
				app := mustNew(t)
				relay := newCtorRelay(&journal{})
				register := func() { app.Workers().Continuous("relay", func(r *ctorRelay) *ctorRelay { return r }) }
				if workerFirst {
					register()
				}
				tc.bind(app, relay)
				if !workerFirst {
					register()
				}
				entry := lifecycleEntry(t, app.Start(t.Context()), "worker-value:relay")
				want := `the value Workers.Continuous("relay") builds, a *credo_test.ctorRelay, holds the resource ` +
					"that " + tc.owner + ", and a resource has one owner; register the worker's run function, the " +
					"method expression (*credo_test.ctorRelay).Run, in place of a constructor that returns that " +
					"binding's value"
				if entry.Phase != credo.PhaseStart || entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
					t.Fatalf("entry = %+v\nwant a start failure containing %q", entry, want)
				}
				if got := relay.ran; isClosed(got) {
					t.Error("the loop ran")
				}
			})
		}
	}
}

// isClosed reports whether ch is closed.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestWorkersShared_WithOverrideReplacesTheBoundWorkerForBoth: an override
// of the bound type replaces the value the controller and the loop share.
func TestWorkersShared_WithOverrideReplacesTheBoundWorkerForBoth(t *testing.T) {
	var built atomic.Int32
	fake := newCtorRelay(&journal{})
	app := testutil.NewApp(t,
		testutil.WithWiring(func(app *credo.App) {
			app.Provide(func() *ctorRelay {
				built.Add(1)
				return newCtorRelay(&journal{})
			})
			app.Workers().Continuous("relay", (*ctorRelay).Run)
			app.Provide(newCtorController)
		}),
		testutil.WithOverride[*ctorRelay](fake),
	)
	testutil.Start(t, app)
	workersAwait(t, fake.ran, "the override's loop")
	if c := app.MustResolve[*ctorController](); c.r != fake {
		t.Error("the controller does not hold the override")
	}
	if built.Load() != 0 {
		t.Errorf("the replaced constructor ran %d times, want 0", built.Load())
	}
}

// TestWorkersShared_WorkerHoldsABoundDatabase: a worker that holds a database
// bound with ProvideValue in a named field leaves the database its one
// teardown, after the loop and every other consumer, and /ready shows the
// database's entry alone, which does not gate with
// credo.OptionalReadiness(); bound with credo.Borrowed(), the database is
// neither started nor torn down. A worker that embeds the database holds
// its resource and fails the start, naming the named field.
func TestWorkersShared_WorkerHoldsABoundDatabase(t *testing.T) {
	var w *ctorDBWorker
	capture := func(db *ctorDB) *ctorDBWorker {
		w = &ctorDBWorker{db: db, ran: make(chan struct{})}
		return w
	}
	for _, optional := range []bool{false, true} {
		name := "gating"
		if optional {
			name = "optional"
		}
		t.Run(name, func(t *testing.T) {
			app := mustNew(t)
			j := &journal{}
			db := newCtorDB(j)
			if optional {
				app.ProvideValue(db, credo.OptionalReadiness())
			} else {
				app.ProvideValue(db)
			}
			app.Provide(func(d *ctorDB) *ctorConsumer { return &ctorConsumer{db: d} })
			app.Workers().Continuous("relay", capture)
			app.UseHealth()
			if err := app.Start(t.Context()); err != nil {
				t.Fatalf("App.Start() = %v", err)
			}
			workersAwait(t, w.ran, "the worker to run")

			db.setReady(errors.New("db down"))
			body := getReady(t, app)
			if got := readyNames(body); !slices.Equal(got, []string{"*credo_test.ctorDB"}) {
				t.Errorf("/ready entries = %q, want the database's alone", got)
			}
			wantCode, wantStatus := http.StatusServiceUnavailable, "down"
			if optional {
				wantCode, wantStatus = http.StatusOK, "degraded"
			}
			if body.code != wantCode || body.Status != wantStatus {
				t.Errorf("/ready = %+v, want %d %s", body, wantCode, wantStatus)
			}

			if err := shutdownApp(t, app, 5*time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
			if db.starts.Load() != 1 || db.stops.Load() != 1 {
				t.Errorf("database started %d and stopped %d times, want once each", db.starts.Load(), db.stops.Load())
			}
			events := j.list()
			last := events[len(events)-1]
			if last != "db shutdown" || !slices.Contains(events, "run returned") ||
				!slices.Contains(events, "consumer shutdown") {
				t.Errorf("events = %q, want the database torn down once, after the loop and the consumer", events)
			}
		})
	}
	t.Run("borrowed", func(t *testing.T) {
		app := mustNew(t)
		db := newCtorDB(&journal{})
		app.ProvideValue(db, credo.Borrowed())
		app.Workers().Continuous("relay", capture)
		app.UseHealth()
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		workersAwait(t, w.ran, "the worker to run")
		if got := readyNames(getReady(t, app)); !slices.Equal(got, []string{"*credo_test.ctorDB"}) {
			t.Errorf("/ready entries = %q, want the database reported once", got)
		}
		if err := shutdownApp(t, app, 5*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		if db.starts.Load() != 0 || db.stops.Load() != 0 {
			t.Errorf("borrowed database started %d and stopped %d times, want 0", db.starts.Load(), db.stops.Load())
		}
	})
	t.Run("embedded", func(t *testing.T) {
		app := mustNew(t)
		db := newCtorDB(&journal{})
		app.ProvideValue(db)
		app.Workers().Continuous("relay", func(d *ctorDB) *ctorDBEmbedder { return &ctorDBEmbedder{d} })
		entry := lifecycleEntry(t, app.Start(t.Context()), "worker-value:relay")
		const want = `the value Workers.Continuous("relay") builds, a *credo_test.ctorDBEmbedder, holds the ` +
			"resource that the ProvideValue binding of *credo_test.ctorDB owns, and a resource has one owner; " +
			"hold the *credo_test.ctorDB in a named field of *credo_test.ctorDBEmbedder, so that it is a resource " +
			"of its own"
		if entry.Phase != credo.PhaseStart || entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
			t.Fatalf("entry = %+v\nwant a start failure containing %q", entry, want)
		}
		if db.stops.Load() != 1 {
			t.Errorf("database stopped %d times by the rollback, want once", db.stops.Load())
		}
	})
}

// TestWorkersShared_RunFunctionOverABorrowedBinding: the run function of a
// borrowed value: the loop runs, the App neither starts nor shuts the value
// down, and /ready reports it once.
func TestWorkersShared_RunFunctionOverABorrowedBinding(t *testing.T) {
	app := mustNew(t)
	relay := newCtorRelay(&journal{})
	app.ProvideValue(relay, credo.Borrowed())
	app.Workers().Continuous("relay", (*ctorRelay).Run)
	app.UseHealth()
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	workersAwait(t, relay.ran, "the loop")
	if got := readyNames(getReady(t, app)); !slices.Equal(got, []string{"*credo_test.ctorRelay"}) {
		t.Errorf("/ready entries = %q, want the borrowed value once", got)
	}
	if err := shutdownApp(t, app, 5*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if relay.starts.Load() != 0 || relay.stops.Load() != 0 {
		t.Errorf("borrowed value started %d and stopped %d times, want 0", relay.starts.Load(), relay.stops.Load())
	}
}

// ctorShared is one value two constructors return.
type ctorShared struct {
	runs  atomic.Int32
	stops atomic.Int32
	both  chan struct{}
}

func (s *ctorShared) Run(ctx context.Context) error {
	if s.runs.Add(1) == 2 {
		close(s.both)
	}
	<-ctx.Done()
	return nil
}

func (*ctorShared) Ready(context.Context) error { return nil }

func (s *ctorShared) Shutdown(context.Context) error {
	s.stops.Add(1)
	return nil
}

// TestWorkersShared_TwoConstructorsOfOneSingleton: two worker constructors
// that return one value fail the start at the second, naming the first and
// the remedy; bound once and registered twice by the method expression of
// its Run, both loops run, the value has one readiness entry, the binding's,
// and it is torn down once.
func TestWorkersShared_TwoConstructorsOfOneSingleton(t *testing.T) {
	t.Run("two constructors", func(t *testing.T) {
		app := mustNew(t)
		shared := &ctorShared{both: make(chan struct{})}
		ctor := func() *ctorShared { return shared }
		app.Workers().Continuous("a", ctor)
		app.Workers().Continuous("b", ctor)
		entry := lifecycleEntry(t, app.Start(t.Context()), "worker-value:b")
		const want = `the value Workers.Continuous("b") builds, a *credo_test.ctorShared, holds the resource that ` +
			`the value the constructor of Workers.Continuous("a") builds owns, and a resource has one owner; bind ` +
			`the *credo_test.ctorShared in place of the value Workers.Continuous("a") builds, and register each ` +
			"worker that runs it by its run function, the method expression (*credo_test.ctorShared).Run"
		if entry.Phase != credo.PhaseStart || entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
			t.Fatalf("entry = %+v\nwant a start failure containing %q", entry, want)
		}
		if got := shared.stops.Load(); got != 1 {
			t.Errorf("the rollback tore the value down %d times, want once, through its owner", got)
		}
	})
	t.Run("bound once, two run functions", func(t *testing.T) {
		app := mustNew(t)
		shared := &ctorShared{both: make(chan struct{})}
		app.ProvideValue(shared)
		app.Workers().Continuous("a", (*ctorShared).Run)
		app.Workers().Continuous("b", (*ctorShared).Run)
		app.UseHealth()
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		workersAwait(t, shared.both, "both loops to run")
		if got := readyNames(getReady(t, app)); !slices.Equal(got, []string{"*credo_test.ctorShared"}) {
			t.Errorf("/ready entries = %q, want the binding's one entry", got)
		}
		if err := shutdownApp(t, app, 5*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		if got := shared.stops.Load(); got != 1 {
			t.Errorf("the shared value was torn down %d times, want once", got)
		}
	})
}

// ctorOwned is a worker with Start and Shutdown of its own.
type ctorOwned struct {
	j    *journal
	id   int
	fail error
	ran  chan struct{}
	once sync.Once
}

func (o *ctorOwned) Start(context.Context) error {
	o.j.add("value start")
	return nil
}

func (o *ctorOwned) Run(ctx context.Context) error {
	o.j.add("run")
	o.once.Do(func() { close(o.ran) })
	<-ctx.Done()
	o.j.add("run returned")
	return nil
}

func (o *ctorOwned) Shutdown(context.Context) error {
	o.j.add("value shutdown")
	return o.fail
}

// ctorStopOnly is a worker with a Shutdown of its own and no Start.
type ctorStopOnly struct {
	j    *journal
	ran  chan struct{}
	once sync.Once
}

func (o *ctorStopOnly) Run(ctx context.Context) error {
	o.once.Do(func() { close(o.ran) })
	<-ctx.Done()
	o.j.add("run returned")
	return nil
}

func (o *ctorStopOnly) Shutdown(context.Context) error {
	o.j.add("value shutdown")
	return nil
}

// TestWorkersConstructor_BuiltValueLifecycle: a result type with Start and
// Shutdown is started before its loop and torn down after it; a constructor
// declared to return credo.Worker that builds a value with Shutdown has it
// torn down after its loop; a Start only the built value has fails the
// start, naming the constructor and the remedy.
func TestWorkersConstructor_BuiltValueLifecycle(t *testing.T) {
	t.Run("result type with Start and Shutdown", func(t *testing.T) {
		app := mustNew(t)
		j := &journal{}
		v := &ctorOwned{j: j, ran: make(chan struct{})}
		app.Workers().Scheduled("owned", "@every 1h", func() *ctorOwned { return v },
			credo.ScheduledConfig{RunOnStart: true})
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		workersAwait(t, v.ran, "the run")
		if err := shutdownApp(t, app, 5*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		want := []string{"value start", "run", "run returned", "value shutdown"}
		if got := j.list(); !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	})
	t.Run("Shutdown the result type hides", func(t *testing.T) {
		app := mustNew(t)
		j := &journal{}
		v := &ctorStopOnly{j: j, ran: make(chan struct{})}
		app.Workers().Continuous("hidden", func() credo.Worker { return v })
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		workersAwait(t, v.ran, "the run")
		if err := shutdownApp(t, app, 5*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		if got, want := j.list(), []string{"run returned", "value shutdown"}; !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	})
	t.Run("Start the result type hides", func(t *testing.T) {
		app := mustNew(t)
		app.Workers().Continuous("hidden", func() credo.Worker {
			return &ctorOwned{j: &journal{}, ran: make(chan struct{})}
		})
		err := app.Start(t.Context())
		entry := lifecycleEntry(t, err, "worker-value:hidden")
		const want = `the constructor of Workers.Continuous("hidden") returns a *credo_test.ctorOwned, whose ` +
			"Shutdown and Start its result type credo.Worker hides, and the App plans Start, Ready and the " +
			"ingress tier only from that type; declare the constructor to return *credo_test.ctorOwned"
		if entry.Phase != credo.PhaseStart || entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
			t.Fatalf("entry = %+v\nwant a start failure containing %q", entry, want)
		}
	})
}

// TestWorkersConstructor_TwoRegistrationsTwoValues: each registration calls
// its constructor, so two registrations of one constructor get two values,
// worker-value:a and worker-value:b, each torn down under its own name.
func TestWorkersConstructor_TwoRegistrationsTwoValues(t *testing.T) {
	app := mustNew(t)
	j := &journal{}
	var mu sync.Mutex
	var built []*ctorOwned
	ctor := func() *ctorOwned {
		mu.Lock()
		defer mu.Unlock()
		v := &ctorOwned{j: j, id: len(built), fail: errors.New("flush failed"), ran: make(chan struct{})}
		built = append(built, v)
		return v
	}
	app.Workers().Continuous("a", ctor)
	app.Workers().Continuous("b", ctor)
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	mu.Lock()
	values := slices.Clone(built)
	mu.Unlock()
	if len(values) != 2 || values[0] == values[1] {
		t.Fatalf("built %d values, want two distinct ones", len(values))
	}
	for _, v := range values {
		workersAwait(t, v.ran, "each value's loop")
	}
	err := shutdownApp(t, app, 5*time.Second)
	for _, name := range []string{"worker-value:a", "worker-value:b"} {
		if e := lifecycleEntry(t, err, name); e.Tier != credo.TierInternal {
			t.Errorf("%s tier = %s, want internal", name, e.Tier)
		}
	}
}

// TestWorkersConstructor_ScheduledValueIsInternal: the built value of a
// scheduled worker is internal while its loop is ingress, so it is torn
// down after the ingress tier.
func TestWorkersConstructor_ScheduledValueIsInternal(t *testing.T) {
	app := mustNew(t)
	j := &journal{}
	v := &ctorOwned{j: j, fail: errors.New("flush failed"), ran: make(chan struct{})}
	app.Workers().Scheduled("report", "@every 1h", func() *ctorOwned { return v },
		credo.ScheduledConfig{RunOnStart: true})
	app.Manage(&stopOnly{name: "ingress component", j: j}, credo.Ingress())
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	workersAwait(t, v.ran, "the run")
	if info, _ := app.Workers().Lookup("report"); info.Scheduled.Tier != credo.TierIngress {
		t.Errorf("loop tier = %s, want ingress", info.Scheduled.Tier)
	}
	err := shutdownApp(t, app, 5*time.Second)
	if e := lifecycleEntry(t, err, "worker-value:report"); e.Tier != credo.TierInternal {
		t.Errorf("worker-value:report tier = %s, want internal", e.Tier)
	}
	events := j.list()
	if i, k := slices.Index(events, "stop:ingress component"), slices.Index(events, "value shutdown"); i < 0 || k < i {
		t.Errorf("events = %q, want the value torn down after the ingress tier", events)
	}
}

// ctorIngressDep is an ingress component a built value depends on.
type ctorIngressDep struct{ _ byte }

func (*ctorIngressDep) Shutdown(context.Context) error { return nil }

// TestWorkersConstructor_BuiltValueOnAnIngressComponent: a built value with
// a teardown of its own that depends on an ingress component fails Finalize
// — or its build, when the teardown is found only on the value — with the
// remedy of splitting the worker; a constructor that returns a binding made
// with credo.Ingress() fails Finalize with the remedy of the run function,
// and the run function of a binding bound without it keeps the loop ingress
// and tears the value down in the internal tier, after the ingress tier.
func TestWorkersConstructor_BuiltValueOnAnIngressComponent(t *testing.T) {
	split := func(call string) string {
		return "the value the constructor of " + call + " builds is internal and has a teardown of its own, " +
			"but depends on the ingress component *credo_test.ctorIngressDep: the value " + call + " builds → " +
			"*credo_test.ctorIngressDep; split the worker, so that the part with the teardown does not use " +
			"*credo_test.ctorIngressDep"
	}
	newApp := func(t *testing.T) *credo.App {
		app := mustNew(t)
		app.Provide(func() *ctorIngressDep { return &ctorIngressDep{} }, credo.Ingress())
		return app
	}
	t.Run("teardown shown by the result type", func(t *testing.T) {
		app := newApp(t)
		app.Workers().Scheduled("report", "@every 1h", func(*ctorIngressDep) *ctorStopOnly { return &ctorStopOnly{} })
		err := app.Finalize()
		if want := "di: " + split(`Workers.Scheduled("report")`); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Finalize() = %v\nwant it to contain %q", err, want)
		}
	})
	t.Run("teardown found only on the value", func(t *testing.T) {
		app := newApp(t)
		app.Workers().Scheduled("report", "@every 1h", func(*ctorIngressDep) credo.Worker {
			return &ctorStopOnly{j: &journal{}, ran: make(chan struct{})}
		})
		mustFinalize(t, app)
		err := app.Start(t.Context())
		entry := lifecycleEntry(t, err, "worker-value:report")
		if want := split(`Workers.Scheduled("report")`); !strings.Contains(entry.Err.Error(), want) {
			t.Fatalf("entry = %+v\nwant it to contain %q", entry, want)
		}
	})
	t.Run("the binding the constructor returns, bound ingress", func(t *testing.T) {
		app := mustNew(t)
		app.Provide(func() *ctorRelay { return newCtorRelay(&journal{}) }, credo.Ingress())
		app.Workers().Scheduled("relay", "@every 1h", func(r *ctorRelay) *ctorRelay { return r })
		const want = `di: the constructor of Workers.Scheduled("relay") returns the ingress component ` +
			"*credo_test.ctorRelay, the value of a binding that owns it: the value Workers.Scheduled(\"relay\") " +
			"builds → *credo_test.ctorRelay; register the worker's run function, the method expression " +
			"(*credo_test.ctorRelay).Run, in place of the constructor"
		if err := app.Finalize(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Finalize() = %v\nwant it to contain %q", err, want)
		}
	})
	t.Run("the binding the run function runs, bound internal", func(t *testing.T) {
		app := mustNew(t)
		j := &journal{}
		relay := newCtorRelay(j)
		relay.shutdownFail = errors.New("flush failed")
		app.Provide(func() *ctorRelay { return relay })
		app.Workers().Scheduled("relay", "@every 1h", (*ctorRelay).Run, credo.ScheduledConfig{RunOnStart: true})
		app.Manage(&stopOnly{name: "ingress component", j: j}, credo.Ingress())
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		workersAwait(t, relay.ran, "the run")
		if info, _ := app.Workers().Lookup("relay"); info.Scheduled.Tier != credo.TierIngress {
			t.Errorf("loop tier = %s, want ingress", info.Scheduled.Tier)
		}
		err := shutdownApp(t, app, 5*time.Second)
		if e := lifecycleEntry(t, err, "*credo_test.ctorRelay"); e.Tier != credo.TierInternal {
			t.Errorf("relay teardown tier = %s, want internal", e.Tier)
		}
		events := j.list()
		i, k := slices.Index(events, "stop:ingress component"), slices.Index(events, "relay shutdown")
		if i < 0 || k < i || slices.Index(events, "run returned") > k {
			t.Errorf("events = %q, want the relay torn down after the loop and the ingress tier", events)
		}
	})
}

// tierDB is a handle that names itself; tierWrapper embeds it.
type tierDB struct {
	starts, stops atomic.Int32
	fail          error
}

func (d *tierDB) ResourceIdentity() any { return d }

func (d *tierDB) Start(context.Context) error {
	d.starts.Add(1)
	return nil
}

func (d *tierDB) Shutdown(context.Context) error {
	d.stops.Add(1)
	return d.fail
}

type tierWrapper struct{ *tierDB }

// TestResourceTier_IsTheOwners: a wrapper that embeds a raw handle and asks
// for credo.Ingress() beside it is refused in either order — a panic for two
// values handed in, a failed start for one a constructor builds — naming the
// option a view leaves to its owner, and a view that asks for it panics at
// the call. A view takes the owner's tier, in either order, with one start
// and one teardown in it; a view of a borrowed handle is neither started nor
// torn down.
func TestResourceTier_IsTheOwners(t *testing.T) {
	t.Run("ProvideValue, raw first", func(t *testing.T) {
		app := mustNew(t)
		db := &tierDB{}
		app.ProvideValue(db)
		msg := mustPanicMessage(t, func() { app.ProvideValue(tierWrapper{db}, credo.Ingress()) })
		registerWant(t, msg, "credo: App.ProvideValue[credo_test.tierWrapper]: credo_test.tierWrapper holds the "+
			"resource that the ProvideValue binding of *credo_test.tierDB owns, and a resource has one owner; "+
			"declare credo_test.tierWrapper a view: give credo.View() to its registration in place of "+
			"credo.Ingress(), which only the owner takes, or, when credo_test.tierWrapper releases state of its "+
			"own, hold the *credo_test.tierDB in a named field of credo_test.tierWrapper, so that it is a resource "+
			"of its own")
	})
	t.Run("ProvideValue, wrapper first", func(t *testing.T) {
		app := mustNew(t)
		db := &tierDB{}
		app.ProvideValue(tierWrapper{db}, credo.Ingress())
		msg := mustPanicMessage(t, func() { app.ProvideValue(db) })
		registerWant(t, msg, "credo: App.ProvideValue[*credo_test.tierDB]: *credo_test.tierDB holds the resource "+
			"that the ProvideValue binding of credo_test.tierWrapper owns, and a resource has one owner; declare "+
			"*credo_test.tierDB a view: give credo.View() to its registration, or, when credo_test.tierWrapper "+
			"releases state of its own, hold the *credo_test.tierDB in a named field of credo_test.tierWrapper, "+
			"so that it is a resource of its own")
	})
	t.Run("wrapper built by Provide", func(t *testing.T) {
		app := mustNew(t)
		db := &tierDB{}
		app.ProvideValue(db)
		app.Provide(func(d *tierDB) tierWrapper { return tierWrapper{d} }, credo.Ingress())
		entry := lifecycleEntry(t, app.Start(t.Context()), "credo_test.tierWrapper")
		const want = "credo_test.tierWrapper holds the resource that the ProvideValue binding of " +
			"*credo_test.tierDB owns, and a resource has one owner; declare credo_test.tierWrapper a view: give " +
			"credo.View() to its registration in place of credo.Ingress(), which only the owner takes"
		if entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
			t.Fatalf("entry = %+v\nwant it to contain %q", entry, want)
		}
	})
	t.Run("a view with credo.Ingress()", func(t *testing.T) {
		app := mustNew(t)
		msg := mustPanicMessage(t, func() {
			app.Provide(func(d *tierDB) tierWrapper { return tierWrapper{d} }, credo.View(), credo.Ingress())
		})
		registerWant(t, msg, "credo: App.Provide[credo_test.tierWrapper]: credo.View() declares "+
			"credo_test.tierWrapper a view of a resource another registration owns, and the tier, the name, the "+
			"readiness entry and the teardown of a resource are its owner's alone; give credo.Ingress() to the "+
			"owner's registration, or drop credo.View()")
	})
	t.Run("Manage with credo.Ingress() beside an internal Provide binding", func(t *testing.T) {
		app := mustNew(t)
		db := &tierDB{}
		app.Provide(func() *tierDB { return db })
		app.Manage(db, credo.Ingress())
		entry := lifecycleEntry(t, app.Start(t.Context()), "*credo_test.tierDB")
		const want = "*credo_test.tierDB holds the resource that the value handed to Manage as *credo_test.tierDB " +
			"owns, and a resource has one owner; declare *credo_test.tierDB a view: give credo.View() to its " +
			"registration"
		if entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
			t.Fatalf("entry = %+v\nwant it to contain %q", entry, want)
		}
	})
	for _, ingress := range []bool{false, true} {
		tier := credo.TierInternal
		var opts []credo.RegistrationOption
		if ingress {
			tier = credo.TierIngress
			opts = append(opts, credo.Ingress())
		}
		for _, viewFirst := range []bool{false, true} {
			name := "the owner's tier, " + tier.String() + ", owner first"
			if viewFirst {
				name = "the owner's tier, " + tier.String() + ", view first"
			}
			t.Run(name, func(t *testing.T) {
				app := mustNew(t)
				db := &tierDB{fail: errors.New("close failed")}
				view := func() { app.Provide(func(d *tierDB) tierWrapper { return tierWrapper{d} }, credo.View()) }
				if viewFirst {
					view()
				}
				app.ProvideValue(db, opts...)
				if !viewFirst {
					view()
				}
				if err := app.Start(t.Context()); err != nil {
					t.Fatalf("App.Start() = %v", err)
				}
				if _, err := app.Resolve[tierWrapper](); err != nil {
					t.Fatal(err)
				}
				err := shutdownApp(t, app, 5*time.Second)
				if e := lifecycleEntry(t, err, "*credo_test.tierDB"); e.Tier != tier {
					t.Errorf("teardown tier = %s, want %s", e.Tier, tier)
				}
				if le, _ := errors.AsType[*credo.LifecycleError](err); len(le.Entries) != 1 {
					t.Errorf("Shutdown() = %v, want the owner's one teardown entry", err)
				}
				if db.starts.Load() != 1 || db.stops.Load() != 1 {
					t.Errorf("started %d and stopped %d times, want once each", db.starts.Load(), db.stops.Load())
				}
			})
		}
	}
	t.Run("a view of a borrowed handle", func(t *testing.T) {
		app := mustNew(t)
		db := &tierDB{}
		app.ProvideValue(db, credo.Borrowed())
		app.Provide(func(d *tierDB) tierWrapper { return tierWrapper{d} }, credo.View())
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("App.Start() = %v", err)
		}
		if _, err := app.Resolve[tierWrapper](); err != nil {
			t.Fatal(err)
		}
		if err := shutdownApp(t, app, 5*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		if db.starts.Load() != 0 || db.stops.Load() != 0 {
			t.Errorf("started %d and stopped %d times, want 0", db.starts.Load(), db.stops.Load())
		}
	})
}

// ctorDBOwner holds the database in a named field and has readiness and a
// teardown of its own: a resource of its own.
type ctorDBOwner struct {
	db    *ctorDB
	stops atomic.Int32
}

func (*ctorDBOwner) Ready(context.Context) error { return nil }

func (o *ctorDBOwner) Shutdown(context.Context) error {
	o.stops.Add(1)
	return nil
}

// TestManage_OneOwnerPerResource: a value handed to Manage owns what it
// holds. A Manage value beside a ProvideValue binding of its resource, a
// wrapper of a bound handle handed in, and two Manage calls of one value
// panic; a constructor whose value embeds a bound handle — owned or borrowed
// — or returns a binding's own value fails the start, naming the remedy; a
// wrapper that holds the handle in a named field is a component of its own,
// with an entry of its own.
func TestManage_OneOwnerPerResource(t *testing.T) {
	t.Run("a value a ProvideValue binding owns", func(t *testing.T) {
		app := mustNew(t)
		db := newCtorDB(&journal{})
		app.ProvideValue(db)
		msg := mustPanicMessage(t, func() { app.Manage(db) })
		registerWant(t, msg, "credo: App.Manage[*credo_test.ctorDB]: Manage(*credo_test.ctorDB) holds the "+
			"resource that the ProvideValue binding of *credo_test.ctorDB owns, and a resource has one owner; drop "+
			"the Manage call, since that binding already owns it")
	})
	t.Run("one value handed to Manage twice", func(t *testing.T) {
		app := mustNew(t)
		db := newCtorDB(&journal{})
		app.Manage(db)
		msg := mustPanicMessage(t, func() { app.Manage(db, credo.ComponentName("again")) })
		registerWant(t, msg, "credo: App.Manage[*credo_test.ctorDB]: Manage(again) holds the resource that the "+
			"value handed to Manage as *credo_test.ctorDB owns, and a resource has one owner; hand each resource "+
			"to Manage once")
	})
	for _, borrowed := range []bool{false, true} {
		name, owner := "a wrapper's constructor over a ProvideValue handle", "owns"
		var opts []credo.RegistrationOption
		if borrowed {
			name, owner = "a wrapper's constructor over a borrowed handle", "owns for its caller (credo.Borrowed())"
			opts = append(opts, credo.Borrowed())
		}
		t.Run(name, func(t *testing.T) {
			app := mustNew(t)
			app.ProvideValue(newCtorDB(&journal{}), opts...)
			app.Manage(func(d *ctorDB) *ctorDBWrapper { return &ctorDBWrapper{d} }, credo.ComponentName("wrapper"))
			entry := lifecycleEntry(t, app.Start(t.Context()), "wrapper")
			want := "the value the constructor handed to Manage(wrapper) builds, a *credo_test.ctorDBWrapper, holds " +
				"the resource that the ProvideValue binding of *credo_test.ctorDB " + owner + ", and a resource has " +
				"one owner; hold the *credo_test.ctorDB in a named field of *credo_test.ctorDBWrapper, so that it is " +
				"a resource of its own"
			if entry.Phase != credo.PhaseStart || entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
				t.Fatalf("entry = %+v\nwant a start failure containing %q", entry, want)
			}
		})
	}
	t.Run("a constructor that returns a binding's own value", func(t *testing.T) {
		app := mustNew(t)
		app.Manage(func(r *ctorRelay) *ctorRelay { return r })
		app.Provide(func() *ctorRelay { return newCtorRelay(&journal{}) })
		entry := lifecycleEntry(t, app.Start(t.Context()), "*credo_test.ctorRelay")
		const want = "the value the constructor handed to Manage(*credo_test.ctorRelay) builds, a " +
			"*credo_test.ctorRelay, holds the resource that the Provide binding of *credo_test.ctorRelay owns, and " +
			"a resource has one owner; drop the Manage call, since that binding already owns the value"
		if entry.Phase != credo.PhaseStart || entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
			t.Fatalf("entry = %+v\nwant a start failure containing %q", entry, want)
		}
	})
	t.Run("a wrapper that holds the handle in a named field", func(t *testing.T) {
		app := mustNew(t)
		db := newCtorDB(&journal{})
		app.ProvideValue(db)
		app.Manage(func(d *ctorDB) *ctorDBOwner { return &ctorDBOwner{db: d} },
			credo.ComponentName("owner"), credo.OptionalReadiness())
		app.UseHealth()
		startServing(t, app)
		if got := readyNames(getReady(t, app)); !slices.Equal(got, []string{"*credo_test.ctorDB", "owner"}) {
			t.Errorf("/ready entries = %q, want the handle's and the owner's", got)
		}
	})
}

package di_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo/internal/di"
)

// events records lifecycle calls in order across components.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	e.log = append(e.log, s)
	e.mu.Unlock()
}

func (e *events) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// unitPool is a resource with Shutdown.
type unitPool struct {
	ev   *events
	name string
}

func (p *unitPool) Shutdown(context.Context) error { p.ev.add("shutdown " + p.name); return nil }

// unitRepo holds a pool and names it as its resource.
type unitRepo struct{ pool *unitPool }

func (r unitRepo) ResourceIdentity() any { return r.pool }

// unitRepoComp holds a pool, names it, and has its own Shutdown.
type unitRepoComp struct{ pool *unitPool }

func (r unitRepoComp) ResourceIdentity() any          { return r.pool }
func (r unitRepoComp) Shutdown(context.Context) error { return r.pool.Shutdown(context.Background()) }

// closeErrConn, closeVoidConn and closeCtxConn have the three Close shapes.
type closeErrConn struct{ ev *events }

func (c *closeErrConn) Close() error { c.ev.add("close err"); return nil }

type closeVoidConn struct{ ev *events }

func (c *closeVoidConn) Close() { c.ev.add("close void") }

type closeCtxConn struct{ ev *events }

func (c *closeCtxConn) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	c.ev.add("close ctx")
	return nil
}

// noClose has no Close method.
type noClose struct{}

func misuseReason(t *testing.T, err error) string {
	t.Helper()
	m, ok := errors.AsType[*di.MisuseError](err)
	if !ok {
		t.Fatalf("err = %v, want *MisuseError", err)
	}
	return m.Reason
}

func TestTeardown_SharedResourceRunsOnceThroughFirstHolder(t *testing.T) {
	ev := &events{}
	pool := &unitPool{ev: ev, name: "pool"}
	c := di.New()
	if err := c.ProvideValue[*unitPool](pool); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideValue[unitRepoComp](unitRepoComp{pool: pool}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := ev.list(); !slices.Equal(got, []string{"shutdown pool"}) {
		t.Fatalf("events = %v, want one shutdown", got)
	}
}

func TestTeardown_HolderWithoutTeardownKeepsResourceUntilRetired(t *testing.T) {
	// unitRepo carries the pool's identity and has no teardown: the pool is
	// torn down once, through its own binding.
	ev := &events{}
	pool := &unitPool{ev: ev, name: "pool"}
	c := di.New()
	c.MustProvideValue[*unitPool](pool)
	c.MustProvide[unitRepo](func(p *unitPool) unitRepo { return unitRepo{pool: p} })
	seal(t, c)
	if _, err := c.Resolve[unitRepo](); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := ev.list(); !slices.Equal(got, []string{"shutdown pool"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestCloser_DistinctResourcesDoNotConflict(t *testing.T) {
	ev := &events{}
	c := di.New()
	c.MustProvideValue[*unitPool](&unitPool{ev: ev})
	if err := c.ProvideValueWith[*closeErrConn](&closeErrConn{ev: ev}, di.Options{Closer: true}); err != nil {
		t.Fatalf("distinct resources must not conflict: %v", err)
	}
}

type identifiedCloser struct {
	pool *unitPool
	ev   *events
}

func (c *identifiedCloser) ResourceIdentity() any { return c.pool }
func (c *identifiedCloser) Close() error          { c.ev.add("close"); return nil }

func TestProvideValue_KindConflictOnSharedResource(t *testing.T) {
	ev := &events{}
	pool := &unitPool{ev: ev}
	c := di.New()
	c.MustProvideValue[*unitPool](pool)
	err := c.ProvideValueWith[*identifiedCloser](&identifiedCloser{pool: pool, ev: ev}, di.Options{Closer: true})
	reason := misuseReason(t, err)
	if !strings.Contains(reason, "disagree on its teardown") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestProvideValue_OwnerConflict(t *testing.T) {
	ev := &events{}
	pool := &unitPool{ev: ev}
	c := di.New()
	c.MustProvideValue[*unitPool](pool)
	err := c.ProvideValueWith[unitRepoComp](unitRepoComp{pool: pool}, di.Options{Borrowed: true})
	reason := misuseReason(t, err)
	if !strings.Contains(reason, "disagree on its owner") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestProvideValue_BorrowedIsNeverShutDown(t *testing.T) {
	ev := &events{}
	c := di.New()
	if err := c.ProvideValueWith[*unitPool](&unitPool{ev: ev, name: "b"}, di.Options{Borrowed: true}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := ev.list(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestCloser_Shapes(t *testing.T) {
	ev := &events{}
	c := di.New()
	if err := c.ProvideValueWith[*closeErrConn](&closeErrConn{ev: ev}, di.Options{Closer: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideValueWith[*closeVoidConn](&closeVoidConn{ev: ev}, di.Options{Closer: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideValueWith[*closeCtxConn](&closeCtxConn{ev: ev}, di.Options{Closer: true}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Internal tier: reverse registration order.
	want := []string{"close ctx", "close void", "close err"}
	if got := ev.list(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestCloser_Misuse(t *testing.T) {
	tests := []struct {
		name string
		call func(c *di.Container) error
		want string
	}{
		{"no Close", func(c *di.Container) error {
			return c.ProvideValueWith[*noClose](&noClose{}, di.Options{Closer: true})
		}, "has no Close"},
		{"already a component", func(c *di.Container) error {
			return c.ProvideValueWith[*unitPool](&unitPool{}, di.Options{Closer: true})
		}, "already a component"},
		{"with Borrowed", func(c *di.Container) error {
			return c.ProvideValueWith[*closeErrConn](&closeErrConn{}, di.Options{Closer: true, Borrowed: true})
		}, "contradict"},
		{"Ingress on a non-component", func(c *di.Container) error {
			return c.ProvideValueWith[*noClose](&noClose{}, di.Options{Ingress: true})
		}, "not a component"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := misuseReason(t, tt.call(di.New()))
			if !strings.Contains(reason, tt.want) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tt.want)
			}
		})
	}
}

func TestOverride(t *testing.T) {
	ev := &events{}
	c := di.New()
	if err := c.ProvideValueWith[*unitPool](&unitPool{ev: ev}, di.Options{Override: true}); err == nil ||
		!strings.Contains(err.Error(), "has none") {
		t.Fatalf("override without a binding: err = %v", err)
	}
	c.MustProvideValue[*unitPool](&unitPool{ev: ev, name: "old"})
	if err := c.ProvideValue[*unitPool](&unitPool{ev: ev}); err == nil {
		t.Fatal("duplicate without Override must fail")
	}
	if err := c.ProvideValueWith[*unitPool](&unitPool{ev: ev, name: "new"}, di.Options{Override: true}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if got := c.MustResolve[*unitPool](); got.name != "new" {
		t.Fatalf("resolved %q, want the override", got.name)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := ev.list(); !slices.Equal(got, []string{"shutdown new"}) {
		t.Fatalf("events = %v; the replaced value is never the App's", got)
	}
}

func TestManage_Misuse(t *testing.T) {
	ev := &events{}
	pool := &unitPool{ev: ev}
	tests := []struct {
		name  string
		setup func(c *di.Container)
		v     any
		o     di.Options
		want  string
	}{
		{"nil", nil, nil, di.Options{}, "must not be nil"},
		{"not a component", nil, &noClose{}, di.Options{}, "no Shutdown"},
		{"twice", func(c *di.Container) {
			if err := c.Manage(pool, di.Options{Name: "a"}); err != nil {
				t.Fatal(err)
			}
		}, pool, di.Options{Name: "b"}, "handed to Manage"},
		{"held by ProvideValue", func(c *di.Container) { c.MustProvideValue[*unitPool](pool) },
			pool, di.Options{}, "already managed through DI"},
		{"duplicate name", func(c *di.Container) {
			if err := c.Manage(&unitPool{ev: ev}, di.Options{Name: "x"}); err != nil {
				t.Fatal(err)
			}
		}, &unitPool{ev: ev}, di.Options{Name: "x"}, "already managed"},
		{"bad constructor", nil, func() {}, di.Options{}, "return 1 or 2 values"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := di.New()
			if tt.setup != nil {
				tt.setup(c)
			}
			reason := misuseReason(t, c.Manage(tt.v, tt.o))
			if !strings.Contains(reason, tt.want) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tt.want)
			}
		})
	}
}

func TestManage_ConstructorIsBuiltByTheStartWalk(t *testing.T) {
	ev := &events{}
	c := di.New()
	c.MustProvideValue[*events](ev)
	if err := c.Manage(func(e *events) *unitPool { return &unitPool{ev: e, name: "managed"} },
		di.Options{Name: "managed"}); err != nil {
		t.Fatal(err)
	}
	if !c.HasStartWork() {
		t.Fatal("a managed constructor is start work")
	}
	seal(t, c)
	plan := c.StartPlan(di.TierInternal)
	if len(plan) != 1 || plan[0].Name() != "managed" {
		t.Fatalf("plan = %v", plan)
	}
	if _, err := c.BuildUnit(plan[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := ev.list(); !slices.Equal(got, []string{"shutdown managed"}) {
		t.Fatalf("events = %v", got)
	}
}

// Tiered teardown fixtures: an ingress server over an internal store.
type tierStore struct{ ev *events }

func (s *tierStore) Shutdown(context.Context) error { s.ev.add("store"); return nil }

type tierServer struct {
	ev    *events
	name  string
	gate  chan struct{}
	enter chan struct{}
}

func (s *tierServer) Shutdown(context.Context) error {
	if s.enter != nil {
		s.enter <- struct{}{}
		<-s.gate
	}
	s.ev.add(s.name)
	return nil
}

type tierServerB struct{ *tierServer }

func TestTeardown_IngressBeforeInternalAndConcurrent(t *testing.T) {
	ev := &events{}
	gate := make(chan struct{})
	enter := make(chan struct{}, 2)
	c := di.New()
	c.MustProvideValue[*events](ev)
	c.MustProvide[*tierStore](func(e *events) *tierStore { return &tierStore{ev: e} })
	if err := c.ProvideWith[*tierServer](func(e *events, _ *tierStore) *tierServer {
		return &tierServer{ev: e, name: "a", gate: gate, enter: enter}
	}, di.Options{Ingress: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideWith[tierServerB](func(e *events, _ *tierStore) tierServerB {
		return tierServerB{&tierServer{ev: e, name: "b", gate: gate, enter: enter}}
	}, di.Options{Ingress: true}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	c.MustResolve[*tierServer]()
	c.MustResolve[tierServerB]()

	done := make(chan error, 1)
	go func() { done <- c.Shutdown(t.Context()) }()
	// Both ingress teardowns must be running at once.
	for range 2 {
		select {
		case <-enter:
		case <-time.After(5 * time.Second):
			t.Fatal("ingress components did not stop concurrently")
		}
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := ev.list()
	if len(got) != 3 || got[2] != "store" {
		t.Fatalf("events = %v, want both servers then the store", got)
	}
}

func TestSeal_InternalComponentDependsOnIngress(t *testing.T) {
	ev := &events{}
	c := di.New()
	c.MustProvideValue[*events](ev)
	if err := c.ProvideWith[*tierServer](func(e *events) *tierServer { return &tierServer{ev: e} },
		di.Options{Ingress: true}); err != nil {
		t.Fatal(err)
	}
	c.MustProvide[unitRepo](func(*tierServer) unitRepo { return unitRepo{} })
	c.MustProvide[*tierStore](func(e *events, _ unitRepo) *tierStore { return &tierStore{ev: e} })
	err := c.Seal()
	if err == nil || !strings.Contains(err.Error(), "internal component *di_test.tierStore depends on the ingress "+
		"component *di_test.tierServer: *di_test.tierStore → di_test.unitRepo → *di_test.tierServer") {
		t.Fatalf("Seal = %v", err)
	}
}

type hiddenComponent interface{ Name() string }

type hiddenImpl struct{ ev *events }

func (h *hiddenImpl) Name() string                   { return "hidden" }
func (h *hiddenImpl) Shutdown(context.Context) error { return nil }

func TestResolve_ComponentFoundOnValueMustNotDependOnIngress(t *testing.T) {
	ev := &events{}
	c := di.New()
	c.MustProvideValue[*events](ev)
	if err := c.ProvideWith[*tierServer](func(e *events) *tierServer { return &tierServer{ev: e} },
		di.Options{Ingress: true}); err != nil {
		t.Fatal(err)
	}
	c.MustProvide[hiddenComponent](func(e *events, _ *tierServer) hiddenComponent { return &hiddenImpl{ev: e} })
	seal(t, c)
	_, err := c.Resolve[hiddenComponent]()
	if err == nil || !strings.Contains(err.Error(), "found only on its built value") {
		t.Fatalf("Resolve = %v", err)
	}
}

type startable struct{ *unitPool }

func (startable) Start(context.Context) error { return nil }

type startableB struct{ *unitPool }

func (startableB) Start(context.Context) error { return nil }

func TestStartPlan_DependencyOrder(t *testing.T) {
	ev := &events{}
	c := di.New()
	c.MustProvideValue[*events](ev)
	// B is registered first but depends on A through a non-component.
	c.MustProvide[startableB](func(e *events, _ unitRepo) startableB { return startableB{&unitPool{ev: e}} })
	c.MustProvide[unitRepo](func(s startable) unitRepo { return unitRepo{pool: s.unitPool} })
	c.MustProvide[startable](func(e *events) startable { return startable{&unitPool{ev: e}} })
	seal(t, c)
	var names []string
	for _, u := range c.StartPlan(di.TierInternal) {
		names = append(names, u.Name())
	}
	want := []string{"di_test.startable", "di_test.startableB"}
	if !slices.Equal(names, want) {
		t.Fatalf("plan = %v, want %v", names, want)
	}
}

// unitMetrics is a component a holder of the pool uses.
type unitMetrics struct{ ev *events }

func (m *unitMetrics) Shutdown(context.Context) error { m.ev.add("shutdown metrics"); return nil }

// unitRepoWith holds the pool, names it, and uses metrics.
type unitRepoWith struct {
	pool *unitPool
	m    *unitMetrics
}

func (r unitRepoWith) ResourceIdentity() any { return r.pool }

func TestTeardown_SharedResourceKeepsAHoldersOtherDependencies(t *testing.T) {
	// Only an edge that closes a cycle is a holder's route to the resource;
	// the metrics a holder uses stay open until the resource is shut down.
	ev := &events{}
	pool := &unitPool{ev: ev, name: "pool"}
	c := di.New()
	c.MustProvideValue[*unitPool](pool)
	c.MustProvide[*unitMetrics](func() *unitMetrics { return &unitMetrics{ev: ev} })
	c.MustProvide[unitRepoWith](func(p *unitPool, m *unitMetrics) unitRepoWith { return unitRepoWith{pool: p, m: m} })
	seal(t, c)
	if _, err := c.Resolve[unitRepoWith](); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := ev.list(); !slices.Equal(got, []string{"shutdown pool", "shutdown metrics"}) {
		t.Fatalf("events = %v, want the pool before the metrics it may use", got)
	}
}

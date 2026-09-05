package di_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo/internal/di"
)

// ctxObservation captures what a Shutdowner saw of its context at call time.
type ctxObservation struct {
	deadline    time.Time
	hasDeadline bool
	err         error
}

// closeLog records Shutdown calls across distinct service types.
type closeLog struct {
	mu    sync.Mutex
	order []string
	ctxs  map[string]ctxObservation
}

func newCloseLog() *closeLog { return &closeLog{ctxs: make(map[string]ctxObservation)} }

func (l *closeLog) record(name string, ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, name)
	obs := ctxObservation{err: ctx.Err()}
	obs.deadline, obs.hasDeadline = ctx.Deadline()
	l.ctxs[name] = obs
}

func (l *closeLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.order)
}

func (l *closeLog) ctxOf(name string) (ctxObservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	obs, ok := l.ctxs[name]
	return obs, ok
}

// sameEntries compares report entries field by field; errors compare by
// identity because a snapshot must not be rewritten after publication.
func sameEntries(a, b []di.ShutdownEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Type != y.Type || x.State != y.State || x.Duration != y.Duration ||
			!errors.Is(x.Err, y.Err) || !errors.Is(y.Err, x.Err) || !slices.Equal(x.Blockers, y.Blockers) {
			return false
		}
	}
	return true
}

// closer is the shared Shutdown behavior embedded by the distinct node types.
type closer struct {
	log   *closeLog
	name  string
	err   error
	panic any
	// block, when non-nil, holds Shutdown until closed (a hung Shutdowner).
	block chan struct{}
	// called is closed on the first Shutdown call.
	called chan struct{}
	once   sync.Once
}

func (c *closer) Shutdown(ctx context.Context) error {
	c.once.Do(func() {
		if c.called != nil {
			close(c.called)
		}
	})
	if c.block != nil {
		<-c.block
	}
	c.log.record(c.name, ctx)
	if c.panic != nil {
		panic(c.panic)
	}
	return c.err
}

func newCloser(log *closeLog, name string) *closer {
	return &closer{log: log, name: name, called: make(chan struct{})}
}

// Distinct registration types sharing the closer behavior.
type nodeDB struct{ *closer }
type nodeCache struct{ *closer }
type nodeRepo struct{ *closer }
type nodeService struct{ *closer }
type nodeAPI struct{ *closer }

// nodePlain has no Shutdown method: an intermediate vertex.
type nodePlain struct{ Repo *nodeRepo }

type storage interface{ Shutdown(context.Context) error }

func entryOf(t *testing.T, report *di.ShutdownError, typ any) di.ShutdownEntry {
	t.Helper()
	want := reflect.TypeOf(typ)
	for _, e := range report.Entries {
		if e.Type == want {
			return e
		}
	}
	t.Fatalf("report has no entry for %s: %v", want, report.Entries)
	return di.ShutdownEntry{}
}

func shutdownReport(t *testing.T, err error) *di.ShutdownError {
	t.Helper()
	report, ok := errors.AsType[*di.ShutdownError](err)
	if !ok {
		t.Fatalf("Shutdown error %v is not a *di.ShutdownError", err)
	}
	return report
}

// waitClosing spins until the container rejects resolution with ErrClosed,
// proving a concurrent Shutdown has entered the closing phase.
func waitClosing[T any](t *testing.T, c *di.Container) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.Resolve[T](); errors.Is(err, di.ErrClosed) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("container never entered the closing phase")
}

func TestShutdown_ConsumersBeforeDependencies_RegardlessOfRegistrationOrder(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	// The consumer is registered BEFORE its dependency; reverse registration
	// order alone would close the DB first.
	c.MustProvide[*nodeService](func(db *nodeDB) *nodeService {
		return &nodeService{newCloser(log, "service")}
	})
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	seal(t, c)
	c.MustResolve[*nodeService]()

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"service", "db"}) {
		t.Fatalf("shutdown order = %v, want [service db]", got)
	}
}

func TestShutdown_ReverseRegistrationTieBreak(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvideValue[*nodeCache](&nodeCache{newCloser(log, "cache")})
	c.MustProvideValue[*nodeRepo](&nodeRepo{newCloser(log, "repo")})

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"repo", "cache", "db"}) {
		t.Fatalf("shutdown order = %v, want reverse registration", got)
	}
}

func TestShutdown_AliasAndCollectionEdges_CloseOnce(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustAlias[storage, *nodeDB]()
	c.MustProvide[*nodeRepo](func(s storage) *nodeRepo { return &nodeRepo{newCloser(log, "repo")} })
	c.MustProvide[*nodeCache](func() *nodeCache { return &nodeCache{newCloser(log, "cache")} })
	c.MustBindMany[storage, *nodeCache]()
	c.MustProvide[*nodeService](func(all []storage) *nodeService {
		return &nodeService{newCloser(log, "service")}
	})
	seal(t, c)
	c.MustResolve[*nodeRepo]()
	c.MustResolve[*nodeService]()

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got := log.snapshot()
	if len(got) != 4 {
		t.Fatalf("shutdown calls = %v, want each instance exactly once", got)
	}
	idx := func(name string) int { return slices.Index(got, name) }
	if idx("repo") > idx("db") {
		t.Errorf("repo (via alias) must close before db: %v", got)
	}
	if idx("service") > idx("cache") {
		t.Errorf("service (via BindMany collection) must close before cache: %v", got)
	}
}

func TestShutdown_NonShutdownerIntermediateKeepsOrder(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	c.MustProvideValue[*nodeRepo](&nodeRepo{newCloser(log, "repo")})
	c.MustProvide[*nodePlain](func(r *nodeRepo) *nodePlain { return &nodePlain{Repo: r} })
	c.MustProvide[*nodeAPI](func(p *nodePlain) *nodeAPI { return &nodeAPI{newCloser(log, "api")} })
	seal(t, c)
	c.MustResolve[*nodeAPI]()

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"api", "repo"}) {
		t.Fatalf("shutdown order = %v, want [api repo] with the plain vertex retired between", got)
	}
}

func TestShutdown_ClosingRejectsResolve(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeRepo](func(db *nodeDB) *nodeRepo { return &nodeRepo{newCloser(log, "repo")} })
	seal(t, c)
	c.MustResolve[*nodeRepo]()

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Cached instances, unbuilt constructors and collections all reject.
	if _, err := c.Resolve[*nodeDB](); !errors.Is(err, di.ErrClosed) {
		t.Fatalf("Resolve of a built value after shutdown = %v, want ErrClosed", err)
	}
	if _, err := c.Resolve[*nodeRepo](); !errors.Is(err, di.ErrClosed) {
		t.Fatalf("Resolve of a built singleton after shutdown = %v, want ErrClosed", err)
	}
	if _, err := c.ResolveAll[storage](); !errors.Is(err, di.ErrClosed) {
		t.Fatalf("ResolveAll after shutdown = %v, want ErrClosed", err)
	}
	if err := c.Shutdown(t.Context()); !errors.Is(err, di.ErrClosed) {
		t.Fatalf("second Shutdown = %v, want ErrClosed", err)
	}
	if _, _, err := c.Replace[*nodeDB](&nodeDB{}); err == nil {
		t.Fatal("Replace after shutdown should be rejected")
	}
}

func TestShutdown_ClosedWinsOverFailedSeal(t *testing.T) {
	c := di.New()
	c.MustProvide[*ServiceWithDep](NewServiceWithDep) // missing dependency
	if err := c.Seal(); err == nil {
		t.Fatal("Seal should fail")
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown after failed Seal: %v", err)
	}
	if _, err := c.Resolve[*ServiceWithDep](); !errors.Is(err, di.ErrClosed) {
		t.Fatalf("Resolve = %v, want ErrClosed to win over the seal error", err)
	}
}

func TestShutdown_WithoutSeal_TearsDownBuiltValues(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	calls := 0
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeRepo](func(db *nodeDB) *nodeRepo {
		calls++
		return &nodeRepo{newCloser(log, "repo")}
	})
	c.Freeze()

	err := c.Shutdown(t.Context())
	if err != nil {
		t.Fatalf("bootstrap Shutdown: %v", err)
	}
	if calls != 0 {
		t.Fatal("teardown must not construct an unbuilt singleton")
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"db"}) {
		t.Fatalf("shutdown order = %v, want [db]", got)
	}
}

func TestShutdown_PendingBuild_BlocksDependencyAndWithholdsResult(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	gate := make(chan struct{})
	started := make(chan struct{})
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeService](func(db *nodeDB) *nodeService {
		close(started)
		<-gate
		return &nodeService{newCloser(log, "service")}
	})
	seal(t, c)

	resolved := make(chan error, 1)
	go func() {
		_, err := c.Resolve[*nodeService]()
		resolved <- err
	}()
	<-started

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- c.Shutdown(t.Context()) }()
	waitClosing[*nodeDB](t, c)

	// The DB is blocked by the pending build: nothing closed yet.
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("closed %v while a dependent build was pending", got)
	}
	close(gate)

	if err := <-resolved; !errors.Is(err, di.ErrClosed) {
		t.Fatalf("Resolve that completed during closing = %v, want ErrClosed", err)
	}
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// The withheld instance is still owned and closed, before its dependency.
	if got := log.snapshot(); !slices.Equal(got, []string{"service", "db"}) {
		t.Fatalf("shutdown order = %v, want [service db]", got)
	}
}

func TestShutdown_PendingBuildFailure_ReleasesDependencies(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	gate := make(chan struct{})
	started := make(chan struct{})
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeService](func(db *nodeDB) (*nodeService, error) {
		close(started)
		<-gate
		return nil, errors.New("init failed")
	})
	seal(t, c)

	go func() { _, _ = c.Resolve[*nodeService]() }()
	<-started
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- c.Shutdown(t.Context()) }()
	waitClosing[*nodeDB](t, c)
	close(gate)

	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"db"}) {
		t.Fatalf("shutdown order = %v, want [db] once the failed build released it", got)
	}
}

func TestShutdown_HungShutdowner_BoundedAndReported(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	hung := newCloser(log, "service")
	hung.block = make(chan struct{})
	t.Cleanup(func() { close(hung.block) })
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeService](func(db *nodeDB) *nodeService { return &nodeService{hung} })
	seal(t, c)
	c.MustResolve[*nodeService]()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Shutdown(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s; the wait must be bounded by ctx", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded in the chain", err)
	}
	report := shutdownReport(t, err)
	if report.Cause == nil || !report.Incomplete() {
		t.Fatalf("report Cause = %v, Incomplete = %v", report.Cause, report.Incomplete())
	}
	svc := entryOf(t, report, (*nodeService)(nil))
	if svc.State != di.ShutdownRunning || svc.Duration <= 0 {
		t.Fatalf("service entry = %+v, want running with elapsed time", svc)
	}
	db := entryOf(t, report, (*nodeDB)(nil))
	if db.State != di.ShutdownBlocked || len(db.Blockers) != 1 || db.Blockers[0] != reflect.TypeOf((*nodeService)(nil)) {
		t.Fatalf("db entry = %+v, want blocked by *nodeService", db)
	}
	// The DB never received a call: no out-of-order fallback.
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("closed %v despite the blocked dependency", got)
	}
	if !strings.Contains(err.Error(), "running") || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("Error() = %q, want running/blocked summary", err)
	}
}

func TestShutdown_ContextEnded_NoRetryAndReportImmutable(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	slow := newCloser(log, "service")
	slow.block = make(chan struct{})
	c.MustProvideValue[*nodeService](&nodeService{slow})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := c.Shutdown(ctx)
	report := shutdownReport(t, err)
	before := slices.Clone(report.Entries)

	// The hung call returns after the boundary: logged, not written back.
	close(slow.block)
	deadline := time.Now().Add(2 * time.Second)
	for len(log.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"service"}) {
		t.Fatalf("the abandoned Shutdown call should complete exactly once, got %v", got)
	}
	if !sameEntries(before, report.Entries) {
		t.Fatalf("report mutated after late completion:\nbefore %v\nafter  %v", before, report.Entries)
	}
	if err.Error() != (&di.ShutdownError{Entries: before, Cause: report.Cause}).Error() {
		t.Fatal("Error() text changed after late completion")
	}
}

func TestShutdown_PanicIsolated(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	bad := newCloser(log, "service")
	bad.panic = "kaboom"
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeService](func(db *nodeDB) *nodeService { return &nodeService{bad} })
	seal(t, c)
	c.MustResolve[*nodeService]()

	err := c.Shutdown(t.Context())
	report := shutdownReport(t, err)
	svc := entryOf(t, report, (*nodeService)(nil))
	if svc.State != di.ShutdownPanicked {
		t.Fatalf("service entry = %+v, want panicked", svc)
	}
	pe, ok := errors.AsType[*di.PanicError](err)
	if !ok || pe.Phase != di.PhaseShutdown || pe.Value != "kaboom" {
		t.Fatalf("PanicError through the report = %+v (ok=%v)", pe, ok)
	}
	// A recovered panic is a completed failed attempt: the dependency proceeds.
	if got := log.snapshot(); !slices.Equal(got, []string{"service", "db"}) {
		t.Fatalf("shutdown order = %v, want [service db]", got)
	}
	if db := entryOf(t, report, (*nodeDB)(nil)); db.State != di.ShutdownSucceeded {
		t.Fatalf("db entry = %+v, want succeeded", db)
	}
	if report.Cause != nil {
		t.Fatalf("Cause = %v, want nil when the context stayed live", report.Cause)
	}
}

func TestShutdown_ErrorsAreUnwrapped(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	failing := newCloser(log, "db")
	sentinel := errors.New("close failed")
	failing.err = sentinel
	c.MustProvideValue[*nodeDB](&nodeDB{failing})
	c.MustProvideValue[*nodeCache](&nodeCache{newCloser(log, "cache")})

	err := c.Shutdown(t.Context())
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, sentinel) = false: %v", err)
	}
	report := shutdownReport(t, err)
	if len(report.Entries) != 2 || report.Entries[0].Type != reflect.TypeOf((*nodeDB)(nil)) {
		t.Fatalf("entries should be in registration order: %v", report.Entries)
	}
	if e := report.Entries[0]; e.State != di.ShutdownFailed || !errors.Is(e.Err, sentinel) || e.Duration < 0 {
		t.Fatalf("db entry = %+v", e)
	}
	if e := report.Entries[1]; e.State != di.ShutdownSucceeded {
		t.Fatalf("cache entry = %+v, want succeeded alongside the failure", e)
	}
	if !strings.Contains(err.Error(), "close failed") {
		t.Errorf("Error() = %q, want the callback error text", err)
	}
}

func TestShutdown_LateConstruction_GetsOneBoundedCleanup(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	gate := make(chan struct{})
	started := make(chan struct{})
	late := newCloser(log, "service")
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeService](func(db *nodeDB) *nodeService {
		close(started)
		<-gate
		return &nodeService{late}
	})
	seal(t, c)

	resolved := make(chan error, 1)
	go func() {
		_, err := c.Resolve[*nodeService]()
		resolved <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := c.Shutdown(ctx)
	report := shutdownReport(t, err)
	if e := entryOf(t, report, (*nodeService)(nil)); e.State != di.ShutdownConstructing || e.Duration <= 0 {
		t.Fatalf("service entry = %+v, want constructing with elapsed time", e)
	}
	if e := entryOf(t, report, (*nodeDB)(nil)); e.State != di.ShutdownBlocked {
		t.Fatalf("db entry = %+v, want blocked by the pending build", e)
	}
	before := slices.Clone(report.Entries)

	// Construction completes after the shutdown context ended.
	close(gate)
	if err := <-resolved; !errors.Is(err, di.ErrClosed) {
		t.Fatalf("late Resolve = %v, want ErrClosed", err)
	}
	select {
	case <-late.called:
	case <-time.After(2 * time.Second):
		t.Fatal("late instance never received its cleanup attempt")
	}
	deadline := time.Now().Add(2 * time.Second)
	obs, ok := log.ctxOf("service")
	for !ok && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		obs, ok = log.ctxOf("service")
	}
	if !ok {
		t.Fatal("cleanup context not recorded")
	}
	// The separate budget is a fixed five seconds, independent of the ended
	// shutdown context.
	if !obs.hasDeadline {
		t.Fatal("late cleanup context should carry a deadline")
	}
	if remaining := time.Until(obs.deadline); remaining > 5*time.Second || remaining < 3*time.Second {
		t.Fatalf("late cleanup deadline in %s, want about five seconds", remaining)
	}
	if obs.err != nil {
		t.Fatalf("late cleanup context was already ended at call time: %v", obs.err)
	}
	if !sameEntries(before, report.Entries) {
		t.Fatal("report mutated by late cleanup")
	}
}

func TestShutdown_SuccessReturnsNil_NotEmptyReport(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeRepo](func(db *nodeDB) *nodeRepo { return &nodeRepo{newCloser(log, "repo")} })
	c.MustProvide[*nodeCache](func() *nodeCache { return &nodeCache{newCloser(log, "cache")} }) // never built
	seal(t, c)
	c.MustResolve[*nodeRepo]()

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
}

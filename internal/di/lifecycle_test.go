package di_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// --- Lifecycle test types ---

type shutdownTracker struct {
	order    *[]string
	name     string
	failWith error
}

func (s *shutdownTracker) Shutdown(ctx context.Context) error {
	*s.order = append(*s.order, s.name)
	return s.failWith
}

// --- Seal/validation tests ---

func TestSeal_CircularDependency(t *testing.T) {
	c := di.New()
	c.MustProvide[*CircularA](NewCircularA)
	c.MustProvide[*CircularB](NewCircularB)

	err := c.Seal()
	if err == nil {
		t.Fatal("expected Seal error for circular dependency")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention 'circular', got: %v", err)
	}
}

func TestSeal_ContextParam_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](func(ctx context.Context) *SimpleService {
		return &SimpleService{Value: "ctx"}
	})

	err := c.Seal()
	if err == nil {
		t.Fatal("expected Seal error for context.Context parameter")
	}
	if !strings.Contains(err.Error(), "context.Context") {
		t.Errorf("error should mention 'context.Context', got: %v", err)
	}
}

func TestSeal_ValidGraph(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)
	c.MustProvide[*ServiceWithDep](NewServiceWithDep)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed on valid graph: %v", err)
	}
}

func TestSeal_ProvideValue(t *testing.T) {
	c := di.New()
	c.MustProvideValue[*SimpleService](&SimpleService{Value: "v"})

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed with ProvideValue: %v", err)
	}
}

type collectionPlugin interface {
	Name() string
}

type collectionPluginConsumer struct {
	plugins []collectionPlugin
}

func NewCollectionPluginConsumer(plugins []collectionPlugin) *collectionPluginConsumer {
	return &collectionPluginConsumer{plugins: plugins}
}

type collectionPluginImpl struct {
	consumer *collectionPluginConsumer
}

func (p *collectionPluginImpl) Name() string { return "plugin" }

func NewCollectionPluginImpl(consumer *collectionPluginConsumer) *collectionPluginImpl {
	return &collectionPluginImpl{consumer: consumer}
}

func TestSeal_InterfaceSliceDependency_AllowsEmptyCollection(t *testing.T) {
	c := di.New()
	c.MustProvide[*collectionPluginConsumer](NewCollectionPluginConsumer)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed with empty BindMany collection: %v", err)
	}
}

func TestSeal_InterfaceSliceDependency_CycleDetected(t *testing.T) {
	c := di.New()
	c.MustProvide[*collectionPluginConsumer](NewCollectionPluginConsumer)
	c.MustProvide[*collectionPluginImpl](NewCollectionPluginImpl)
	c.MustBindMany[collectionPlugin, *collectionPluginImpl]()

	err := c.Seal()
	if err == nil {
		t.Fatal("expected Seal error for collection-based circular dependency")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention 'circular', got: %v", err)
	}
}

// --- Shutdown tests ---

// Types for the validation-order test: five consumers that each miss a
// different dependency, and two independent cycles.
type (
	orderMissing1 struct{}
	orderMissing2 struct{}
	orderMissing3 struct{}
	orderMissing4 struct{}
	orderMissing5 struct{}

	orderNeeds1 struct{}
	orderNeeds2 struct{}
	orderNeeds3 struct{}
	orderNeeds4 struct{}
	orderNeeds5 struct{}

	orderCycleA1 struct{}
	orderCycleB1 struct{}
	orderCycleA2 struct{}
	orderCycleB2 struct{}
)

func newOrderNeeds1(*orderMissing1) *orderNeeds1 { return &orderNeeds1{} }
func newOrderNeeds2(*orderMissing2) *orderNeeds2 { return &orderNeeds2{} }
func newOrderNeeds3(*orderMissing3) *orderNeeds3 { return &orderNeeds3{} }
func newOrderNeeds4(*orderMissing4) *orderNeeds4 { return &orderNeeds4{} }
func newOrderNeeds5(*orderMissing5) *orderNeeds5 { return &orderNeeds5{} }

func newOrderCycleA1(*orderCycleB1) *orderCycleA1 { return &orderCycleA1{} }
func newOrderCycleB1(*orderCycleA1) *orderCycleB1 { return &orderCycleB1{} }
func newOrderCycleA2(*orderCycleB2) *orderCycleA2 { return &orderCycleA2{} }
func newOrderCycleB2(*orderCycleA2) *orderCycleB2 { return &orderCycleB2{} }

// TestSeal_ReportsInRegistrationOrder: the validation error reads the same on
// every run and reports every problem. Each finding sits at the registration
// it belongs to — a missing dependency at the consumer its path starts from, a
// cycle at its earliest registered member, which its text starts at.
func TestSeal_ReportsInRegistrationOrder(t *testing.T) {
	const want = "di: missing dependency: *di_test.orderNeeds1 → *di_test.orderMissing1 (not registered); " +
		"provide *di_test.orderMissing1 before Finalize\n" +
		"di: circular dependency: *di_test.orderCycleB2 → *di_test.orderCycleA2 → *di_test.orderCycleB2; " +
		"remove one of these constructor parameters\n" +
		"di: missing dependency: *di_test.orderNeeds2 → *di_test.orderMissing2 (not registered); " +
		"provide *di_test.orderMissing2 before Finalize\n" +
		"di: missing dependency: *di_test.orderNeeds3 → *di_test.orderMissing3 (not registered); " +
		"provide *di_test.orderMissing3 before Finalize\n" +
		"di: circular dependency: *di_test.orderCycleA1 → *di_test.orderCycleB1 → *di_test.orderCycleA1; " +
		"remove one of these constructor parameters\n" +
		"di: missing dependency: *di_test.orderNeeds4 → *di_test.orderMissing4 (not registered); " +
		"provide *di_test.orderMissing4 before Finalize\n" +
		"di: missing dependency: *di_test.orderNeeds5 → *di_test.orderMissing5 (not registered); " +
		"provide *di_test.orderMissing5 before Finalize"

	// Map iteration order varies per run of the loop, so a container that
	// walks its maps fails this within a few iterations.
	for i := range 50 {
		c := di.New()
		c.MustProvide[*orderNeeds1](newOrderNeeds1)
		c.MustProvide[*orderCycleB2](newOrderCycleB2)
		c.MustProvide[*orderNeeds2](newOrderNeeds2)
		c.MustProvide[*orderCycleA2](newOrderCycleA2)
		c.MustProvide[*orderNeeds3](newOrderNeeds3)
		c.MustProvide[*orderCycleA1](newOrderCycleA1)
		c.MustProvide[*orderNeeds4](newOrderNeeds4)
		c.MustProvide[*orderCycleB1](newOrderCycleB1)
		c.MustProvide[*orderNeeds5](newOrderNeeds5)

		err := c.Seal()
		if err == nil {
			t.Fatal("expected Seal to fail")
		}
		if got := err.Error(); got != want {
			t.Fatalf("run %d: Seal error =\n%s\nwant\n%s", i, got, want)
		}
	}
}

// Types for the dependency-path tests: an order service that reaches a
// missing HTTP client through an aliased payment gateway, and a plugin
// collection whose member misses its store.
type (
	pathOrders   struct{}
	pathGateway  interface{ Charge() }
	pathStripe   struct{}
	pathClient   struct{}
	pathPlugin   interface{ Name() string }
	pathAudit    struct{}
	pathStore    struct{}
	pathPlugins  struct{}
	pathSelfLoop struct{}
)

func (*pathStripe) Charge()     {}
func (*pathAudit) Name() string { return "audit" }

// TestSeal_MissingDependencyPath: a missing dependency is reported with its
// whole path from the entry point that needs it, naming an alias by the
// interface its consumer asked for and a collection member by its slice; a
// shared missing dependency is reported once.
func TestSeal_MissingDependencyPath(t *testing.T) {
	c := di.New()
	c.MustProvide[*pathStripe](func(*pathClient) *pathStripe { return &pathStripe{} })
	c.MustAlias[pathGateway, *pathStripe]()
	c.MustProvide[*pathOrders](func(pathGateway) *pathOrders { return &pathOrders{} })
	c.MustProvide[*pathAudit](func(*pathStore) *pathAudit { return &pathAudit{} })
	c.MustBindMany[pathPlugin, *pathAudit]()
	c.MustProvide[*pathPlugins](func([]pathPlugin) *pathPlugins { return &pathPlugins{} })

	const want = "di: missing dependency: *di_test.pathOrders → di_test.pathGateway (alias of " +
		"*di_test.pathStripe) → *di_test.pathClient (not registered); provide *di_test.pathClient before Finalize\n" +
		"di: missing dependency: *di_test.pathPlugins → *di_test.pathAudit (in []di_test.pathPlugin) → " +
		"*di_test.pathStore (not registered); provide *di_test.pathStore before Finalize"
	err := c.Seal()
	if err == nil || err.Error() != want {
		t.Fatalf("Seal error =\n%v\nwant\n%s", err, want)
	}
}

// TestSeal_SelfDependency: a constructor that takes its own type is a cycle of
// one.
func TestSeal_SelfDependency(t *testing.T) {
	c := di.New()
	c.MustProvide[*pathSelfLoop](func(*pathSelfLoop) *pathSelfLoop { return &pathSelfLoop{} })

	const want = "di: circular dependency: *di_test.pathSelfLoop → *di_test.pathSelfLoop; " +
		"remove one of these constructor parameters"
	if err := c.Seal(); err == nil || err.Error() != want {
		t.Fatalf("Seal error = %v, want %s", err, want)
	}
}

func TestShutdown_ReverseOrder(t *testing.T) {
	c := di.New()
	var order []string

	c.MustProvideValue[*shutdownTracker](&shutdownTracker{order: &order, name: "first"})

	type secondShutdown struct{ *shutdownTracker }
	c.MustProvideValue[*secondShutdown](&secondShutdown{
		shutdownTracker: &shutdownTracker{order: &order, name: "second"},
	})

	type thirdShutdown struct{ *shutdownTracker }
	c.MustProvideValue[*thirdShutdown](&thirdShutdown{
		shutdownTracker: &shutdownTracker{order: &order, name: "third"},
	})

	err := c.Shutdown(t.Context())
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	if len(order) != 3 {
		t.Fatalf("expected 3 shutdowns, got %d", len(order))
	}
	if order[0] != "third" || order[1] != "second" || order[2] != "first" {
		t.Errorf("shutdown order = %v, want [third second first]", order)
	}
}

func TestShutdown_CollectsErrors(t *testing.T) {
	c := di.New()
	var order []string

	c.MustProvideValue[*shutdownTracker](&shutdownTracker{
		order:    &order,
		name:     "first",
		failWith: errors.New("shutdown error 1"),
	})

	type secondShutdown struct{ *shutdownTracker }
	c.MustProvideValue[*secondShutdown](&secondShutdown{
		shutdownTracker: &shutdownTracker{
			order:    &order,
			name:     "second",
			failWith: errors.New("shutdown error 2"),
		},
	})

	err := c.Shutdown(t.Context())
	if err == nil {
		t.Fatal("expected shutdown errors")
	}
	if !strings.Contains(err.Error(), "shutdown error 1") || !strings.Contains(err.Error(), "shutdown error 2") {
		t.Errorf("error should contain both shutdown errors, got: %v", err)
	}
}

func TestShutdown_SkipsNonShutdowner(t *testing.T) {
	c := di.New()
	c.MustProvideValue[*SimpleService](&SimpleService{Value: "no shutdown"})

	err := c.Shutdown(t.Context())
	if err != nil {
		t.Fatalf("Shutdown should not fail for non-Shutdowner services: %v", err)
	}
}

func TestShutdown_ContextAlreadyDone_SkipsAll(t *testing.T) {
	c := di.New()
	var order []string
	c.MustProvideValue[*shutdownTracker](&shutdownTracker{order: &order, name: "first"})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := c.Shutdown(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled shutdown context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
	if len(order) != 0 {
		t.Errorf("no Shutdowner should run with a done context, got %v", order)
	}
}

type cancellingShutdowner struct {
	order  *[]string
	cancel context.CancelFunc
}

func (s *cancellingShutdowner) Shutdown(ctx context.Context) error {
	*s.order = append(*s.order, "canceller")
	s.cancel()
	return nil
}

func TestShutdown_ContextDoneMidway_SkipsRemaining(t *testing.T) {
	c := di.New()
	var order []string

	// Registered first → would shut down last.
	c.MustProvideValue[*shutdownTracker](&shutdownTracker{order: &order, name: "first"})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Registered second → shuts down first and cancels the context.
	c.MustProvideValue[*cancellingShutdowner](&cancellingShutdowner{order: &order, cancel: cancel})

	err := c.Shutdown(ctx)
	if err == nil {
		t.Fatal("expected error after mid-shutdown cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
	if len(order) != 1 || order[0] != "canceller" {
		t.Errorf("only the canceller should have run, got %v", order)
	}
}

func TestShutdown_LazyNotResolved(t *testing.T) {
	var calls atomic.Int32
	c := di.New()
	c.MustProvide[*shutdownTracker](func() *shutdownTracker {
		calls.Add(1)
		return &shutdownTracker{order: &[]string{}, name: "lazy"}
	})

	// Don't resolve — singleton should not be constructed.
	err := c.Shutdown(t.Context())
	if err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if calls.Load() != 0 {
		t.Error("constructor should not be called during shutdown of unresolved singleton")
	}
}

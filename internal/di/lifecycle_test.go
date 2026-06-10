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

func TestSeal_MissingDependency(t *testing.T) {
	c := di.New()
	// ServiceWithDep depends on SimpleService, not registered.
	di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)

	err := c.Seal()
	if err == nil {
		t.Fatal("expected Seal error for missing dependency")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestSeal_CircularDependency(t *testing.T) {
	c := di.New()
	di.MustProvide[*CircularA](c, NewCircularA)
	di.MustProvide[*CircularB](c, NewCircularB)

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
	di.MustProvide[*SimpleService](c, func(ctx context.Context) *SimpleService {
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
	di.MustProvide[*SimpleService](c, NewSimpleService)
	di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed on valid graph: %v", err)
	}
}

func TestSeal_EmptyContainer(t *testing.T) {
	c := di.New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed on empty container: %v", err)
	}
}

func TestSeal_ProvideValue(t *testing.T) {
	c := di.New()
	di.MustProvideValue[*SimpleService](c, &SimpleService{Value: "v"})

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
	di.MustProvide[*collectionPluginConsumer](c, NewCollectionPluginConsumer)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed with empty BindMany collection: %v", err)
	}
}

func TestSeal_InterfaceSliceDependency_CycleDetected(t *testing.T) {
	c := di.New()
	di.MustProvide[*collectionPluginConsumer](c, NewCollectionPluginConsumer)
	di.MustProvide[*collectionPluginImpl](c, NewCollectionPluginImpl)
	di.MustBindMany[collectionPlugin, *collectionPluginImpl](c)

	err := c.Seal()
	if err == nil {
		t.Fatal("expected Seal error for collection-based circular dependency")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention 'circular', got: %v", err)
	}
}

// --- Shutdown tests ---

func TestShutdown_ReverseOrder(t *testing.T) {
	c := di.New()
	var order []string

	di.MustProvideValue[*shutdownTracker](c, &shutdownTracker{order: &order, name: "first"})

	type secondShutdown struct{ *shutdownTracker }
	di.MustProvideValue[*secondShutdown](c, &secondShutdown{
		shutdownTracker: &shutdownTracker{order: &order, name: "second"},
	})

	type thirdShutdown struct{ *shutdownTracker }
	di.MustProvideValue[*thirdShutdown](c, &thirdShutdown{
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

	di.MustProvideValue[*shutdownTracker](c, &shutdownTracker{
		order:    &order,
		name:     "first",
		failWith: errors.New("shutdown error 1"),
	})

	type secondShutdown struct{ *shutdownTracker }
	di.MustProvideValue[*secondShutdown](c, &secondShutdown{
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
	di.MustProvideValue[*SimpleService](c, &SimpleService{Value: "no shutdown"})

	err := c.Shutdown(t.Context())
	if err != nil {
		t.Fatalf("Shutdown should not fail for non-Shutdowner services: %v", err)
	}
}

func TestShutdown_ContextAlreadyDone_SkipsAll(t *testing.T) {
	c := di.New()
	var order []string
	di.MustProvideValue[*shutdownTracker](c, &shutdownTracker{order: &order, name: "first"})

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
	di.MustProvideValue[*shutdownTracker](c, &shutdownTracker{order: &order, name: "first"})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Registered second → shuts down first and cancels the context.
	di.MustProvideValue[*cancellingShutdowner](c, &cancellingShutdowner{order: &order, cancel: cancel})

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
	di.MustProvide[*shutdownTracker](c, func() *shutdownTracker {
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

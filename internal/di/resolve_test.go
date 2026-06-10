package di_test

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

func TestResolve_Singleton(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	s1, err := di.Resolve[*SimpleService](c)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	s2, err := di.Resolve[*SimpleService](c)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if s1 != s2 {
		t.Error("Singleton should return the same instance")
	}
	if s1.Value != "hello" {
		t.Errorf("Value = %q, want %q", s1.Value, "hello")
	}
}

func TestResolve_DependencyChain(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)
	di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)
	di.MustProvide[*ServiceWithTwoDeps](c, NewServiceWithTwoDeps)

	svc, err := di.Resolve[*ServiceWithTwoDeps](c)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if svc.A == nil || svc.B == nil {
		t.Fatal("dependencies should be injected")
	}
	if svc.A.Value != "hello" {
		t.Errorf("A.Value = %q, want %q", svc.A.Value, "hello")
	}
	if svc.B.Simple == nil {
		t.Error("B.Simple should be injected")
	}
	// Singletons: A and B.Simple should be the same instance.
	if svc.A != svc.B.Simple {
		t.Error("Singleton SimpleService should be the same instance across deps")
	}
}

func TestResolve_NotRegistered(t *testing.T) {
	c := di.New()

	_, err := di.Resolve[*SimpleService](c)
	if err == nil {
		t.Fatal("expected error for unregistered service")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

// CircularA depends on CircularB, and vice versa.
type CircularA struct{ B *CircularB }
type CircularB struct{ A *CircularA }

func NewCircularA(b *CircularB) *CircularA { return &CircularA{B: b} }
func NewCircularB(a *CircularA) *CircularB { return &CircularB{A: a} }

func TestResolve_CircularDependency(t *testing.T) {
	c := di.New()
	di.MustProvide[*CircularA](c, NewCircularA)
	di.MustProvide[*CircularB](c, NewCircularB)

	_, err := di.Resolve[*CircularA](c)
	if err == nil {
		t.Fatal("expected circular dependency error")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error should mention 'circular', got: %v", err)
	}
}

func TestResolve_ConstructorError(t *testing.T) {
	c := di.New()
	di.MustProvide[*ServiceWithError](c, NewServiceFailing)

	_, err := di.Resolve[*ServiceWithError](c)
	if err == nil {
		t.Fatal("expected constructor error")
	}
	if !strings.Contains(err.Error(), "construction failed") {
		t.Errorf("error should contain constructor message, got: %v", err)
	}
}

func TestResolve_MissingDependency(t *testing.T) {
	c := di.New()
	// ServiceWithDep depends on SimpleService, but it's not registered.
	di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)

	_, err := di.Resolve[*ServiceWithDep](c)
	if err == nil {
		t.Fatal("expected missing dependency error")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestResolve_ZeroParamConstructor(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	svc, err := di.Resolve[*SimpleService](c)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if svc.Value != "hello" {
		t.Errorf("Value = %q, want %q", svc.Value, "hello")
	}
}

func TestResolve_ConcurrentSingleton(t *testing.T) {
	var callCount atomic.Int32

	c := di.New()
	di.MustProvide[*SimpleService](c, func() *SimpleService {
		callCount.Add(1)
		return &SimpleService{Value: "concurrent"}
	})

	const goroutines = 100
	var wg sync.WaitGroup
	results := make([]*SimpleService, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Go(func() {
			results[i], errs[i] = di.Resolve[*SimpleService](c)
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Resolve failed: %v", i, err)
		}
	}

	// Constructor should be called exactly once.
	if got := callCount.Load(); got != 1 {
		t.Errorf("constructor called %d times, want 1", got)
	}

	// All results should be the same instance.
	first := results[0]
	for i, r := range results[1:] {
		if r != first {
			t.Errorf("goroutine %d: different instance", i+1)
		}
	}
}

func TestMustResolve_Panics(t *testing.T) {
	c := di.New()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for missing service")
		}
	}()
	di.MustResolve[*SimpleService](c)
}

func TestResolve_ProvideValue(t *testing.T) {
	c := di.New()
	original := &SimpleService{Value: "pre-built"}
	di.MustProvideValue[*SimpleService](c, original)

	svc, err := di.Resolve[*SimpleService](c)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if svc != original {
		t.Error("ProvideValue should return the exact same instance")
	}
}

// Interface registration test.
type Greeter interface {
	Greet() string
}

type englishGreeter struct{}

func (g *englishGreeter) Greet() string { return "hello" }

type frenchGreeter struct{}

func (g *frenchGreeter) Greet() string { return "bonjour" }

func NewGreeter() Greeter {
	return &englishGreeter{}
}

func NewEnglishGreeter() *englishGreeter { return &englishGreeter{} }

func NewFrenchGreeter() *frenchGreeter { return &frenchGreeter{} }

func TestResolve_Interface(t *testing.T) {
	c := di.New()
	di.MustProvide[Greeter](c, NewGreeter)

	g, err := di.Resolve[Greeter](c)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if g.Greet() != "hello" {
		t.Errorf("Greet() = %q, want %q", g.Greet(), "hello")
	}
}

type GreeterAggregator struct {
	Greeters []Greeter
}

func NewGreeterAggregator(greeters []Greeter) *GreeterAggregator {
	return &GreeterAggregator{Greeters: greeters}
}

func TestResolveAll_EmptySlice(t *testing.T) {
	c := di.New()

	greeters, err := di.ResolveAll[Greeter](c)
	if err != nil {
		t.Fatalf("ResolveAll[Greeter]: %v", err)
	}
	if greeters == nil {
		t.Fatal("ResolveAll[Greeter] should return an empty slice, not nil")
	}
	if len(greeters) != 0 {
		t.Fatalf("len(ResolveAll[Greeter]) = %d, want 0", len(greeters))
	}
}

func TestResolveAll_OrderAndSingletonIdentity(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)
	di.MustProvide[*frenchGreeter](c, NewFrenchGreeter)
	di.MustBindMany[Greeter, *englishGreeter](c)
	di.MustBindMany[Greeter, *frenchGreeter](c)

	greeters, err := di.ResolveAll[Greeter](c)
	if err != nil {
		t.Fatalf("ResolveAll[Greeter]: %v", err)
	}
	if len(greeters) != 2 {
		t.Fatalf("len(ResolveAll[Greeter]) = %d, want 2", len(greeters))
	}
	if got := greeters[0].Greet(); got != "hello" {
		t.Errorf("greeters[0].Greet() = %q, want %q", got, "hello")
	}
	if got := greeters[1].Greet(); got != "bonjour" {
		t.Errorf("greeters[1].Greet() = %q, want %q", got, "bonjour")
	}

	english := di.MustResolve[*englishGreeter](c)
	french := di.MustResolve[*frenchGreeter](c)
	if greeters[0] != english {
		t.Error("ResolveAll should reuse the english greeter singleton")
	}
	if greeters[1] != french {
		t.Error("ResolveAll should reuse the french greeter singleton")
	}
}

func TestResolveAll_NonInterface_Error(t *testing.T) {
	c := di.New()

	_, err := di.ResolveAll[*SimpleService](c)
	if err == nil {
		t.Fatal("expected error when ResolveAll target is not an interface")
	}
	if !strings.Contains(err.Error(), "interface") {
		t.Errorf("error should mention 'interface', got: %v", err)
	}
}

func TestResolveAll_ConstructorError(t *testing.T) {
	c := di.New()
	di.MustProvide[*badGreeter](c, NewBadGreeter)
	di.MustBindMany[Greeter, *badGreeter](c)

	_, err := di.ResolveAll[Greeter](c)
	if err == nil {
		t.Fatal("expected constructor error")
	}
	if !strings.Contains(err.Error(), "greeter exploded") {
		t.Errorf("error should contain constructor message, got: %v", err)
	}
}

func TestResolve_InterfaceSliceInjection(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)
	di.MustProvide[*frenchGreeter](c, NewFrenchGreeter)
	di.MustBindMany[Greeter, *englishGreeter](c)
	di.MustBindMany[Greeter, *frenchGreeter](c)
	di.MustProvide[*GreeterAggregator](c, NewGreeterAggregator)

	agg, err := di.Resolve[*GreeterAggregator](c)
	if err != nil {
		t.Fatalf("Resolve[*GreeterAggregator]: %v", err)
	}
	if len(agg.Greeters) != 2 {
		t.Fatalf("len(agg.Greeters) = %d, want 2", len(agg.Greeters))
	}
	if agg.Greeters[0].Greet() != "hello" || agg.Greeters[1].Greet() != "bonjour" {
		t.Fatalf("unexpected greeter order: %q, %q", agg.Greeters[0].Greet(), agg.Greeters[1].Greet())
	}
}

func TestResolve_InterfaceSliceInjection_EmptyCollection(t *testing.T) {
	c := di.New()
	di.MustProvide[*GreeterAggregator](c, NewGreeterAggregator)

	agg, err := di.Resolve[*GreeterAggregator](c)
	if err != nil {
		t.Fatalf("Resolve[*GreeterAggregator]: %v", err)
	}
	if agg.Greeters == nil {
		t.Fatal("constructor should receive an empty slice, not nil")
	}
	if len(agg.Greeters) != 0 {
		t.Fatalf("len(agg.Greeters) = %d, want 0", len(agg.Greeters))
	}
}

func TestResolve_InterfaceSliceInjection_DirectRegistrationWins(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)
	di.MustBindMany[Greeter, *englishGreeter](c)

	direct := []Greeter{&frenchGreeter{}}
	di.MustProvideValue[[]Greeter](c, direct)
	di.MustProvide[*GreeterAggregator](c, NewGreeterAggregator)

	agg, err := di.Resolve[*GreeterAggregator](c)
	if err != nil {
		t.Fatalf("Resolve[*GreeterAggregator]: %v", err)
	}
	if len(agg.Greeters) != 1 {
		t.Fatalf("len(agg.Greeters) = %d, want 1", len(agg.Greeters))
	}
	if agg.Greeters[0] != direct[0] {
		t.Fatal("direct []Greeter registration should take precedence over BindMany")
	}

	greeters, err := di.ResolveAll[Greeter](c)
	if err != nil {
		t.Fatalf("ResolveAll[Greeter]: %v", err)
	}
	if len(greeters) != 1 || greeters[0].Greet() != "hello" {
		t.Fatalf("ResolveAll should still use BindMany entries, got %v entries", len(greeters))
	}
}

func TestMustResolveAll_Panics(t *testing.T) {
	c := di.New()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustResolveAll")
		}
	}()

	_ = di.MustResolveAll[*SimpleService](c)
}

type badGreeter struct{}

func (g *badGreeter) Greet() string { return "bad" }

func NewBadGreeter() (*badGreeter, error) {
	return nil, fmt.Errorf("greeter exploded")
}

type ServiceWithConfig struct {
	Value string
}

func NewServiceWithConfig() *ServiceWithConfig {
	return &ServiceWithConfig{Value: "configured"}
}

// ServiceWithThreeDeps tests constructors with 3 parameters.
type ServiceWithThreeDeps struct {
	A *SimpleService
	B *ServiceWithDep
	C *ServiceWithConfig
}

func NewServiceWithThreeDeps(a *SimpleService, b *ServiceWithDep, c *ServiceWithConfig) *ServiceWithThreeDeps {
	return &ServiceWithThreeDeps{A: a, B: b, C: c}
}

func TestResolve_ThreeParams(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)
	di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)
	di.MustProvide[*ServiceWithConfig](c, NewServiceWithConfig)
	di.MustProvide[*ServiceWithThreeDeps](c, NewServiceWithThreeDeps)

	svc, err := di.Resolve[*ServiceWithThreeDeps](c)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if svc.A == nil || svc.B == nil || svc.C == nil {
		t.Fatal("all three deps should be injected")
	}
}

func TestResolve_ConstructorErrorFormatting(t *testing.T) {
	c := di.New()
	di.MustProvide[*ServiceWithError](c, func() (*ServiceWithError, error) {
		return nil, fmt.Errorf("db: connection refused")
	})

	_, err := di.Resolve[*ServiceWithError](c)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "db: connection refused") {
		t.Errorf("error should contain original message, got: %v", err)
	}
}

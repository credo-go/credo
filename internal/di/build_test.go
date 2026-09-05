package di_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

func TestSeal_FreezesContainer(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Provide after Seal should fail.
	err := c.Provide[*ServiceWithDep](NewServiceWithDep)
	if err == nil {
		t.Fatal("expected error for Provide after Seal")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error should mention 'frozen', got: %v", err)
	}
}

func TestSeal_ProvideValueAfterBuild_Error(t *testing.T) {
	c := di.New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := c.ProvideValue[*SimpleService](&SimpleService{})
	if err == nil {
		t.Fatal("expected error for ProvideValue after Seal")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error should mention 'frozen', got: %v", err)
	}
}

func TestSeal_AliasAfterBuild_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*pgUserRepo](NewPgUserRepo)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := c.Alias[UserRepo, *pgUserRepo]()
	if err == nil {
		t.Fatal("expected error for Alias after Seal")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error should mention 'frozen', got: %v", err)
	}
}

func TestSeal_BindManyAfterBuild_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*englishGreeter](NewEnglishGreeter)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := c.BindMany[Greeter, *englishGreeter]()
	if err == nil {
		t.Fatal("expected error for BindMany after Seal")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error should mention 'frozen', got: %v", err)
	}
}

func TestSeal_ValidationError(t *testing.T) {
	c := di.New()
	// ServiceWithDep depends on SimpleService, which is not registered.
	c.MustProvide[*ServiceWithDep](NewServiceWithDep)

	err := c.Seal()
	if err == nil {
		t.Fatal("expected Seal to fail with missing dependency")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestSeal_Idempotent(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	err1 := c.Seal()
	err2 := c.Seal()
	if !errors.Is(err1, err2) {
		t.Errorf("Seal should be idempotent, got err1=%v err2=%v", err1, err2)
	}
}

func TestSeal_ResolveAfterBuild(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	svc, err := c.Resolve[*SimpleService]()
	if err != nil {
		t.Fatalf("Resolve after Seal: %v", err)
	}
	if svc.Value != "hello" {
		t.Errorf("Value = %q, want %q", svc.Value, "hello")
	}

	// Provide after Seal should still fail.
	err = c.Provide[*ServiceWithDep](NewServiceWithDep)
	if err == nil {
		t.Fatal("expected error for Provide after Seal")
	}
}

func TestSeal_Empty(t *testing.T) {
	c := di.New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal on empty container: %v", err)
	}
}

func TestSeal_ResolveAfterFailedSeal(t *testing.T) {
	c := di.New()
	// ServiceWithDep depends on SimpleService, which is not registered.
	c.MustProvide[*ServiceWithDep](NewServiceWithDep)

	sealErr := c.Seal()
	if sealErr == nil {
		t.Fatal("expected Seal to fail")
	}

	// Resolve after failed Seal should return the build error.
	_, err := c.Resolve[*ServiceWithDep]()
	if err == nil {
		t.Fatal("expected Resolve to fail after failed Seal")
	}
	if !strings.Contains(err.Error(), "seal failed") {
		t.Errorf("error should mention 'build failed', got: %v", err)
	}
}

func TestSeal_ResolveBeforeSeal_Rejected(t *testing.T) {
	c := di.New()
	calls := 0
	c.MustProvide[*SimpleService](func() *SimpleService {
		calls++
		return NewSimpleService()
	})
	c.MustProvideValue[*ServiceWithConfig](&ServiceWithConfig{Value: "prebuilt"})

	// Constructor execution starts only after Seal: neither a constructor nor
	// a prebuilt value is resolvable during registration.
	if _, err := c.Resolve[*SimpleService](); err == nil || !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("Resolve before Seal = %v, want not-finalized error", err)
	}
	if _, err := c.Resolve[*ServiceWithConfig](); err == nil || !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("Resolve of prebuilt value before Seal = %v, want not-finalized error", err)
	}
	if _, err := c.ResolveAll[Greeter](); err == nil || !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("ResolveAll before Seal = %v, want not-finalized error", err)
	}
	if calls != 0 {
		t.Fatalf("constructor ran %d times before Seal, want 0", calls)
	}

	seal(t, c)
	if svc := c.MustResolve[*SimpleService](); svc.Value != "hello" || calls != 1 {
		t.Fatalf("after Seal: Value = %q, calls = %d", svc.Value, calls)
	}
}

func TestFreeze_ClosesRegistrationWithoutSeal(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	c.Freeze()

	if err := c.Provide[*ServiceWithDep](NewServiceWithDep); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("Provide after Freeze = %v, want frozen error", err)
	}
	if _, _, err := c.Replace[*SimpleService](&SimpleService{}); err == nil {
		t.Fatal("Replace after Freeze should be rejected")
	}
	// Freeze neither validates nor admits resolution.
	if _, err := c.Resolve[*SimpleService](); err == nil || !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("Resolve after Freeze = %v, want not-finalized error", err)
	}
	// Seal still runs its own validation afterwards and admits resolution.
	seal(t, c)
	if svc := c.MustResolve[*SimpleService](); svc.Value != "hello" {
		t.Fatalf("Resolve after Freeze+Seal: Value = %q", svc.Value)
	}
}

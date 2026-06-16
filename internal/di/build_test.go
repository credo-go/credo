package di_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

func TestSeal_FreezesContainer(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Provide after Seal should fail.
	err := di.Provide[*ServiceWithDep](c, NewServiceWithDep)
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

	err := di.ProvideValue[*SimpleService](c, &SimpleService{})
	if err == nil {
		t.Fatal("expected error for ProvideValue after Seal")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error should mention 'frozen', got: %v", err)
	}
}

func TestSeal_AliasAfterBuild_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*pgUserRepo](c, NewPgUserRepo)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := di.Alias[UserRepo, *pgUserRepo](c)
	if err == nil {
		t.Fatal("expected error for Alias after Seal")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error should mention 'frozen', got: %v", err)
	}
}

func TestSeal_BindManyAfterBuild_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := di.BindMany[Greeter, *englishGreeter](c)
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
	di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)

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
	di.MustProvide[*SimpleService](c, NewSimpleService)

	err1 := c.Seal()
	err2 := c.Seal()
	if !errors.Is(err1, err2) {
		t.Errorf("Seal should be idempotent, got err1=%v err2=%v", err1, err2)
	}
}

func TestSeal_ResolveAfterBuild(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	svc, err := di.Resolve[*SimpleService](c)
	if err != nil {
		t.Fatalf("Resolve after Seal: %v", err)
	}
	if svc.Value != "hello" {
		t.Errorf("Value = %q, want %q", svc.Value, "hello")
	}

	// Provide after Seal should still fail.
	err = di.Provide[*ServiceWithDep](c, NewServiceWithDep)
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
	di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)

	sealErr := c.Seal()
	if sealErr == nil {
		t.Fatal("expected Seal to fail")
	}

	// Resolve after failed Seal should return the build error.
	_, err := di.Resolve[*ServiceWithDep](c)
	if err == nil {
		t.Fatal("expected Resolve to fail after failed Seal")
	}
	if !strings.Contains(err.Error(), "seal failed") {
		t.Errorf("error should mention 'build failed', got: %v", err)
	}
}

func TestSeal_ResolveBeforeBuild(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	// Resolve before Seal should work (bootstrap phase).
	svc, err := di.Resolve[*SimpleService](c)
	if err != nil {
		t.Fatalf("Resolve before Seal should work: %v", err)
	}
	if svc.Value != "hello" {
		t.Errorf("Value = %q, want %q", svc.Value, "hello")
	}
}

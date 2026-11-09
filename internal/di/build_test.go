package di_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

func TestSeal_FreezesContainer(t *testing.T) {
	c := di.New()
	c.MustProvide(NewSimpleService)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Provide after Seal should fail.
	err := c.Provide(NewServiceWithDep)
	if err == nil {
		t.Fatal("expected error for Provide after Seal")
	}
	if !strings.Contains(err.Error(), afterSeal) {
		t.Errorf("error = %v, want it to contain %q", err, afterSeal)
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
	if !strings.Contains(err.Error(), afterSeal) {
		t.Errorf("error = %v, want it to contain %q", err, afterSeal)
	}
}

func TestSeal_AliasAfterBuild_Error(t *testing.T) {
	c := di.New()
	c.MustProvide(NewPgUserRepo)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := c.Alias[UserRepo, *pgUserRepo]()
	if err == nil {
		t.Fatal("expected error for Alias after Seal")
	}
	if !strings.Contains(err.Error(), afterSeal) {
		t.Errorf("error = %v, want it to contain %q", err, afterSeal)
	}
}

func TestSeal_BindManyAfterBuild_Error(t *testing.T) {
	c := di.New()
	c.MustProvide(NewEnglishGreeter)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	err := c.BindMany[Greeter, *englishGreeter]()
	if err == nil {
		t.Fatal("expected error for BindMany after Seal")
	}
	if !strings.Contains(err.Error(), afterSeal) {
		t.Errorf("error = %v, want it to contain %q", err, afterSeal)
	}
}

func TestSeal_ValidationError(t *testing.T) {
	c := di.New()
	// ServiceWithDep depends on SimpleService, which is not registered.
	c.MustProvide(NewServiceWithDep)

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
	c.MustProvide(NewSimpleService)

	err1 := c.Seal()
	err2 := c.Seal()
	if !errors.Is(err1, err2) {
		t.Errorf("Seal should be idempotent, got err1=%v err2=%v", err1, err2)
	}
}

func TestSeal_ResolveAfterBuild(t *testing.T) {
	c := di.New()
	c.MustProvide(NewSimpleService)

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
	err = c.Provide(NewServiceWithDep)
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
	c.MustProvide(NewServiceWithDep)

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

// afterSeal is the reason of a registration after Seal.
const afterSeal = "called after the container was finalized by Finalize; register before that call"

func TestSeal_ResolveSealsImplicitly(t *testing.T) {
	c := di.New()
	calls := 0
	c.MustProvide(func() *SimpleService {
		calls++
		return NewSimpleService()
	})

	// The first Resolve seals: the constructor runs then, and the container
	// records the call as what finalized it.
	if svc, err := c.Resolve[*SimpleService](); err != nil || svc.Value != "hello" || calls != 1 {
		t.Fatalf("Resolve = %v, %v; calls = %d", svc, err, calls)
	}
	want := "called after the container was finalized by Resolve[*di_test.SimpleService]; register before that call"
	if reason := misuseReason(t, c.Provide(NewServiceWithDep)); reason != want {
		t.Fatalf("Provide after the first Resolve: reason = %q, want %q", reason, want)
	}
	// A later Seal leaves the recorded trigger as it was.
	seal(t, c)
	if reason := misuseReason(t, c.Provide(NewServiceWithDep)); reason != want {
		t.Fatalf("Provide after a later Seal: reason = %q, want %q", reason, want)
	}
}

func TestSeal_ResolveAllSealsImplicitly(t *testing.T) {
	c := di.New()
	c.MustProvide(NewEnglishGreeter)
	c.MustBindMany[Greeter, *englishGreeter]()
	if got, err := c.ResolveAll[Greeter](); err != nil || len(got) != 1 {
		t.Fatalf("ResolveAll = %v, %v", got, err)
	}
	want := "called after the container was finalized by ResolveAll[di_test.Greeter]; register before that call"
	if reason := misuseReason(t, c.Provide(NewSimpleService)); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

func TestSealBy_RecordsTheFirstTrigger(t *testing.T) {
	c := di.New()
	if err := c.SealBy(di.Trigger{By: "Run at main.go:9", Remedy: "register before that call"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SealBy(di.Trigger{By: "Finalize at main.go:10", Remedy: "register before that call"}); err != nil {
		t.Fatal(err)
	}
	called := false
	c.SealOnResolve(func() di.Trigger {
		called = true
		return di.Trigger{By: "Resolve[x]"}
	})
	if called {
		t.Error("SealOnResolve computed a trigger for a sealed container")
	}
	want := "called after the container was finalized by Run at main.go:9; register before that call"
	if reason := misuseReason(t, c.Provide(NewSimpleService)); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

func TestSealOnResolve_SealsWithItsTrigger(t *testing.T) {
	c := di.New()
	c.MustProvide(NewSimpleService)
	c.SealOnResolve(func() di.Trigger {
		return di.Trigger{By: "MustResolve[*app.S] at main.go:3", Remedy: "register before that call"}
	})
	if _, err := c.Resolve[*SimpleService](); err != nil {
		t.Fatal(err)
	}
	want := "called after the container was finalized by MustResolve[*app.S] at main.go:3; register before that call"
	if reason := misuseReason(t, c.Provide(NewServiceWithDep)); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

func TestSeal_FailedImplicitSealIsEveryResolvesError(t *testing.T) {
	c := di.New()
	c.MustProvide(NewServiceWithDep) // needs *SimpleService, which is not registered
	_, first := c.Resolve[*ServiceWithDep]()
	if first == nil || !strings.Contains(first.Error(), "missing dependency") {
		t.Fatalf("first Resolve = %v, want the seal's missing-dependency report", first)
	}
	_, later := c.Resolve[*ServiceWithDep]()
	if later == nil || later.Error() != first.Error() {
		t.Fatalf("later Resolve = %v, want %v", later, first)
	}
}

func TestFreeze_ClosesRegistrationWithoutSeal(t *testing.T) {
	c := di.New()
	c.MustProvide(NewSimpleService)

	c.Freeze()

	if err := c.Provide(NewServiceWithDep); err == nil || !strings.Contains(err.Error(), "called after shutdown began") {
		t.Fatalf("Provide after Freeze = %v, want after-shutdown error", err)
	}
	if err := c.ProvideValueWith[*SimpleService](&SimpleService{}, di.Options{Override: true}); err == nil {
		t.Fatal("an override after Freeze should be rejected")
	}
	// Freeze neither validates nor admits resolution: the App is shutting
	// down, so a Resolve does not seal it.
	if _, err := c.Resolve[*SimpleService](); !errors.Is(err, di.ErrClosed) {
		t.Fatalf("Resolve after Freeze = %v, want ErrClosed", err)
	}
	// Seal still runs its own validation afterwards and admits resolution.
	seal(t, c)
	if svc := c.MustResolve[*SimpleService](); svc.Value != "hello" {
		t.Fatalf("Resolve after Freeze+Seal: Value = %q", svc.Value)
	}
}

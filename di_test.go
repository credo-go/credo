package credo_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// --- DI test types ---

type diSimpleService struct {
	Value string
}

func newDISimpleService() *diSimpleService {
	return &diSimpleService{Value: "hello"}
}

type diServiceWithDep struct {
	Simple *diSimpleService
}

func newDIServiceWithDep(s *diSimpleService) *diServiceWithDep {
	return &diServiceWithDep{Simple: s}
}

// --- Provide / Resolve tests ---

func TestProvide_Resolve(t *testing.T) {
	app := mustNew(t)
	if err := credo.Provide[*diSimpleService](app, newDISimpleService); err != nil {
		t.Fatalf("Provide: %v", err)
	}

	svc, err := credo.Resolve[*diSimpleService](app)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svc.Value != "hello" {
		t.Errorf("Value = %q, want %q", svc.Value, "hello")
	}
}

func TestMustProvide_MustResolve(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)

	svc := credo.MustResolve[*diSimpleService](app)
	if svc.Value != "hello" {
		t.Errorf("Value = %q, want %q", svc.Value, "hello")
	}
}

func TestProvideValue_Resolve(t *testing.T) {
	app := mustNew(t)
	original := &diSimpleService{Value: "pre-built"}
	if err := credo.ProvideValue[*diSimpleService](app, original); err != nil {
		t.Fatalf("ProvideValue: %v", err)
	}

	svc, err := credo.Resolve[*diSimpleService](app)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svc != original {
		t.Error("ProvideValue should return the exact same instance")
	}
}

func TestProvideFactory_Resolve(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)

	// T is inferred from fn's signature — the compiler checks the whole chain.
	err := credo.ProvideFactory(app, func(app *credo.App) (*diServiceWithDep, error) {
		simple, err := credo.Resolve[*diSimpleService](app)
		if err != nil {
			return nil, err
		}
		return &diServiceWithDep{Simple: simple}, nil
	})
	if err != nil {
		t.Fatalf("ProvideFactory: %v", err)
	}

	svc, err := credo.Resolve[*diServiceWithDep](app)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svc.Simple == nil || svc.Simple.Value != "hello" {
		t.Errorf("dependency not wired through fn: %+v", svc.Simple)
	}
}

func TestMustProvideFactory_PanicsOnDuplicate(t *testing.T) {
	app := mustNew(t)
	credo.MustProvideFactory(app, func(*credo.App) (*diSimpleService, error) {
		return &diSimpleService{}, nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate MustProvideFactory")
		}
	}()
	credo.MustProvideFactory(app, func(*credo.App) (*diSimpleService, error) {
		return &diSimpleService{}, nil
	})
}

func TestMustProvideValue(t *testing.T) {
	app := mustNew(t)
	credo.MustProvideValue[*diSimpleService](app, &diSimpleService{Value: "v"})

	svc := credo.MustResolve[*diSimpleService](app)
	if svc.Value != "v" {
		t.Errorf("Value = %q, want %q", svc.Value, "v")
	}
}

func TestProvide_DependencyChain(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)
	credo.MustProvide[*diServiceWithDep](app, newDIServiceWithDep)

	svc, err := credo.Resolve[*diServiceWithDep](app)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svc.Simple == nil {
		t.Fatal("Simple dep should be injected")
	}
	if svc.Simple.Value != "hello" {
		t.Errorf("Simple.Value = %q, want %q", svc.Simple.Value, "hello")
	}
}

func TestProvide_Duplicate(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)

	err := credo.Provide[*diSimpleService](app, newDISimpleService)
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestResolve_NotRegistered(t *testing.T) {
	app := mustNew(t)

	_, err := credo.Resolve[*diSimpleService](app)
	if err == nil {
		t.Fatal("expected error for unregistered service")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

// --- Bind tests ---

type diUserRepo interface {
	FindByID(id int) string
}

type diPgUserRepo struct{}

func (p *diPgUserRepo) FindByID(id int) string { return "pg-user" }
func newDIPgUserRepo() *diPgUserRepo           { return &diPgUserRepo{} }

func TestAlias_ResolveByInterface(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diPgUserRepo](app, newDIPgUserRepo)
	credo.MustAlias[diUserRepo, *diPgUserRepo](app)

	repo, err := credo.Resolve[diUserRepo](app)
	if err != nil {
		t.Fatalf("Resolve[diUserRepo]: %v", err)
	}
	if repo.FindByID(1) != "pg-user" {
		t.Errorf("FindByID(1) = %q, want %q", repo.FindByID(1), "pg-user")
	}
}

func TestAlias_Error(t *testing.T) {
	app := mustNew(t)

	err := credo.Alias[diUserRepo, *diPgUserRepo](app)
	if err == nil {
		t.Fatal("expected error when T is not registered")
	}
}

// --- Finalize tests ---

func TestFinalize(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)

	if err := credo.Finalize(app); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Resolve after Finalize works.
	svc, err := credo.Resolve[*diSimpleService](app)
	if err != nil {
		t.Fatalf("Resolve after Finalize: %v", err)
	}
	if svc.Value != "hello" {
		t.Errorf("Value = %q, want %q", svc.Value, "hello")
	}

	// Provide after Finalize should fail.
	err = credo.Provide[*diServiceWithDep](app, newDIServiceWithDep)
	if err == nil {
		t.Fatal("expected error for Provide after Finalize")
	}
}

// --- Finalize validation tests ---

func TestFinalize_ValidGraph(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)
	credo.MustProvide[*diServiceWithDep](app, newDIServiceWithDep)

	if err := credo.Finalize(app); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

func TestFinalize_MissingDep(t *testing.T) {
	app := mustNew(t)
	// diServiceWithDep depends on diSimpleService which is not registered.
	credo.MustProvide[*diServiceWithDep](app, newDIServiceWithDep)

	err := credo.Finalize(app)
	if err == nil {
		t.Fatal("expected Finalize error")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestFinalize_Empty(t *testing.T) {
	app := mustNew(t)
	if err := credo.Finalize(app); err != nil {
		t.Fatalf("Finalize on empty container: %v", err)
	}
}

// --- RawConfig auto-registration tests ---

func TestNew_WithRawConfig_AutoRegistersRawConfig(t *testing.T) {
	rc := newServerConfigRC(map[string]any{})
	app, err := credo.New(credo.WithRawConfig(rc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resolved, err := credo.Resolve[credo.RawConfig](app)
	if err != nil {
		t.Fatalf("Resolve[RawConfig]: %v", err)
	}
	if resolved != rc {
		t.Error("resolved RawConfig should be the same instance passed to WithRawConfig")
	}
}

func TestNew_NoConfig_AutoLoadsRawConfig(t *testing.T) {
	app := mustNew(t)
	rc, err := credo.Resolve[credo.RawConfig](app)
	if err != nil {
		t.Fatalf("Resolve[RawConfig]: %v (auto-load should always register RawConfig)", err)
	}
	if rc == nil {
		t.Fatal("auto-loaded RawConfig should not be nil")
	}
}

// --- Container shutdown in Shutdown() ---

type diShutdownTracker struct {
	order *[]string
	name  string
}

func (s *diShutdownTracker) Shutdown(_ context.Context) error {
	*s.order = append(*s.order, s.name)
	return nil
}

func TestApp_Shutdown_ShutdownsContainer(t *testing.T) {
	host, port, _ := freePort(t)
	app := mustNew(t, credo.WithAddr(host, port))

	var order []string
	credo.MustProvideValue[*diShutdownTracker](app, &diShutdownTracker{
		order: &order,
		name:  "svc",
	})

	// We need to run and shut down to test the full lifecycle.
	// Use a goroutine for Run since it blocks.
	started := make(chan struct{})
	go func() {
		// Wait for running state, then signal.
		for !app.IsRunning() {
			// spin until running
		}
		close(started)
	}()

	go app.Run()
	<-started

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if len(order) != 1 || order[0] != "svc" {
		t.Errorf("shutdown order = %v, want [svc]", order)
	}
}

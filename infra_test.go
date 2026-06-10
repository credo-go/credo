package credo_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// --- Model 1: Infra as first parameter ---

type model1Service struct {
	Infra credo.Infra
	Value string
}

func newModel1Service(infra credo.Infra) *model1Service {
	return &model1Service{Infra: infra, Value: "m1"}
}

func TestInfra_Model1_Injection(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*model1Service](app, newModel1Service)

	svc, err := credo.Resolve[*model1Service](app)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svc.Value != "m1" {
		t.Errorf("Value = %q, want %q", svc.Value, "m1")
	}
	if svc.Infra.Logger == nil {
		t.Fatal("Logger should not be nil")
	}
}

func TestInfra_Model1_LoggerScoping(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	credo.MustProvide[*model1Service](app, newModel1Service)
	svc := credo.MustResolve[*model1Service](app)

	svc.Infra.Logger.Info("test message")
	output := buf.String()
	if !strings.Contains(output, "service=model1Service") {
		t.Errorf("log output should contain service name, got: %s", output)
	}
}

// --- Model 1 with additional dependencies ---

type model1WithDep struct {
	Infra  credo.Infra
	Simple *diSimpleService
}

func newModel1WithDep(infra credo.Infra, s *diSimpleService) *model1WithDep {
	return &model1WithDep{Infra: infra, Simple: s}
}

func TestInfra_Model1_WithOtherDeps(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)
	credo.MustProvide[*model1WithDep](app, newModel1WithDep)

	svc, err := credo.Resolve[*model1WithDep](app)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svc.Infra.Logger == nil {
		t.Fatal("Infra.Logger should not be nil")
	}
	if svc.Simple == nil {
		t.Fatal("Simple dep should be injected")
	}
	if svc.Simple.Value != "hello" {
		t.Errorf("Simple.Value = %q, want %q", svc.Simple.Value, "hello")
	}
}

// --- Pure constructor (no Infra) ---

func TestInfra_PureConstructor_StillWorks(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*diSimpleService](app, newDISimpleService)
	credo.MustProvide[*diServiceWithDep](app, newDIServiceWithDep)

	svc, err := credo.Resolve[*diServiceWithDep](app)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if svc.Simple == nil || svc.Simple.Value != "hello" {
		t.Error("pure constructor should still work without Infra")
	}
}

// --- Default-logger fallback (no logger configured) ---

func TestInfra_DefaultLoggerFallback(t *testing.T) {
	app := mustNew(t) // no WithLogger
	credo.MustProvide[*model1Service](app, newModel1Service)

	svc := credo.MustResolve[*model1Service](app)

	// Logger should fall back to the framework default logger (non-nil).
	if svc.Infra.Logger == nil {
		t.Fatal("Logger should not be nil (default fallback)")
	}
}

// --- NewInfra (non-DI scoped Infra) ---

func TestApp_NewInfra_ScopedLogger(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	infra := app.NewInfra("MyMiddleware")
	infra.Logger.Info("hello")

	output := buf.String()
	if !strings.Contains(output, "service=MyMiddleware") {
		t.Errorf("log output should contain service=MyMiddleware, got: %s", output)
	}
}

func TestApp_NewInfra_NilSafety(t *testing.T) {
	// NewInfra must not panic when the app has no logger configured;
	// it falls back to the framework default logger via app.Logger().
	infra := new(credo.App).NewInfra("X")
	if infra.Logger == nil {
		t.Fatal("NewInfra on a zero App should fall back to the default logger")
	}

	infra = (*credo.App)(nil).NewInfra("X")
	if infra.Logger == nil {
		t.Fatal("NewInfra on a nil App should fall back to the default logger")
	}
}

// --- Finalize with Infra ---

func TestInfra_Finalize_Model1_Valid(t *testing.T) {
	app := mustNew(t)
	credo.MustProvide[*model1Service](app, newModel1Service)

	if err := credo.Finalize(app); err != nil {
		t.Fatalf("Finalize should pass for Model 1: %v", err)
	}
}

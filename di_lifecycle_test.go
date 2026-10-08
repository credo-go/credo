package credo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

type diPanicky struct{}

func TestResolve_BeforeFinalize_Rejected(t *testing.T) {
	app := mustNew(t)
	calls := 0
	app.Provide[*diSimpleService](func() *diSimpleService {
		calls++
		return newDISimpleService()
	})
	expectPanicContaining(t, "credo: App.Resolve[*credo_test.diSimpleService]: called before Finalize", func() {
		_, _ = app.Resolve[*diSimpleService]()
	})
	if calls != 0 {
		t.Fatalf("constructor ran %d times before Finalize", calls)
	}
	mustFinalize(t, app)
	if svc := app.MustResolve[*diSimpleService](); svc.Value != "hello" || calls != 1 {
		t.Fatalf("after Finalize: Value = %q, calls = %d", svc.Value, calls)
	}
}

func TestHas(t *testing.T) {
	app := mustNew(t)
	if app.Has[*diSimpleService]() {
		t.Fatal("Has before registration = true")
	}
	calls := 0
	app.Provide[*diSimpleService](func() *diSimpleService {
		calls++
		return newDISimpleService()
	})
	if !app.Has[*diSimpleService]() || !app.Has[credo.RawConfig]() {
		t.Fatal("Has should report constructor and value bindings")
	}
	if calls != 0 {
		t.Fatal("Has must not construct")
	}
	// An override after Has still replaces the binding: Has reserves nothing.
	app.ProvideValue[*diSimpleService](&diSimpleService{Value: "override"}, credo.Override())
	mustFinalize(t, app)
	if !app.Has[*diSimpleService]() {
		t.Fatal("Has after Finalize = false")
	}
	if calls != 0 {
		t.Fatal("Has must not construct after Finalize")
	}
}

func TestDIDiagnostics_PublicTypes(t *testing.T) {
	app := mustNew(t)
	app.Provide[*diPanicky](func() *diPanicky { panic("boom") })
	mustFinalize(t, app)

	_, err := app.Resolve[*diPanicky]()
	pe, ok := errors.AsType[*credo.DIPanicError](err)
	if !ok || pe.Phase != credo.DIPanicConstruction || pe.Value != "boom" || pe.Stack == "" {
		t.Fatalf("Resolve = %v, want a construction DIPanicError with value and stack", err)
	}
	if _, err := app.Resolve[*diPanicky](); !errors.Is(err, pe) {
		t.Fatalf("later Resolve = %v, want the same terminal failure", err)
	}
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v, want nil (construction failures are diagnostics only)", err)
	}
	if _, err := app.Resolve[*diPanicky](); !errors.Is(err, credo.ErrDIClosed) {
		t.Fatalf("Resolve after Shutdown = %v, want ErrDIClosed", err)
	}
}

func TestResolve_DuringDrain_LogsDebug(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.ProvideValue[*diSimpleService](&diSimpleService{Value: "v"})
	// An ingress stop hook runs before the container enters closing.
	app.OnStop(func(context.Context) error {
		_, err := app.Resolve[*diSimpleService]()
		return err
	}, credo.Ingress())
	mustFinalize(t, app)
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v (DI stays live through the ingress tier)", err)
	}
	var noted bool
	for _, e := range parseJSONLines(t, logs.Bytes()) {
		if e["level"] == "DEBUG" && strings.Contains(e["msg"].(string), "Resolve during drain") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("a Resolve from a drain hook should be logged at Debug")
	}
}

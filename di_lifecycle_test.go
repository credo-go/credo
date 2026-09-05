package credo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// diCloser is a Shutdowner value for ownership and diagnostics tests.
type diCloser struct {
	name   string
	closed bool
}

func (c *diCloser) Shutdown(context.Context) error {
	c.closed = true
	return nil
}

type diPanicky struct{}

func TestResolve_BeforeFinalize_Rejected(t *testing.T) {
	app := mustNew(t)
	calls := 0
	app.MustProvide[*diSimpleService](func() *diSimpleService {
		calls++
		return newDISimpleService()
	})
	if _, err := app.Resolve[*diSimpleService](); err == nil || !strings.Contains(err.Error(), "not finalized") {
		t.Fatalf("Resolve before Finalize = %v, want not-finalized error", err)
	}
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
	app.MustProvide[*diSimpleService](func() *diSimpleService {
		calls++
		return newDISimpleService()
	})
	if !app.Has[*diSimpleService]() || !app.Has[credo.RawConfig]() {
		t.Fatal("Has should report constructor and value bindings")
	}
	if calls != 0 {
		t.Fatal("Has must not construct")
	}
	if _, _, err := app.Replace[*diSimpleService](&diSimpleService{}); err != nil {
		t.Fatalf("Has must not protect: Replace = %v", err)
	}
}

func TestAdoptValue(t *testing.T) {
	app := mustNew(t)
	original := &diSimpleService{Value: "prebuilt"}
	app.MustProvideValue[*diSimpleService](original)

	got, err := app.AdoptValue[*diSimpleService](func(s *diSimpleService) error {
		if s.Value == "" {
			return errors.New("empty")
		}
		return nil
	})
	if err != nil || got != original {
		t.Fatalf("AdoptValue = (%p, %v), want %p", got, err, original)
	}
	if _, _, err := app.Replace[*diSimpleService](&diSimpleService{}); err == nil {
		t.Fatal("adopted binding should be Replace-protected")
	}

	// A constructor binding is rejected without running.
	calls := 0
	app.MustProvide[*diServiceWithDep](func(s *diSimpleService) *diServiceWithDep {
		calls++
		return &diServiceWithDep{Simple: s}
	})
	if _, err := app.AdoptValue[*diServiceWithDep](nil); err == nil || !strings.Contains(err.Error(), "constructor") {
		t.Fatalf("AdoptValue of a constructor = %v, want explanatory error", err)
	}
	if calls != 0 {
		t.Fatal("AdoptValue must never construct")
	}
	mustFinalize(t, app)
	if _, err := app.AdoptValue[*diSimpleService](nil); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("AdoptValue after Finalize = %v, want frozen error", err)
	}
}

func TestReplace_ReturnsSupersededInstance(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	oldDB := &diCloser{name: "old"}
	app.MustProvideValue[*diCloser](oldDB)

	newDB := &diCloser{name: "new"}
	old, existed, err := app.Replace[*diCloser](newDB)
	if err != nil || !existed || old != oldDB {
		t.Fatalf("Replace = (%v, %v, %v), want (old, true, nil)", old, existed, err)
	}
	// An unbuilt constructor yields no previous instance.
	app.MustProvide[*diSimpleService](newDISimpleService)
	oldSvc, existed, err := app.Replace[*diSimpleService](&diSimpleService{Value: "mock"})
	if err != nil || existed || oldSvc != nil {
		t.Fatalf("Replace of an unbuilt constructor = (%v, %v, %v), want (nil, false, nil)", oldSvc, existed, err)
	}
	// MustReplace mirrors the previous-instance information.
	if got, existed := app.MustReplace[*diSimpleService](&diSimpleService{Value: "mock2"}); !existed || got.Value != "mock" {
		t.Fatalf("MustReplace = (%v, %v)", got, existed)
	}

	// The container owns newDB and no longer tracks oldDB.
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if !newDB.closed || oldDB.closed {
		t.Fatalf("closed: new=%v old=%v; the caller owns the superseded instance", newDB.closed, oldDB.closed)
	}
	var warned bool
	for _, e := range parseJSONLines(t, logs.Bytes()) {
		if e["level"] == "WARN" && strings.Contains(e["msg"].(string), "Replace superseded a Shutdowner") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("replacing a Shutdowner should log a Warn diagnostic")
	}
}

func TestDIDiagnostics_PublicTypes(t *testing.T) {
	app := mustNew(t)
	app.MustProvide[*diPanicky](func() *diPanicky { panic("boom") })
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
	app.MustProvideValue[*diSimpleService](&diSimpleService{Value: "v"})
	app.OnDrain(func(context.Context) error {
		_, err := app.Resolve[*diSimpleService]()
		return err
	})
	mustFinalize(t, app)
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v (DI stays live through the drain phases)", err)
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

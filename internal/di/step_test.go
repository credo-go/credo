package di_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

type stepStore struct{ name string }

type stepPinger interface{ ping() }

func (*stepStore) ping() {}

func noopStep(call string) di.StartStep {
	return di.StartStep{Call: call, Run: func(context.Context, any) error { return nil }}
}

// unitOfType returns the registry unit bound to t.
func unitOfType(t *testing.T, c *di.Container, typ reflect.Type) *di.Unit {
	t.Helper()
	for _, u := range c.Units() {
		if u.Type() == typ {
			return u
		}
	}
	t.Fatalf("no unit for %s", typ)
	return nil
}

func TestAddStartStep_AttachesToTheBindingAtSeal(t *testing.T) {
	c := di.New()
	typ := reflect.TypeFor[*stepStore]()
	// The step may precede the binding inside the registration window.
	if err := c.AddStartStep(typ, noopStep("first")); err != nil {
		t.Fatalf("AddStartStep() = %v", err)
	}
	if err := c.ProvideValue(&stepStore{name: "a"}); err != nil {
		t.Fatalf("ProvideValue() = %v", err)
	}
	if err := c.AddStartStep(typ, noopStep("second")); err != nil {
		t.Fatalf("AddStartStep() = %v", err)
	}
	if !c.HasStartWork() {
		t.Fatal("HasStartWork() = false, want true with a start step")
	}
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal() = %v", err)
	}

	steps := unitOfType(t, c, typ).Steps()
	if len(steps) != 2 || steps[0].Call != "first" || steps[1].Call != "second" {
		t.Fatalf("Steps() = %v, want first then second", steps)
	}
}

func TestAddStartStep_RunsOnTheUnitAnAliasNames(t *testing.T) {
	c := di.New()
	if err := c.ProvideValue(&stepStore{}); err != nil {
		t.Fatalf("ProvideValue() = %v", err)
	}
	if err := c.Alias[stepPinger, *stepStore](); err != nil {
		t.Fatalf("Alias() = %v", err)
	}
	if err := c.AddStartStep(reflect.TypeFor[stepPinger](), noopStep("aliased")); err != nil {
		t.Fatalf("AddStartStep() = %v", err)
	}
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	if steps := unitOfType(t, c, reflect.TypeFor[*stepStore]()).Steps(); len(steps) != 1 {
		t.Fatalf("Steps() = %d, want 1 on the aliased binding", len(steps))
	}
}

func TestAddStartStep_MissingBindingIsAFinding(t *testing.T) {
	c := di.New()
	if err := c.AddStartStep(reflect.TypeFor[*stepStore](), noopStep("store.Register[*x]")); err != nil {
		t.Fatalf("AddStartStep() = %v", err)
	}
	err := c.Seal()
	if err == nil {
		t.Fatal("Seal() = nil, want a finding for the unbound step")
	}
	for _, want := range []string{"store.Register[*x]", "has no binding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Seal() = %q, want it to contain %q", err, want)
		}
	}
}

func TestAddStartStep_Misuse(t *testing.T) {
	c := di.New()
	err := c.AddStartStep(reflect.TypeFor[*stepStore](), di.StartStep{Call: "store.Register[*x]"})
	m, ok := errors.AsType[*di.MisuseError](err)
	if !ok || m.Call != "store.Register[*x]" {
		t.Fatalf("AddStartStep(nil Run) = %v, want a misuse naming the call", err)
	}

	if sealErr := c.Seal(); sealErr != nil {
		t.Fatalf("Seal() = %v", sealErr)
	}
	err = c.AddStartStep(reflect.TypeFor[*stepStore](), noopStep("store.Register[*x]"))
	m, ok = errors.AsType[*di.MisuseError](err)
	if !ok || m.Call != "store.Register[*x]" {
		t.Fatalf("AddStartStep after Seal = %v, want a misuse naming the call", err)
	}
}

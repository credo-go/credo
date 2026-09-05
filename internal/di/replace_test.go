package di

import (
	"context"
	"strings"
	"testing"
)

type replaceSvc struct {
	id string
}

type replaceDep struct {
	id string
}

func mustSeal(t *testing.T, c *Container) {
	t.Helper()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
}

func TestReplace_NewBinding(t *testing.T) {
	c := New()
	old, existed, err := c.Replace[*replaceSvc](&replaceSvc{id: "a"})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if existed || old != nil {
		t.Fatalf("Replace of a new binding returned (%v, %v), want (nil, false)", old, existed)
	}
	if c.RegistrationCount() != 1 {
		t.Errorf("RegistrationCount = %d, want 1", c.RegistrationCount())
	}
	mustSeal(t, c)
	if got := c.MustResolve[*replaceSvc](); got.id != "a" {
		t.Errorf("id = %q, want a", got.id)
	}
}

func TestReplace_OverwritesProvideValue_ReturnsOld(t *testing.T) {
	c := New()
	real := &replaceSvc{id: "real"}
	c.MustProvideValue[*replaceSvc](real)
	old, existed, err := c.Replace[*replaceSvc](&replaceSvc{id: "mock"})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	// The superseded instance is handed back with its cleanup responsibility.
	if !existed || old != real {
		t.Fatalf("Replace returned (%p, %v), want (real %p, true)", old, existed, real)
	}
	// Overwriting an existing registration must not create a duplicate.
	if c.RegistrationCount() != 1 {
		t.Errorf("RegistrationCount = %d, want 1 (no duplicate)", c.RegistrationCount())
	}
	mustSeal(t, c)
	if got := c.MustResolve[*replaceSvc](); got.id != "mock" {
		t.Errorf("id = %q, want mock", got.id)
	}
}

func TestReplace_OverwritesConstructor_NeverRunsIt(t *testing.T) {
	c := New()
	called := false
	c.MustProvide[*replaceSvc](func() *replaceSvc {
		called = true
		return &replaceSvc{id: "real"}
	})
	old, existed := c.MustReplace[*replaceSvc](&replaceSvc{id: "mock"})
	if existed || old != nil {
		t.Fatalf("Replace over an unbuilt constructor returned (%v, %v), want (nil, false)", old, existed)
	}

	mustSeal(t, c)
	if got := c.MustResolve[*replaceSvc](); got.id != "mock" {
		t.Errorf("id = %q, want mock", got.id)
	}
	if called {
		t.Error("constructor should not run after Replace overwrote it")
	}
}

type replaceCloser struct {
	order *[]string
	name  string
}

func (r *replaceCloser) Shutdown(context.Context) error {
	*r.order = append(*r.order, r.name)
	return nil
}

func TestReplace_SupersededInstanceIsNotShutDown(t *testing.T) {
	c := New()
	var order []string
	first := &replaceCloser{order: &order, name: "first"}
	c.MustProvideValue[*replaceCloser](first)
	old, existed, err := c.Replace[*replaceCloser](&replaceCloser{order: &order, name: "second"})
	if err != nil || !existed || old != first {
		t.Fatalf("Replace = (%p, %v, %v), want (first %p, true, nil)", old, existed, err, first)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Ownership of the old instance moved to the caller: only the replacement
	// is closed by the container.
	if len(order) != 1 || order[0] != "second" {
		t.Fatalf("shutdown order = %v, want [second]", order)
	}
}

func TestReplace_Frozen(t *testing.T) {
	c := New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_, _, err := c.Replace[*replaceSvc](&replaceSvc{id: "x"})
	if err == nil {
		t.Fatal("expected error replacing on a sealed container")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error = %q, want it to mention 'frozen'", err)
	}
}

func TestReplace_DependentResolvesAfterSeal(t *testing.T) {
	c := New()
	c.MustProvideValue[*replaceDep](&replaceDep{id: "real"})
	c.MustProvide[*replaceSvc](func(d *replaceDep) *replaceSvc {
		return &replaceSvc{id: d.id}
	})
	// Swap the dependency for a mock before sealing.
	c.MustReplace[*replaceDep](&replaceDep{id: "mock"})

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal after Replace: %v", err)
	}
	if got := c.MustResolve[*replaceSvc](); got.id != "mock" {
		t.Errorf("dependent resolved with id = %q, want mock", got.id)
	}
}

func TestMustReplace_PanicsWhenFrozen(t *testing.T) {
	c := New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected MustReplace to panic on a sealed container")
		}
	}()
	c.MustReplace[*replaceSvc](&replaceSvc{id: "x"})
}

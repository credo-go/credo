package di

import (
	"strings"
	"testing"
)

type replaceSvc struct {
	id string
}

type replaceDep struct {
	id string
}

func TestReplace_NewBinding(t *testing.T) {
	c := New()
	if err := Replace[*replaceSvc](c, &replaceSvc{id: "a"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if c.RegistrationCount() != 1 {
		t.Errorf("RegistrationCount = %d, want 1", c.RegistrationCount())
	}
	if got := MustResolve[*replaceSvc](c); got.id != "a" {
		t.Errorf("id = %q, want a", got.id)
	}
}

func TestReplace_OverwritesProvideValue(t *testing.T) {
	c := New()
	MustProvideValue[*replaceSvc](c, &replaceSvc{id: "real"})
	if err := Replace[*replaceSvc](c, &replaceSvc{id: "mock"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	// Overwriting an existing registration must not create a duplicate.
	if c.RegistrationCount() != 1 {
		t.Errorf("RegistrationCount = %d, want 1 (no duplicate)", c.RegistrationCount())
	}
	if got := MustResolve[*replaceSvc](c); got.id != "mock" {
		t.Errorf("id = %q, want mock", got.id)
	}
}

func TestReplace_OverwritesConstructor(t *testing.T) {
	c := New()
	called := false
	MustProvide[*replaceSvc](c, func() *replaceSvc {
		called = true
		return &replaceSvc{id: "real"}
	})
	MustReplace[*replaceSvc](c, &replaceSvc{id: "mock"})

	if got := MustResolve[*replaceSvc](c); got.id != "mock" {
		t.Errorf("id = %q, want mock", got.id)
	}
	if called {
		t.Error("constructor should not run after Replace overwrote it")
	}
}

func TestReplace_SupersedesResolvedSingleton(t *testing.T) {
	c := New()
	MustProvideValue[*replaceSvc](c, &replaceSvc{id: "real"})
	// Resolve once so the singleton is cached.
	if got := MustResolve[*replaceSvc](c); got.id != "real" {
		t.Fatalf("pre-replace id = %q, want real", got.id)
	}
	MustReplace[*replaceSvc](c, &replaceSvc{id: "mock"})
	if got := MustResolve[*replaceSvc](c); got.id != "mock" {
		t.Errorf("post-replace id = %q, want mock (cached singleton not superseded)", got.id)
	}
}

func TestReplace_Frozen(t *testing.T) {
	c := New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	err := Replace[*replaceSvc](c, &replaceSvc{id: "x"})
	if err == nil {
		t.Fatal("expected error replacing on a sealed container")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error = %q, want it to mention 'frozen'", err)
	}
}

func TestReplace_DependentResolvesAfterSeal(t *testing.T) {
	c := New()
	MustProvideValue[*replaceDep](c, &replaceDep{id: "real"})
	MustProvide[*replaceSvc](c, func(d *replaceDep) *replaceSvc {
		return &replaceSvc{id: d.id}
	})
	// Swap the dependency for a mock before sealing.
	MustReplace[*replaceDep](c, &replaceDep{id: "mock"})

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal after Replace: %v", err)
	}
	if got := MustResolve[*replaceSvc](c); got.id != "mock" {
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
	MustReplace[*replaceSvc](c, &replaceSvc{id: "x"})
}

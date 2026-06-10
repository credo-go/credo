package credo_test

import (
	"testing"

	"github.com/credo-go/credo"
)

type replaceService struct {
	name string
}

func TestReplace_NewBinding(t *testing.T) {
	app := mustNew(t)
	if err := credo.Replace[*replaceService](app, &replaceService{name: "x"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := credo.MustResolve[*replaceService](app); got.name != "x" {
		t.Errorf("name = %q, want x", got.name)
	}
}

func TestReplace_OverridesExisting(t *testing.T) {
	app := mustNew(t)
	credo.MustProvideValue[*replaceService](app, &replaceService{name: "real"})
	if err := credo.Replace[*replaceService](app, &replaceService{name: "mock"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := credo.MustResolve[*replaceService](app); got.name != "mock" {
		t.Errorf("name = %q, want mock", got.name)
	}
}

func TestReplace_AfterFinalizeErrors(t *testing.T) {
	app := mustNew(t)
	if err := credo.Finalize(app); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := credo.Replace[*replaceService](app, &replaceService{name: "mock"}); err == nil {
		t.Fatal("expected error replacing after Finalize")
	}
}

func TestMustReplace_PanicsAfterFinalize(t *testing.T) {
	app := mustNew(t)
	if err := credo.Finalize(app); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected MustReplace to panic after Finalize")
		}
	}()
	credo.MustReplace[*replaceService](app, &replaceService{name: "mock"})
}

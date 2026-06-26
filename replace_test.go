package credo_test

import "testing"

type replaceService struct {
	name string
}

func TestReplace_NewBinding(t *testing.T) {
	app := mustNew(t)
	if err := app.Replace[*replaceService](&replaceService{name: "x"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := app.MustResolve[*replaceService](); got.name != "x" {
		t.Errorf("name = %q, want x", got.name)
	}
}

func TestReplace_OverridesExisting(t *testing.T) {
	app := mustNew(t)
	app.MustProvideValue[*replaceService](&replaceService{name: "real"})
	if err := app.Replace[*replaceService](&replaceService{name: "mock"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := app.MustResolve[*replaceService](); got.name != "mock" {
		t.Errorf("name = %q, want mock", got.name)
	}
}

func TestReplace_AfterFinalizeErrors(t *testing.T) {
	app := mustNew(t)
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := app.Replace[*replaceService](&replaceService{name: "mock"}); err == nil {
		t.Fatal("expected error replacing after Finalize")
	}
}

func TestMustReplace_PanicsAfterFinalize(t *testing.T) {
	app := mustNew(t)
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected MustReplace to panic after Finalize")
		}
	}()
	app.MustReplace[*replaceService](&replaceService{name: "mock"})
}

package di_test

import (
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// --- Alias test types ---

type UserRepo interface {
	FindByID(id int) string
}

// pgUserRepo has a field so it is not a zero-size struct; this ensures
// pointer identity comparisons in tests are meaningful.
type pgUserRepo struct{ dsn string }

func (p *pgUserRepo) FindByID(id int) string { return "pg-user" }

func NewPgUserRepo() *pgUserRepo { return &pgUserRepo{dsn: "postgres://localhost/test"} }

// --- Alias tests ---

func TestAlias_ResolveByInterface(t *testing.T) {
	c := di.New()
	di.MustProvide[*pgUserRepo](c, NewPgUserRepo)
	di.MustAlias[UserRepo, *pgUserRepo](c)

	repo, err := di.Resolve[UserRepo](c)
	if err != nil {
		t.Fatalf("Resolve[UserRepo]: %v", err)
	}
	if repo.FindByID(1) != "pg-user" {
		t.Errorf("FindByID(1) = %q, want %q", repo.FindByID(1), "pg-user")
	}
}

func TestAlias_SameInstance(t *testing.T) {
	c := di.New()
	di.MustProvide[*pgUserRepo](c, NewPgUserRepo)
	di.MustAlias[UserRepo, *pgUserRepo](c)

	concrete, err := di.Resolve[*pgUserRepo](c)
	if err != nil {
		t.Fatalf("Resolve[*pgUserRepo]: %v", err)
	}
	iface, err := di.Resolve[UserRepo](c)
	if err != nil {
		t.Fatalf("Resolve[UserRepo]: %v", err)
	}
	if concrete != iface.(*pgUserRepo) {
		t.Error("Alias should return the same singleton instance")
	}
}

func TestAlias_NotInterface_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*pgUserRepo](c, NewPgUserRepo)

	// *pgUserRepo is not an interface — should fail.
	err := di.Alias[*pgUserRepo, *pgUserRepo](c)
	if err == nil {
		t.Fatal("expected error when I is not an interface")
	}
	if !strings.Contains(err.Error(), "interface") {
		t.Errorf("error should mention 'interface', got: %v", err)
	}
}

func TestAlias_ConcreteNotRegistered_Error(t *testing.T) {
	c := di.New()

	err := di.Alias[UserRepo, *pgUserRepo](c)
	if err == nil {
		t.Fatal("expected error when T is not registered")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestAlias_DoesNotImplement_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	// *SimpleService does not implement UserRepo.
	err := di.Alias[UserRepo, *SimpleService](c)
	if err == nil {
		t.Fatal("expected error when T does not implement I")
	}
	if !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("error should mention 'does not implement', got: %v", err)
	}
}

func TestAlias_DuplicateAlias_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*pgUserRepo](c, NewPgUserRepo)
	di.MustAlias[UserRepo, *pgUserRepo](c)

	err := di.Alias[UserRepo, *pgUserRepo](c)
	if err == nil {
		t.Fatal("expected error for duplicate binding")
	}
	if !strings.Contains(err.Error(), "already has an alias") {
		t.Errorf("error should mention 'already has a binding', got: %v", err)
	}
}

func TestAlias_InterfaceAlreadyRegistered_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*pgUserRepo](c, NewPgUserRepo)
	// Register interface directly.
	di.MustProvide[UserRepo](c, func() UserRepo { return &pgUserRepo{} })

	err := di.Alias[UserRepo, *pgUserRepo](c)
	if err == nil {
		t.Fatal("expected error when interface already has direct registration")
	}
	if !strings.Contains(err.Error(), "already has a direct registration") {
		t.Errorf("error should mention 'already has a direct registration', got: %v", err)
	}
}

func TestMustAlias_Panics(t *testing.T) {
	c := di.New()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustAlias")
		}
	}()
	// T not registered → should panic.
	di.MustAlias[UserRepo, *pgUserRepo](c)
}

func TestAlias_Seal_Passes(t *testing.T) {
	c := di.New()
	di.MustProvide[*pgUserRepo](c, NewPgUserRepo)
	di.MustAlias[UserRepo, *pgUserRepo](c)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal should pass with valid alias: %v", err)
	}
}

func TestBindMany_ResolveAllByInterface(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)
	di.MustProvide[*frenchGreeter](c, NewFrenchGreeter)
	di.MustBindMany[Greeter, *englishGreeter](c)
	di.MustBindMany[Greeter, *frenchGreeter](c)

	greeters, err := di.ResolveAll[Greeter](c)
	if err != nil {
		t.Fatalf("ResolveAll[Greeter]: %v", err)
	}
	if len(greeters) != 2 {
		t.Fatalf("len(ResolveAll[Greeter]) = %d, want 2", len(greeters))
	}
}

func TestBindMany_NotInterface_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)

	err := di.BindMany[*englishGreeter, *englishGreeter](c)
	if err == nil {
		t.Fatal("expected error when I is not an interface")
	}
	if !strings.Contains(err.Error(), "interface") {
		t.Errorf("error should mention 'interface', got: %v", err)
	}
}

func TestBindMany_ConcreteNotRegistered_Error(t *testing.T) {
	c := di.New()

	err := di.BindMany[Greeter, *englishGreeter](c)
	if err == nil {
		t.Fatal("expected error when T is not registered")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestBindMany_ConcreteTypeMustNotBeInterface(t *testing.T) {
	c := di.New()
	di.MustProvide[Greeter](c, NewGreeter)

	err := di.BindMany[Greeter, Greeter](c)
	if err == nil {
		t.Fatal("expected error when T is an interface")
	}
	if !strings.Contains(err.Error(), "concrete type") {
		t.Errorf("error should mention 'concrete type', got: %v", err)
	}
}

func TestBindMany_DoesNotImplement_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	err := di.BindMany[Greeter, *SimpleService](c)
	if err == nil {
		t.Fatal("expected error when T does not implement I")
	}
	if !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("error should mention 'does not implement', got: %v", err)
	}
}

func TestBindMany_Duplicate_Error(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)
	di.MustBindMany[Greeter, *englishGreeter](c)

	err := di.BindMany[Greeter, *englishGreeter](c)
	if err == nil {
		t.Fatal("expected error for duplicate multi-binding")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}
}

func TestMustBindMany_Panics(t *testing.T) {
	c := di.New()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustBindMany")
		}
	}()

	di.MustBindMany[Greeter, *englishGreeter](c)
}

func TestBindMany_Seal_Passes(t *testing.T) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)
	di.MustBindMany[Greeter, *englishGreeter](c)

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal should pass with valid BindMany: %v", err)
	}
}

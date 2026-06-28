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
	c.MustProvide[*pgUserRepo](NewPgUserRepo)
	c.MustAlias[UserRepo, *pgUserRepo]()

	repo, err := c.Resolve[UserRepo]()
	if err != nil {
		t.Fatalf("Resolve[UserRepo]: %v", err)
	}
	if repo.FindByID(1) != "pg-user" {
		t.Errorf("FindByID(1) = %q, want %q", repo.FindByID(1), "pg-user")
	}
}

func TestAlias_SameInstance(t *testing.T) {
	c := di.New()
	c.MustProvide[*pgUserRepo](NewPgUserRepo)
	c.MustAlias[UserRepo, *pgUserRepo]()

	concrete, err := c.Resolve[*pgUserRepo]()
	if err != nil {
		t.Fatalf("Resolve[*pgUserRepo]: %v", err)
	}
	iface, err := c.Resolve[UserRepo]()
	if err != nil {
		t.Fatalf("Resolve[UserRepo]: %v", err)
	}
	if concrete != iface.(*pgUserRepo) {
		t.Error("Alias should return the same singleton instance")
	}
}

func TestAlias_NotInterface_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*pgUserRepo](NewPgUserRepo)

	// *pgUserRepo is not an interface — should fail.
	err := c.Alias[*pgUserRepo, *pgUserRepo]()
	if err == nil {
		t.Fatal("expected error when I is not an interface")
	}
	if !strings.Contains(err.Error(), "interface") {
		t.Errorf("error should mention 'interface', got: %v", err)
	}
}

func TestAlias_ConcreteNotRegistered_Error(t *testing.T) {
	c := di.New()

	err := c.Alias[UserRepo, *pgUserRepo]()
	if err == nil {
		t.Fatal("expected error when T is not registered")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestAlias_DoesNotImplement_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	// *SimpleService does not implement UserRepo.
	err := c.Alias[UserRepo, *SimpleService]()
	if err == nil {
		t.Fatal("expected error when T does not implement I")
	}
	if !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("error should mention 'does not implement', got: %v", err)
	}
}

func TestAlias_DuplicateAlias_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*pgUserRepo](NewPgUserRepo)
	c.MustAlias[UserRepo, *pgUserRepo]()

	err := c.Alias[UserRepo, *pgUserRepo]()
	if err == nil {
		t.Fatal("expected error for duplicate binding")
	}
	if !strings.Contains(err.Error(), "already has an alias") {
		t.Errorf("error should mention 'already has a binding', got: %v", err)
	}
}

func TestAlias_InterfaceAlreadyRegistered_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*pgUserRepo](NewPgUserRepo)
	// Register interface directly.
	c.MustProvide[UserRepo](func() UserRepo { return &pgUserRepo{} })

	err := c.Alias[UserRepo, *pgUserRepo]()
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
	c.MustAlias[UserRepo, *pgUserRepo]()
}

func TestAlias_Seal_Passes(t *testing.T) {
	c := di.New()
	c.MustProvide[*pgUserRepo](NewPgUserRepo)
	c.MustAlias[UserRepo, *pgUserRepo]()

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal should pass with valid alias: %v", err)
	}
}

func TestBindMany_ResolveAllByInterface(t *testing.T) {
	c := di.New()
	c.MustProvide[*englishGreeter](NewEnglishGreeter)
	c.MustProvide[*frenchGreeter](NewFrenchGreeter)
	c.MustBindMany[Greeter, *englishGreeter]()
	c.MustBindMany[Greeter, *frenchGreeter]()

	greeters, err := c.ResolveAll[Greeter]()
	if err != nil {
		t.Fatalf("ResolveAll[Greeter]: %v", err)
	}
	if len(greeters) != 2 {
		t.Fatalf("len(ResolveAll[Greeter]) = %d, want 2", len(greeters))
	}
}

func TestBindMany_NotInterface_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*englishGreeter](NewEnglishGreeter)

	err := c.BindMany[*englishGreeter, *englishGreeter]()
	if err == nil {
		t.Fatal("expected error when I is not an interface")
	}
	if !strings.Contains(err.Error(), "interface") {
		t.Errorf("error should mention 'interface', got: %v", err)
	}
}

func TestBindMany_ConcreteNotRegistered_Error(t *testing.T) {
	c := di.New()

	err := c.BindMany[Greeter, *englishGreeter]()
	if err == nil {
		t.Fatal("expected error when T is not registered")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should mention 'not registered', got: %v", err)
	}
}

func TestBindMany_ConcreteTypeMustNotBeInterface(t *testing.T) {
	c := di.New()
	c.MustProvide[Greeter](NewGreeter)

	err := c.BindMany[Greeter, Greeter]()
	if err == nil {
		t.Fatal("expected error when T is an interface")
	}
	if !strings.Contains(err.Error(), "concrete type") {
		t.Errorf("error should mention 'concrete type', got: %v", err)
	}
}

func TestBindMany_DoesNotImplement_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	err := c.BindMany[Greeter, *SimpleService]()
	if err == nil {
		t.Fatal("expected error when T does not implement I")
	}
	if !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("error should mention 'does not implement', got: %v", err)
	}
}

func TestBindMany_Duplicate_Error(t *testing.T) {
	c := di.New()
	c.MustProvide[*englishGreeter](NewEnglishGreeter)
	c.MustBindMany[Greeter, *englishGreeter]()

	err := c.BindMany[Greeter, *englishGreeter]()
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

	c.MustBindMany[Greeter, *englishGreeter]()
}

func TestBindMany_Seal_Passes(t *testing.T) {
	c := di.New()
	c.MustProvide[*englishGreeter](NewEnglishGreeter)
	c.MustBindMany[Greeter, *englishGreeter]()

	if err := c.Seal(); err != nil {
		t.Fatalf("Seal should pass with valid BindMany: %v", err)
	}
}

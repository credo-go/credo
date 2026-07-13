package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// --- Test types ---

type SimpleService struct {
	Value string
}

func NewSimpleService() *SimpleService {
	return &SimpleService{Value: "hello"}
}

type ServiceWithDep struct {
	Simple *SimpleService
}

func NewServiceWithDep(s *SimpleService) *ServiceWithDep {
	return &ServiceWithDep{Simple: s}
}

type ServiceWithError struct{}

func NewServiceWithError() (*ServiceWithError, error) {
	return &ServiceWithError{}, nil
}

func NewServiceFailing() (*ServiceWithError, error) {
	return nil, errors.New("construction failed")
}

type ServiceWithTwoDeps struct {
	A *SimpleService
	B *ServiceWithDep
}

func NewServiceWithTwoDeps(a *SimpleService, b *ServiceWithDep) *ServiceWithTwoDeps {
	return &ServiceWithTwoDeps{A: a, B: b}
}

// --- Provide tests ---

func TestProvide_ValidConstructors(t *testing.T) {
	tests := []struct {
		name         string
		register     func(c *di.Container) error
		wantRegCount int
	}{
		{
			name: "zero params",
			register: func(c *di.Container) error {
				return c.Provide[*SimpleService](NewSimpleService)
			},
			wantRegCount: 1,
		},
		{
			name: "one param",
			register: func(c *di.Container) error {
				c.MustProvide[*SimpleService](NewSimpleService)
				return c.Provide[*ServiceWithDep](NewServiceWithDep)
			},
			wantRegCount: 2,
		},
		{
			name: "returns error",
			register: func(c *di.Container) error {
				return c.Provide[*ServiceWithError](NewServiceWithError)
			},
			wantRegCount: 1,
		},
		{
			name: "two deps",
			register: func(c *di.Container) error {
				c.MustProvide[*SimpleService](NewSimpleService)
				c.MustProvide[*ServiceWithDep](NewServiceWithDep)
				return c.Provide[*ServiceWithTwoDeps](NewServiceWithTwoDeps)
			},
			wantRegCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := di.New()
			if err := tt.register(c); err != nil {
				t.Fatalf("register failed: %v", err)
			}
			if got := c.RegistrationCount(); got != tt.wantRegCount {
				t.Errorf("RegistrationCount() = %d, want %d", got, tt.wantRegCount)
			}
		})
	}
}

func TestProvide_InvalidConstructors(t *testing.T) {
	tests := []struct {
		name        string
		constructor any
	}{
		{
			name:        "not a function",
			constructor: "not a func",
		},
		{
			name:        "no return values",
			constructor: func() {},
		},
		{
			name:        "three return values",
			constructor: func() (*SimpleService, int, error) { return nil, 0, nil },
		},
		{
			name:        "wrong return type",
			constructor: func() string { return "" },
		},
		{
			name:        "second return not error",
			constructor: func() (*SimpleService, string) { return nil, "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := di.New()
			err := c.Provide[*SimpleService](tt.constructor)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestProvide_Duplicate(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	err := c.Provide[*SimpleService](NewSimpleService)
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestCanProvideValue(t *testing.T) {
	t.Run("available without mutation", func(t *testing.T) {
		c := di.New()

		if err := c.CanProvideValue[*SimpleService](); err != nil {
			t.Fatalf("CanProvideValue() = %v, want nil", err)
		}
		if got := c.RegistrationCount(); got != 0 {
			t.Fatalf("RegistrationCount() = %d after preflight, want 0", got)
		}
		if err := c.ProvideValue[*SimpleService](&SimpleService{}); err != nil {
			t.Fatalf("ProvideValue() after successful preflight = %v", err)
		}
	})

	t.Run("direct duplicate matches ProvideValue", func(t *testing.T) {
		c := di.New()
		c.MustProvide[*SimpleService](NewSimpleService)

		preflightErr := c.CanProvideValue[*SimpleService]()
		provideErr := c.ProvideValue[*SimpleService](&SimpleService{})
		if preflightErr == nil || provideErr == nil {
			t.Fatalf("errors = (%v, %v), want duplicate errors", preflightErr, provideErr)
		}
		if preflightErr.Error() != provideErr.Error() {
			t.Fatalf("CanProvideValue error = %q, ProvideValue error = %q", preflightErr, provideErr)
		}
	})

	t.Run("alias is not a direct duplicate", func(t *testing.T) {
		c := di.New()
		c.MustProvide[*pgUserRepo](NewPgUserRepo)
		c.MustAlias[UserRepo, *pgUserRepo]()

		if err := c.CanProvideValue[UserRepo](); err != nil {
			t.Fatalf("CanProvideValue() with alias only = %v, want nil", err)
		}
		if err := c.ProvideValue[UserRepo](NewPgUserRepo()); err != nil {
			t.Fatalf("ProvideValue() with alias only = %v, want nil", err)
		}
	})

	t.Run("frozen matches ProvideValue", func(t *testing.T) {
		c := di.New()
		if err := c.Seal(); err != nil {
			t.Fatalf("Seal() = %v", err)
		}

		preflightErr := c.CanProvideValue[*SimpleService]()
		provideErr := c.ProvideValue[*SimpleService](&SimpleService{})
		if preflightErr == nil || provideErr == nil {
			t.Fatalf("errors = (%v, %v), want frozen errors", preflightErr, provideErr)
		}
		if preflightErr.Error() != provideErr.Error() {
			t.Fatalf("CanProvideValue error = %q, ProvideValue error = %q", preflightErr, provideErr)
		}
	})
}

func TestMustProvide_Panics(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate MustProvide")
		}
	}()
	c.MustProvide[*SimpleService](NewSimpleService)
}

func TestProvideValue(t *testing.T) {
	c := di.New()
	svc := &SimpleService{Value: "provided"}
	if err := c.ProvideValue[*SimpleService](svc); err != nil {
		t.Fatalf("ProvideValue failed: %v", err)
	}

	if got := c.RegistrationCount(); got != 1 {
		t.Errorf("RegistrationCount() = %d, want 1", got)
	}
	if got := c.SingletonCount(); got != 1 {
		t.Errorf("SingletonCount() = %d, want 1 (pre-cached)", got)
	}
}

func TestProvideProtectedValue_RejectsReplace(t *testing.T) {
	c := di.New()
	original := &SimpleService{Value: "original"}
	if err := c.ProvideProtectedValue[*SimpleService](original); err != nil {
		t.Fatalf("ProvideProtectedValue() = %v", err)
	}
	if err := c.Replace[*SimpleService](&SimpleService{Value: "replacement"}); err == nil {
		t.Fatal("Replace should reject a protected binding")
	}
	if resolved := c.MustResolve[*SimpleService](); resolved != original {
		t.Fatalf("Resolve() = %p, want original %p", resolved, original)
	}
}

func TestProtectBinding(t *testing.T) {
	t.Run("existing and idempotent", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "original"}
		c.MustProvideValue[*SimpleService](original)
		if err := c.ProtectBinding[*SimpleService](); err != nil {
			t.Fatalf("ProtectBinding() = %v", err)
		}
		if err := c.ProtectBinding[*SimpleService](); err != nil {
			t.Fatalf("second ProtectBinding() = %v", err)
		}
		if err := c.Replace[*SimpleService](&SimpleService{}); err == nil {
			t.Fatal("Replace should reject a protected existing binding")
		}
		if resolved := c.MustResolve[*SimpleService](); resolved != original {
			t.Fatalf("Resolve() = %p, want original %p", resolved, original)
		}
	})

	t.Run("matching expected value", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "original"}
		c.MustProvideValue[*SimpleService](original)
		if err := c.ProtectBinding[*SimpleService](original); err != nil {
			t.Fatalf("ProtectBinding(expected) = %v", err)
		}
		if err := c.Replace[*SimpleService](&SimpleService{}); err == nil {
			t.Fatal("Replace should reject a compare-and-protected binding")
		}
	})

	t.Run("mismatched expected value leaves binding replaceable", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "original"}
		replacement := &SimpleService{Value: "replacement"}
		c.MustProvideValue[*SimpleService](original)
		if err := c.Replace[*SimpleService](replacement); err != nil {
			t.Fatalf("Replace() = %v", err)
		}
		if err := c.ProtectBinding[*SimpleService](original); err == nil {
			t.Fatal("ProtectBinding(expected) should reject a changed value")
		}
		if err := c.Replace[*SimpleService](&SimpleService{Value: "final"}); err != nil {
			t.Fatalf("mismatched ProtectBinding made binding permanent: %v", err)
		}
	})

	t.Run("mismatch preserves existing protection", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "original"}
		c.MustProvideValue[*SimpleService](original)
		if err := c.ProtectBinding[*SimpleService](); err != nil {
			t.Fatalf("ProtectBinding() = %v", err)
		}
		if err := c.ProtectBinding[*SimpleService](&SimpleService{}); err == nil {
			t.Fatal("ProtectBinding(expected) should reject a different value")
		}
		if err := c.Replace[*SimpleService](&SimpleService{}); err == nil {
			t.Fatal("mismatched comparison removed existing protection")
		}
	})

	t.Run("unresolved expected value leaves binding replaceable", func(t *testing.T) {
		c := di.New()
		c.MustProvide[*SimpleService](NewSimpleService)
		if err := c.ProtectBinding[*SimpleService](&SimpleService{}); err == nil {
			t.Fatal("ProtectBinding(expected) should reject an unresolved singleton")
		}
		if err := c.Replace[*SimpleService](&SimpleService{}); err != nil {
			t.Fatalf("unresolved ProtectBinding made binding permanent: %v", err)
		}
	})

	t.Run("non-comparable expected value leaves binding replaceable", func(t *testing.T) {
		c := di.New()
		original := []string{"original"}
		c.MustProvideValue[[]string](original)
		if err := c.ProtectBinding[[]string](original); err == nil {
			t.Fatal("ProtectBinding(expected) should reject a non-comparable value")
		}
		if err := c.Replace[[]string]([]string{"replacement"}); err != nil {
			t.Fatalf("non-comparable ProtectBinding made binding permanent: %v", err)
		}
	})

	t.Run("multiple expected values leave binding replaceable", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "original"}
		c.MustProvideValue[*SimpleService](original)
		if err := c.ProtectBinding[*SimpleService](original, original); err == nil {
			t.Fatal("ProtectBinding should reject multiple expected values")
		}
		if err := c.Replace[*SimpleService](&SimpleService{}); err != nil {
			t.Fatalf("multiple expected values made binding permanent: %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		c := di.New()
		if err := c.ProtectBinding[*SimpleService](); err == nil {
			t.Fatal("ProtectBinding should reject an unregistered type")
		}
	})

	t.Run("frozen", func(t *testing.T) {
		c := di.New()
		c.MustProvideValue[*SimpleService](&SimpleService{})
		if err := c.Seal(); err != nil {
			t.Fatalf("Seal() = %v", err)
		}
		if err := c.ProtectBinding[*SimpleService](); err == nil {
			t.Fatal("ProtectBinding should reject a frozen container")
		}
	})
}

func TestProtectBinding_CompareAndProtectIsAtomic(t *testing.T) {
	for i := range 100 {
		c := di.New()
		original := &SimpleService{Value: "original"}
		replacement := &SimpleService{Value: "replacement"}
		c.MustProvideValue[*SimpleService](original)

		start := make(chan struct{})
		protectResult := make(chan error, 1)
		replaceResult := make(chan error, 1)
		go func() {
			<-start
			protectResult <- c.ProtectBinding[*SimpleService](original)
		}()
		go func() {
			<-start
			replaceResult <- c.Replace[*SimpleService](replacement)
		}()
		close(start)

		protectErr := <-protectResult
		replaceErr := <-replaceResult
		resolved := c.MustResolve[*SimpleService]()
		switch {
		case protectErr == nil:
			if replaceErr == nil {
				t.Fatalf("iteration %d: ProtectBinding and Replace both succeeded", i)
			}
			if resolved != original {
				t.Fatalf("iteration %d: protected value = %p, want original %p", i, resolved, original)
			}
			if err := c.Replace[*SimpleService](&SimpleService{}); err == nil {
				t.Fatalf("iteration %d: successful protection did not persist", i)
			}
		case replaceErr == nil:
			if resolved != replacement {
				t.Fatalf("iteration %d: resolved value = %p, want replacement %p", i, resolved, replacement)
			}
			if err := c.Replace[*SimpleService](&SimpleService{}); err != nil {
				t.Fatalf("iteration %d: failed protection made replacement permanent: %v", i, err)
			}
		default:
			t.Fatalf("iteration %d: ProtectBinding and Replace both failed: (%v, %v)", i, protectErr, replaceErr)
		}
	}
}

func TestProvideValue_Duplicate(t *testing.T) {
	c := di.New()
	c.MustProvideValue[*SimpleService](&SimpleService{})

	err := c.ProvideValue[*SimpleService](&SimpleService{})
	if err == nil {
		t.Fatal("expected error for duplicate ProvideValue")
	}
}

func TestProvide_NilConstructor(t *testing.T) {
	c := di.New()
	if err := c.Provide[*SimpleService](nil); err == nil {
		t.Fatal("expected error for nil constructor, got nil")
	}
}

// --- ProvideFactory tests ---

type funcService struct {
	Dep *SimpleService
}

func TestProvideFactory_ResolveAndCache(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	calls := 0
	err := c.ProvideFactory[*funcService](func() (*funcService, error) {
		calls++
		dep, err := c.Resolve[*SimpleService]()
		if err != nil {
			return nil, err
		}
		return &funcService{Dep: dep}, nil
	})
	if err != nil {
		t.Fatalf("ProvideFactory failed: %v", err)
	}
	if calls != 0 {
		t.Fatalf("factory ran at registration time (calls = %d), want lazy", calls)
	}

	first := c.MustResolve[*funcService]()
	second := c.MustResolve[*funcService]()
	if calls != 1 {
		t.Errorf("constructor calls = %d, want 1 (singleton)", calls)
	}
	if first != second {
		t.Error("Resolve returned different instances, want cached singleton")
	}
	if first.Dep == nil || first.Dep.Value != "hello" {
		t.Errorf("dependency not resolved inside fn: %+v", first.Dep)
	}
}

func TestProvideFactory_ConstructionError(t *testing.T) {
	c := di.New()
	c.MustProvideFactory[*SimpleService](func() (*SimpleService, error) {
		return nil, errors.New("boom")
	})

	_, err := c.Resolve[*SimpleService]()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Resolve error = %v, want construction error containing %q", err, "boom")
	}
}

func TestProvideFactory_Duplicate(t *testing.T) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)

	err := c.ProvideFactory[*SimpleService](func() (*SimpleService, error) {
		return &SimpleService{}, nil
	})
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestProvideFactory_Nil(t *testing.T) {
	c := di.New()
	if err := c.ProvideFactory[*SimpleService](nil); err == nil {
		t.Fatal("expected error for nil factory")
	}
}

func TestProvideFactory_Frozen(t *testing.T) {
	c := di.New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	err := c.ProvideFactory[*SimpleService](func() (*SimpleService, error) {
		return &SimpleService{}, nil
	})
	if err == nil {
		t.Fatal("expected error after Seal")
	}
}

func TestMustProvideFactory_Panics(t *testing.T) {
	c := di.New()
	c.MustProvideFactory[*SimpleService](func() (*SimpleService, error) {
		return &SimpleService{}, nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate MustProvideFactory")
		}
	}()
	c.MustProvideFactory[*SimpleService](func() (*SimpleService, error) {
		return &SimpleService{}, nil
	})
}

type funcShutdowner struct {
	closed *bool
}

func (s *funcShutdowner) Shutdown(ctx context.Context) error {
	*s.closed = true
	return nil
}

func TestProvideFactory_ShutdownParticipates(t *testing.T) {
	c := di.New()
	closed := false
	c.MustProvideFactory[*funcShutdowner](func() (*funcShutdowner, error) {
		return &funcShutdowner{closed: &closed}, nil
	})
	c.MustResolve[*funcShutdowner]()

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if !closed {
		t.Error("Shutdown was not called on the ProvideFactory-constructed instance")
	}
}

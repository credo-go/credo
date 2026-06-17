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
				return di.Provide[*SimpleService](c, NewSimpleService)
			},
			wantRegCount: 1,
		},
		{
			name: "one param",
			register: func(c *di.Container) error {
				di.MustProvide[*SimpleService](c, NewSimpleService)
				return di.Provide[*ServiceWithDep](c, NewServiceWithDep)
			},
			wantRegCount: 2,
		},
		{
			name: "returns error",
			register: func(c *di.Container) error {
				return di.Provide[*ServiceWithError](c, NewServiceWithError)
			},
			wantRegCount: 1,
		},
		{
			name: "two deps",
			register: func(c *di.Container) error {
				di.MustProvide[*SimpleService](c, NewSimpleService)
				di.MustProvide[*ServiceWithDep](c, NewServiceWithDep)
				return di.Provide[*ServiceWithTwoDeps](c, NewServiceWithTwoDeps)
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
			err := di.Provide[*SimpleService](c, tt.constructor)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestProvide_Duplicate(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	err := di.Provide[*SimpleService](c, NewSimpleService)
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestMustProvide_Panics(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate MustProvide")
		}
	}()
	di.MustProvide[*SimpleService](c, NewSimpleService)
}

func TestProvideValue(t *testing.T) {
	c := di.New()
	svc := &SimpleService{Value: "provided"}
	if err := di.ProvideValue[*SimpleService](c, svc); err != nil {
		t.Fatalf("ProvideValue failed: %v", err)
	}

	if got := c.RegistrationCount(); got != 1 {
		t.Errorf("RegistrationCount() = %d, want 1", got)
	}
	if got := c.SingletonCount(); got != 1 {
		t.Errorf("SingletonCount() = %d, want 1 (pre-cached)", got)
	}
}

func TestProvideValue_Duplicate(t *testing.T) {
	c := di.New()
	di.MustProvideValue[*SimpleService](c, &SimpleService{})

	err := di.ProvideValue[*SimpleService](c, &SimpleService{})
	if err == nil {
		t.Fatal("expected error for duplicate ProvideValue")
	}
}

func TestProvide_NilConstructor(t *testing.T) {
	c := di.New()
	if err := di.Provide[*SimpleService](c, nil); err == nil {
		t.Fatal("expected error for nil constructor, got nil")
	}
}

// --- ProvideFactory tests ---

type funcService struct {
	Dep *SimpleService
}

func TestProvideFactory_ResolveAndCache(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	calls := 0
	err := di.ProvideFactory[*funcService](c, func() (*funcService, error) {
		calls++
		dep, err := di.Resolve[*SimpleService](c)
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

	first := di.MustResolve[*funcService](c)
	second := di.MustResolve[*funcService](c)
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
	di.MustProvideFactory[*SimpleService](c, func() (*SimpleService, error) {
		return nil, errors.New("boom")
	})

	_, err := di.Resolve[*SimpleService](c)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Resolve error = %v, want construction error containing %q", err, "boom")
	}
}

func TestProvideFactory_Duplicate(t *testing.T) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)

	err := di.ProvideFactory[*SimpleService](c, func() (*SimpleService, error) {
		return &SimpleService{}, nil
	})
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestProvideFactory_Nil(t *testing.T) {
	c := di.New()
	if err := di.ProvideFactory[*SimpleService](c, nil); err == nil {
		t.Fatal("expected error for nil factory")
	}
}

func TestProvideFactory_Frozen(t *testing.T) {
	c := di.New()
	if err := c.Seal(); err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	err := di.ProvideFactory[*SimpleService](c, func() (*SimpleService, error) {
		return &SimpleService{}, nil
	})
	if err == nil {
		t.Fatal("expected error after Seal")
	}
}

func TestMustProvideFactory_Panics(t *testing.T) {
	c := di.New()
	di.MustProvideFactory[*SimpleService](c, func() (*SimpleService, error) {
		return &SimpleService{}, nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate MustProvideFactory")
		}
	}()
	di.MustProvideFactory[*SimpleService](c, func() (*SimpleService, error) {
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
	di.MustProvideFactory[*funcShutdowner](c, func() (*funcShutdowner, error) {
		return &funcShutdowner{closed: &closed}, nil
	})
	di.MustResolve[*funcShutdowner](c)

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if !closed {
		t.Error("Shutdown was not called on the ProvideFactory-constructed instance")
	}
}

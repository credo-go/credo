package di_test

import (
	"context"
	"errors"
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

// --- Provide + Shutdown ---

type funcShutdowner struct {
	closed *bool
}

func (s *funcShutdowner) Shutdown(ctx context.Context) error {
	*s.closed = true
	return nil
}

func TestProvide_ShutdownParticipates(t *testing.T) {
	c := di.New()
	closed := false
	c.MustProvide[*funcShutdowner](func() *funcShutdowner {
		return &funcShutdowner{closed: &closed}
	})
	seal(t, c)
	c.MustResolve[*funcShutdowner]()

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if !closed {
		t.Error("Shutdown was not called on the constructed instance")
	}
}

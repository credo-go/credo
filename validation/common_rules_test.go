package validation_test

import (
	"errors"
	"testing"

	"github.com/credo-go/credo/validation"
)

// --- By ---

func TestBy_Valid(t *testing.T) {
	rule := validation.By(func(s string) error {
		if len(s) < 2 {
			return errors.New("too short")
		}
		return nil
	})
	if err := rule.Validate("hello"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestBy_Invalid(t *testing.T) {
	rule := validation.By(func(s string) error {
		if len(s) < 2 {
			return errors.New("too short")
		}
		return nil
	})
	err := rule.Validate("a")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != "invalid" {
		t.Errorf("code = %q, want %q", ve.Code, "invalid")
	}
	if ve.Message != "too short" {
		t.Errorf("message = %q, want %q", ve.Message, "too short")
	}
}

func TestBy_CustomValidationError(t *testing.T) {
	rule := validation.By(func(s string) error {
		return &validation.ValidationError{
			Code:    "country_code",
			Message: "invalid country code",
			Params:  map[string]any{"length": 2},
		}
	})
	err := rule.Validate("XXX")
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != "country_code" {
		t.Errorf("code = %q, want %q", ve.Code, "country_code")
	}
	if ve.Params["length"] != 2 {
		t.Errorf("params[length] = %v, want 2", ve.Params["length"])
	}
}

func TestBy_WithValidateStruct(t *testing.T) {
	type Input struct {
		Code string `json:"code"`
	}
	input := &Input{Code: "X"}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Code, validation.By(func(s string) error {
			if len(s) != 2 {
				return errors.New("must be exactly 2 characters")
			}
			return nil
		})),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if errs[0].Field != "code" {
		t.Errorf("field = %q, want %q", errs[0].Field, "code")
	}
}

// --- In ---

func TestIn_Valid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"first", "active"},
		{"middle", "pending"},
		{"last", "disabled"},
	}
	rule := validation.In("active", "pending", "disabled")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := rule.Validate(tt.value); err != nil {
				t.Errorf("expected nil, got %v", err)
			}
		})
	}
}

func TestIn_Invalid(t *testing.T) {
	rule := validation.In("active", "pending", "disabled")
	err := rule.Validate("unknown")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != "in" {
		t.Errorf("code = %q, want %q", ve.Code, "in")
	}
}

func TestIn_Ints(t *testing.T) {
	rule := validation.In(1, 2, 3)
	if err := rule.Validate(2); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if err := rule.Validate(4); err == nil {
		t.Error("expected error for 4, got nil")
	}
}

// --- NotNil ---

func TestNotNil_NonNil(t *testing.T) {
	s := "hello"
	rule := validation.NotNil[string]()
	if err := rule.Validate(&s); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestNotNil_Nil(t *testing.T) {
	rule := validation.NotNil[string]()
	err := rule.Validate(nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != "not_nil" {
		t.Errorf("code = %q, want %q", ve.Code, "not_nil")
	}
}

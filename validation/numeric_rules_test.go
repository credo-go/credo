package validation_test

import (
	"errors"
	"testing"

	"github.com/credo-go/credo/validation"
)

// --- Min ---

func TestMin_Int_Valid(t *testing.T) {
	if err := validation.Min(18).Validate(18); err != nil {
		t.Errorf("expected nil for boundary, got %v", err)
	}
	if err := validation.Min(18).Validate(25); err != nil {
		t.Errorf("expected nil for above, got %v", err)
	}
}

func TestMin_Int_Invalid(t *testing.T) {
	err := validation.Min(18).Validate(17)
	assertValidationError(t, err, "min", "must be at least 18")
}

func TestMin_Float64(t *testing.T) {
	if err := validation.Min(0.5).Validate(0.5); err != nil {
		t.Errorf("expected nil for boundary, got %v", err)
	}
	err := validation.Min(0.5).Validate(0.4)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMin_ErrorParams(t *testing.T) {
	err := validation.Min(18).Validate(10)
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Params["min"] != 18 {
		t.Errorf("params[min] = %v, want 18", ve.Params["min"])
	}
}

// --- Max ---

func TestMax_Valid(t *testing.T) {
	if err := validation.Max(100).Validate(100); err != nil {
		t.Errorf("expected nil for boundary, got %v", err)
	}
	if err := validation.Max(100).Validate(50); err != nil {
		t.Errorf("expected nil for below, got %v", err)
	}
}

func TestMax_Invalid(t *testing.T) {
	err := validation.Max(100).Validate(101)
	assertValidationError(t, err, "max", "must be at most 100")
}

func TestMax_ErrorParams(t *testing.T) {
	err := validation.Max(100).Validate(200)
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Params["max"] != 100 {
		t.Errorf("params[max] = %v, want 100", ve.Params["max"])
	}
}

// --- Between ---

func TestBetween_Valid(t *testing.T) {
	rule := validation.Between(1, 10)
	for _, v := range []int{1, 5, 10} {
		if err := rule.Validate(v); err != nil {
			t.Errorf("expected nil for %d, got %v", v, err)
		}
	}
}

func TestBetween_BelowMin(t *testing.T) {
	err := validation.Between(1, 10).Validate(0)
	assertValidationError(t, err, "between", "")
}

func TestBetween_AboveMax(t *testing.T) {
	err := validation.Between(1, 10).Validate(11)
	assertValidationError(t, err, "between", "")
}

func TestBetween_ErrorParams(t *testing.T) {
	err := validation.Between(1, 10).Validate(0)
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Params["min"] != 1 {
		t.Errorf("params[min] = %v, want 1", ve.Params["min"])
	}
	if ve.Params["max"] != 10 {
		t.Errorf("params[max] = %v, want 10", ve.Params["max"])
	}
}

func TestBetween_Float64(t *testing.T) {
	rule := validation.Between(0.0, 1.0)
	if err := rule.Validate(0.5); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if err := rule.Validate(1.1); err == nil {
		t.Error("expected error for 1.1, got nil")
	}
}

// --- Integration ---

func TestNumericRules_WithValidateStruct(t *testing.T) {
	type Input struct {
		Age      int     `json:"age"`
		Score    float64 `json:"score"`
		Quantity int     `json:"quantity"`
	}
	input := &Input{Age: 10, Score: 1.5, Quantity: 0}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Age, validation.Min(18)),
		validation.Field(&input.Score, validation.Between(0.0, 1.0)),
		validation.Field(&input.Quantity, validation.Min(1)),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 3 {
		t.Fatalf("len = %d, want 3", len(errs))
	}
}

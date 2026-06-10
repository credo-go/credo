package validation_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/credo-go/credo/validation"
)

func TestErrors_Unwrap(t *testing.T) {
	errs := validation.Errors{
		{Field: "name", Code: "required", Message: "is required"},
		{Field: "age", Code: "min", Message: "too small"},
	}

	// errors.As must reach individual field errors even when the
	// validation result is wrapped further up the chain.
	wrapped := fmt.Errorf("bind user: %w", errs)

	ve, ok := errors.AsType[*validation.ValidationError](wrapped)
	if !ok {
		t.Fatal("errors.AsType should find a *ValidationError through the wrap chain")
	}
	if ve.Field != "name" {
		t.Errorf("first unwrapped error field = %q, want %q", ve.Field, "name")
	}

	if got := validation.Errors(nil).Unwrap(); got != nil {
		t.Errorf("empty Errors.Unwrap() = %v, want nil", got)
	}
}

func TestValidationError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  validation.ValidationError
		want string
	}{
		{
			name: "with field",
			err:  validation.ValidationError{Field: "name", Code: "required", Message: "is required"},
			want: "name: is required",
		},
		{
			name: "without field",
			err:  validation.ValidationError{Code: "required", Message: "is required"},
			want: "is required",
		},
		{
			name: "nested field path",
			err:  validation.ValidationError{Field: "address.city", Code: "required", Message: "is required"},
			want: "address.city: is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrors_Error(t *testing.T) {
	tests := []struct {
		name string
		errs validation.Errors
		want string
	}{
		{
			name: "single error",
			errs: validation.Errors{
				{Field: "name", Code: "required", Message: "is required"},
			},
			want: "name: is required",
		},
		{
			name: "multiple errors",
			errs: validation.Errors{
				{Field: "name", Code: "required", Message: "is required"},
				{Field: "email", Code: "email", Message: "must be a valid email address"},
			},
			want: "name: is required; email: must be a valid email address",
		},
		{
			name: "empty errors",
			errs: validation.Errors{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.errs.Error()
			if got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrors_ImplementsError(t *testing.T) {
	// Compile-time guarantee that Errors satisfies the error interface.
	var _ error = validation.Errors{}

	// At runtime, a populated Errors produces a non-empty message.
	err := error(validation.Errors{
		{Field: "name", Code: "required", Message: "is required"},
	})
	if err.Error() == "" {
		t.Fatal("Errors.Error() should produce a non-empty message")
	}
}

func TestErrors_MarshalJSON(t *testing.T) {
	errs := validation.Errors{
		{Field: "name", Code: "required", Message: "is required"},
		{Field: "age", Code: "min", Message: "must be at least 18", Params: map[string]any{"min": 18}},
	}

	data, err := json.Marshal(errs)
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}

	if result[0]["field"] != "name" {
		t.Errorf("result[0][field] = %v, want %q", result[0]["field"], "name")
	}
	if result[0]["code"] != "required" {
		t.Errorf("result[0][code] = %v, want %q", result[0]["code"], "required")
	}
	if result[1]["field"] != "age" {
		t.Errorf("result[1][field] = %v, want %q", result[1]["field"], "age")
	}
	if result[1]["code"] != "min" {
		t.Errorf("result[1][code] = %v, want %q", result[1]["code"], "min")
	}

	// Params should be present on second error
	params, ok := result[1]["params"].(map[string]any)
	if !ok {
		t.Fatal("result[1][params] should be a map")
	}
	if params["min"] != float64(18) { // JSON numbers are float64
		t.Errorf("params[min] = %v, want 18", params["min"])
	}
}

func TestErrors_MarshalJSON_Empty(t *testing.T) {
	errs := validation.Errors{}

	data, err := json.Marshal(errs)
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	if string(data) != "[]" {
		t.Errorf("MarshalJSON() = %s, want []", data)
	}
}

func TestErrors_MarshalJSON_OmitsEmptyParams(t *testing.T) {
	errs := validation.Errors{
		{Field: "name", Code: "required", Message: "is required"},
	}

	data, err := json.Marshal(errs)
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	// Params should be omitted when nil
	var result []map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, exists := result[0]["params"]; exists {
		t.Errorf("params should be omitted when nil, got %v", result[0]["params"])
	}
}

func TestErrors_As(t *testing.T) {
	err := error(validation.Errors{
		{Field: "name", Code: "required", Message: "is required"},
	})

	ve, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatal("errors.AsType should match validation.Errors")
	}
	if len(ve) != 1 {
		t.Errorf("len = %d, want 1", len(ve))
	}
}

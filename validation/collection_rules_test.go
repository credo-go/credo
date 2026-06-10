package validation_test

import (
	"errors"
	"testing"

	"github.com/credo-go/credo/validation"
)

// --- NotEmptySlice / NotEmptyMap ---

func TestNotEmptySlice(t *testing.T) {
	rule := validation.NotEmptySlice[string]()

	tests := []struct {
		name    string
		value   []string
		wantErr bool
	}{
		{"nil slice", nil, true},
		{"empty slice", []string{}, true},
		{"one element", []string{"a"}, false},
		{"zero-value element still counts", []string{""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rule.Validate(tt.value)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

func TestNotEmptySlice_ErrorCode(t *testing.T) {
	err := validation.NotEmptySlice[int]().Validate(nil)
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != "not_empty" {
		t.Errorf("code = %q, want %q", ve.Code, "not_empty")
	}
}

func TestNotEmptyMap(t *testing.T) {
	rule := validation.NotEmptyMap[string, int]()

	tests := []struct {
		name    string
		value   map[string]int
		wantErr bool
	}{
		{"nil map", nil, true},
		{"empty map", map[string]int{}, true},
		{"one entry", map[string]int{"a": 0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rule.Validate(tt.value)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

func TestNotEmptyMap_ErrorCode(t *testing.T) {
	err := validation.NotEmptyMap[string, string]().Validate(nil)
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != "not_empty" {
		t.Errorf("code = %q, want %q", ve.Code, "not_empty")
	}
}

func TestNotEmptySlice_InValidateStruct(t *testing.T) {
	type Order struct {
		Items []string `json:"items"`
	}
	o := Order{}
	err := validation.ValidateStruct(&o,
		validation.Field(&o.Items, validation.NotEmptySlice[string]()),
	)
	if err == nil {
		t.Fatal("expected error for empty Items")
	}
	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 1 || errs[0].Field != "items" {
		t.Fatalf("errors = %v, want single error on field %q", errs, "items")
	}
}

// --- Each ---

func TestEach_AllValid(t *testing.T) {
	rule := validation.Each(validation.Length(1, 10))
	err := rule.Validate([]string{"hello", "world"})
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestEach_SomeInvalid(t *testing.T) {
	rule := validation.Each(validation.Required[string](), validation.Length(2, 10))
	err := rule.Validate([]string{"hello", "", "a"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	// "" fails Required, "a" fails Length
	if len(errs) != 2 {
		t.Fatalf("len = %d, want 2; errors: %v", len(errs), errs)
	}
}

func TestEach_Empty(t *testing.T) {
	rule := validation.Each(validation.Required[string]())
	err := rule.Validate([]string{})
	if err != nil {
		t.Errorf("expected nil for empty slice, got %v", err)
	}
}

func TestEach_ErrorFieldPath(t *testing.T) {
	rule := validation.Each(validation.Required[string]())
	err := rule.Validate([]string{"ok", ""})

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 1 {
		t.Fatalf("len = %d, want 1", len(errs))
	}
	if errs[0].Field != "[1]" {
		t.Errorf("field = %q, want %q", errs[0].Field, "[1]")
	}
}

func TestEach_WithValidateStruct(t *testing.T) {
	type Input struct {
		Tags []string `json:"tags"`
	}
	input := &Input{Tags: []string{"go", "", "x"}}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Tags, validation.Each(
			validation.Required[string](), validation.Length(2, 20),
		)),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	// "" fails Required, "x" fails Length
	if len(errs) != 2 {
		t.Fatalf("len = %d, want 2; errors: %v", len(errs), errs)
	}

	// Check field paths: should be "tags[1]" and "tags[2]"
	fieldMap := map[string]bool{}
	for _, e := range errs {
		fieldMap[e.Field] = true
	}
	if !fieldMap["tags[1]"] {
		t.Errorf("missing error for tags[1]; got fields: %v", fieldMap)
	}
	if !fieldMap["tags[2]"] {
		t.Errorf("missing error for tags[2]; got fields: %v", fieldMap)
	}
}

// --- When ---

func TestWhen_ConditionTrue(t *testing.T) {
	rule := validation.When(true, validation.Required[string]())
	err := rule.Validate("")
	if err == nil {
		t.Fatal("expected error when condition is true, got nil")
	}
}

func TestWhen_ConditionFalse(t *testing.T) {
	rule := validation.When(false, validation.Required[string]())
	err := rule.Validate("")
	if err != nil {
		t.Errorf("expected nil when condition is false, got %v", err)
	}
}

func TestWhen_MultipleRules(t *testing.T) {
	rule := validation.When(true,
		validation.Required[string](),
		validation.Length(5, 10),
	)
	// "ab" passes Required but fails Length
	err := rule.Validate("ab")
	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 1 {
		t.Fatalf("len = %d, want 1", len(errs))
	}
	if errs[0].Code != "length" {
		t.Errorf("code = %q, want %q", errs[0].Code, "length")
	}
}

func TestWhen_CrossFieldValidation(t *testing.T) {
	type Order struct {
		PaymentMethod string `json:"payment_method"`
		CardNumber    string `json:"card_number"`
	}
	order := &Order{PaymentMethod: "card", CardNumber: ""}

	err := validation.ValidateStruct(order,
		validation.Field(&order.CardNumber,
			validation.When(order.PaymentMethod == "card",
				validation.Required[string](),
			),
		),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 1 {
		t.Fatalf("len = %d, want 1", len(errs))
	}
	if errs[0].Field != "card_number" {
		t.Errorf("field = %q, want %q", errs[0].Field, "card_number")
	}
}

// --- NilSafe ---

func TestNilSafe_Nil(t *testing.T) {
	rule := validation.NilSafe(validation.Required[string](), validation.Length(2, 100))
	err := rule.Validate(nil)
	if err != nil {
		t.Errorf("expected nil for nil pointer, got %v", err)
	}
}

func TestNilSafe_NonNil_Valid(t *testing.T) {
	s := "hello"
	rule := validation.NilSafe(validation.Length(2, 100))
	err := rule.Validate(&s)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestNilSafe_NonNil_Invalid(t *testing.T) {
	s := "a"
	rule := validation.NilSafe(validation.Length(2, 100))
	err := rule.Validate(&s)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 1 {
		t.Fatalf("len = %d, want 1", len(errs))
	}
	if errs[0].Code != "length" {
		t.Errorf("code = %q, want %q", errs[0].Code, "length")
	}
}

func TestNilSafe_PATCHUpdate(t *testing.T) {
	type UpdateUserInput struct {
		Name  *string `json:"name"`
		Email *string `json:"email"`
	}

	// Only email sent (name is nil — not sent)
	email := "bad"
	input := &UpdateUserInput{Email: &email}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, validation.NilSafe(validation.Length(2, 100))),
		validation.Field(&input.Email, validation.NilSafe(validation.Email())),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	// Name is nil → skipped, Email is "bad" → fails
	if len(errs) != 1 {
		t.Fatalf("len = %d, want 1; errors: %v", len(errs), errs)
	}
	if errs[0].Field != "email" {
		t.Errorf("field = %q, want %q", errs[0].Field, "email")
	}
	if errs[0].Code != "email" {
		t.Errorf("code = %q, want %q", errs[0].Code, "email")
	}
}

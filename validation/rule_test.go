package validation_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/credo-go/credo/validation"
)

// --- Test helpers ---

// alwaysPass is a Rule[T] that always passes.
type alwaysPass[T any] struct{}

func (r alwaysPass[T]) Validate(_ T) error { return nil }

// alwaysFail is a Rule[T] that always returns a ValidationError.
type alwaysFail[T any] struct {
	code    string
	message string
}

func (r alwaysFail[T]) Validate(_ T) error {
	return &validation.ValidationError{Code: r.code, Message: r.message}
}

// --- ValidateStruct tests ---

func TestValidateStruct_SingleField_Pass(t *testing.T) {
	type Input struct {
		Name string `json:"name"`
	}
	input := &Input{Name: "Alice"}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, alwaysPass[string]{}),
	)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateStruct_SingleField_Fail(t *testing.T) {
	type Input struct {
		Name string `json:"name"`
	}
	input := &Input{Name: ""}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, alwaysFail[string]{code: "required", message: "is required"}),
	)
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
	if errs[0].Field != "name" {
		t.Errorf("field = %q, want %q", errs[0].Field, "name")
	}
	if errs[0].Code != "required" {
		t.Errorf("code = %q, want %q", errs[0].Code, "required")
	}
}

func TestValidateStruct_MultipleFields(t *testing.T) {
	type Input struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Age   int    `json:"age"`
	}
	input := &Input{}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, alwaysFail[string]{code: "required", message: "is required"}),
		validation.Field(&input.Email, alwaysFail[string]{code: "required", message: "is required"}),
		validation.Field(&input.Age, alwaysFail[int]{code: "required", message: "is required"}),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 3 {
		t.Fatalf("len = %d, want 3", len(errs))
	}

	fields := map[string]bool{}
	for _, e := range errs {
		fields[e.Field] = true
	}
	for _, f := range []string{"name", "email", "age"} {
		if !fields[f] {
			t.Errorf("missing error for field %q", f)
		}
	}
}

func TestValidateStruct_AllPass(t *testing.T) {
	type Input struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	input := &Input{Name: "Alice", Age: 30}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, alwaysPass[string]{}),
		validation.Field(&input.Age, alwaysPass[int]{}),
	)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateStruct_CollectsAllErrors(t *testing.T) {
	type Input struct {
		Name string `json:"name"`
	}
	input := &Input{}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name,
			alwaysFail[string]{code: "required", message: "is required"},
			alwaysFail[string]{code: "length", message: "must be between 2 and 100 characters"},
		),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 2 {
		t.Fatalf("len = %d, want 2", len(errs))
	}
	if errs[0].Code != "required" {
		t.Errorf("errs[0].Code = %q, want %q", errs[0].Code, "required")
	}
	if errs[1].Code != "length" {
		t.Errorf("errs[1].Code = %q, want %q", errs[1].Code, "length")
	}
}

func TestValidateStruct_FieldNameFromJsonTag(t *testing.T) {
	type Input struct {
		FullName string `json:"full_name"`
	}
	input := &Input{}

	err := validation.ValidateStruct(input,
		validation.Field(&input.FullName, alwaysFail[string]{code: "required", message: "is required"}),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if errs[0].Field != "full_name" {
		t.Errorf("field = %q, want %q", errs[0].Field, "full_name")
	}
}

func TestValidateStruct_FieldNameFallback(t *testing.T) {
	type Input struct {
		FullName string // no json tag
	}
	input := &Input{}

	err := validation.ValidateStruct(input,
		validation.Field(&input.FullName, alwaysFail[string]{code: "required", message: "is required"}),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if errs[0].Field != "FullName" {
		t.Errorf("field = %q, want %q", errs[0].Field, "FullName")
	}
}

func TestValidateStruct_JsonTagOmitempty(t *testing.T) {
	type Input struct {
		Name string `json:"name,omitempty"`
	}
	input := &Input{}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, alwaysFail[string]{code: "required", message: "is required"}),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if errs[0].Field != "name" {
		t.Errorf("field = %q, want %q", errs[0].Field, "name")
	}
}

func TestValidateStruct_NilStruct(t *testing.T) {
	type Input struct {
		Name string `json:"name"`
	}
	var input *Input

	// Can't create Field refs from nil pointer (Go language constraint).
	// ValidateStruct with nil struct and no fields should return nil.
	err := validation.ValidateStruct(input)
	if err != nil {
		t.Errorf("expected nil for nil struct, got %v", err)
	}
}

func TestValidateStruct_NonStructPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for non-struct pointer")
		}
	}()

	s := "not a struct"
	validation.ValidateStruct(&s)
}

func TestValidateStruct_NonPointerPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for non-pointer")
		}
	}()

	type Input struct{ Name string }
	validation.ValidateStruct(Input{})
}

// --- Nested validation ---

type testAddress struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

func (a *testAddress) Validate() error {
	return validation.ValidateStruct(a,
		validation.Field(&a.Street, alwaysFail[string]{code: "required", message: "is required"}),
		validation.Field(&a.City, alwaysFail[string]{code: "required", message: "is required"}),
	)
}

func TestValidateStruct_NestedValidatable(t *testing.T) {
	type Input struct {
		Name    string      `json:"name"`
		Address testAddress `json:"address"`
	}
	input := &Input{}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, alwaysFail[string]{code: "required", message: "is required"}),
		validation.Field(&input.Address), // no rules → auto-call Validate()
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}

	// Should have: name, address.street, address.city
	if len(errs) != 3 {
		t.Fatalf("len = %d, want 3; errors: %v", len(errs), errs)
	}

	fieldMap := map[string]bool{}
	for _, e := range errs {
		fieldMap[e.Field] = true
	}
	for _, f := range []string{"name", "address.street", "address.city"} {
		if !fieldMap[f] {
			t.Errorf("missing error for field %q; got fields: %v", f, fieldMap)
		}
	}
}

// --- Export test helpers ---

func TestGetErrorFieldName(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want string
	}{
		{"json tag", `json:"user_name"`, "user_name"},
		{"json omitempty", `json:"name,omitempty"`, "name"},
		{"json dash", `json:"-"`, "TestField"},
		{"no tag", ``, "TestField"},
		{"empty json tag", `json:","`, "TestField"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := reflect.StructField{
				Name: "TestField",
				Tag:  reflect.StructTag(tt.tag),
			}
			got := validation.ExportGetErrorFieldName(f)
			if got != tt.want {
				t.Errorf("getErrorFieldName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinFieldPath(t *testing.T) {
	tests := []struct {
		parent string
		child  string
		want   string
	}{
		{"items", "[0]", "items[0]"},
		{"address", "city", "address.city"},
		{"", "name", "name"},
		{"name", "", "name"},
		{"", "", ""},
		{"items", "[2].name", "items[2].name"},
	}
	for _, tt := range tests {
		t.Run(tt.parent+"+"+tt.child, func(t *testing.T) {
			got := validation.ExportJoinFieldPath(tt.parent, tt.child)
			if got != tt.want {
				t.Errorf("joinFieldPath(%q, %q) = %q, want %q", tt.parent, tt.child, got, tt.want)
			}
		})
	}
}

func TestPrefixErrors_SingleError(t *testing.T) {
	err := &validation.ValidationError{Field: "city", Code: "required", Message: "is required"}

	result, ok := errors.AsType[validation.Errors](validation.ExportPrefixErrors("address", err))
	if !ok || len(result) != 1 {
		t.Fatalf("len = %d, want 1", len(result))
	}
	if result[0].Field != "address.city" {
		t.Errorf("field = %q, want %q", result[0].Field, "address.city")
	}
}

func TestPrefixErrors_MultipleErrors(t *testing.T) {
	err := validation.Errors{
		{Field: "street", Code: "required", Message: "is required"},
		{Field: "city", Code: "required", Message: "is required"},
	}

	result, ok := errors.AsType[validation.Errors](validation.ExportPrefixErrors("address", err))
	if !ok || len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result[0].Field != "address.street" {
		t.Errorf("result[0].field = %q, want %q", result[0].Field, "address.street")
	}
	if result[1].Field != "address.city" {
		t.Errorf("result[1].field = %q, want %q", result[1].Field, "address.city")
	}
}

func TestPrefixErrors_InternalErrorPassesThrough(t *testing.T) {
	internal := errors.New("pq: connection refused")

	if got := validation.ExportPrefixErrors("address", internal); !errors.Is(got, internal) {
		t.Fatalf("prefixErrors = %v, want the internal error unchanged", got)
	}
	if _, isViolation := errors.AsType[validation.Errors](validation.ExportPrefixErrors("address", internal)); isViolation {
		t.Fatal("an internal error became a violation")
	}
	// The chain counts, as in the error pipeline: a wrapped validation
	// failure is still client-visible.
	wrapped := fmt.Errorf("lookup: %w", validation.NewError("code", "message"))
	errs, ok := errors.AsType[validation.Errors](validation.ExportPrefixErrors("address", wrapped))
	if !ok || len(errs) != 1 || errs[0].Field != "address" || errs[0].Code != "code" {
		t.Fatalf("prefixErrors(wrapped) = %+v, want one violation of address", errs)
	}
}

func TestCollectErrors_KeepsParams(t *testing.T) {
	original := &validation.ValidationError{
		Code:    "email",
		Message: "must be a valid email address",
		Params:  map[string]any{"format": "RFC 5322"},
	}

	var dst validation.Errors
	if internal := validation.ExportCollectErrors(&dst, original, "email"); internal != nil {
		t.Fatalf("collectErrors = %v, want nil", internal)
	}
	if len(dst) != 1 || dst[0].Code != "email" || dst[0].Field != "email" || dst[0].Params["format"] != "RFC 5322" {
		t.Fatalf("dst = %+v", dst)
	}
	if original.Field != "" {
		t.Fatalf("collectErrors mutated the rule's error: Field = %q", original.Field)
	}
}

func TestNewError(t *testing.T) {
	err := validation.NewError("country_code", "must be a 2-letter code")
	if err.Code != "country_code" || err.Message != "must be a 2-letter code" || err.Field != "" || err.Params != nil {
		t.Fatalf("NewError = %+v", err)
	}
}

func TestNewRuleError(t *testing.T) {
	ve := validation.ExportNewRuleError("min", "must be at least 18", map[string]any{"min": 18})
	if ve.Code != "min" {
		t.Errorf("code = %q, want %q", ve.Code, "min")
	}
	if ve.Message != "must be at least 18" {
		t.Errorf("message = %q, want %q", ve.Message, "must be at least 18")
	}
	if ve.Params["min"] != 18 {
		t.Errorf("params[min] = %v, want 18", ve.Params["min"])
	}
}

// --- Embedded struct field resolution ---

func TestValidateStruct_EmbeddedStruct(t *testing.T) {
	type Base struct {
		ID int `json:"id"`
	}
	type Input struct {
		Base
		Name string `json:"name"`
	}
	input := &Input{}

	err := validation.ValidateStruct(input,
		validation.Field(&input.ID, alwaysFail[int]{code: "required", message: "is required"}),
		validation.Field(&input.Name, alwaysFail[string]{code: "required", message: "is required"}),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 2 {
		t.Fatalf("len = %d, want 2", len(errs))
	}

	fieldMap := map[string]bool{}
	for _, e := range errs {
		fieldMap[e.Field] = true
	}
	if !fieldMap["id"] {
		t.Error("missing error for embedded field 'id'")
	}
	if !fieldMap["name"] {
		t.Error("missing error for field 'name'")
	}
}

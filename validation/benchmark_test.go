package validation_test

import (
	"testing"

	"github.com/credo-go/credo/validation"
)

type benchStruct struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

// BenchmarkValidateStruct measures 3-field struct validation with
// Required + Length + Email rules (baseline for validation overhead).
func BenchmarkValidateStruct(b *testing.B) {
	s := &benchStruct{
		Name:  "Alice",
		Email: "alice@example.com",
		Age:   30,
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = validation.ValidateStruct(s,
			validation.Field(&s.Name, validation.Required[string](), validation.Length(2, 100)),
			validation.Field(&s.Email, validation.Required[string](), validation.Email()),
			validation.Field(&s.Age, validation.Required[int]()),
		)
	}
}

// BenchmarkEach_AllValid measures Each[string] over 100 valid elements.
// All elements pass — deferred prefix means no joinFieldPath calls.
func BenchmarkEach_AllValid(b *testing.B) {
	items := make([]string, 100)
	for i := range items {
		items[i] = "valid-item"
	}

	rule := validation.Each[string](validation.Required[string]())

	b.ReportAllocs()
	for b.Loop() {
		_ = rule.Validate(items)
	}
}

// BenchmarkEach_WithErrors measures Each[string] over 10 failing elements,
// exercising collectErrors → joinFieldPath on every element.
func BenchmarkEach_WithErrors(b *testing.B) {
	// All empty strings → Required fails for each element.
	items := make([]string, 10)

	rule := validation.Each[string](validation.Required[string]())

	b.ReportAllocs()
	for b.Loop() {
		_ = rule.Validate(items)
	}
}

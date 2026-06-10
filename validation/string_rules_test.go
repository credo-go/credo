package validation_test

import (
	"errors"
	"regexp"
	"testing"

	"github.com/credo-go/credo/validation"
)

// --- Required ---

func TestRequired_String_Valid(t *testing.T) {
	if err := validation.Required[string]().Validate("hello"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestRequired_String_Zero(t *testing.T) {
	err := validation.Required[string]().Validate("")
	assertValidationError(t, err, "required", "is required")
}

func TestRequired_Int_Valid(t *testing.T) {
	if err := validation.Required[int]().Validate(42); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestRequired_Int_Zero(t *testing.T) {
	err := validation.Required[int]().Validate(0)
	assertValidationError(t, err, "required", "is required")
}

func TestRequired_Bool_False(t *testing.T) {
	err := validation.Required[bool]().Validate(false)
	assertValidationError(t, err, "required", "is required")
}

func TestRequired_Bool_True(t *testing.T) {
	if err := validation.Required[bool]().Validate(true); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// --- Email ---

func TestEmail(t *testing.T) {
	tests := []struct {
		email string
		valid bool
	}{
		{"user@example.com", true},
		{"user+tag@example.com", true},
		{"user.name@example.co.uk", true},
		{"a@b.c", true},
		{"", true}, // empty passes (use Required for non-empty)
		{"notanemail", false},
		{"@example.com", false},
		{"user@", false},
		{"user @example.com", false},
	}
	rule := validation.Email()
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			err := rule.Validate(tt.email)
			if tt.valid && err != nil {
				t.Errorf("expected nil for %q, got %v", tt.email, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("expected error for %q, got nil", tt.email)
			}
			if !tt.valid && err != nil {
				assertValidationError(t, err, "email", "")
			}
		})
	}
}

// --- URL ---

func TestURL(t *testing.T) {
	tests := []struct {
		url   string
		valid bool
	}{
		{"https://example.com", true},
		{"http://example.com/path?q=1", true},
		{"ftp://files.example.com", true},
		{"", true}, // empty passes
		{"example.com", false},
		{"://bad", false},
		{"/relative/path", false},
	}
	rule := validation.URL()
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := rule.Validate(tt.url)
			if tt.valid && err != nil {
				t.Errorf("expected nil for %q, got %v", tt.url, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("expected error for %q, got nil", tt.url)
			}
			if !tt.valid && err != nil {
				assertValidationError(t, err, "url", "")
			}
		})
	}
}

// --- UUID ---

func TestUUID(t *testing.T) {
	tests := []struct {
		uuid  string
		valid bool
	}{
		{"550e8400-e29b-41d4-a716-446655440000", true},
		{"550E8400-E29B-41D4-A716-446655440000", true}, // uppercase
		{"550e8400e29b41d4a716446655440000", true},     // no hyphens
		{"", true}, // empty passes
		{"not-a-uuid", false},
		{"550e8400-e29b-41d4-a716", false},              // too short
		{"550e8400-e29b-41d4-a716-44665544000g", false}, // invalid hex
	}
	rule := validation.UUID()
	for _, tt := range tests {
		t.Run(tt.uuid, func(t *testing.T) {
			err := rule.Validate(tt.uuid)
			if tt.valid && err != nil {
				t.Errorf("expected nil for %q, got %v", tt.uuid, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("expected error for %q, got nil", tt.uuid)
			}
		})
	}
}

// --- Regex ---

func TestRegex(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Z]{2}$`)
	rule := validation.Regex(pattern)

	tests := []struct {
		value string
		valid bool
	}{
		{"US", true},
		{"TR", true},
		{"", true}, // empty passes
		{"usa", false},
		{"U", false},
		{"USA", false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			err := rule.Validate(tt.value)
			if tt.valid && err != nil {
				t.Errorf("expected nil for %q, got %v", tt.value, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("expected error for %q, got nil", tt.value)
			}
		})
	}
}

// --- Length ---

func TestLength(t *testing.T) {
	rule := validation.Length(2, 10)
	tests := []struct {
		value string
		valid bool
	}{
		{"ab", true},
		{"abcdefghij", true},
		{"hello", true},
		{"", true}, // empty passes
		{"a", false},
		{"abcdefghijk", false}, // 11 chars
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			err := rule.Validate(tt.value)
			if tt.valid && err != nil {
				t.Errorf("expected nil for %q, got %v", tt.value, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("expected error for %q, got nil", tt.value)
			}
		})
	}
}

func TestLength_Unicode(t *testing.T) {
	// "hello" in Turkish: "merhaba" (7 runes)
	// Japanese: "こんにちは" (5 runes)
	rule := validation.Length(1, 5)
	if err := rule.Validate("こんにちは"); err != nil {
		t.Errorf("expected nil for 5 runes, got %v", err)
	}
	if err := rule.Validate("こんにちはa"); err == nil {
		t.Error("expected error for 6 runes, got nil")
	}
}

func TestLength_ErrorParams(t *testing.T) {
	rule := validation.Length(2, 100)
	err := rule.Validate("a")

	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if ve.Code != "length" {
		t.Errorf("code = %q, want %q", ve.Code, "length")
	}
	if ve.Params["min"] != 2 {
		t.Errorf("params[min] = %v, want 2", ve.Params["min"])
	}
	if ve.Params["max"] != 100 {
		t.Errorf("params[max] = %v, want 100", ve.Params["max"])
	}
}

// --- Integration ---

func TestStringRules_WithValidateStruct(t *testing.T) {
	type Input struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	input := &Input{Name: "a", Email: "notanemail"}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Name, validation.Required[string](), validation.Length(2, 100)),
		validation.Field(&input.Email, validation.Required[string](), validation.Email()),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 2 {
		t.Fatalf("len = %d, want 2", len(errs))
	}

	codes := map[string]string{}
	for _, e := range errs {
		codes[e.Field] = e.Code
	}
	if codes["name"] != "length" {
		t.Errorf("name code = %q, want %q", codes["name"], "length")
	}
	if codes["email"] != "email" {
		t.Errorf("email code = %q, want %q", codes["email"], "email")
	}
}

// --- Test helper ---

func assertValidationError(t *testing.T, err error, wantCode, wantMessage string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if wantCode != "" && ve.Code != wantCode {
		t.Errorf("code = %q, want %q", ve.Code, wantCode)
	}
	if wantMessage != "" && ve.Message != wantMessage {
		t.Errorf("message = %q, want %q", ve.Message, wantMessage)
	}
}

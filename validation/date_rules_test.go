package validation_test

import (
	"errors"
	"testing"
	"time"

	"github.com/credo-go/credo/validation"
)

var (
	past   = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	now    = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	future = time.Date(2030, 12, 31, 0, 0, 0, 0, time.UTC)
)

// --- DateBefore ---

func TestDateBefore_Valid(t *testing.T) {
	rule := validation.DateBefore(now)
	if err := rule.Validate(past); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestDateBefore_Invalid(t *testing.T) {
	rule := validation.DateBefore(now)
	err := rule.Validate(future)
	assertValidationError(t, err, "date_before", "")
}

func TestDateBefore_Equal(t *testing.T) {
	rule := validation.DateBefore(now)
	err := rule.Validate(now) // equal is NOT before
	if err == nil {
		t.Fatal("expected error for equal time, got nil")
	}
}

func TestDateBefore_Zero(t *testing.T) {
	rule := validation.DateBefore(now)
	if err := rule.Validate(time.Time{}); err != nil {
		t.Errorf("expected nil for zero time, got %v", err)
	}
}

func TestDateBefore_ErrorParams(t *testing.T) {
	rule := validation.DateBefore(now)
	err := rule.Validate(future)
	ve, ok := errors.AsType[*validation.ValidationError](err)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	threshold, ok := ve.Params["threshold"].(time.Time)
	if !ok {
		t.Fatal("params[threshold] should be time.Time")
	}
	if !threshold.Equal(now) {
		t.Errorf("params[threshold] = %v, want %v", threshold, now)
	}
}

// --- DateAfter ---

func TestDateAfter_Valid(t *testing.T) {
	rule := validation.DateAfter(now)
	if err := rule.Validate(future); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestDateAfter_Invalid(t *testing.T) {
	rule := validation.DateAfter(now)
	err := rule.Validate(past)
	assertValidationError(t, err, "date_after", "")
}

func TestDateAfter_Equal(t *testing.T) {
	rule := validation.DateAfter(now)
	err := rule.Validate(now) // equal is NOT after
	if err == nil {
		t.Fatal("expected error for equal time, got nil")
	}
}

func TestDateAfter_Zero(t *testing.T) {
	rule := validation.DateAfter(now)
	if err := rule.Validate(time.Time{}); err != nil {
		t.Errorf("expected nil for zero time, got %v", err)
	}
}

// --- Integration: cross-field ---

func TestDateRules_CrossField(t *testing.T) {
	type Event struct {
		StartDate time.Time `json:"start_date"`
		EndDate   time.Time `json:"end_date"`
	}
	event := &Event{
		StartDate: now,
		EndDate:   past, // end before start — invalid
	}

	err := validation.ValidateStruct(event,
		validation.Field(&event.EndDate, validation.DateAfter(event.StartDate)),
	)

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("expected validation.Errors, got %T", err)
	}
	if len(errs) != 1 {
		t.Fatalf("len = %d, want 1", len(errs))
	}
	if errs[0].Field != "end_date" {
		t.Errorf("field = %q, want %q", errs[0].Field, "end_date")
	}
}

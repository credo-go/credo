package i18n

import (
	"encoding/json"
	"testing"
)

func TestMessageFromJSON_StringValue(t *testing.T) {
	raw := json.RawMessage(`"is required"`)
	msg, err := messageFromJSON("v.required", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.ID != "v.required" {
		t.Errorf("ID = %q, want %q", msg.ID, "v.required")
	}
	if msg.Other != "is required" {
		t.Errorf("Other = %q, want %q", msg.Other, "is required")
	}
}

func TestMessageFromJSON_PluralObject(t *testing.T) {
	raw := json.RawMessage(`{"one": "{{.min}} item", "other": "{{.min}} items"}`)
	msg, err := messageFromJSON("v.min_items", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.ID != "v.min_items" {
		t.Errorf("ID = %q, want %q", msg.ID, "v.min_items")
	}
	if msg.One != "{{.min}} item" {
		t.Errorf("One = %q, want %q", msg.One, "{{.min}} item")
	}
	if msg.Other != "{{.min}} items" {
		t.Errorf("Other = %q, want %q", msg.Other, "{{.min}} items")
	}
}

func TestMessageFromJSON_AllPluralForms(t *testing.T) {
	raw := json.RawMessage(`{
		"zero": "z", "one": "o", "two": "t",
		"few": "f", "many": "m", "other": "x"
	}`)
	msg, err := messageFromJSON("test", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Zero != "z" || msg.One != "o" || msg.Two != "t" ||
		msg.Few != "f" || msg.Many != "m" || msg.Other != "x" {
		t.Errorf("plural forms not correctly parsed: %+v", msg)
	}
}

func TestMessageFromJSON_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`[1, 2, 3]`)
	_, err := messageFromJSON("bad", raw)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestMessageFromJSON_NoOther_DeterministicFallback(t *testing.T) {
	// Object with "few" and "one" but no "other" — should deterministically pick "one"
	// because "one" has higher CLDR priority than "few".
	raw := json.RawMessage(`{"few": "few items", "one": "one item"}`)
	msg, err := messageFromJSON("test.fallback", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Other != "one item" {
		t.Errorf("Other = %q, want %q (deterministic fallback to 'one')", msg.Other, "one item")
	}
	if msg.Few != "few items" {
		t.Errorf("Few = %q, want %q", msg.Few, "few items")
	}
	if msg.One != "one item" {
		t.Errorf("One = %q, want %q", msg.One, "one item")
	}
}

func TestMessageFromJSON_NoOther_OnlyZero(t *testing.T) {
	// Object with only "zero" and no "other" — should fall back to "zero".
	raw := json.RawMessage(`{"zero": "no items"}`)
	msg, err := messageFromJSON("test.zero_only", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Other != "no items" {
		t.Errorf("Other = %q, want %q", msg.Other, "no items")
	}
}

func TestMessageFromJSON_EmptyID(t *testing.T) {
	raw := json.RawMessage(`"test"`)
	msg, err := messageFromJSON("", raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.ID != "" {
		t.Errorf("ID = %q, want empty", msg.ID)
	}
}

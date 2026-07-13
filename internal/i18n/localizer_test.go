package i18n

import (
	"errors"
	"testing"

	"golang.org/x/text/language"
)

func newTestBundle(t *testing.T) *Bundle {
	t.Helper()
	b := NewBundle(language.English)
	if err := b.LoadDirFS(testFS(), "."); err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}
	return b
}

func TestLocalizer_Localize_English(t *testing.T) {
	b := newTestBundle(t)
	l := NewLocalizer(b, "en")

	s, err := l.Localize("v.required", nil)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	if s != "is required" {
		t.Errorf("got %q, want %q", s, "is required")
	}
}

func TestLocalizer_Localize_Turkish(t *testing.T) {
	b := newTestBundle(t)
	l := NewLocalizer(b, "tr")

	s, err := l.Localize("v.required", nil)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	if s != "zorunludur" {
		t.Errorf("got %q, want %q", s, "zorunludur")
	}
}

func TestLocalizer_Localize_FallbackToDefault(t *testing.T) {
	b := newTestBundle(t)
	// Request French, which isn't loaded — should fall back to English.
	l := NewLocalizer(b, "fr")

	s, err := l.Localize("v.required", nil)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	if s != "is required" {
		t.Errorf("got %q, want %q (expected English fallback)", s, "is required")
	}
}

func TestLocalizer_Localize_MessageNotFound(t *testing.T) {
	b := newTestBundle(t)
	l := NewLocalizer(b, "en")

	_, err := l.Localize("nonexistent.key", nil)
	if err == nil {
		t.Fatal("expected MessageNotFoundError")
	}

	mnf, ok := errors.AsType[*MessageNotFoundError](err)
	if !ok {
		t.Fatalf("expected *MessageNotFoundError, got %T: %v", err, err)
	}
	if mnf.ID != "nonexistent.key" {
		t.Errorf("ID = %q, want %q", mnf.ID, "nonexistent.key")
	}
}

func TestLocalizer_LocalizeWithTag(t *testing.T) {
	b := newTestBundle(t)
	l := NewLocalizer(b, "tr")

	s, tag, err := l.LocalizeWithTag("http.not_found", nil)
	if err != nil {
		t.Fatalf("LocalizeWithTag: %v", err)
	}
	if s != "Bulunamadı" {
		t.Errorf("got %q, want %q", s, "Bulunamadı")
	}
	if tag != language.Turkish {
		t.Errorf("tag = %v, want Turkish", tag)
	}
}

func TestLocalizer_LocalizePlural(t *testing.T) {
	b := NewBundle(language.English)
	_ = b.AddMessages(language.English,
		&Message{
			ID:    "items",
			One:   "{{.count}} item",
			Other: "{{.count}} items",
		},
	)

	l := NewLocalizer(b, "en")

	// count = 1 → One form
	s, err := l.LocalizePlural("items", 1, nil)
	if err != nil {
		t.Fatalf("LocalizePlural(1): %v", err)
	}
	if s != "1 item" {
		t.Errorf("count=1: got %q, want %q", s, "1 item")
	}

	// count = 5 → Other form
	s, err = l.LocalizePlural("items", 5, nil)
	if err != nil {
		t.Fatalf("LocalizePlural(5): %v", err)
	}
	if s != "5 items" {
		t.Errorf("count=5: got %q, want %q", s, "5 items")
	}
}

func TestLocalizer_LocalizePlural_InvalidCount(t *testing.T) {
	b := NewBundle(language.English)
	l := NewLocalizer(b, "en")

	_, err := l.LocalizePlural("items", 3.14, nil)
	if err == nil {
		t.Error("expected error for float count")
	}
}

func TestLocalizer_LocalizePlural_DoesNotMutateCallerMap(t *testing.T) {
	b := NewBundle(language.English)
	_ = b.AddMessages(language.English,
		&Message{
			ID:    "items",
			One:   "{{.count}} item",
			Other: "{{.count}} items",
		},
	)

	l := NewLocalizer(b, "en")
	data := map[string]any{"extra": "value"}

	_, err := l.LocalizePlural("items", 5, data)
	if err != nil {
		t.Fatalf("LocalizePlural: %v", err)
	}

	// Verify caller's map was not mutated.
	if _, exists := data["count"]; exists {
		t.Error("caller's data map was mutated: 'count' key was injected")
	}
	if len(data) != 1 {
		t.Errorf("caller's data map has %d entries, want 1", len(data))
	}
}

func TestLocalizer_MultiplePreferences(t *testing.T) {
	b := newTestBundle(t)
	// Prefer German (not loaded), then Turkish
	l := NewLocalizer(b, "de", "tr")

	s, err := l.Localize("v.required", nil)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	// German not available, should match Turkish or fall back
	// Since de is not loaded, matcher picks next available
	if s != "zorunludur" && s != "is required" {
		t.Errorf("got %q, want Turkish or English", s)
	}
}

// TestLocalizer_ExtensionTagMatchesBase locks in that a requested tag carrying a
// Unicode (-u-) extension resolves to the registered base tag. x/text folds the
// caller's extensions into the tag its Match returns, so keying messages by that
// tag missed the registered entry and fell back to the default language.
func TestLocalizer_ExtensionTagMatchesBase(t *testing.T) {
	b := NewBundle(language.English)
	if err := b.AddMessages(language.English, &Message{ID: "greeting", Other: "Hello"}); err != nil {
		t.Fatalf("AddMessages(en): %v", err)
	}
	if err := b.AddMessages(language.Turkish, &Message{ID: "greeting", Other: "Merhaba"}); err != nil {
		t.Fatalf("AddMessages(tr): %v", err)
	}

	l := NewLocalizer(b, "tr-u-nu-latn")
	s, err := l.Localize("greeting", nil)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	if s != "Merhaba" {
		t.Errorf("got %q, want %q (extension tag must match base tr, not fall back to en)", s, "Merhaba")
	}
}

// TestLocalizer_EmptyBundleNoPanic verifies that matching against a bundle with
// no messages — and therefore a nil matcher — does not panic and reports the
// message as not found.
func TestLocalizer_EmptyBundleNoPanic(t *testing.T) {
	b := NewBundle(language.English)
	l := NewLocalizer(b, "tr")

	_, err := l.Localize("greeting", nil)
	if _, ok := errors.AsType[*MessageNotFoundError](err); !ok {
		t.Fatalf("Localize on empty bundle: got %T (%v), want *MessageNotFoundError", err, err)
	}
}

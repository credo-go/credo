package credo

import (
	"testing"
	"testing/fstest"

	internali18n "github.com/credo-go/credo/internal/i18n"
	"github.com/credo-go/credo/validation"
)

func newTranslateTestBundle(t *testing.T) *internali18n.Bundle {
	t.Helper()
	fsys := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "is required", "http.not_found": "Not found", "http.internal_server_error": "Internal server error"}`),
		},
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "zorunludur", "http.not_found": "Bulunamadı", "http.internal_server_error": "Sunucu hatası"}`),
		},
		"tr/fields.json": &fstest.MapFile{
			Data: []byte(`{"email": "e-posta adresi", "name": "isim"}`),
		},
	}
	b, err := internali18n.NewBundleFromString("en")
	if err != nil {
		t.Fatalf("NewBundleFromString: %v", err)
	}
	if err := b.LoadDirFS(fsys, "."); err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}
	return b
}

// --- translateValidationErrors tests ---

func TestTranslateValidationErrors_Turkish(t *testing.T) {
	b := newTranslateTestBundle(t)
	ve := validation.Errors{
		{Field: "email", Code: "required", Message: "is required"},
		{Field: "name", Code: "required", Message: "is required"},
	}

	translated := translateValidationErrors(b, "tr", ve)

	if len(translated) != 2 {
		t.Fatalf("len = %d, want 2", len(translated))
	}
	if translated[0].Message != "zorunludur" {
		t.Errorf("translated[0].Message = %q, want %q", translated[0].Message, "zorunludur")
	}
	if translated[0].Field != "email" {
		t.Errorf("translated[0].Field = %q, want %q", translated[0].Field, "email")
	}
}

func TestTranslateValidationErrors_WithFieldInjection(t *testing.T) {
	fsys := fstest.MapFS{
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "{{.field}} zorunludur"}`),
		},
		"tr/fields.json": &fstest.MapFile{
			Data: []byte(`{"email": "e-posta adresi"}`),
		},
	}
	b, _ := internali18n.NewBundleFromString("en")
	_ = b.LoadDirFS(fsys, ".")

	ve := validation.Errors{
		{Field: "email", Code: "required", Message: "is required"},
	}

	translated := translateValidationErrors(b, "tr", ve)
	if translated[0].Message != "e-posta adresi zorunludur" {
		t.Errorf("Message = %q, want %q", translated[0].Message, "e-posta adresi zorunludur")
	}
}

func TestTranslateValidationErrors_WithParams(t *testing.T) {
	fsys := fstest.MapFS{
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.length": "{{.min}} ile {{.max}} karakter arasında olmalıdır"}`),
		},
	}
	b, _ := internali18n.NewBundleFromString("en")
	_ = b.LoadDirFS(fsys, ".")

	ve := validation.Errors{
		{
			Field:   "name",
			Code:    "length",
			Message: "must be between 2 and 100 characters",
			Params:  map[string]any{"min": 2, "max": 100},
		},
	}

	translated := translateValidationErrors(b, "tr", ve)
	want := "2 ile 100 karakter arasında olmalıdır"
	if translated[0].Message != want {
		t.Errorf("Message = %q, want %q", translated[0].Message, want)
	}
}

func TestTranslateValidationErrors_MissingTranslation(t *testing.T) {
	b, _ := internali18n.NewBundleFromString("en")

	ve := validation.Errors{
		{Field: "name", Code: "custom_rule", Message: "custom message"},
	}

	translated := translateValidationErrors(b, "en", ve)
	if translated[0].Message != "custom message" {
		t.Errorf("Message = %q, want %q (original)", translated[0].Message, "custom message")
	}
}

func TestTranslateValidationErrors_DoesNotMutateOriginal(t *testing.T) {
	b := newTranslateTestBundle(t)
	original := validation.Errors{
		{Field: "email", Code: "required", Message: "is required"},
	}

	_ = translateValidationErrors(b, "tr", original)

	if original[0].Message != "is required" {
		t.Errorf("original mutated: Message = %q, want %q", original[0].Message, "is required")
	}
}

// --- resolveMessage tests ---

func TestResolveMessage_WithI18n(t *testing.T) {
	b := newTranslateTestBundle(t)

	app := &App{i18nBundle: b}
	ctx := &Context{app: app, locale: "tr"}

	got := resolveMessage(ctx, MsgKeyNotFound)
	if got != "Bulunamadı" {
		t.Errorf("resolveMessage() = %q, want %q", got, "Bulunamadı")
	}
}

func TestResolveMessage_BuiltInFallback(t *testing.T) {
	// No i18n configured — should fall through to builtInMessages
	ctx := &Context{}

	got := resolveMessage(ctx, MsgKeyNotFound)
	if got != "Not Found" {
		t.Errorf("resolveMessage() = %q, want %q", got, "Not Found")
	}
}

func TestResolveMessage_KeyFallback(t *testing.T) {
	// No i18n, unknown key — should return the key itself
	ctx := &Context{}

	got := resolveMessage(ctx, "app.custom_error")
	if got != "app.custom_error" {
		t.Errorf("resolveMessage() = %q, want %q", got, "app.custom_error")
	}
}

func TestResolveMessage_I18nMiss_BuiltInHit(t *testing.T) {
	// i18n configured but key not in locale files — should fallback to builtInMessages
	fsys := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"some.other.key": "other"}`),
		},
	}
	b, _ := internali18n.NewBundleFromString("en")
	_ = b.LoadDirFS(fsys, ".")

	app := &App{i18nBundle: b}
	ctx := &Context{app: app, locale: "en"}

	got := resolveMessage(ctx, MsgKeyConflict)
	if got != "Conflict" {
		t.Errorf("resolveMessage() = %q, want %q", got, "Conflict")
	}
}

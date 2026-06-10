package i18n

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"golang.org/x/text/language"
)

func TestBundle_AddMessages(t *testing.T) {
	b := NewBundle(language.English)
	err := b.AddMessages(language.English,
		&Message{ID: "v.required", Other: "is required"},
		&Message{ID: "v.email", Other: "must be a valid email"},
	)
	if err != nil {
		t.Fatalf("AddMessages: %v", err)
	}

	msgs := b.messageTemplates(language.English)
	if len(msgs) != 2 {
		t.Fatalf("messages count = %d, want 2", len(msgs))
	}
	if msgs["v.required"] == nil || msgs["v.email"] == nil {
		t.Error("expected v.required and v.email to be loaded")
	}
}

func TestBundle_AddMessages_EmptyID(t *testing.T) {
	b := NewBundle(language.English)
	err := b.AddMessages(language.English, &Message{ID: "", Other: "test"})
	if err == nil {
		t.Error("expected error for empty ID")
	}
}

func TestBundle_SetFields(t *testing.T) {
	b := NewBundle(language.English)
	b.SetFields(language.Turkish, map[string]string{
		"email": "e-posta adresi",
	})
	name := b.fieldName(language.Turkish, "email")
	if name != "e-posta adresi" {
		t.Errorf("fieldName = %q, want %q", name, "e-posta adresi")
	}
}

func TestBundle_FieldName_Fallback(t *testing.T) {
	b := NewBundle(language.English)
	name := b.fieldName(language.English, "email")
	if name != "email" {
		t.Errorf("fieldName = %q, want %q (raw fallback)", name, "email")
	}
}

func TestBundle_LoadDirFS(t *testing.T) {
	fsys := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "is required", "http.404": "Not found"}`),
		},
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "zorunludur", "http.404": "Bulunamadı"}`),
		},
		"tr/fields.json": &fstest.MapFile{
			Data: []byte(`{"email": "e-posta adresi"}`),
		},
	}

	b := NewBundle(language.English)
	if err := b.LoadDirFS(fsys, "."); err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}

	// Check English messages loaded
	enMsgs := b.messageTemplates(language.English)
	if enMsgs == nil || enMsgs["v.required"] == nil {
		t.Error("English messages not loaded")
	}

	// Check Turkish messages loaded
	trMsgs := b.messageTemplates(language.Turkish)
	if trMsgs == nil || trMsgs["v.required"] == nil {
		t.Error("Turkish messages not loaded")
	}

	// Check Turkish fields loaded
	name := b.fieldName(language.Turkish, "email")
	if name != "e-posta adresi" {
		t.Errorf("Turkish field email = %q, want %q", name, "e-posta adresi")
	}

	// Check language tags
	tags := b.LanguageTags()
	if len(tags) != 2 {
		t.Fatalf("tags count = %d, want 2", len(tags))
	}
}

func TestBundle_LoadDirFS_InvalidDir(t *testing.T) {
	fsys := fstest.MapFS{}
	b := NewBundle(language.English)
	err := b.LoadDirFS(fsys, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent dir")
	}
}

func TestBundle_LoadDirFS_SkipsNonLangDirs(t *testing.T) {
	fsys := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "is required"}`),
		},
		"_templates/messages.json": &fstest.MapFile{
			Data: []byte(`{"test": "value"}`),
		},
	}

	b := NewBundle(language.English)
	if err := b.LoadDirFS(fsys, "."); err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}

	tags := b.LanguageTags()
	if len(tags) != 1 {
		t.Errorf("tags count = %d, want 1 (should skip non-language dirs)", len(tags))
	}
}

func TestBundle_LoadDir(t *testing.T) {
	// Use the bundled test locales.
	b := NewBundle(language.English)
	if err := b.LoadDir("locales"); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	enMsgs := b.messageTemplates(language.English)
	if enMsgs == nil {
		t.Fatal("English messages not loaded from filesystem")
	}
	if enMsgs["v.required"] == nil {
		t.Error("v.required not found in English messages")
	}
	if enMsgs["http.404"] == nil {
		t.Error("http.404 not found in English messages")
	}
}

func TestBundle_LoadDirFS_PluralMessages(t *testing.T) {
	fsys := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{
				"v.min_items": {"one": "must have at least {{.min}} item", "other": "must have at least {{.min}} items"}
			}`),
		},
	}

	b := NewBundle(language.English)
	if err := b.LoadDirFS(fsys, "."); err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}

	msgs := b.messageTemplates(language.English)
	mt := msgs["v.min_items"]
	if mt == nil {
		t.Fatal("v.min_items not loaded")
	}
	if mt.One != "must have at least {{.min}} item" {
		t.Errorf("One = %q", mt.One)
	}
	if mt.Other != "must have at least {{.min}} items" {
		t.Errorf("Other = %q", mt.Other)
	}
}

func TestBundle_DefaultLanguage(t *testing.T) {
	b := NewBundle(language.Turkish)
	if b.DefaultLanguage() != language.Turkish {
		t.Errorf("DefaultLanguage = %v, want Turkish", b.DefaultLanguage())
	}
}

func TestBundle_LoadDirFS_InvalidFieldsJSON(t *testing.T) {
	fsys := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "is required"}`),
		},
		"en/fields.json": &fstest.MapFile{
			Data: []byte(`{invalid json`),
		},
	}

	b := NewBundle(language.English)
	err := b.LoadDirFS(fsys, ".")
	if err == nil {
		t.Fatal("expected error for invalid fields.json, got nil")
	}
}

// testFS creates a minimal fs.FS for testing.
func testFS() fs.FS {
	return fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "is required", "http.404": "Not found", "http.500": "Internal server error"}`),
		},
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "zorunludur", "http.404": "Bulunamadı", "http.500": "Sunucu hatası"}`),
		},
		"tr/fields.json": &fstest.MapFile{
			Data: []byte(`{"email": "e-posta adresi", "name": "isim"}`),
		},
	}
}

func TestBundle_TranslateForLang(t *testing.T) {
	b := NewBundle(language.English)
	if err := b.LoadDirFS(testFS(), "."); err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}

	s, ok := b.TranslateForLang("tr", "v.required", nil)
	if !ok {
		t.Fatal("TranslateForLang returned false")
	}
	if s != "zorunludur" {
		t.Errorf("got %q, want %q", s, "zorunludur")
	}

	// Fallback to default
	s, ok = b.TranslateForLang("fr", "v.required", nil)
	if !ok {
		t.Fatal("TranslateForLang fallback returned false")
	}
	if s != "is required" {
		t.Errorf("got %q, want %q", s, "is required")
	}

	// Missing key
	_, ok = b.TranslateForLang("en", "nonexistent", nil)
	if ok {
		t.Error("expected false for nonexistent key")
	}
}

func TestBundle_TranslateForLang_WithData(t *testing.T) {
	b := NewBundle(language.English)
	_ = b.AddMessages(language.Turkish,
		&Message{ID: "v.length", Other: "{{.min}} ile {{.max}} arasında"},
	)

	s, ok := b.TranslateForLang("tr", "v.length", map[string]any{"min": 2, "max": 100})
	if !ok {
		t.Fatal("TranslateForLang returned false")
	}
	if s != "2 ile 100 arasında" {
		t.Errorf("got %q, want %q", s, "2 ile 100 arasında")
	}
}

func TestBundle_TranslatePluralForLang(t *testing.T) {
	b := NewBundle(language.English)
	if err := b.AddMessages(language.English,
		&Message{ID: "items", One: "{{.count}} item", Other: "{{.count}} items"},
		&Message{ID: "thing", One: "a thing", Other: "things"},
	); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}
	if err := b.AddMessages(language.Turkish,
		&Message{ID: "items", One: "tek öğe", Other: "{{.count}} öğe"},
	); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}

	tests := []struct {
		name  string
		lang  string
		key   string
		count any
		want  string
	}{
		{"en one", "en", "items", 1, "1 item"},
		{"en other", "en", "items", 5, "5 items"},
		{"en string count", "en", "items", "1.5", "1.5 items"},
		{"tr one", "tr", "items", 1, "tek öğe"},
		{"tr other", "tr", "items", 3, "3 öğe"},
		// Unknown language falls back to the default language's message
		// and selects the form with the default language's rule.
		{"fallback lang one", "fr", "items", 1, "1 item"},
		{"fallback lang other", "fr", "items", 2, "2 items"},
		// Invalid count renders the Other form instead of failing.
		{"invalid count uses Other", "en", "thing", struct{}{}, "things"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := b.TranslatePluralForLang(tt.lang, tt.key, tt.count, nil)
			if !ok {
				t.Fatal("TranslatePluralForLang returned false")
			}
			if s != tt.want {
				t.Errorf("got %q, want %q", s, tt.want)
			}
		})
	}

	if _, ok := b.TranslatePluralForLang("en", "nonexistent", 1, nil); ok {
		t.Error("expected false for nonexistent key")
	}
}

func TestBundle_TranslatePluralForLang_DoesNotMutateData(t *testing.T) {
	b := NewBundle(language.English)
	_ = b.AddMessages(language.English,
		&Message{ID: "items", One: "{{.count}} {{.kind}}", Other: "{{.count}} {{.kind}}s"},
	)

	data := map[string]any{"kind": "box"}
	s, ok := b.TranslatePluralForLang("en", "items", 2, data)
	if !ok || s != "2 boxs" {
		t.Fatalf("got %q (ok=%v), want \"2 boxs\"", s, ok)
	}
	if _, exists := data["count"]; exists {
		t.Error("TranslatePluralForLang mutated the caller's data map")
	}
}

func TestBundle_FieldNameForLang(t *testing.T) {
	b := NewBundle(language.English)
	if err := b.LoadDirFS(testFS(), "."); err != nil {
		t.Fatalf("LoadDirFS: %v", err)
	}

	name := b.FieldNameForLang("tr", "email")
	if name != "e-posta adresi" {
		t.Errorf("got %q, want %q", name, "e-posta adresi")
	}

	// Untranslated field
	name = b.FieldNameForLang("tr", "unknown_field")
	if name != "unknown_field" {
		t.Errorf("got %q, want %q", name, "unknown_field")
	}
}

func TestBundle_HasMessages(t *testing.T) {
	b := NewBundle(language.English)
	if b.HasMessages() {
		t.Error("empty bundle should return false")
	}
	_ = b.AddMessages(language.English, &Message{ID: "test", Other: "test"})
	if !b.HasMessages() {
		t.Error("non-empty bundle should return true")
	}
}

func TestBundle_DefaultLang(t *testing.T) {
	b := NewBundle(language.Turkish)
	if b.DefaultLang() != "tr" {
		t.Errorf("DefaultLang = %q, want %q", b.DefaultLang(), "tr")
	}
}

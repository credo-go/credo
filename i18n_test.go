package credo_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

func i18nTestFS() fstest.MapFS {
	return fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{
				"v.required": "is required",
				"v.email": "must be a valid email",
				"http.not_found": "Not found",
				"http.internal_server_error": "Internal server error",
				"items": {"one": "{{.count}} item", "other": "{{.count}} items"}
			}`),
		},
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{
				"v.required": "zorunludur",
				"v.email": "geçerli bir e-posta adresi olmalıdır",
				"http.not_found": "Bulunamadı",
				"http.internal_server_error": "Sunucu hatası",
				"items": {"one": "tek öğe", "other": "{{.count}} öğe"}
			}`),
		},
		"tr/fields.json": &fstest.MapFile{
			Data: []byte(`{"email": "e-posta adresi"}`),
		},
	}
}

func TestCtx_TPlural(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.GET("/items", func(ctx *credo.Context) error {
		parts := []string{
			ctx.TPlural("items", 1),
			ctx.TPlural("items", 5),
			ctx.TPlural("nonexistent", 1),
		}
		return ctx.Response().Text(200, strings.Join(parts, "|"))
	})

	tests := []struct {
		lang string
		want string
	}{
		{"en", "1 item|5 items|nonexistent"},
		{"tr", "tek öğe|5 öğe|nonexistent"},
	}

	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/items", nil)
			r.Header.Set("Accept-Language", tt.lang)
			app.ServeHTTP(w, r)

			if w.Body.String() != tt.want {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.want)
			}
		})
	}
}

func TestCtx_TPlural_WithoutI18n(t *testing.T) {
	app := mustNew(t)
	app.GET("/items", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.TPlural("items", 2))
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/items", nil))

	if w.Body.String() != "items" {
		t.Errorf("body = %q, want %q (key returned when i18n inactive)", w.Body.String(), "items")
	}
}

func TestUseI18n_ValidationErrors_Turkish(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.POST("/test", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "email", Code: "required", Message: "is required"},
		}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(pd.Errors) != 1 {
		t.Fatalf("errors count = %d, want 1", len(pd.Errors))
	}
	if pd.Errors[0].Message != "zorunludur" {
		t.Errorf("translated message = %q, want %q", pd.Errors[0].Message, "zorunludur")
	}
	if pd.Errors[0].Field != "email" {
		t.Errorf("field = %q, want %q", pd.Errors[0].Field, "email")
	}
}

func TestUseI18n_HTTPError_Turkish(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.GET("/missing", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/missing", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Title != "Bulunamadı" {
		t.Errorf("title = %q, want %q", pd.Title, "Bulunamadı")
	}
}

func TestUseI18n_EnglishDefault(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.POST("/test", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "email", Code: "required", Message: "is required"},
		}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)
	r.Header.Set("Accept-Language", "en")
	app.ServeHTTP(w, r)

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(pd.Errors) != 1 {
		t.Fatalf("errors count = %d, want 1", len(pd.Errors))
	}
	if pd.Errors[0].Message != "is required" {
		t.Errorf("message = %q, want %q", pd.Errors[0].Message, "is required")
	}
}

func TestUseI18n_CustomDetect(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
		Detect: func(r *http.Request) string {
			return r.URL.Query().Get("lang")
		},
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.POST("/test", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "email", Code: "required", Message: "is required"},
		}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test?lang=tr", nil)
	app.ServeHTTP(w, r)

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Errors[0].Message != "zorunludur" {
		t.Errorf("message = %q, want %q", pd.Errors[0].Message, "zorunludur")
	}
}

func TestUseI18n_NoDir_Inactive(t *testing.T) {
	app := mustNew(t)
	// Point to a non-existent directory
	err := app.UseI18n(credo.I18nConfig{
		Dir:     "nonexistent_locales/",
		Default: "en",
	})
	if err != nil {
		t.Fatalf("UseI18n should return nil for missing dir, got: %v", err)
	}

	// ctx.T should return the key as-is
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.T("v.required"))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Body.String() != "v.required" {
		t.Errorf("T() = %q, want %q (key passthrough)", w.Body.String(), "v.required")
	}
}

func TestUseI18n_MalformedTemplate_Error(t *testing.T) {
	badFS := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "{{.field is required"}`),
		},
	}

	app := mustNew(t)
	err := app.UseI18n(credo.I18nConfig{
		DirFS:   badFS,
		Default: "en",
	})
	if err == nil {
		t.Error("expected error for malformed template")
	}
}

func TestUseI18n_MalformedJSON_Error(t *testing.T) {
	badFS := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{bad json`),
		},
	}

	app := mustNew(t)
	err := app.UseI18n(credo.I18nConfig{
		DirFS:   badFS,
		Default: "en",
	})
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestCtx_Locale_ResolvesAcceptLanguage(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.Locale())
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	// Full Accept-Language header with quality values — Locale() should
	// return the resolved tag ("tr"), not the raw header.
	r.Header.Set("Accept-Language", "tr-TR,tr;q=0.9,en;q=0.8")
	app.ServeHTTP(w, r)

	if w.Body.String() != "tr" {
		t.Errorf("Locale() = %q, want resolved %q", w.Body.String(), "tr")
	}
}

func TestCtx_Locale(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.Locale())
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)

	if w.Body.String() != "tr" {
		t.Errorf("Locale() = %q, want %q", w.Body.String(), "tr")
	}
}

func TestCtx_T(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.T("v.required"))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)

	if w.Body.String() != "zorunludur" {
		t.Errorf("T() = %q, want %q", w.Body.String(), "zorunludur")
	}
}

func TestCtx_T_NoI18n(t *testing.T) {
	app := mustNew(t)
	// No UseI18n call

	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.T("v.required"))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Body.String() != "v.required" {
		t.Errorf("T() = %q, want %q (key passthrough)", w.Body.String(), "v.required")
	}
}

func TestHandleError_HTTPStatusProvider_I18n(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	app.GET("/test", func(ctx *credo.Context) error {
		return &httpStatusErr{msg: "store: not found", status: 404}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Title != "Bulunamadı" {
		t.Errorf("title = %q, want %q", pd.Title, "Bulunamadı")
	}
}

func TestTranslateError_Immutability(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	original := validation.Errors{
		{Field: "email", Code: "required", Message: "is required"},
	}

	app.POST("/test", func(ctx *credo.Context) error {
		return original
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)

	// Original should be unchanged.
	if original[0].Message != "is required" {
		t.Errorf("original mutated: Message = %q, want %q", original[0].Message, "is required")
	}
}

func TestUseI18n_NoArgs_Defaults(t *testing.T) {
	app := mustNew(t)
	// No args — should use defaults (dir="locales/", default="en")
	// Since locales/ doesn't exist in the test CWD, this should be inactive.
	err := app.UseI18n()
	if err != nil {
		t.Fatalf("UseI18n: %v", err)
	}
}

func TestUseI18n_ZeroConfig_Defaults(t *testing.T) {
	app := mustNew(t)
	// Zero I18nConfig — should use the same defaults as the no-arg call.
	// Since locales/ doesn't exist in the test CWD, this should be inactive.
	err := app.UseI18n(credo.I18nConfig{})
	if err != nil {
		t.Fatalf("UseI18n: %v", err)
	}
}

// badI18nRC is a mock RawConfig where "i18n" key exists but Unmarshal fails.
type badI18nRC struct{}

func (b *badI18nRC) Exists(key string) bool { return key == "i18n" }
func (b *badI18nRC) Unmarshal(key string, dst any) error {
	if key == "i18n" {
		return fmt.Errorf("forced decode error")
	}
	return fmt.Errorf("key %q not found", key)
}

func TestUseI18n_InvalidRawConfig_Error(t *testing.T) {
	app, err := credo.New(credo.WithRawConfig(&badI18nRC{}))
	if err != nil {
		t.Fatal(err)
	}

	err = app.UseI18n()
	if err == nil {
		t.Error("expected error for invalid i18n config in RawConfig")
	}
}

func TestUseI18n_LogsOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "i18n loaded") {
		t.Errorf("expected 'i18n loaded' log, got: %q", buf.String())
	}
}

func TestUseI18n_LogsWhenInactive(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	if err := app.UseI18n(credo.I18nConfig{
		Dir:     "nonexistent_locales/",
		Default: "en",
	}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "i18n inactive") {
		t.Errorf("expected 'i18n inactive' log, got: %q", buf.String())
	}
}

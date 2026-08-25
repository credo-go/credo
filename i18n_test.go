package credo_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
				"required": "is required",
				"email": "must be a valid email",
				"not_found": "Not found",
				"internal_server_error": "Internal server error",
				"items": {"one": "{{.count}} item", "other": "{{.count}} items"}
			}`),
		},
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{
				"required": "zorunludur",
				"email": "geçerli bir e-posta adresi olmalıdır",
				"not_found": "Bulunamadı",
				"internal_server_error": "Sunucu hatası",
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

	var pd credo.ErrorResponse
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

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Message != "Bulunamadı" {
		t.Errorf("message = %q, want %q", pd.Message, "Bulunamadı")
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

	var pd credo.ErrorResponse
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

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Errors[0].Message != "zorunludur" {
		t.Errorf("message = %q, want %q", pd.Errors[0].Message, "zorunludur")
	}
}

func TestUseI18n_ExplicitMissingDirErrors(t *testing.T) {
	app := mustNew(t)
	err := app.UseI18n(credo.I18nConfig{
		Dir:     "nonexistent_locales/",
		Default: "en",
	})
	if err == nil {
		t.Fatal("expected an explicit missing locale directory to fail")
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
		return ctx.Response().Text(200, ctx.T("required"))
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
		return &httpStatusError{msg: "store: not found", status: 404}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Message != "Bulunamadı" {
		t.Errorf("message = %q, want %q", pd.Message, "Bulunamadı")
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

func TestUseI18n_ConventionalDirectoryExistsButIsInvalid(t *testing.T) {
	root := t.TempDir()
	localeDir := filepath.Join(root, "locales", "en")
	if err := os.MkdirAll(localeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localeDir, "fields.json"), []byte(`{"email":"email address"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	app := mustNew(t)
	if err := app.UseI18n(); err == nil {
		t.Fatal("an existing malformed conventional catalog must fail, not look absent")
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

type missingDirI18nRC struct{}

func (*missingDirI18nRC) Exists(key string) bool { return key == "i18n" }
func (*missingDirI18nRC) Unmarshal(key string, dst any) error {
	if key != "i18n" {
		return fmt.Errorf("key %q not found", key)
	}
	return json.Unmarshal([]byte(`{"Dir":"missing-from-raw-config","Default":"en"}`), dst)
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

func TestUseI18n_ExplicitRawConfigDirErrors(t *testing.T) {
	app, err := credo.New(credo.WithRawConfig(&missingDirI18nRC{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.UseI18n(); err == nil {
		t.Fatal("expected missing RawConfig i18n.dir to fail")
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

	if err := app.UseI18n(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "i18n inactive") {
		t.Errorf("expected 'i18n inactive' log, got: %q", buf.String())
	}
}

func TestUseI18n_ProgrammaticMessagesAndFields(t *testing.T) {
	messages := credo.I18nMessages{
		"validation_failed": "Validation failed",
		"required":          "{{.field}} is required",
		"hello":             "Hello {{.name}}",
	}
	fields := credo.I18nFields{"email": "email address"}
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		Default:  "en",
		Messages: messages,
		Fields:   fields,
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}

	// Setup owns snapshots, not the caller's mutable maps.
	messages["hello"] = "mutated"
	fields["email"] = "mutated"
	app.GET("/hello", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.T("hello", map[string]any{"name": "Ada"}))
	})
	app.POST("/validate", func(*credo.Context) error {
		return validation.Errors{{Field: "email", Code: "required", Message: "fallback"}}
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hello", nil))
	if got := w.Body.String(); got != "Hello Ada" {
		t.Fatalf("message = %q, want Hello Ada", got)
	}

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/validate", nil))
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Field != "email" || body.Errors[0].Message != "email address is required" {
		t.Fatalf("validation errors = %#v", body.Errors)
	}
}

func TestUseI18n_ProgrammaticAndFileCatalogLayering(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		Default: "en",
		Messages: credo.I18nMessages{
			"shared":   "programmatic",
			"map_only": "preserved",
		},
		Fields: credo.I18nFields{
			"shared":   "programmatic field",
			"map_only": "preserved field",
		},
		DirFS: fstest.MapFS{
			"en/messages.json": &fstest.MapFile{Data: []byte(`{"shared":"file"}`)},
			"en/fields.json":   &fstest.MapFile{Data: []byte(`{"shared":"file field"}`)},
		},
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}
	app.GET("/values", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, map[string]string{
			"shared":   ctx.T("shared"),
			"map_only": ctx.T("map_only"),
		})
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/values", nil))
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["shared"] != "file" || got["map_only"] != "preserved" {
		t.Fatalf("messages = %#v", got)
	}
}

func TestUseI18n_ProgrammaticSourceValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  credo.I18nConfig
	}{
		{name: "fields only", cfg: credo.I18nConfig{Fields: credo.I18nFields{"email": "email"}}},
		{name: "dir and dirfs", cfg: credo.I18nConfig{Dir: "locales", DirFS: fstest.MapFS{}}},
		{name: "empty explicit fs", cfg: credo.I18nConfig{DirFS: fstest.MapFS{}}},
		{name: "missing explicit dir with messages", cfg: credo.I18nConfig{
			Dir: "missing", Messages: credo.I18nMessages{"safe": "Safe"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			if err := app.UseI18n(tt.cfg); err == nil {
				t.Fatal("expected setup error")
			}
		})
	}
}

func TestUseI18n_MessageKeyResolverScopesAndExplicitKeys(t *testing.T) {
	var refs []credo.MessageRef
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		Default: "en",
		Messages: credo.I18nMessages{
			"problem.not_found":         "Missing",
			"problem.validation_failed": "Invalid request",
			"problem.bind_failed":       "Malformed request",
			"validation.required":       "{{.field}} required",
			"explicit.validation":       "Explicit {{.field}}",
			"request.syntax":            "Malformed JSON",
		},
		ResolveMessageKey: func(ref credo.MessageRef) string {
			refs = append(refs, ref)
			switch ref.Scope {
			case credo.MessageScopeValidation:
				return "validation." + ref.Code
			case credo.MessageScopeBind:
				return "request." + ref.Code
			default:
				return "problem." + ref.Code
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	app.GET("/missing", func(*credo.Context) error { return credo.ErrNotFound })
	app.POST("/validation", func(*credo.Context) error {
		return validation.Errors{
			{Field: "a", Code: "required", Message: "fallback"},
			{Field: "b", Code: "required", MessageKey: "explicit.validation", Message: "fallback"},
		}
	})
	app.POST("/bind", func(*credo.Context) error {
		return &credo.BindError{Reason: credo.BindReasonSyntax}
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Message != "Missing" {
		t.Errorf("error message = %q, want Missing", body.Message)
	}

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/validation", nil))
	body = credo.ErrorResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Message != "Invalid request" || body.Errors[0].Message != "a required" || body.Errors[1].Message != "Explicit b" {
		t.Fatalf("resolved body = %#v", body)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/bind", nil))
	body = credo.ErrorResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "bind_failed" || body.Message != "Malformed request" || body.Errors[0].Code != "syntax" || body.Errors[0].Message != "Malformed JSON" {
		t.Fatalf("bind body = %#v", body)
	}

	if len(refs) != 5 {
		t.Fatalf("resolver refs = %#v; explicit nested key must bypass resolver", refs)
	}
	if refs[0].Scope != credo.MessageScopeError || refs[1].Scope != credo.MessageScopeError || refs[2].Scope != credo.MessageScopeValidation || refs[3].Scope != credo.MessageScopeError || refs[4].Scope != credo.MessageScopeBind {
		t.Fatalf("resolver scopes = %#v", refs)
	}
}

func TestUseI18n_EmptyResolvedMessageKeyFailsClosed(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		Messages: credo.I18nMessages{"not_found": "Missing"},
		ResolveMessageKey: func(credo.MessageRef) string {
			return ""
		},
	}); err != nil {
		t.Fatal(err)
	}
	app.GET("/missing", func(*credo.Context) error { return credo.ErrNotFound })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "internal_server_error" || body.Message != "Internal Server Error" || body.Success {
		t.Fatalf("body = %#v", body)
	}
}

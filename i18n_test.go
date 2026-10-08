package credo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.GET("/items", func(ctx *credo.Context) error {
		parts := []string{
			ctx.TPlural("items", 1),
			ctx.TPlural("items", 5),
			ctx.TPlural("nonexistent", 1),
		}
		return ctx.Response().Text(200, strings.Join(parts, "|"))
	})

	startServing(t, app)
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
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.POST("/test", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "email", Code: "required", Message: "is required"},
		}
	})

	startServing(t, app)
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
	if len(pd.Error.Violations) != 1 {
		t.Fatalf("errors count = %d, want 1", len(pd.Error.Violations))
	}
	if pd.Error.Violations[0].Message != "zorunludur" {
		t.Errorf("translated message = %q, want %q", pd.Error.Violations[0].Message, "zorunludur")
	}
	if pd.Error.Violations[0].Field != "email" {
		t.Errorf("field = %q, want %q", pd.Error.Violations[0].Field, "email")
	}
}

func TestUseI18n_HTTPError_Turkish(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.GET("/missing", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	startServing(t, app)
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
	if pd.Error.Message != "Bulunamadı" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "Bulunamadı")
	}
}

func TestUseI18n_EnglishDefault(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.POST("/test", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "email", Code: "required", Message: "is required"},
		}
	})

	startServing(t, app)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)
	r.Header.Set("Accept-Language", "en")
	app.ServeHTTP(w, r)

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(pd.Error.Violations) != 1 {
		t.Fatalf("errors count = %d, want 1", len(pd.Error.Violations))
	}
	if pd.Error.Violations[0].Message != "is required" {
		t.Errorf("message = %q, want %q", pd.Error.Violations[0].Message, "is required")
	}
}

func TestUseI18n_CustomDetect(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
		Detect: func(ctx *credo.Context) string {
			return ctx.Request().URL.Query().Get("lang")
		},
	})

	app.POST("/test", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "email", Code: "required", Message: "is required"},
		}
	})

	startServing(t, app)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test?lang=tr", nil)
	app.ServeHTTP(w, r)

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Error.Violations[0].Message != "zorunludur" {
		t.Errorf("message = %q, want %q", pd.Error.Violations[0].Message, "zorunludur")
	}
}

func TestUseI18n_ExplicitMissingDirErrors(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		Dir:     "nonexistent_locales/",
		Default: "en",
	})
	if err := startErr(t, app); err == nil || !strings.Contains(err.Error(), "i18n") {
		t.Fatalf("Start() = %v, want an explicit missing locale directory to fail the start", err)
	}
}

func TestUseI18n_MalformedTemplate_Error(t *testing.T) {
	badFS := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "{{.field is required"}`),
		},
	}

	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		DirFS:   badFS,
		Default: "en",
	})
	if err := startErr(t, app); err == nil {
		t.Error("expected a malformed template to fail the start")
	}
}

func TestUseI18n_MalformedJSON_Error(t *testing.T) {
	badFS := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{bad json`),
		},
	}

	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		DirFS:   badFS,
		Default: "en",
	})
	if err := startErr(t, app); err == nil {
		t.Error("expected malformed JSON to fail the start")
	}
}

func TestCtx_Locale_ResolvesAcceptLanguage(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.Locale())
	})

	startServing(t, app)
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
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.Locale())
	})

	startServing(t, app)
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
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.T("required"))
	})

	startServing(t, app)
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
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return &httpStatusError{msg: "store: not found", status: 404}
	})

	startServing(t, app)
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
	if pd.Error.Message != "Bulunamadı" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "Bulunamadı")
	}
}

func TestTranslateError_Immutability(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})

	original := validation.Errors{
		{Field: "email", Code: "required", Message: "is required"},
	}

	app.POST("/test", func(ctx *credo.Context) error {
		return original
	})

	startServing(t, app)
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
	app.UseI18n()
	startServing(t, app)
}

func TestUseI18n_ZeroConfig_Defaults(t *testing.T) {
	app := mustNew(t)
	// Zero I18nConfig — should use the same defaults as the no-arg call.
	// Since locales/ doesn't exist in the test CWD, this should be inactive.
	app.UseI18n(credo.I18nConfig{})
	startServing(t, app)
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
	app.UseI18n()
	if err := startErr(t, app); err == nil {
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

	expectPanicContaining(t, "invalid i18n config", func() { app.UseI18n() })
}

func TestUseI18n_ExplicitRawConfigDirErrors(t *testing.T) {
	app, err := credo.New(credo.WithRawConfig(&missingDirI18nRC{}))
	if err != nil {
		t.Fatal(err)
	}
	app.UseI18n()
	if err := startErr(t, app); err == nil {
		t.Fatal("expected missing RawConfig i18n.dir to fail the start")
	}
}

func TestUseI18n_LogsOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
	})
	if strings.Contains(buf.String(), "i18n loaded") {
		t.Fatal("UseI18n read the catalogs at registration")
	}
	startServing(t, app)

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

	app.UseI18n()
	startServing(t, app)

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
	app.UseI18n(credo.I18nConfig{
		Default:  "en",
		Messages: messages,
		Fields:   fields,
	})

	// Setup owns snapshots, not the caller's mutable maps.
	messages["hello"] = "mutated"
	fields["email"] = "mutated"
	app.GET("/hello", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.T("hello", map[string]any{"name": "Ada"}))
	})
	app.POST("/validate", func(*credo.Context) error {
		return validation.Errors{{Field: "email", Code: "required", Message: "fallback"}}
	})

	startServing(t, app)
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
	if len(body.Error.Violations) != 1 || body.Error.Violations[0].Field != "email" || body.Error.Violations[0].Message != "email address is required" {
		t.Fatalf("validation errors = %#v", body.Error.Violations)
	}
}

func TestUseI18n_ProgrammaticAndFileCatalogLayering(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
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
	})
	app.GET("/values", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, map[string]string{
			"shared":   ctx.T("shared"),
			"map_only": ctx.T("map_only"),
		})
	})

	startServing(t, app)
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

func TestUseI18n_Misuse(t *testing.T) {
	tests := []struct {
		name string
		cfgs []credo.I18nConfig
		want string
	}{
		{name: "fields only", cfgs: []credo.I18nConfig{{Fields: credo.I18nFields{"email": "email"}}},
			want: "Fields require at least one message"},
		{name: "dir and dirfs", cfgs: []credo.I18nConfig{{Dir: "locales", DirFS: fstest.MapFS{}}},
			want: "mutually exclusive"},
		{name: "two configs", cfgs: []credo.I18nConfig{{}, {}}, want: "at most one config"},
		{name: "invalid default", cfgs: []credo.I18nConfig{{Default: "not a tag!"}}, want: "App.UseI18n"},
		{name: "malformed programmatic message", cfgs: []credo.I18nConfig{{
			Messages: credo.I18nMessages{"broken": "{{.field"},
		}}, want: "App.UseI18n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			expectPanicContaining(t, tt.want, func() { app.UseI18n(tt.cfgs...) })
		})
	}
}

func TestUseI18n_ExplicitSourceFailsTheStart(t *testing.T) {
	tests := []struct {
		name string
		cfg  credo.I18nConfig
	}{
		{name: "empty explicit fs", cfg: credo.I18nConfig{DirFS: fstest.MapFS{}}},
		{name: "missing explicit dir with messages", cfg: credo.I18nConfig{
			Dir: "missing", Messages: credo.I18nMessages{"safe": "Safe"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.UseI18n(tt.cfg)
			err := startErr(t, app)
			if err == nil {
				t.Fatal("expected the start to fail")
			}
			if _, ok := errors.AsType[*credo.LifecycleError](err); !ok {
				t.Fatalf("Start() = %T, want *credo.LifecycleError", err)
			}
		})
	}
}

func TestUseI18n_MessageKeyResolverScopesAndExplicitKeys(t *testing.T) {
	var refs []credo.MessageRef
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{
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
	})
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

	startServing(t, app)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != "Missing" {
		t.Errorf("error message = %q, want Missing", body.Error.Message)
	}

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/validation", nil))
	body = credo.ErrorResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != "Invalid request" || body.Error.Violations[0].Message != "a required" || body.Error.Violations[1].Message != "Explicit b" {
		t.Fatalf("resolved body = %#v", body)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/bind", nil))
	body = credo.ErrorResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "bind_failed" || body.Error.Message != "Malformed request" || body.Error.Violations[0].Code != "syntax" || body.Error.Violations[0].Message != "Malformed JSON" {
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
	app.UseI18n(credo.I18nConfig{
		Messages: credo.I18nMessages{"not_found": "Missing"},
		ResolveMessageKey: func(credo.MessageRef) string {
			return ""
		},
	})
	app.GET("/missing", func(*credo.Context) error { return credo.ErrNotFound })

	startServing(t, app)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "internal_server_error" || body.Error.Message != "Internal Server Error" || body.Success {
		t.Fatalf("body = %#v", body)
	}
}

// startErr runs the App's start phase and returns its error; the App is shut
// down when the test ends.
func startErr(t *testing.T, app *credo.App) error {
	t.Helper()
	t.Cleanup(func() { _ = app.Shutdown(context.WithoutCancel(t.Context())) })
	return app.Start(t.Context())
}

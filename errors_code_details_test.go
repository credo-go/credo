package credo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

var errAny = errors.New("wrapped application failure")

func problemFromResponse(t *testing.T, w *httptest.ResponseRecorder) credo.ProblemDetails {
	t.Helper()
	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal problem body %q: %v", w.Body.String(), err)
	}
	return pd
}

func TestHTTPError_WithMessageKeyAndDetails_CopyOnWrite(t *testing.T) {
	base := credo.NewHTTPError(http.StatusConflict, "email_exists")

	derived := base.WithMessageKey("user.email_exists").WithDetails(map[string]string{"field": "email"})

	if base.MessageKey != "" || base.Details != nil {
		t.Errorf("base mutated: MessageKey=%q Details=%v, want zero values", base.MessageKey, base.Details)
	}
	if derived.MessageKey != "user.email_exists" {
		t.Errorf("derived.MessageKey = %q, want %q", derived.MessageKey, "user.email_exists")
	}
	if derived.Status != http.StatusConflict || derived.Code != "email_exists" {
		t.Errorf("derived lost base fields: Status=%d Code=%q", derived.Status, derived.Code)
	}
	got := derived.Error()
	if !strings.Contains(got, "code=email_exists") || !strings.Contains(got, "key=user.email_exists") {
		t.Errorf("Error() = %q, want it to mention code=email_exists and key=user.email_exists", got)
	}
}

func TestHandleError_ExplicitCodeAndDetails(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "dup_email").
			WithMessageKey("user.email_exists").
			WithDetails(map[string]string{"field": "email"})
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	pd := problemFromResponse(t, w)
	if pd.Code != "dup_email" {
		t.Errorf("code = %q, want %q (explicit code wins over the frozen default)", pd.Code, "dup_email")
	}
	details, ok := pd.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %#v, want a JSON object", pd.Details)
	}
	if details["field"] != "email" {
		t.Errorf("details.field = %v, want %q", details["field"], "email")
	}
}

func TestHandleError_DefaultCodeFromFrozenTable(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if pd := problemFromResponse(t, w); pd.Code != "conflict" {
		t.Errorf("code = %q, want %q (frozen default for 409)", pd.Code, "conflict")
	}
}

func TestHandleError_UnknownStatusCodeFallback(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(499)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if w.Code != 499 {
		t.Fatalf("status = %d, want 499", w.Code)
	}
	pd := problemFromResponse(t, w)
	if pd.Code != "http_499" {
		t.Errorf("code = %q, want %q (stable fallback for an unknown status)", pd.Code, "http_499")
	}
	if pd.Title != "HTTP 499" {
		t.Errorf("title = %q, want %q (no StatusText for 499)", pd.Title, "HTTP 499")
	}
}

func TestHandleError_LiteralMessageKeepsDefaultCode(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusBadRequest).
			WithMessageKey("just a literal message")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	pd := problemFromResponse(t, w)
	if pd.Code != "bad_request" {
		t.Errorf("code = %q, want %q (a literal message key never affects the code)", pd.Code, "bad_request")
	}
	if pd.Title != "just a literal message" {
		t.Errorf("title = %q, want the literal message", pd.Title)
	}
}

func TestHandleError_BuiltinSentinelCarriesStoredCode(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if pd := problemFromResponse(t, w); pd.Code != "not_found" {
		t.Errorf("code = %q, want %q (materialized at construction)", pd.Code, "not_found")
	}
}

// TestHTTPError_SentinelStoredFields locks the in-memory sentinel contract:
// Code materialized at construction, MessageKey empty. Direct readers and
// marshalers of HTTPError observe these stored fields, not the effective
// classification key.
func TestHTTPError_SentinelStoredFields(t *testing.T) {
	tests := []struct {
		name   string
		err    *credo.HTTPError
		status int
		code   string
	}{
		{name: "ErrNotFound", err: credo.ErrNotFound, status: 404, code: "not_found"},
		{name: "ErrMethodNotAllowed", err: credo.ErrMethodNotAllowed, status: 405, code: "method_not_allowed"},
		{name: "ErrBadRequest", err: credo.ErrBadRequest, status: 400, code: "bad_request"},
		{name: "ErrUnauthorized", err: credo.ErrUnauthorized, status: 401, code: "unauthorized"},
		{name: "ErrForbidden", err: credo.ErrForbidden, status: 403, code: "forbidden"},
		{name: "ErrInternalServerError", err: credo.ErrInternalServerError, status: 500, code: "internal_server_error"},
		{name: "ErrConflict", err: credo.ErrConflict, status: 409, code: "conflict"},
		{name: "ErrUnprocessableEntity", err: credo.ErrUnprocessableEntity, status: 422, code: "unprocessable_entity"},
		{name: "ErrUnsupportedMediaType", err: credo.ErrUnsupportedMediaType, status: 415, code: "unsupported_media_type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Status != tt.status {
				t.Errorf("Status = %d, want %d", tt.err.Status, tt.status)
			}
			if tt.err.Code != tt.code {
				t.Errorf("Code = %q, want %q", tt.err.Code, tt.code)
			}
			if tt.err.MessageKey != "" {
				t.Errorf("MessageKey = %q, want empty (presentation keys are opt-in)", tt.err.MessageKey)
			}
		})
	}
}

// TestHTTPError_DirectMarshal locks the direct-marshal shape of HTTPError:
// stored fields only, empty optional members omitted, no effective-key
// synthesis.
func TestHTTPError_DirectMarshal(t *testing.T) {
	raw, err := json.Marshal(credo.ErrNotFound)
	if err != nil {
		t.Fatalf("marshal sentinel: %v", err)
	}
	want := `{"status":404,"code":"not_found"}`
	if string(raw) != want {
		t.Errorf("marshal = %s, want %s", raw, want)
	}
}

func TestHandleError_ValidationErrorsTopLevelCode(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return validation.Errors{{Field: "name", Code: "required", Message: "cannot be blank"}}
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if pd := problemFromResponse(t, w); pd.Code != "validation_failed" {
		t.Errorf("code = %q, want %q", pd.Code, "validation_failed")
	}
}

func TestHandleError_BindErrorTopLevelCodeIsReason(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	app := mustNew(t, credo.WithoutAccessLog())
	app.POST("/test", func(ctx *credo.Context) error {
		var p payload
		if err := ctx.Request().BindBody(&p); err != nil {
			return err
		}
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("{invalid"))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	pd := problemFromResponse(t, w)
	if pd.Code != "syntax" {
		t.Errorf("top-level code = %q, want %q (the bind reason)", pd.Code, "syntax")
	}
	if len(pd.Errors) != 1 || pd.Errors[0].Code != "syntax" {
		t.Errorf("errors[] = %+v, want a single entry with code %q", pd.Errors, "syntax")
	}
}

// TestHandleError_RendererProjectsAlternateCodeCasing locks the documented
// escape for organizations that standardize on a different code casing: the
// ErrorRenderer mutates info.Problem.Code and returns nil, and the framework
// renders the projected code because info.Problem is read after the renderer
// returns.
func TestHandleError_RendererProjectsAlternateCodeCasing(t *testing.T) {
	app := mustNew(t)
	app.SetErrorRenderer(func(_ *credo.Context, info credo.ErrorInfo) any {
		info.Problem.Code = strings.ToUpper(info.Problem.Code)
		return nil
	})
	app.GET("/test", func(ctx *credo.Context) error { return credo.ErrConflict })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if pd := problemFromResponse(t, w); pd.Code != "CONFLICT" {
		t.Errorf("code = %q, want %q (renderer projection)", pd.Code, "CONFLICT")
	}
}

// TestErrorWire_Snapshots locks the exact wire bytes of representative
// default-pipeline responses. The JSON profile is deterministic, so any drift
// in these bodies is a deliberate wire decision.
func TestErrorWire_Snapshots(t *testing.T) {
	tests := []struct {
		name    string
		handler credo.Handler
		status  int
		body    string
	}{
		{
			name:    "sentinel not found",
			handler: func(*credo.Context) error { return credo.ErrNotFound },
			status:  404,
			body:    `{"type":"about:blank","title":"Not Found","status":404,"instance":"/snap","code":"not_found"}`,
		},
		{
			name:    "413 gains its frozen code",
			handler: func(*credo.Context) error { return credo.NewHTTPError(413) },
			status:  413,
			body:    `{"type":"about:blank","title":"Request Entity Too Large","status":413,"instance":"/snap","code":"request_entity_too_large"}`,
		},
		{
			name:    "418 teapot",
			handler: func(*credo.Context) error { return credo.NewHTTPError(418) },
			status:  418,
			body:    `{"type":"about:blank","title":"I'm a teapot","status":418,"instance":"/snap","code":"im_a_teapot"}`,
		},
		{
			name:    "unknown 499",
			handler: func(*credo.Context) error { return credo.NewHTTPError(499) },
			status:  499,
			body:    `{"type":"about:blank","title":"HTTP 499","status":499,"instance":"/snap","code":"http_499"}`,
		},
		{
			name:    "generic 500",
			handler: func(*credo.Context) error { return errAny },
			status:  500,
			body:    `{"type":"about:blank","title":"Internal Server Error","status":500,"instance":"/snap","code":"internal_server_error"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/snap", tt.handler)

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest("GET", "/snap", nil))

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if w.Body.String() != tt.body {
				t.Errorf("body = %s\nwant  %s", w.Body.String(), tt.body)
			}
		})
	}
}

// TestNewHTTPError_PanicsOnMisuse locks the strict constructor validation:
// developer invariant violations fail loudly at the call site.
func TestNewHTTPError_PanicsOnMisuse(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{name: "status below domain", call: func() { credo.NewHTTPError(99) }},
		{name: "status above domain", call: func() { credo.NewHTTPError(1000) }},
		{name: "zero status", call: func() { credo.NewHTTPError(0) }},
		{name: "empty code", call: func() { credo.NewHTTPError(400, "") }},
		{name: "dotted key as code", call: func() { credo.NewHTTPError(404, "user.not_found") }},
		{name: "literal message as code", call: func() { credo.NewHTTPError(400, "just a literal message") }},
		{name: "uppercase code", call: func() { credo.NewHTTPError(409, "EMAIL_EXISTS") }},
		{name: "two code arguments", call: func() { credo.NewHTTPError(409, "email_exists", "extra") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewHTTPError did not panic")
				}
			}()
			tt.call()
		})
	}
}

// TestHandleError_ConstructorPanicFailsClosed proves that a request-time
// constructor panic is caught by built-in recovery and rendered as a generic
// 500 without publishing the invalid value.
func TestHandleError_ConstructorPanicFailsClosed(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "E-posta zaten kayıtlı")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	pd := problemFromResponse(t, w)
	if pd.Code != "internal_server_error" {
		t.Errorf("code = %q, want %q", pd.Code, "internal_server_error")
	}
	if strings.Contains(w.Body.String(), "E-posta") {
		t.Errorf("body = %s, want no trace of the invalid value", w.Body.String())
	}
}

// TestHandleError_MalformedDirectStructFailsClosed proves the classification
// boundary: a directly constructed HTTPError with invalid fields renders as a
// generic 500 and publishes none of the invalid value's client-facing fields.
func TestHandleError_MalformedDirectStructFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		err  *credo.HTTPError
	}{
		{name: "malformed code", err: &credo.HTTPError{Status: http.StatusConflict, Code: "Not A Code", MessageKey: "leaky title"}},
		{name: "invalid status", err: &credo.HTTPError{Status: 42, Code: "conflict"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/test", func(ctx *credo.Context) error { return tt.err })

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
			pd := problemFromResponse(t, w)
			if pd.Code != "internal_server_error" {
				t.Errorf("code = %q, want %q", pd.Code, "internal_server_error")
			}
			if strings.Contains(w.Body.String(), "leaky") || strings.Contains(w.Body.String(), "Not A Code") {
				t.Errorf("body = %s, want no invalid client-facing fields", w.Body.String())
			}
		})
	}
}

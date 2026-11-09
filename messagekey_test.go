package credo_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

// TestMessageKey_IsANamedType pins the message key's type: every API that
// takes or carries a key uses credo.MessageKey, a named string type, so a key
// computed as a string does not compile without a conversion, and the
// validation package's key is the same type without one.
func TestMessageKey_IsANamedType(t *testing.T) {
	key := reflect.TypeFor[credo.MessageKey]()
	if key == reflect.TypeFor[string]() || key.Kind() != reflect.String {
		t.Fatalf("credo.MessageKey = %v, want a named string type", key)
	}
	if key != reflect.TypeFor[validation.MessageKey]() {
		t.Fatalf("credo.MessageKey = %v, want the alias of validation.MessageKey", key)
	}
	if got := reflect.TypeOf((*credo.HTTPError).WithMessageKey).In(1); got != key {
		t.Errorf("WithMessageKey takes %v, want %v", got, key)
	}
	if got := reflect.TypeOf(credo.RenderMessageKey).In(0); got != key {
		t.Errorf("RenderMessageKey takes %v, want %v", got, key)
	}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[credo.HTTPError](),
		reflect.TypeFor[credo.ErrorInfo](),
		reflect.TypeFor[credo.RenderInfo](),
		reflect.TypeFor[validation.ValidationError](),
	} {
		f, ok := typ.FieldByName("MessageKey")
		if !ok || f.Type != key {
			t.Errorf("%v.MessageKey = %v, want %v", typ, f.Type, key)
		}
	}
	// A validation key is a credo.MessageKey without a conversion.
	var k credo.MessageKey = validation.ValidationError{MessageKey: "v.required"}.MessageKey
	if k != "v.required" {
		t.Errorf("key = %q", k)
	}
}

// TestCatalogMiss_DebugWarns: a key the catalog does not define renders the
// code's built-in message, and in debug mode the pipeline warns once per
// error with the key, the code and the route.
func TestCatalogMiss_DebugWarns(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger), credo.WithDebug())
	app.UseI18n(credo.I18nConfig{Default: "en", Messages: credo.I18nMessages{"user.taken": "Taken"}})
	app.GET("/miss", func(*credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "email_exists").WithMessageKey("Email already exists")
	})
	app.GET("/hit", func(*credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict).WithMessageKey("user.taken")
	})
	startServing(t, app)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/miss", nil))
	if body := problemFromResponse(t, w); body.Message != "Conflict" || body.Code != "email_exists" {
		t.Fatalf("body = %+v, want the status text and the code, never the key", body)
	}
	warn := logLine(t, buf, "credo: message key is not in the catalog; the built-in message for the code is rendered")
	if warn["level"] != "WARN" || warn["key"] != "Email already exists" || warn["code"] != "email_exists" || warn["route"] != "GET /miss" {
		t.Fatalf("warning = %v", warn)
	}

	buf.Reset()
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hit", nil))
	if body := problemFromResponse(t, w); body.Message != "Taken" {
		t.Fatalf("message = %q, want the catalog's", body.Message)
	}
	if strings.Contains(buf.String(), "not in the catalog") {
		t.Fatalf("a key the catalog defines warned: %s", buf.String())
	}
}

func TestCatalogMiss_NoWarningOutsideDebug(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.GET("/miss", func(*credo.Context) error {
		return credo.NewHTTPError(http.StatusBadRequest, credo.CodeBindFailed).WithMessageKey("app.missing")
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/miss", nil))
	if body := problemFromResponse(t, w); body.Message != "Malformed Request" {
		t.Fatalf("message = %q, want the code's built-in message", body.Message)
	}
	if strings.Contains(buf.String(), "not in the catalog") {
		t.Fatalf("warned outside debug mode: %s", buf.String())
	}
}

// TestWithMessage_LiteralTextWinsOverKey: literal text renders as is, is
// never looked up, wins over a key, and leaves ErrorInfo.MessageKey empty.
func TestWithMessage_LiteralTextWinsOverKey(t *testing.T) {
	app := mustNew(t)
	app.UseI18n(credo.I18nConfig{Default: "en", Messages: credo.I18nMessages{"user.taken": "Taken"}})
	var info credo.ErrorInfo
	app.UseErrorRenderer(func(_ *credo.Context, i *credo.ErrorInfo) any {
		info = *i
		return nil
	})
	app.GET("/x", func(*credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict).WithMessageKey("user.taken").WithMessage("user.taken")
	})
	startServing(t, app)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if body := problemFromResponse(t, w); body.Message != "user.taken" || body.Code != "conflict" {
		t.Fatalf("body = %+v, want the literal text unchanged and the status's code", body)
	}
	if info.MessageKey != "" || info.Message != "user.taken" {
		t.Fatalf("info = %+v, want no key and the literal message", info)
	}
	if got := credo.NewHTTPError(http.StatusConflict).WithMessage("say \"hi\"").Error(); got != `status=409, code=conflict, message="say \"hi\""` {
		t.Errorf("Error() = %q", got)
	}
}

// TestNewHTTPError_StatusCodes: each code the removed MsgKey constants
// repeated is the code NewHTTPError derives from its status.
func TestNewHTTPError_StatusCodes(t *testing.T) {
	for status, code := range map[int]string{
		400: "bad_request", 401: "unauthorized", 403: "forbidden", 404: "not_found",
		405: "method_not_allowed", 409: "conflict", 422: "unprocessable_entity",
		415: "unsupported_media_type", 500: "internal_server_error", 429: "too_many_requests",
		503: "service_unavailable", 504: "gateway_timeout", 408: "request_timeout",
	} {
		if got := credo.NewHTTPError(status).Code; got != code {
			t.Errorf("NewHTTPError(%d).Code = %q, want %q", status, got, code)
		}
	}
	if credo.CodeValidationFailed != "validation_failed" || credo.CodeBindFailed != "bind_failed" {
		t.Errorf("CodeValidationFailed, CodeBindFailed = %q, %q", credo.CodeValidationFailed, credo.CodeBindFailed)
	}

	app := mustNew(t)
	app.POST("/v", func(*credo.Context) error {
		return validation.Errors{{Field: "a", Code: "required", Message: "a is required"}}
	})
	app.POST("/b", func(c *credo.Context) error {
		var v struct{ A int }
		return c.Request().BindBody(&v)
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v", nil))
	if body := problemFromResponse(t, w); body.Code != credo.CodeValidationFailed {
		t.Errorf("validation code = %q, want %q", body.Code, credo.CodeValidationFailed)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/b", strings.NewReader("{"))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	if body := problemFromResponse(t, w); body.Code != credo.CodeBindFailed {
		t.Errorf("bind code = %q, want %q", body.Code, credo.CodeBindFailed)
	}
}

// TestFrameworkKeys_ClientInputInDetails: the framework's rejections render
// their code's built-in message, and the client's value is in the details,
// never in the message.
func TestFrameworkKeys_ClientInputInDetails(t *testing.T) {
	bind := func(c *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		return c.Request().BindBody(&v)
	}
	cases := []struct {
		name       string
		decompress bool
		header     map[string]string
		body       string
		status     int
		code       string
		message    string
		details    map[string]any
	}{
		{"encoding without decompression", false, map[string]string{"Content-Type": "application/json", "Content-Encoding": "br"}, "x",
			415, credo.CodeUnsupportedContentEncoding, "Unsupported Content-Encoding.", map[string]any{"encoding": "br"}},
		{"encoding the decompressor refuses", true, map[string]string{"Content-Type": "application/json", "Content-Encoding": "br"}, "x",
			415, credo.CodeUnsupportedContentEncoding, "Unsupported Content-Encoding.", map[string]any{"encoding": "br"}},
		{"content type", false, map[string]string{"Content-Type": "text/evil<script>"}, "x",
			415, "unsupported_media_type", "Unsupported Media Type", map[string]any{"content_type": "text/evil<script>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t)
			if tc.decompress {
				app.UseDecompress()
			}
			app.POST("/b", bind)
			r := httptest.NewRequest(http.MethodPost, "/b", strings.NewReader(tc.body))
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tc.status, w.Body.String())
			}
			body := problemFromResponse(t, w)
			if body.Code != tc.code || body.Message != tc.message {
				t.Fatalf("body = %+v, want code %q and message %q", body, tc.code, tc.message)
			}
			if !reflect.DeepEqual(body.Details, tc.details) {
				t.Fatalf("details = %#v, want %#v", body.Details, tc.details)
			}
		})
	}
}

// TestFrameworkKeys_BodyLimits: the size limits render their status's
// message, with no key.
func TestFrameworkKeys_BodyLimits(t *testing.T) {
	bind := func(c *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		return c.Request().BindBody(&v)
	}
	big := `{"name":"` + strings.Repeat("x", 4096) + `"}`
	for _, decompress := range []bool{false, true} {
		app := mustNew(t, credo.WithMaxBodyBytes(64))
		body := []byte(big)
		if decompress {
			app.UseDecompress(credo.DecompressConfig{MaxBytes: 64})
			var gz bytes.Buffer
			zw := gzip.NewWriter(&gz)
			_, _ = zw.Write(body)
			_ = zw.Close()
			body = gz.Bytes()
		}
		app.POST("/b", bind)
		r := httptest.NewRequest(http.MethodPost, "/b", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if decompress {
			r.Header.Set("Content-Encoding", "gzip")
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("decompress=%v: status = %d, body %s", decompress, w.Code, w.Body.String())
		}
		if got := problemFromResponse(t, w); got.Code != "request_entity_too_large" || got.Message != "Request Entity Too Large" {
			t.Fatalf("decompress=%v: body = %+v", decompress, got)
		}
	}
}

// TestDeveloperErrors_LoggedAs500: an invalid redirect status and a bind
// field of an unsupported type are developer errors: a 500 whose reason is
// logged and never rendered.
func TestDeveloperErrors_LoggedAs500(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.GET("/redirect", func(c *credo.Context) error { return c.Response().Redirect(200, "/x") })
	app.GET("/bind", func(c *credo.Context) error {
		var q struct {
			T complex128 `query:"t"`
		}
		return c.Request().BindQuery(&q)
	})
	for path, reason := range map[string]string{
		"/redirect": "credo: Redirect: status 200 is not a redirect status (300..308)",
		"/bind?t=1": `credo: bind: field "t" has type kind complex128, which the binder does not support`,
	} {
		buf.Reset()
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "credo:") {
			t.Fatalf("%s: status %d, body %s", path, w.Code, w.Body.String())
		}
		line := logLine(t, buf, "credo: server error")
		if line["error"] != reason {
			t.Errorf("%s: logged error = %v, want %q", path, line["error"], reason)
		}
	}
}

// logLine returns the first JSON log line whose msg is msg.
func logLine(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			return m
		}
	}
	t.Fatalf("no log line %q in:\n%s", msg, buf.String())
	return nil
}

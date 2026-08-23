package credo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

func problemFromResponse(t *testing.T, w *httptest.ResponseRecorder) credo.ProblemDetails {
	t.Helper()
	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal problem body %q: %v", w.Body.String(), err)
	}
	return pd
}

func TestHTTPError_WithCodeAndDetails_CopyOnWrite(t *testing.T) {
	base := credo.NewHTTPError(http.StatusConflict, "user.email_exists")

	derived := base.WithCode("dup_email").WithDetails(map[string]string{"field": "email"})

	if base.Code != "" || base.Details != nil {
		t.Errorf("base mutated: Code=%q Details=%v, want zero values", base.Code, base.Details)
	}
	if derived.Code != "dup_email" {
		t.Errorf("derived.Code = %q, want %q", derived.Code, "dup_email")
	}
	if derived.Status != http.StatusConflict || derived.MessageKey != "user.email_exists" {
		t.Errorf("derived lost base fields: Status=%d MessageKey=%q", derived.Status, derived.MessageKey)
	}
	if got := derived.Error(); !strings.Contains(got, "code=dup_email") {
		t.Errorf("Error() = %q, want it to mention code=dup_email", got)
	}
}

func TestHandleError_ExplicitCodeAndDetails(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "user.email_exists").
			WithCode("dup_email").
			WithDetails(map[string]string{"field": "email"})
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	pd := problemFromResponse(t, w)
	if pd.Code != "dup_email" {
		t.Errorf("code = %q, want %q (explicit WithCode wins over derivation)", pd.Code, "dup_email")
	}
	details, ok := pd.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %#v, want a JSON object", pd.Details)
	}
	if details["field"] != "email" {
		t.Errorf("details.field = %v, want %q", details["field"], "email")
	}
}

func TestHandleError_CodeDerivedFromMessageKey(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "user.email_exists")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if pd := problemFromResponse(t, w); pd.Code != "email_exists" {
		t.Errorf("code = %q, want %q (last segment of the message key)", pd.Code, "email_exists")
	}
}

func TestHandleError_LiteralMessageYieldsNoCode(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusBadRequest, "just a literal message")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if body := w.Body.String(); strings.Contains(body, `"code"`) {
		t.Errorf("body = %s, want no code member for a dotless literal message", body)
	}
}

func TestHandleError_BuiltinSentinelCarriesDerivedCode(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if pd := problemFromResponse(t, w); pd.Code != "not_found" {
		t.Errorf("code = %q, want %q (derived from %q)", pd.Code, "not_found", credo.MsgKeyNotFound)
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

package credo_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/validation"
)

// ruleInput is a bind target whose single rule returns ruleErr.
type ruleInput struct {
	Code    string `json:"code"`
	ruleErr error
}

func (in *ruleInput) Validate() error {
	return validation.ValidateStruct(in,
		validation.Field(&in.Code, validation.By(func(string) error { return in.ruleErr })),
	)
}

// serveRuleError binds a body into a ruleInput whose rule returns ruleErr and
// returns the recorded response with the app's log.
func serveRuleError(t *testing.T, ruleErr error) (*httptest.ResponseRecorder, credo.ErrorResponse, string) {
	t.Helper()
	logger, logs := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.POST("/codes", func(ctx *credo.Context) error {
		in := &ruleInput{ruleErr: ruleErr}
		if err := ctx.Request().BindBody(in); err != nil {
			return err
		}
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/codes", strings.NewReader(`{"code":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return w, body, logs.String()
}

func TestRuleError_InternalErrorIsA500(t *testing.T) {
	w, body, logs := serveRuleError(t, errors.New("pq: connection refused"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if body.Success || body.Error.Code != "internal_server_error" || len(body.Error.Violations) != 0 {
		t.Fatalf("body = %+v, want the default 500 envelope", body)
	}
	if strings.Contains(w.Body.String(), "pq:") {
		t.Fatalf("the rule's internal text reached the client: %s", w.Body.String())
	}
	if !strings.Contains(logs, "pq: connection refused") {
		t.Fatalf("the rule's internal text was not logged: %s", logs)
	}
}

func TestRuleError_ValidationErrorIsA422(t *testing.T) {
	w, body, _ := serveRuleError(t, validation.NewError("country_code", "must be a 2-letter code"))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if body.Error.Code != "validation_failed" || len(body.Error.Violations) != 1 {
		t.Fatalf("body = %+v, want one violation", body)
	}
	v := body.Error.Violations[0]
	if v.Field != "code" || v.Code != "country_code" || v.Message != "must be a 2-letter code" {
		t.Fatalf("violation = %+v", v)
	}
}

func TestRuleError_StoreUnavailableIsA503(t *testing.T) {
	w, body, _ := serveRuleError(t, fmt.Errorf("lookup country: %w", store.ErrUnavailable))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if len(body.Error.Violations) != 0 {
		t.Fatalf("body = %+v, want no violations", body)
	}
}

func TestHandleError_ExplicitStatusWinsOverWrappedValidation(t *testing.T) {
	vErrs := validation.Errors{{Field: "tenant", Code: "taken", Message: "is taken"}}
	app := mustNew(t)
	app.POST("/tenants", func(*credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "tenant_conflict").WithInternal(vErrs)
	})
	app.POST("/plain", func(*credo.Context) error { return vErrs })
	app.POST("/single", func(*credo.Context) error {
		return validation.NewError("taken", "is taken")
	})

	cases := []struct {
		path       string
		status     int
		code       string
		violations int
	}{
		{"/tenants", http.StatusConflict, "tenant_conflict", 0},
		{"/plain", http.StatusUnprocessableEntity, "validation_failed", 1},
		{"/single", http.StatusUnprocessableEntity, "validation_failed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(nil)))
			var body credo.ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if w.Code != tc.status || body.Error.Code != tc.code || len(body.Error.Violations) != tc.violations {
				t.Fatalf("got %d %q with %d violations, want %d %q with %d",
					w.Code, body.Error.Code, len(body.Error.Violations), tc.status, tc.code, tc.violations)
			}
		})
	}
}

func TestHandleError_RendererSeesWrappedValidationBehindExplicitStatus(t *testing.T) {
	vErrs := validation.Errors{{Field: "tenant", Code: "taken", Message: "is taken"}}
	app := mustNew(t)
	var seen error
	app.UseErrorRenderer(func(_ *credo.Context, info *credo.ErrorInfo) any {
		seen = info.Err
		return nil
	})
	app.POST("/tenants", func(*credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "tenant_conflict").WithInternal(vErrs)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/tenants", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if _, ok := errors.AsType[validation.Errors](seen); !ok {
		t.Fatalf("ErrorInfo.Err = %v, want the wrapped validation errors reachable", seen)
	}
}

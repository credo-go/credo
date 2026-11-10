package credo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

// TestErrorBodies_ArePinned pins the bytes of the default and the RFC 9457
// error bodies. Neither is built from HTTPError's struct shape: the pipeline
// normalizes an error into an ErrorInfo and renders ErrorResponse or
// ProblemDetails, so the fields of HTTPError carry no wire names.
func TestErrorBodies_ArePinned(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		status      int
		wantDefault string
		wantProblem string
	}{
		{
			name: "a sentinel", err: credo.ErrNotFound, status: http.StatusNotFound,
			wantDefault: `{"success":false,"error":{"code":"not_found","message":"Not Found"}}`,
			wantProblem: `{"type":"about:blank","title":"Not Found","status":404,"instance":"/e","code":"not_found"}`,
		},
		{
			name: "a code, a message, details and an internal error",
			err: credo.NewHTTPError(http.StatusConflict, "email_exists").
				WithMessage("Email already exists").
				WithDetails(map[string]string{"field": "email"}).
				WithInternal(errors.New("unique_violation on users_email_key")),
			status:      http.StatusConflict,
			wantDefault: `{"success":false,"error":{"code":"email_exists","message":"Email already exists","details":{"field":"email"}}}`,
			wantProblem: `{"type":"about:blank","title":"Conflict","status":409,"detail":"Email already exists","instance":"/e","code":"email_exists","details":{"field":"email"}}`,
		},
		{
			name:        "a message key without a catalog",
			err:         credo.NewHTTPError(http.StatusForbidden, "plan_limit").WithMessageKey("billing.plan_limit"),
			status:      http.StatusForbidden,
			wantDefault: `{"success":false,"error":{"code":"plan_limit","message":"Forbidden"}}`,
			wantProblem: `{"type":"about:blank","title":"Forbidden","status":403,"instance":"/e","code":"plan_limit"}`,
		},
		{
			name:        "violations",
			err:         validation.Violations{{Field: "name", Code: "required", Message: "cannot be blank"}},
			status:      http.StatusUnprocessableEntity,
			wantDefault: `{"success":false,"error":{"code":"validation_failed","message":"Validation Failed","violations":[{"field":"name","code":"required","message":"cannot be blank"}]}}`,
			wantProblem: `{"type":"about:blank","title":"Unprocessable Entity","status":422,"detail":"Validation Failed","instance":"/e","code":"validation_failed","violations":[{"field":"name","code":"required","message":"cannot be blank"}]}`,
		},
	}
	renderers := []struct {
		name        string
		install     func(*credo.App)
		contentType string
		want        func(i int) string
	}{
		{"default", func(*credo.App) {}, "application/json; charset=utf-8",
			func(i int) string { return tests[i].wantDefault }},
		{"RFC 9457", func(a *credo.App) { a.UseErrorRenderer(credo.RFC9457ErrorRenderer()) }, "application/problem+json",
			func(i int) string { return tests[i].wantProblem }},
	}
	for _, r := range renderers {
		for i, tt := range tests {
			t.Run(r.name+"/"+tt.name, func(t *testing.T) {
				app := mustNew(t)
				r.install(app)
				app.GET("/e", func(*credo.Context) error { return tt.err })

				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/e", nil))
				if w.Code != tt.status {
					t.Errorf("status = %d, want %d", w.Code, tt.status)
				}
				if got := w.Header().Get("Content-Type"); got != r.contentType {
					t.Errorf("Content-Type = %q, want %q", got, r.contentType)
				}
				if got := strings.TrimSpace(w.Body.String()); got != r.want(i) {
					t.Errorf("body = %s\nwant   %s", got, r.want(i))
				}
			})
		}
	}
}

// TestHTTPError_IsNotAWireShape: HTTPError names no JSON member. Its one tag
// keeps Internal out of a direct marshal, a guard and not a wire name, so an
// HTTPError written into a response by mistake does not expose the error the
// pipeline only logs.
func TestHTTPError_IsNotAWireShape(t *testing.T) {
	typ := reflect.TypeFor[credo.HTTPError]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag, tagged := f.Tag.Lookup("json")
		switch {
		case f.Name == "Internal":
			if tag != "-" {
				t.Errorf(`HTTPError.Internal has the json tag %q, want "-"`, tag)
			}
		case tagged:
			t.Errorf("HTTPError.%s has the json tag %q, want none: the type is not a wire shape", f.Name, tag)
		}
	}

	raw, err := json.Marshal(credo.NewHTTPError(http.StatusConflict, "email_exists").
		WithInternal(&driverError{Table: "users"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "Internal") || strings.Contains(string(raw), "users") {
		t.Errorf("a direct marshal exposes the internal error: %s", raw)
	}
}

// driverError is an error with an exported field, as a database driver returns.
type driverError struct{ Table string }

func (e *driverError) Error() string { return "constraint on " + e.Table }

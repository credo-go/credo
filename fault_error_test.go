package credo_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/fault"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/validation"
)

func serveError(t *testing.T, app *credo.App, err error) *httptest.ResponseRecorder {
	t.Helper()
	app.GET("/e", func(*credo.Context) error { return err })
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/e", nil))
	return w
}

// TestFaultProvider_KindIsTheDefaultCode: a fault without an explicit code
// renders its kind as the code, so a duplicate stays distinguishable from a
// retryable serialization or deadlock failure; the statuses do not change.
func TestFaultProvider_KindIsTheDefaultCode(t *testing.T) {
	for kind, status := range map[fault.Kind]int{
		fault.KindNotFound: 404, fault.KindAlreadyExists: 409, fault.KindConstraint: 409,
		fault.KindSerialization: 409, fault.KindDeadlock: 409, fault.KindContention: 409,
		fault.KindConflict: 409, fault.KindTimeout: 504, fault.KindUnavailable: 503, fault.KindReadOnly: 503,
	} {
		w := serveError(t, mustNew(t), &unknownSemanticHTTPError{kind: kind})
		if w.Code != status {
			t.Errorf("%s: status = %d, want %d", kind, w.Code, status)
		}
		if body := problemFromResponse(t, w); body.Code != string(kind) {
			t.Errorf("%s: code = %q, want the kind", kind, body.Code)
		}
	}
	w := serveError(t, mustNew(t), store.ErrSerialization)
	if body := problemFromResponse(t, w); w.Code != 409 || body.Code != "serialization" || body.Message != "Conflict" {
		t.Errorf("store.ErrSerialization: %d %+v", w.Code, body)
	}
}

// TestFaultError_Renders: a fault.Error renders its kind's status, its code
// and its details; its internal cause is logged, never rendered.
func TestFaultError_Renders(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	taken := fault.New(fault.KindAlreadyExists, "tenant_domain_taken")
	w := serveError(t, app, taken.WithDetails(map[string]any{"domain": "a.example"}).WithInternal(errors.New("pq: duplicate key secret_idx")))
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "secret_idx") {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	body := problemFromResponse(t, w)
	if body.Code != "tenant_domain_taken" || body.Message != "Conflict" || !reflect.DeepEqual(body.Details, map[string]any{"domain": "a.example"}) {
		t.Fatalf("body = %+v", body)
	}
	if line := logLine(t, buf, "credo: client error"); line["error"] != "pq: duplicate key secret_idx" {
		t.Fatalf("logged cause = %v", line["error"])
	}

	w = serveError(t, mustNew(t), fault.New(fault.KindUnavailable))
	if body := problemFromResponse(t, w); w.Code != 503 || body.Code != "unavailable" {
		t.Fatalf("New(KindUnavailable): %d %+v", w.Code, body)
	}
}

// TestFaultError_InvalidFailsClosed: a value built without New whose kind is
// unknown or whose code is invalid renders a generic 500, and New's panic
// during a request is recovered into one.
func TestFaultError_InvalidFailsClosed(t *testing.T) {
	for name, err := range map[string]error{
		"unknown kind": &fault.Error{Kind: fault.KindUnknown, Code: "x"},
		"undeclared":   &fault.Error{Kind: "future_kind", Code: "x"},
		"invalid code": &fault.Error{Kind: fault.KindConflict, Code: "Leaky Text"},
		"empty code":   &fault.Error{Kind: fault.KindConflict},
	} {
		w := serveError(t, mustNew(t), err)
		body := problemFromResponse(t, w)
		if w.Code != 500 || body.Code != "internal_server_error" || strings.Contains(w.Body.String(), "Leaky") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	app := mustNew(t)
	app.GET("/p", func(*credo.Context) error { return fault.New(fault.KindUnknown) })
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "declared") {
		t.Fatalf("panic in New: %d %s", w.Code, w.Body.String())
	}
}

// TestFaultError_ClassificationOrder: a fault.Error wins over validation and
// bind errors it wraps and over a store error it wraps; an HTTPError that
// wraps a fault.Error wins over it.
func TestFaultError_ClassificationOrder(t *testing.T) {
	taken := fault.New(fault.KindAlreadyExists, "tenant_domain_taken")
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"wraps validation errors", taken.WithInternal(validation.Violations{{Field: "domain", Code: "required", Message: "secret field"}}), 409, "tenant_domain_taken"},
		{"wraps a bind error", taken.WithInternal(&credo.BindError{Reason: credo.BindReasonSyntax}), 409, "tenant_domain_taken"},
		{"wraps a store error", taken.WithInternal(store.Wrap(store.ErrAlreadyExists, errors.New("dup"))), 409, "tenant_domain_taken"},
		{"wrapped by an HTTPError", credo.NewHTTPError(http.StatusBadRequest, "explicit").WithInternal(taken), 400, "explicit"},
		{"wrapped with fmt", fmt.Errorf("service: %w", taken), 409, "tenant_domain_taken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveError(t, mustNew(t), tc.err)
			body := problemFromResponse(t, w)
			if w.Code != tc.status || body.Code != tc.code || len(body.Violations) != 0 || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

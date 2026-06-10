package credo_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

func TestHandleError_HTTPError(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusNotFound, "user.not_found")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Type != "about:blank" {
		t.Errorf("type = %q, want %q", pd.Type, "about:blank")
	}
	// No i18n, no builtInMessages match → key itself is used as title
	if pd.Title != "user.not_found" {
		t.Errorf("title = %q, want %q", pd.Title, "user.not_found")
	}
	if pd.Status != http.StatusNotFound {
		t.Errorf("pd.status = %d, want %d", pd.Status, http.StatusNotFound)
	}
}

func TestHandleError_ValidationErrors(t *testing.T) {
	app := mustNew(t)
	app.POST("/users", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "name", Code: "required", Message: "name is required"},
			{Field: "email", Code: "email", Message: "must be a valid email"},
		}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Type != "https://credo.dev/errors/validation" {
		t.Errorf("type = %q, want %q", pd.Type, "https://credo.dev/errors/validation")
	}
	// builtInMessages fallback for MsgKeyValidationFailed
	if pd.Title != "Validation Failed" {
		t.Errorf("title = %q, want %q", pd.Title, "Validation Failed")
	}
	if pd.Status != http.StatusUnprocessableEntity {
		t.Errorf("pd.status = %d, want %d", pd.Status, http.StatusUnprocessableEntity)
	}
	if len(pd.Errors) != 2 {
		t.Fatalf("errors len = %d, want 2", len(pd.Errors))
	}
	if pd.Errors[0].Field != "name" || pd.Errors[0].Code != "required" {
		t.Errorf("errors[0] = %+v, want field=name code=required", pd.Errors[0])
	}
	if pd.Errors[1].Field != "email" || pd.Errors[1].Code != "email" {
		t.Errorf("errors[1] = %+v, want field=email code=email", pd.Errors[1])
	}
}

// httpStatusErr simulates a store-style error with HTTPStatus() int.
type httpStatusErr struct {
	msg    string
	status int
}

func (e *httpStatusErr) Error() string   { return e.msg }
func (e *httpStatusErr) HTTPStatus() int { return e.status }

func TestHandleError_HTTPStatusInterface(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/99", func(ctx *credo.Context) error {
		return &httpStatusErr{msg: "store: record not found", status: http.StatusNotFound}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/99", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Status != http.StatusNotFound {
		t.Errorf("pd.Status = %d, want %d", pd.Status, http.StatusNotFound)
	}
	if pd.Title != "Not Found" {
		t.Errorf("pd.Title = %q, want %q", pd.Title, "Not Found")
	}
	// Detail must NOT leak internal error messages (e.g., "store: record not found").
	if pd.Detail != "" {
		t.Errorf("pd.Detail = %q, want empty (should not leak internal message)", pd.Detail)
	}
}

func TestHandleError_HTTPStatusInterface_Wrapped(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/99", func(ctx *credo.Context) error {
		inner := &httpStatusErr{msg: "store: duplicate record", status: http.StatusConflict}
		return errors.Join(errors.New("repo: create user"), inner)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/99", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestHandleError_GenericError(t *testing.T) {
	app := mustNew(t)
	app.GET("/internal", func(ctx *credo.Context) error {
		return errors.New("secret db password leak")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/internal", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Title != "Internal Server Error" {
		t.Errorf("title = %q, want %q", pd.Title, "Internal Server Error")
	}
	// Must NOT leak the error message
	body := w.Body.String()
	if contains(body, "secret") || contains(body, "password") || contains(body, "leak") {
		t.Errorf("response body leaks internal error: %s", body)
	}
}

func TestHandleError_HEAD(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusForbidden)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", w.Body.String())
	}
}

func TestHandleError_Committed(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		// Commit the response first, then return an error.
		ctx.Response().WriteHeader(http.StatusOK)
		return errors.New("should be logged but not written")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	// Status should remain 200 (committed), not changed by error handler.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleError_ContentType(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusBadRequest)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	ct := w.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/problem+json")
	}
}

func TestNewProblemDetails(t *testing.T) {
	pd := credo.NewProblemDetails(http.StatusConflict, "Resource Conflict")

	if pd.Type != "about:blank" {
		t.Errorf("Type = %q, want %q", pd.Type, "about:blank")
	}
	if pd.Title != "Resource Conflict" {
		t.Errorf("Title = %q, want %q", pd.Title, "Resource Conflict")
	}
	if pd.Status != http.StatusConflict {
		t.Errorf("Status = %d, want %d", pd.Status, http.StatusConflict)
	}
	if pd.Detail != "" {
		t.Errorf("Detail = %q, want empty", pd.Detail)
	}
	if pd.Instance != "" {
		t.Errorf("Instance = %q, want empty", pd.Instance)
	}
	if pd.Errors != nil {
		t.Errorf("Errors = %v, want nil", pd.Errors)
	}
}

func TestHandleError_Instance(t *testing.T) {
	app := mustNew(t)
	app.GET("/api/users/{id}", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusNotFound)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/users/42", nil)
	app.ServeHTTP(w, r)

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Instance != "/api/users/42" {
		t.Errorf("instance = %q, want %q", pd.Instance, "/api/users/42")
	}
}

func TestHandleError_UsesAppLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	app.GET("/fail", func(ctx *credo.Context) error {
		return fmt.Errorf("unexpected failure")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/fail", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !contains(buf.String(), "credo: unhandled error") {
		t.Errorf("expected app logger to receive error log, got: %q", buf.String())
	}
}

func TestLogServerError_SentinelHTTPError(t *testing.T) {
	newApp := func(t *testing.T) (*credo.App, *bytes.Buffer) {
		t.Helper()
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		app, err := credo.New(credo.WithLogger(logger))
		if err != nil {
			t.Fatal(err)
		}
		return app, &buf
	}

	t.Run("5xx without Internal is still logged", func(t *testing.T) {
		app, buf := newApp(t)
		app.GET("/boom", func(ctx *credo.Context) error {
			return credo.NewHTTPError(http.StatusBadGateway)
		})
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/boom", nil))

		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", w.Code)
		}
		out := buf.String()
		if !contains(out, "credo: server error") || !contains(out, "status=502") {
			t.Errorf("expected server error log with status=502, got: %q", out)
		}
	})

	t.Run("4xx without Internal is not logged as server error", func(t *testing.T) {
		app, buf := newApp(t)
		app.GET("/bad", func(ctx *credo.Context) error {
			return credo.NewHTTPError(http.StatusBadRequest)
		})
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/bad", nil))

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
		if contains(buf.String(), "credo: server error") {
			t.Errorf("4xx must not be logged as a server error, got: %q", buf.String())
		}
	})
}

// --- ErrorRenderer tests ---

func TestHandleError_ErrorRendererCalled(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	called := false
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		called = true
		receivedInfo = info
		ctx.Response().Header().Set("Content-Type", "application/json")
		ctx.Response().WriteHeader(info.Problem.Status)
		json.NewEncoder(ctx.Response()).Encode(map[string]string{"error": info.Problem.Title}) //nolint:errcheck
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusNotFound, "user.not_found")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if !called {
		t.Fatal("ErrorRenderer was not called")
	}
	if receivedInfo.Problem.Status != http.StatusNotFound {
		t.Errorf("pd.Status = %d, want %d", receivedInfo.Problem.Status, http.StatusNotFound)
	}
	if receivedInfo.Problem.Title != "user.not_found" {
		t.Errorf("pd.Title = %q, want %q", receivedInfo.Problem.Title, "user.not_found")
	}
	if receivedInfo.MessageKey != "user.not_found" {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, "user.not_found")
	}
	if receivedInfo.Err == nil {
		t.Error("info.Err should not be nil")
	}
}

func TestHandleError_ErrorRendererFallback(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	// ErrorRenderer that does NOT write a response (doesn't commit).
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		// intentionally empty — no write
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusBadRequest)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	// Should fall back to default RFC 7807 response.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/problem+json")
	}
	if !contains(buf.String(), "ErrorRenderer did not write response") {
		t.Errorf("expected warning log about fallback, got: %q", buf.String())
	}
}

func TestHandleError_ErrorRendererPanics(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		panic("renderer exploded")
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusBadRequest)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)

	// Should NOT panic.
	app.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if !contains(buf.String(), "ErrorRenderer panic") {
		t.Errorf("expected panic log, got: %q", buf.String())
	}
}

func TestHandleError_ErrorRendererNil(t *testing.T) {
	app := mustNew(t)
	// ErrorRenderer is nil by default → RFC 7807 JSON.
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/problem+json")
	}
	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Title != "Conflict" {
		t.Errorf("title = %q, want %q", pd.Title, "Conflict")
	}
}

func TestHandleError_RemovesImmutableCacheControl(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			return next(ctx)
		}
	})
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache, must-revalidate")
	}
}

func TestHandleError_PreservesNonImmutableCacheControl(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("Cache-Control", "public, max-age=60")
			return next(ctx)
		}
	})
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q, want %q", got, "public, max-age=60")
	}
}

func TestHandleError_HEADRemovesImmutableCacheControl(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("Cache-Control", "public, max-age=31536000, Immutable")
			return next(ctx)
		}
	})
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache, must-revalidate")
	}
}

func TestHandleError_CustomRendererRemovesImmutableCacheControl(t *testing.T) {
	app := mustNew(t)
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		ctx.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		ctx.Response().Header().Set("Content-Type", "application/problem+json")
		ctx.Response().WriteHeader(info.Problem.Status)
		json.NewEncoder(ctx.Response()).Encode(info.Problem) //nolint:errcheck
	})
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.ErrConflict
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache, must-revalidate")
	}
}

func TestHandleError_CommittedBeforeRenderer(t *testing.T) {
	app := mustNew(t)

	rendererCalled := false
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		rendererCalled = true
	})

	app.GET("/test", func(ctx *credo.Context) error {
		ctx.Response().WriteHeader(http.StatusOK) // commit
		return errors.New("error after commit")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if rendererCalled {
		t.Error("ErrorRenderer should not be called when response is already committed")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (committed status)", w.Code, http.StatusOK)
	}
}

func TestHandleError_HEADCallsRenderer(t *testing.T) {
	app := mustNew(t)

	rendererCalled := false
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		rendererCalled = true
		ctx.Response().Header().Set("X-Error-Code", info.MessageKey)
		// Don't write body — framework handles HEAD NoContent fallback.
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusForbidden)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/test", nil)
	app.ServeHTTP(w, r)

	if !rendererCalled {
		t.Error("ErrorRenderer should be called for HEAD requests (to allow setting headers)")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", w.Body.String())
	}
	if got := w.Header().Get("X-Error-Code"); got != credo.MsgKeyForbidden {
		t.Errorf("X-Error-Code = %q, want %q", got, credo.MsgKeyForbidden)
	}
}

func TestHandleError_RendererReceivesInstance(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	called := false
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		called = true
		receivedInfo = info
		ctx.Response().Header().Set("Content-Type", "application/problem+json")
		ctx.Response().WriteHeader(info.Problem.Status)
		json.NewEncoder(ctx.Response()).Encode(info.Problem) //nolint:errcheck
	})

	app.GET("/api/items/{id}", func(ctx *credo.Context) error {
		return credo.ErrNotFound
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/items/42", nil)
	app.ServeHTTP(w, r)

	if !called {
		t.Fatal("ErrorRenderer was not called")
	}
	if receivedInfo.Problem.Instance != "/api/items/42" {
		t.Errorf("pd.Instance = %q, want %q", receivedInfo.Problem.Instance, "/api/items/42")
	}
}

func TestHandleError_RendererReceivesValidationErrors(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	called := false
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		called = true
		receivedInfo = info
		ctx.Response().Header().Set("Content-Type", "application/problem+json")
		ctx.Response().WriteHeader(info.Problem.Status)
		json.NewEncoder(ctx.Response()).Encode(info.Problem) //nolint:errcheck
	})

	app.POST("/users", func(ctx *credo.Context) error {
		return validation.Errors{
			{Field: "name", Code: "required", Message: "name is required"},
		}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/users", nil)
	app.ServeHTTP(w, r)

	if !called {
		t.Fatal("ErrorRenderer was not called")
	}
	if receivedInfo.Problem.Status != http.StatusUnprocessableEntity {
		t.Errorf("pd.Status = %d, want %d", receivedInfo.Problem.Status, http.StatusUnprocessableEntity)
	}
	if len(receivedInfo.Problem.Errors) != 1 {
		t.Fatalf("pd.Errors len = %d, want 1", len(receivedInfo.Problem.Errors))
	}
	if receivedInfo.Problem.Errors[0].Field != "name" {
		t.Errorf("pd.Errors[0].Field = %q, want %q", receivedInfo.Problem.Errors[0].Field, "name")
	}
	if receivedInfo.MessageKey != credo.MsgKeyValidationFailed {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, credo.MsgKeyValidationFailed)
	}
}

func TestHandleError_ErrorInfoErrForSentry(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		receivedInfo = info
		ctx.Response().WriteHeader(info.Problem.Status)
	})

	// Handler returns an HTTPError wrapping an internal error.
	innerErr := errors.New("db connection refused")
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(500, "db.error").WithInternal(innerErr)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	// Renderer can use errors.Is to find the root cause (Sentry use case).
	if !errors.Is(receivedInfo.Err, innerErr) {
		t.Errorf("errors.Is(info.Err, innerErr) = false, want true")
	}
}

func TestHandleError_ErrorInfoMessageKey_HTTPStatusProvider(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		receivedInfo = info
		ctx.Response().WriteHeader(info.Problem.Status)
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return &httpStatusErr{msg: "store: not found", status: http.StatusNotFound}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if receivedInfo.MessageKey != credo.MsgKeyNotFound {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, credo.MsgKeyNotFound)
	}
}

func TestHandleError_ErrorInfoMessageKey_GenericError(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		receivedInfo = info
		ctx.Response().WriteHeader(info.Problem.Status)
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return errors.New("something unexpected")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if receivedInfo.MessageKey != credo.MsgKeyInternalError {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, credo.MsgKeyInternalError)
	}
}

// --- statusToKey coverage ---

func TestNewHTTPError_RequestTimeout_UsesI18nKey(t *testing.T) {
	e := credo.NewHTTPError(http.StatusRequestTimeout)
	if e.MessageKey != credo.MsgKeyRequestTimeout {
		t.Errorf("NewHTTPError(408).MessageKey = %q, want %q", e.MessageKey, credo.MsgKeyRequestTimeout)
	}
}

func TestHandleError_RequestTimeout(t *testing.T) {
	app := mustNew(t)
	app.GET("/slow", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusRequestTimeout)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/slow", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusRequestTimeout {
		t.Errorf("status = %d, want %d", w.Code, http.StatusRequestTimeout)
	}

	var pd credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Title != "Request Timeout" {
		t.Errorf("title = %q, want %q", pd.Title, "Request Timeout")
	}
}

func TestClassifyError_HTTPStatusProvider_408(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		receivedInfo = info
		ctx.Response().WriteHeader(info.Problem.Status)
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return &httpStatusErr{msg: "request timed out", status: http.StatusRequestTimeout}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusRequestTimeout {
		t.Errorf("status = %d, want %d", w.Code, http.StatusRequestTimeout)
	}
	if receivedInfo.MessageKey != credo.MsgKeyRequestTimeout {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, credo.MsgKeyRequestTimeout)
	}
}

// --- helpers ---

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

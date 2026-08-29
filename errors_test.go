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
	"github.com/credo-go/credo/fault"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/validation"
)

func TestHandleError_HTTPError(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusNotFound, "not_found").
			WithMessageKey("user.not_found")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Success {
		t.Error("success = true, want false")
	}
	// No i18n, no built-in match → explicit key is the literal fallback.
	if pd.Error.Message != "user.not_found" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "user.not_found")
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

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// builtInMessages fallback for MsgKeyValidationFailed
	if pd.Error.Message != "Validation Failed" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "Validation Failed")
	}
	if len(pd.Error.Violations) != 2 {
		t.Fatalf("errors len = %d, want 2", len(pd.Error.Violations))
	}
	if pd.Error.Violations[0].Field != "name" || pd.Error.Violations[0].Code != "required" {
		t.Errorf("errors[0] = %+v, want field=name code=required", pd.Error.Violations[0])
	}
	if pd.Error.Violations[1].Field != "email" || pd.Error.Violations[1].Code != "email" {
		t.Errorf("errors[1] = %+v, want field=email code=email", pd.Error.Violations[1])
	}
}

// httpStatusError simulates a store-style error with HTTPStatus() int.
type httpStatusError struct {
	msg    string
	status int
}

func (e *httpStatusError) Error() string   { return e.msg }
func (e *httpStatusError) HTTPStatus() int { return e.status }

func TestHandleError_HTTPStatusInterface(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/99", func(ctx *credo.Context) error {
		return &httpStatusError{msg: "store: record not found", status: http.StatusNotFound}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/99", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Error.Message != "Not Found" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "Not Found")
	}
	if contains(w.Body.String(), "store: record not found") {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

func TestHandleError_HTTPStatusInterface_Wrapped(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/99", func(ctx *credo.Context) error {
		inner := &httpStatusError{msg: "store: duplicate record", status: http.StatusConflict}
		return errors.Join(errors.New("repo: create user"), inner)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/99", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestHandleError_SemanticFaultPolicy(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		title  string
	}{
		{"not found", store.ErrNotFound, http.StatusNotFound, "Not Found"},
		{"already exists", store.ErrAlreadyExists, http.StatusConflict, "Conflict"},
		{"constraint", store.ErrConstraint, http.StatusConflict, "Conflict"},
		{"serialization", store.ErrSerialization, http.StatusConflict, "Conflict"},
		{"deadlock", store.ErrDeadlock, http.StatusConflict, "Conflict"},
		{"contention", store.ErrContention, http.StatusConflict, "Conflict"},
		{"timeout", store.ErrTimeout, http.StatusGatewayTimeout, "Gateway Timeout"},
		{"unavailable", store.ErrUnavailable, http.StatusServiceUnavailable, "Service Unavailable"},
		{"read only", store.ErrReadOnly, http.StatusServiceUnavailable, "Service Unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/test", func(ctx *credo.Context) error {
				return errors.Join(errors.New("repo"), fmt.Errorf("store: %w", tt.err))
			})

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			var pd credo.ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if pd.Error.Message != tt.title {
				t.Errorf("message = %q, want %q", pd.Error.Message, tt.title)
			}
		})
	}
}

func TestHandleError_HTTPErrorOverridesSemanticFault(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusUnprocessableEntity).
			WithInternal(store.ErrConstraint)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

type unknownSemanticHTTPError struct {
	kind fault.Kind
}

func (*unknownSemanticHTTPError) Error() string           { return "sensitive future fault" }
func (e *unknownSemanticHTTPError) FaultKind() fault.Kind { return e.kind }
func (*unknownSemanticHTTPError) HTTPStatus() int         { return http.StatusTeapot }

func TestHandleError_UnknownSemanticFaultFailsClosed(t *testing.T) {
	for _, kind := range []fault.Kind{fault.KindUnknown, fault.Kind("future_kind")} {
		t.Run(string(kind), func(t *testing.T) {
			app := mustNew(t)
			app.GET("/test", func(ctx *credo.Context) error {
				return &unknownSemanticHTTPError{kind: kind}
			})

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want fail-closed %d", w.Code, http.StatusInternalServerError)
			}
			if contains(w.Body.String(), "sensitive") || contains(w.Body.String(), "future") {
				t.Fatalf("body leaked semantic fault metadata: %s", w.Body.String())
			}
		})
	}
}

func TestHandleError_StructuredStoreMetadataDoesNotLeak(t *testing.T) {
	cause := errors.New("duplicate key secret@example.com")
	structured := &store.Error{
		Kind:       store.KindConstraint,
		Resource:   "users",
		Constraint: "users_email_key",
		Code:       "23505",
		Cause:      cause,
	}

	app := mustNew(t)
	var renderedErr error
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		renderedErr = info.Err
		return credo.ErrorResponse{Error: credo.ErrorBody{Code: info.Code, Message: info.Message, Details: info.Details, Violations: info.Violations}}
	})
	app.GET("/test", func(ctx *credo.Context) error { return structured })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	if renderedErr != structured { //nolint:errorlint // The renderer must receive this exact error value.
		t.Fatal("custom renderer did not receive the original structured error")
	}
	for _, secret := range []string{"secret@example.com", "users_email_key", "23505"} {
		if contains(w.Body.String(), secret) {
			t.Fatalf("body leaked %q: %s", secret, w.Body.String())
		}
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

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Error.Message != "Internal Server Error" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "Internal Server Error")
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
	if ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
	}
}

func TestDefaultSuccessPayloadIsNotAutomaticallyEnveloped(t *testing.T) {
	app := mustNew(t)
	app.GET("/ok", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, map[string]string{"value": "ok"})
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if got, want := w.Body.String(), `{"value":"ok"}`; got != want {
		t.Fatalf("body = %s, want %s; success:true requires SuccessRenderer + Render", got, want)
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
	if pd.Violations != nil {
		t.Errorf("Violations = %v, want nil", pd.Violations)
	}
}

func TestRFC9457ErrorRenderer_Instance(t *testing.T) {
	app := mustNew(t)
	app.SetErrorRenderer(credo.RFC9457ErrorRenderer())
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

func TestRFC9457ErrorRenderer_ShapeAndTypeResolver(t *testing.T) {
	app := mustNew(t)
	app.SetErrorRenderer(credo.RFC9457ErrorRenderer(credo.RFC9457Config{
		ResolveType: func(info *credo.ErrorInfo) string {
			return "https://errors.example/" + info.Code
		},
	}))
	app.GET("/conflict", func(*credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "email_exists").
			WithMessageKey("Email already exists").
			WithDetails(map[string]any{"field": "email"})
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conflict", nil))
	if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	var body credo.ProblemDetails
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "https://errors.example/email_exists" || body.Title != "Conflict" || body.Detail != "Email already exists" || body.Status != http.StatusConflict {
		t.Fatalf("problem = %#v", body)
	}
	if body.Code != "email_exists" || body.Instance != "/conflict" {
		t.Fatalf("problem extensions = %#v", body)
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

func TestLogServerError_UsesClassifiedSemanticStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	app.GET("/fault", func(ctx *credo.Context) error {
		// The semantic policy must win over the deliberately conflicting legacy
		// status for both response classification and server-error logging.
		return &unknownSemanticHTTPError{kind: fault.KindUnavailable}
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fault", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	logOutput := buf.String()
	if !contains(logOutput, "credo: server error") || !contains(logOutput, "status=503") {
		t.Fatalf("semantic server fault log did not use classified status: %q", logOutput)
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
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		called = true
		receivedInfo = *info
		return map[string]string{"error": info.Message}
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusNotFound, "not_found").
			WithMessageKey("user.not_found")
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
	if receivedInfo.Status != http.StatusNotFound {
		t.Errorf("info.Status = %d, want %d", receivedInfo.Status, http.StatusNotFound)
	}
	if receivedInfo.Message != "user.not_found" {
		t.Errorf("info.Message = %q, want %q", receivedInfo.Message, "user.not_found")
	}
	if receivedInfo.MessageKey != "user.not_found" {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, "user.not_found")
	}
	if receivedInfo.Err == nil {
		t.Error("info.Err should not be nil")
	}
}

func TestHandleError_ErrorRendererNilBody(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	// A nil body is the documented "keep the default error body" signal;
	// headers set by the renderer decorate that default response.
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		ctx.Response().Header().Set("X-Error-Code", info.MessageKey)
		return nil
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusBadRequest)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
	}
	if got := w.Header().Get("X-Error-Code"); got != "bad_request" {
		t.Errorf("X-Error-Code = %q, want %q (renderer headers must survive)", got, "bad_request")
	}
	// nil is intentional, not an accident: nothing is logged for it.
	if contains(buf.String(), "did not write response") {
		t.Errorf("nil body must not log a fallback warning, got: %q", buf.String())
	}
}

func TestHandleError_ErrorRendererBody(t *testing.T) {
	app := mustNew(t)

	// The common case: the renderer returns the body and the framework owns
	// status, Content-Type, and the write.
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		return map[string]any{"code": info.MessageKey, "message": info.Message}
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusConflict, "email_exists").
			WithMessageKey("user.email_exists")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %q)", err, w.Body.String())
	}
	if got["code"] != "user.email_exists" {
		t.Errorf("body code = %v, want user.email_exists", got["code"])
	}
}

func TestHandleError_ErrorRendererMutatesStatus(t *testing.T) {
	app := mustNew(t)

	// info.Status is the renderer's status seam: mutating it before
	// returning changes the written status for both body shapes.
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		if info.MessageKey == "not_found" {
			info.Status = http.StatusGone
		}
		return map[string]any{"status": info.Status}
	})

	app.GET("/test", func(ctx *credo.Context) error { return credo.ErrNotFound })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))
	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want %d (renderer-mutated)", w.Code, http.StatusGone)
	}
}

func TestHandleError_ErrorRendererInvalidStatusFailsClosed(t *testing.T) {
	app := mustNew(t)
	app.SetErrorRenderer(func(_ *credo.Context, info *credo.ErrorInfo) any {
		info.Status = 0
		return map[string]string{"must": "not leak"}
	})
	app.GET("/test", func(*credo.Context) error { return credo.ErrNotFound })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
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

func TestHandleError_HEADDiscardsRendererBody(t *testing.T) {
	app := mustNew(t)

	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		return map[string]any{"code": info.MessageKey}
	})
	app.GET("/test", func(ctx *credo.Context) error { return credo.ErrForbidden })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("HEAD", "/test", nil))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty even when the renderer returns one", w.Body.String())
	}
}

func TestHandleError_CommittedRendererIgnoresBody(t *testing.T) {
	app := mustNew(t)

	// Full-control escape hatch: once the renderer commits the response
	// itself, the returned body must not be appended on top.
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		_ = ctx.Response().Text(info.Status, "plain error")
		return map[string]any{"must": "be ignored"}
	})
	app.GET("/test", func(ctx *credo.Context) error { return credo.ErrBadRequest })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	if w.Body.String() != "plain error" {
		t.Errorf("body = %q, want only the committed write", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the committed text/plain", ct)
	}
}

func TestHandleError_ErrorRendererPanics(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
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
	if !contains(buf.String(), "error pipeline panic") {
		t.Errorf("expected panic log, got: %q", buf.String())
	}
}

func TestHandleError_ErrorRendererNil(t *testing.T) {
	app := mustNew(t)
	// ErrorRenderer is nil by default → Credo JSON error envelope.
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
	if ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
	}
	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Error.Message != "Conflict" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "Conflict")
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
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		ctx.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		ctx.Response().Header().Set("Content-Type", "application/problem+json")
		ctx.Response().WriteHeader(info.Status)
		json.NewEncoder(ctx.Response()).Encode(info) //nolint:errcheck
		return nil
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
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		rendererCalled = true
		return nil
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
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		rendererCalled = true
		ctx.Response().Header().Set("X-Error-Code", info.MessageKey)
		// nil body — the framework sends the status-only HEAD response.
		return nil
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
	if got := w.Header().Get("X-Error-Code"); got != "forbidden" {
		t.Errorf("X-Error-Code = %q, want %q", got, "forbidden")
	}
}

func TestHandleError_RendererCanReadRequestPath(t *testing.T) {
	app := mustNew(t)

	var receivedPath string
	called := false
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		called = true
		receivedPath = ctx.Request().URL.Path
		return map[string]any{"code": info.Code, "message": info.Message}
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
	if receivedPath != "/api/items/42" {
		t.Errorf("request path = %q, want %q", receivedPath, "/api/items/42")
	}
}

func TestHandleError_RendererReceivesValidationErrors(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	called := false
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		called = true
		receivedInfo = *info
		return map[string]any{"code": info.Code, "message": info.Message, "violations": info.Violations}
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
	if receivedInfo.Status != http.StatusUnprocessableEntity {
		t.Errorf("info.Status = %d, want %d", receivedInfo.Status, http.StatusUnprocessableEntity)
	}
	if len(receivedInfo.Violations) != 1 {
		t.Fatalf("info.Violations len = %d, want 1", len(receivedInfo.Violations))
	}
	if receivedInfo.Violations[0].Field != "name" {
		t.Errorf("info.Violations[0].Field = %q, want %q", receivedInfo.Violations[0].Field, "name")
	}
	if receivedInfo.MessageKey != credo.MsgKeyValidationFailed {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, credo.MsgKeyValidationFailed)
	}
}

func TestHandleError_ErrorInfoErrForSentry(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		receivedInfo = *info
		return nil
	})

	// Handler returns an HTTPError wrapping an internal error.
	innerErr := errors.New("db connection refused")
	app.GET("/test", func(ctx *credo.Context) error {
		return credo.NewHTTPError(500).WithMessageKey("db.error").WithInternal(innerErr)
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
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		receivedInfo = *info
		return nil
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return &httpStatusError{msg: "store: not found", status: http.StatusNotFound}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if receivedInfo.MessageKey != "not_found" {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, "not_found")
	}
}

func TestHandleError_ErrorInfoMessageKey_GenericError(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		receivedInfo = *info
		return nil
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
	if receivedInfo.MessageKey != "internal_server_error" {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, "internal_server_error")
	}
}

// --- frozen status-code coverage ---

func TestNewHTTPError_RequestTimeout_UsesFrozenCode(t *testing.T) {
	e := credo.NewHTTPError(http.StatusRequestTimeout)
	if e.Code != "request_timeout" {
		t.Errorf("NewHTTPError(408).Code = %q, want %q", e.Code, "request_timeout")
	}
	if e.MessageKey != "" {
		t.Errorf("NewHTTPError(408).MessageKey = %q, want empty", e.MessageKey)
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

	var pd credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pd.Error.Message != "Request Timeout" {
		t.Errorf("message = %q, want %q", pd.Error.Message, "Request Timeout")
	}
}

func TestClassifyError_HTTPStatusProvider_408(t *testing.T) {
	app := mustNew(t)

	var receivedInfo credo.ErrorInfo
	app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
		receivedInfo = *info
		return nil
	})

	app.GET("/test", func(ctx *credo.Context) error {
		return &httpStatusError{msg: "request timed out", status: http.StatusRequestTimeout}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusRequestTimeout {
		t.Errorf("status = %d, want %d", w.Code, http.StatusRequestTimeout)
	}
	if receivedInfo.MessageKey != "request_timeout" {
		t.Errorf("info.MessageKey = %q, want %q", receivedInfo.MessageKey, "request_timeout")
	}
}

func TestErrorPipeline_DoesNotWriteAfterActualHijack(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*credo.App)
		handler   credo.Handler
	}{
		{
			name: "handler_returns_error_after_hijack",
			handler: func(ctx *credo.Context) error {
				if _, _, err := ctx.Response().Hijack(); err != nil {
					return err
				}
				return errors.New("post-hijack application error")
			},
		},
		{
			name: "renderer_hijacks_then_returns",
			configure: func(app *credo.App) {
				app.SetErrorRenderer(func(ctx *credo.Context, _ *credo.ErrorInfo) any {
					_, _, _ = ctx.Response().Hijack()
					return nil
				})
			},
			handler: func(*credo.Context) error { return credo.ErrBadRequest },
		},
		{
			name: "renderer_hijacks_then_panics",
			configure: func(app *credo.App) {
				app.SetErrorRenderer(func(ctx *credo.Context, _ *credo.ErrorInfo) any {
					_, _, _ = ctx.Response().Hijack()
					panic("renderer panic after hijack")
				})
			},
			handler: func(*credo.Context) error { return credo.ErrBadRequest },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			if tt.configure != nil {
				tt.configure(app)
			}
			app.GET("/ws", tt.handler)

			w := newHijackResponseWriter()
			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			app.ServeHTTP(w, r)

			if w.hijackCalls != 1 {
				t.Fatalf("Hijack calls = %d, want 1", w.hijackCalls)
			}
			if w.writeHeaderCalls != 0 || w.Body.Len() != 0 {
				t.Fatalf("post-hijack HTTP writes = %d/%q, want none", w.writeHeaderCalls, w.Body.String())
			}
		})
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

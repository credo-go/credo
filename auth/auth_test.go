package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/auth"
)

// --- test types ---

type testUser struct {
	ID   int
	Name string
}

// mockAuthenticator is an Authenticator[T] for testing.
type mockAuthenticator[T any] struct {
	user T
	err  error
}

func (m *mockAuthenticator[T]) Authenticate(r *http.Request) (T, error) {
	return m.user, m.err
}

// --- SetUser / GetUser tests ---

func TestSetUser_GetUser_Success(t *testing.T) {
	ctx := t.Context()
	user := &testUser{ID: 1, Name: "alice"}

	ctx = auth.SetUser(ctx, user)
	got, ok := auth.GetUser[*testUser](ctx)

	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.ID != 1 || got.Name != "alice" {
		t.Errorf("got %+v, want {ID:1, Name:alice}", got)
	}
}

func TestGetUser_NotSet(t *testing.T) {
	ctx := t.Context()

	got, ok := auth.GetUser[*testUser](ctx)
	if ok {
		t.Error("expected ok=false when no user set")
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestGetUser_TypeMismatch(t *testing.T) {
	ctx := t.Context()
	ctx = auth.SetUser(ctx, "string-user")

	got, ok := auth.GetUser[*testUser](ctx)
	if ok {
		t.Error("expected ok=false for type mismatch")
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestSetUser_NilPointer(t *testing.T) {
	ctx := t.Context()
	ctx = auth.SetUser[*testUser](ctx, nil)

	got, ok := auth.GetUser[*testUser](ctx)
	if !ok {
		t.Fatal("expected ok=true for nil pointer")
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestSetUser_DistinctTypesCoexist(t *testing.T) {
	type serviceAccount struct{ Name string }

	ctx := t.Context()
	ctx = auth.SetUser(ctx, &testUser{ID: 1, Name: "alice"})
	ctx = auth.SetUser(ctx, &serviceAccount{Name: "ci-bot"})

	u, ok := auth.GetUser[*testUser](ctx)
	if !ok || u.Name != "alice" {
		t.Errorf("user slot = (%+v, %v), want alice", u, ok)
	}
	sa, ok := auth.GetUser[*serviceAccount](ctx)
	if !ok || sa.Name != "ci-bot" {
		t.Errorf("service account slot = (%+v, %v), want ci-bot", sa, ok)
	}
}

func TestSetUser_StructValue(t *testing.T) {
	ctx := t.Context()
	user := testUser{ID: 42, Name: "bob"}

	ctx = auth.SetUser(ctx, user)
	got, ok := auth.GetUser[testUser](ctx)

	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.ID != 42 || got.Name != "bob" {
		t.Errorf("got %+v, want {ID:42, Name:bob}", got)
	}
}

func TestRequireUser_Success(t *testing.T) {
	ctx := t.Context()
	user := &testUser{ID: 1, Name: "alice"}
	ctx = auth.SetUser(ctx, user)

	got, err := auth.RequireUser[*testUser](ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != 1 || got.Name != "alice" {
		t.Errorf("got %+v, want {ID:1, Name:alice}", got)
	}
}

func TestRequireUser_NotSet(t *testing.T) {
	ctx := t.Context()

	got, err := auth.RequireUser[*testUser](ctx)
	if !errors.Is(err, auth.ErrUserMissing) {
		t.Fatalf("expected ErrUserMissing, got %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestRequireUser_TypeMismatch(t *testing.T) {
	ctx := auth.SetUser(t.Context(), "string-user")

	got, err := auth.RequireUser[*testUser](ctx)
	if !errors.Is(err, auth.ErrUserMissing) {
		t.Fatalf("expected ErrUserMissing, got %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// --- Middleware tests ---

func TestMiddleware_AuthSuccess(t *testing.T) {
	user := &testUser{ID: 1, Name: "alice"}
	authenticator := &mockAuthenticator[*testUser]{user: user}

	var captured *testUser
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		u, ok := auth.GetUser[*testUser](ctx.Request().Request.Context())
		if !ok {
			t.Error("expected user in context")
		}
		captured = u
		return ctx.Response().NoContent(200)
	}).Middleware(auth.Middleware[*testUser](authenticator, nil))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if captured == nil || captured.ID != 1 {
		t.Errorf("captured user = %+v, want {ID:1, Name:alice}", captured)
	}
}

func TestMiddleware_AuthFailure_DefaultError(t *testing.T) {
	authenticator := &mockAuthenticator[*testUser]{
		err: errors.New("invalid token"),
	}

	handlerCalled := false
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		handlerCalled = true
		return ctx.Response().NoContent(200)
	}).Middleware(auth.Middleware[*testUser](authenticator, nil))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if handlerCalled {
		t.Error("handler should not be called on auth failure")
	}
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestMiddleware_AuthFailure_CustomError(t *testing.T) {
	authenticator := &mockAuthenticator[*testUser]{
		err: errors.New("bad creds"),
	}

	var capturedErr error
	onError := func(err error, ctx *credo.Context) error {
		capturedErr = err
		return ctx.Response().JSON(403, map[string]string{"error": "forbidden"})
	}

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).Middleware(auth.Middleware[*testUser](authenticator, onError))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 403 {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if capturedErr == nil || capturedErr.Error() != "bad creds" {
		t.Errorf("capturedErr = %v, want 'bad creds'", capturedErr)
	}
}

func TestMiddleware_AuthFailure_CustomErrorReturnsNil_UsesDefaultUnauthorized(t *testing.T) {
	authErr := errors.New("expired")
	authenticator := &mockAuthenticator[*testUser]{err: authErr}

	onErrorCalled := false
	onError := func(err error, ctx *credo.Context) error {
		onErrorCalled = true
		if !errors.Is(err, authErr) {
			t.Errorf("expected onError err to wrap authErr, got %v", err)
		}
		return nil
	}

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		t.Fatal("handler should not be called")
		return nil
	}).Middleware(auth.Middleware[*testUser](authenticator, onError))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if !onErrorCalled {
		t.Fatal("expected onError to be called")
	}
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestMiddleware_NilAuthenticator_DefaultUnauthorized(t *testing.T) {
	var authenticator auth.Authenticator[*testUser]

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		t.Fatal("handler should not be called")
		return nil
	}).Middleware(auth.Middleware[*testUser](authenticator, nil))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestMiddleware_TypedNilAuthenticator_DefaultUnauthorized(t *testing.T) {
	var typedNil *mockAuthenticator[*testUser]
	var authenticator auth.Authenticator[*testUser] = typedNil

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		t.Fatal("handler should not be called")
		return nil
	}).Middleware(auth.Middleware[*testUser](authenticator, nil))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestMiddleware_UserAccessibleInHandler(t *testing.T) {
	user := &testUser{ID: 99, Name: "charlie"}
	authenticator := &mockAuthenticator[*testUser]{user: user}

	app := mustNew(t)
	app.GET("/profile", func(ctx *credo.Context) error {
		u, ok := auth.GetUser[*testUser](ctx.Request().Request.Context())
		if !ok {
			t.Fatal("expected user in handler context")
		}
		return ctx.Response().JSON(200, map[string]any{
			"id":   u.ID,
			"name": u.Name,
		})
	}).Middleware(auth.Middleware[*testUser](authenticator, nil))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/profile", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestMiddleware_IntegrationWithApp(t *testing.T) {
	user := &testUser{ID: 7, Name: "eve"}
	authenticator := &mockAuthenticator[*testUser]{user: user}

	app := mustNew(t)

	// Apply middleware at group level
	g := app.Group("/api")
	g.Middleware(auth.Middleware[*testUser](authenticator, nil))
	g.GET("/me", func(ctx *credo.Context) error {
		u, ok := auth.GetUser[*testUser](ctx.Request().Request.Context())
		if !ok {
			return credo.ErrUnauthorized
		}
		return ctx.Response().JSON(200, map[string]string{"name": u.Name})
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/me", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- Edge case tests ---

func TestGetUser_ViaContext(t *testing.T) {
	// Verify GetUser works through the ctx.Context() accessor — the
	// blessed path for handing the request's context.Context to auth.
	user := &testUser{ID: 5, Name: "dave"}

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		u, ok := auth.GetUser[*testUser](ctx.Context())
		if !ok {
			t.Error("expected user via ctx.Context()")
		}
		if u.ID != 5 {
			t.Errorf("got ID=%d, want 5", u.ID)
		}
		return ctx.Response().NoContent(200)
	}).Middleware(auth.Middleware[*testUser](
		&mockAuthenticator[*testUser]{user: user}, nil,
	))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestMiddleware_ErrorWrapsInternal(t *testing.T) {
	authenticator := &mockAuthenticator[*testUser]{err: errors.New("token expired")}

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		t.Fatal("handler should not be called")
		return nil
	}).Middleware(auth.Middleware[*testUser](authenticator, nil))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	// Framework classifies *HTTPError internally → 401 response.
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func mustNew(t *testing.T, opts ...credo.Option) *credo.App {
	t.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

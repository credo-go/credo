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

// --- Middleware tests ---

func TestMiddleware_AuthSuccess(t *testing.T) {
	user := &testUser{ID: 1, Name: "alice"}
	authenticator := &mockAuthenticator[*testUser]{user: user}

	var captured *testUser
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		u, ok := ctx.GetUser[*testUser]()
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
		u, ok := ctx.GetUser[*testUser]()
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
		u, ok := ctx.GetUser[*testUser]()
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

func TestMiddleware_PrincipalViaContextMethod(t *testing.T) {
	// Verify the user stored by auth.Middleware is readable in the handler
	// through the ctx.GetUser method — the only public principal accessor.
	user := &testUser{ID: 5, Name: "dave"}

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		u, ok := ctx.GetUser[*testUser]()
		if !ok {
			t.Error("expected user via ctx.GetUser")
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

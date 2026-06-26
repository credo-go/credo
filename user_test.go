package credo_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
)

type princUser struct {
	ID   int
	Name string
}

type svcAccount struct {
	Name string
}

// newPrincipalCtx returns a *credo.Context backed by a fresh request, for
// unit-testing the user accessor methods in isolation.
func newPrincipalCtx() *credo.Context {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	return credo.NewContext(w, r)
}

func TestContext_SetGetUser_Pointer(t *testing.T) {
	ctx := newPrincipalCtx()
	user := &princUser{ID: 1, Name: "alice"}

	ctx.SetUser(user)
	got, ok := ctx.GetUser[*princUser]()

	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.ID != 1 || got.Name != "alice" {
		t.Errorf("got %+v, want {ID:1 Name:alice}", got)
	}
}

func TestContext_GetUser_NotSet(t *testing.T) {
	ctx := newPrincipalCtx()

	got, ok := ctx.GetUser[*princUser]()
	if ok {
		t.Error("expected ok=false when no user set")
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestContext_GetUser_TypeMismatch(t *testing.T) {
	ctx := newPrincipalCtx()
	ctx.SetUser("string-user")

	got, ok := ctx.GetUser[*princUser]()
	if ok {
		t.Error("expected ok=false for type mismatch")
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestContext_SetUser_NilPointer(t *testing.T) {
	ctx := newPrincipalCtx()
	ctx.SetUser[*princUser](nil)

	got, ok := ctx.GetUser[*princUser]()
	if !ok {
		t.Fatal("expected ok=true for a stored nil pointer")
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestContext_SetUser_StructValue(t *testing.T) {
	ctx := newPrincipalCtx()
	ctx.SetUser(princUser{ID: 42, Name: "bob"})

	got, ok := ctx.GetUser[princUser]()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.ID != 42 || got.Name != "bob" {
		t.Errorf("got %+v, want {ID:42 Name:bob}", got)
	}
}

func TestContext_SetUser_DistinctTypesCoexist(t *testing.T) {
	ctx := newPrincipalCtx()
	ctx.SetUser(&princUser{ID: 1, Name: "alice"})
	ctx.SetUser(&svcAccount{Name: "ci-bot"})

	u, ok := ctx.GetUser[*princUser]()
	if !ok || u.Name != "alice" {
		t.Errorf("user slot = (%+v, %v), want alice", u, ok)
	}
	sa, ok := ctx.GetUser[*svcAccount]()
	if !ok || sa.Name != "ci-bot" {
		t.Errorf("service-account slot = (%+v, %v), want ci-bot", sa, ok)
	}
}

func TestContext_RequireUser_Success(t *testing.T) {
	ctx := newPrincipalCtx()
	ctx.SetUser(&princUser{ID: 1, Name: "alice"})

	got, err := ctx.RequireUser[*princUser]()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != 1 {
		t.Errorf("got %+v, want ID=1", got)
	}
}

func TestContext_RequireUser_Missing(t *testing.T) {
	ctx := newPrincipalCtx()

	got, err := ctx.RequireUser[*princUser]()
	if !errors.Is(err, credo.ErrUserMissing) {
		t.Fatalf("expected ErrUserMissing, got %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestContext_RequireUser_TypeMismatch(t *testing.T) {
	ctx := newPrincipalCtx()
	ctx.SetUser("string-user")

	if _, err := ctx.RequireUser[*princUser](); !errors.Is(err, credo.ErrUserMissing) {
		t.Fatalf("expected ErrUserMissing, got %v", err)
	}
}

// TestRequireUser_RendersUnauthorized verifies the end-to-end render path:
// a handler that returns the RequireUser error produces a 401, because
// ErrUserMissing is wrapped in ErrUnauthorized.
func TestRequireUser_RendersUnauthorized(t *testing.T) {
	app := mustNew(t)
	app.GET("/me", func(ctx *credo.Context) error {
		_, err := ctx.RequireUser[*princUser]()
		return err
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/me", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

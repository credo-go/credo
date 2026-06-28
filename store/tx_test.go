package store_test

import (
	"context"
	"testing"

	"github.com/credo-go/credo/store"
)

func TestWithTx_GetTx_RoundTrip(t *testing.T) {
	ctx := context.Background()
	tx := "mock-tx-handle"

	ctx = store.WithTx[string](ctx, tx)
	got, ok := store.GetTx[string](ctx)
	if !ok {
		t.Fatal("GetTx returned false, want true")
	}
	if got != tx {
		t.Errorf("GetTx = %q, want %q", got, tx)
	}
}

func TestGetTx_NotPresent(t *testing.T) {
	ctx := context.Background()
	got, ok := store.GetTx[string](ctx)
	if ok {
		t.Fatal("GetTx returned true for empty context, want false")
	}
	if got != "" {
		t.Errorf("GetTx = %q, want zero value", got)
	}
}

func TestConn_WithTx(t *testing.T) {
	ctx := context.Background()
	tx := "active-tx"
	fallback := "fallback-conn"

	ctx = store.WithTx[string](ctx, tx)
	got := store.Conn[string](ctx, fallback)
	if got != tx {
		t.Errorf("Conn = %q, want TX %q", got, tx)
	}
}

func TestConn_WithoutTx(t *testing.T) {
	ctx := context.Background()
	fallback := "fallback-conn"

	got := store.Conn[string](ctx, fallback)
	if got != fallback {
		t.Errorf("Conn = %q, want fallback %q", got, fallback)
	}
}

// testTxA and testTxB are distinct types to verify type-keyed isolation.
type testTxA struct{ id string }
type testTxB struct{ id string }

func TestWithTx_DifferentTypes_NoCollision(t *testing.T) {
	ctx := context.Background()
	a := testTxA{id: "tx-a"}
	b := testTxB{id: "tx-b"}

	ctx = store.WithTx[testTxA](ctx, a)
	ctx = store.WithTx[testTxB](ctx, b)

	gotA, okA := store.GetTx[testTxA](ctx)
	gotB, okB := store.GetTx[testTxB](ctx)

	if !okA || gotA.id != "tx-a" {
		t.Errorf("GetTx[testTxA] = %v, %v; want {tx-a}, true", gotA, okA)
	}
	if !okB || gotB.id != "tx-b" {
		t.Errorf("GetTx[testTxB] = %v, %v; want {tx-b}, true", gotB, okB)
	}
}

func TestWithTxInScope_SameType_NoCollision(t *testing.T) {
	ctx := context.Background()
	scopeA := store.NewTxScope()
	scopeB := store.NewTxScope()

	ctx = store.WithTxInScope[string](ctx, scopeA, "tx-a")

	gotA, okA := store.GetTxInScope[string](ctx, scopeA)
	if !okA || gotA != "tx-a" {
		t.Fatalf("GetTxInScope(scopeA) = %q, %v", gotA, okA)
	}

	gotB, okB := store.GetTxInScope[string](ctx, scopeB)
	if okB {
		t.Fatalf("GetTxInScope(scopeB) unexpectedly found tx %q", gotB)
	}
	if got := store.ConnInScope[string](ctx, scopeB, "fallback"); got != "fallback" {
		t.Fatalf("ConnInScope(scopeB) = %q, want fallback", got)
	}
}

func TestTxScope_Methods_RoundTrip(t *testing.T) {
	ctx := context.Background()
	scope := store.NewTxScope()

	ctx = scope.WithTx[string](ctx, "scoped-tx")

	got, ok := scope.GetTx[string](ctx)
	if !ok || got != "scoped-tx" {
		t.Fatalf("scope.GetTx = %q, %v; want %q, true", got, ok, "scoped-tx")
	}
	if conn := scope.Conn[string](ctx, "fallback"); conn != "scoped-tx" {
		t.Errorf("scope.Conn = %q, want TX %q", conn, "scoped-tx")
	}
}

func TestTxScope_Conn_Fallback(t *testing.T) {
	ctx := context.Background()
	scope := store.NewTxScope()

	if conn := scope.Conn[string](ctx, "fallback"); conn != "fallback" {
		t.Errorf("scope.Conn (no tx) = %q, want fallback", conn)
	}
	if _, ok := scope.GetTx[string](ctx); ok {
		t.Error("scope.GetTx returned true for empty context, want false")
	}
}

// The methods are sugar over the WithTxInScope/GetTxInScope/ConnInScope free
// functions, so a value stored through a method must be readable through the
// matching free function and vice versa — same scope, same key.
func TestTxScope_Methods_MatchFreeFunctions(t *testing.T) {
	ctx := context.Background()
	scope := store.NewTxScope()

	ctx = scope.WithTx[string](ctx, "via-method")
	if got, ok := store.GetTxInScope[string](ctx, scope); !ok || got != "via-method" {
		t.Errorf("GetTxInScope after scope.WithTx = %q, %v; want %q, true", got, ok, "via-method")
	}

	ctx = store.WithTxInScope[string](ctx, scope, "via-free-fn")
	if got, ok := scope.GetTx[string](ctx); !ok || got != "via-free-fn" {
		t.Errorf("scope.GetTx after WithTxInScope = %q, %v; want %q, true", got, ok, "via-free-fn")
	}
}

func TestTxScope_Methods_DistinctScopes_NoCollision(t *testing.T) {
	ctx := context.Background()
	scopeA := store.NewTxScope()
	scopeB := store.NewTxScope()

	ctx = scopeA.WithTx[string](ctx, "tx-a")

	if got, ok := scopeA.GetTx[string](ctx); !ok || got != "tx-a" {
		t.Fatalf("scopeA.GetTx = %q, %v; want %q, true", got, ok, "tx-a")
	}
	if got, ok := scopeB.GetTx[string](ctx); ok {
		t.Fatalf("scopeB.GetTx unexpectedly found %q", got)
	}
	if conn := scopeB.Conn[string](ctx, "fallback"); conn != "fallback" {
		t.Errorf("scopeB.Conn = %q, want fallback", conn)
	}
}

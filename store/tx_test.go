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

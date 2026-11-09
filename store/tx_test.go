package store_test

import (
	"errors"
	"testing"

	"github.com/credo-go/credo/store"
)

func TestTxScope_Methods_RoundTrip(t *testing.T) {
	ctx := t.Context()
	scope := store.NewTxScope[string]()

	ctx = scope.WithTx(ctx, "scoped-tx")

	got, ok := scope.GetTx(ctx)
	if !ok || got != "scoped-tx" {
		t.Fatalf("scope.GetTx = %q, %v; want %q, true", got, ok, "scoped-tx")
	}
	if conn := scope.Conn(ctx, "fallback"); conn != "scoped-tx" {
		t.Errorf("scope.Conn = %q, want TX %q", conn, "scoped-tx")
	}
}

func TestTxScope_Conn_Fallback(t *testing.T) {
	ctx := t.Context()
	scope := store.NewTxScope[string]()

	if conn := scope.Conn(ctx, "fallback"); conn != "fallback" {
		t.Errorf("scope.Conn (no tx) = %q, want fallback", conn)
	}
	if _, ok := scope.GetTx(ctx); ok {
		t.Error("scope.GetTx returned true for empty context, want false")
	}
}

func TestTxScope_Methods_DistinctScopes_NoCollision(t *testing.T) {
	ctx := t.Context()
	scopeA := store.NewTxScope[string]()
	scopeB := store.NewTxScope[string]()

	ctx = scopeA.WithTx(ctx, "tx-a")

	if got, ok := scopeA.GetTx(ctx); !ok || got != "tx-a" {
		t.Fatalf("scopeA.GetTx = %q, %v; want %q, true", got, ok, "tx-a")
	}
	if got, ok := scopeB.GetTx(ctx); ok {
		t.Fatalf("scopeB.GetTx unexpectedly found %q", got)
	}
	if conn := scopeB.Conn(ctx, "fallback"); conn != "fallback" {
		t.Errorf("scopeB.Conn = %q, want fallback", conn)
	}
}

type testConnection interface {
	connectionID() string
}

type concreteTestTx struct {
	id string
}

func (tx *concreteTestTx) connectionID() string { return tx.id }

func TestTxScope_ConcreteValueRoundTripsThroughInterfaceScope(t *testing.T) {
	scope := store.NewTxScope[testConnection]()
	tx := &concreteTestTx{id: "tx-interface"}

	ctx := scope.WithTx(t.Context(), tx)
	got, ok := scope.GetTx(ctx)
	if !ok {
		t.Fatal("scope.GetTx returned false, want true")
	}
	if got != tx {
		t.Fatalf("scope.GetTx = %T %v, want original %T pointer", got, got, tx)
	}
	if got.connectionID() != "tx-interface" {
		t.Errorf("connectionID = %q, want tx-interface", got.connectionID())
	}
}

func TestTxScope_RequireTx(t *testing.T) {
	scope := store.NewTxScope[string]()

	got, err := scope.RequireTx(t.Context())
	if !errors.Is(err, store.ErrTxMissing) {
		t.Fatalf("RequireTx without transaction = %q, %v; want ErrTxMissing", got, err)
	}

	ctx := scope.WithTx(t.Context(), "required-tx")
	got, err = scope.RequireTx(ctx)
	if err != nil || got != "required-tx" {
		t.Fatalf("RequireTx with transaction = %q, %v; want required-tx, nil", got, err)
	}
}

func TestTxScope_NestedContextShadowsWithoutMutatingParent(t *testing.T) {
	scope := store.NewTxScope[string]()
	outer := scope.WithTx(t.Context(), "outer")
	inner := scope.WithTx(outer, "inner")

	if got, _ := scope.GetTx(inner); got != "inner" {
		t.Errorf("inner GetTx = %q, want inner", got)
	}
	if got, _ := scope.GetTx(outer); got != "outer" {
		t.Errorf("outer GetTx after child write = %q, want outer", got)
	}
}

func TestTxScope_DifferentTypesAndScopesStayIsolated(t *testing.T) {
	strings := store.NewTxScope[string]()
	ints := store.NewTxScope[int]()
	otherStrings := store.NewTxScope[string]()

	ctx := strings.WithTx(t.Context(), "string-tx")
	ctx = ints.WithTx(ctx, 42)

	if got, ok := strings.GetTx(ctx); !ok || got != "string-tx" {
		t.Fatalf("strings.GetTx = %q, %v", got, ok)
	}
	if got, ok := ints.GetTx(ctx); !ok || got != 42 {
		t.Fatalf("ints.GetTx = %d, %v", got, ok)
	}
	if _, ok := otherStrings.GetTx(ctx); ok {
		t.Fatal("distinct string scope unexpectedly found transaction")
	}
}

func TestTxScope_NilPanics(t *testing.T) {
	var scope *store.TxScope[string]
	ctx := t.Context()
	for name, call := range map[string]func(){
		"WithTx":    func() { scope.WithTx(ctx, "tx") },
		"GetTx":     func() { scope.GetTx(ctx) },
		"RequireTx": func() { _, _ = scope.RequireTx(ctx) },
		"Conn":      func() { scope.Conn(ctx, "fallback") },
	} {
		t.Run(name, func(t *testing.T) {
			assertPanicValue(t, "store: tx scope must not be nil", call)
		})
	}
}

func TestTxScope_WithTxNilPointerPanics(t *testing.T) {
	scope := store.NewTxScope[*concreteTestTx]()
	assertPanicValue(t, "store: transaction must not be nil", func() {
		scope.WithTx(t.Context(), nil)
	})
}

func TestTxScope_WithTxTypedNilInterfacePanics(t *testing.T) {
	scope := store.NewTxScope[testConnection]()
	var concrete *concreteTestTx
	var tx testConnection = concrete

	assertPanicValue(t, "store: transaction must not be nil", func() {
		scope.WithTx(t.Context(), tx)
	})
}

func assertPanicValue(t *testing.T, want any, fn func()) {
	t.Helper()
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("panic = %v, want %v", got, want)
		}
	}()
	fn()
}

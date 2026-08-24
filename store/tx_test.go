package store_test

import (
	"errors"
	"testing"

	"github.com/credo-go/credo/store"
)

func TestWithTx_GetTx_RoundTrip(t *testing.T) {
	ctx := t.Context()
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
	ctx := t.Context()
	got, ok := store.GetTx[string](ctx)
	if ok {
		t.Fatal("GetTx returned true for empty context, want false")
	}
	if got != "" {
		t.Errorf("GetTx = %q, want zero value", got)
	}
}

func TestConn_WithTx(t *testing.T) {
	ctx := t.Context()
	tx := "active-tx"
	fallback := "fallback-conn"

	ctx = store.WithTx[string](ctx, tx)
	got := store.Conn[string](ctx, fallback)
	if got != tx {
		t.Errorf("Conn = %q, want TX %q", got, tx)
	}
}

func TestConn_WithoutTx(t *testing.T) {
	ctx := t.Context()
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
	ctx := t.Context()
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
	ctx := t.Context()
	scopeA := store.NewTxScope[string]()
	scopeB := store.NewTxScope[string]()

	ctx = store.WithTxInScope(ctx, scopeA, "tx-a")

	gotA, okA := store.GetTxInScope(ctx, scopeA)
	if !okA || gotA != "tx-a" {
		t.Fatalf("GetTxInScope(scopeA) = %q, %v", gotA, okA)
	}

	gotB, okB := store.GetTxInScope(ctx, scopeB)
	if okB {
		t.Fatalf("GetTxInScope(scopeB) unexpectedly found tx %q", gotB)
	}
	if got := store.ConnInScope(ctx, scopeB, "fallback"); got != "fallback" {
		t.Fatalf("ConnInScope(scopeB) = %q, want fallback", got)
	}
}

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

// The methods are sugar over the WithTxInScope/GetTxInScope/ConnInScope free
// functions, so a value stored through a method must be readable through the
// matching free function and vice versa — same scope, same key.
func TestTxScope_Methods_MatchFreeFunctions(t *testing.T) {
	ctx := t.Context()
	scope := store.NewTxScope[string]()

	ctx = scope.WithTx(ctx, "via-method")
	if got, ok := store.GetTxInScope(ctx, scope); !ok || got != "via-method" {
		t.Errorf("GetTxInScope after scope.WithTx = %q, %v; want %q, true", got, ok, "via-method")
	}

	ctx = store.WithTxInScope(ctx, scope, "via-free-fn")
	if got, ok := scope.GetTx(ctx); !ok || got != "via-free-fn" {
		t.Errorf("scope.GetTx after WithTxInScope = %q, %v; want %q, true", got, ok, "via-free-fn")
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

	freeCtx := store.WithTxInScope[testConnection](t.Context(), scope, tx)
	freeGot, ok := store.GetTxInScope(freeCtx, scope)
	if !ok || freeGot != tx {
		t.Fatalf("free scoped round-trip = %T %v, %v; want original pointer", freeGot, freeGot, ok)
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

	freeGot, err := store.RequireTxInScope(ctx, scope)
	if err != nil || freeGot != got {
		t.Fatalf("RequireTxInScope = %q, %v; want %q, nil", freeGot, err, got)
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
	defer func() {
		if got := recover(); got != "store: tx scope must not be nil" {
			t.Fatalf("panic = %v, want nil-scope message", got)
		}
	}()
	scope.WithTx(t.Context(), "tx")
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

func TestWithTx_UnscopedTypedNilPanics(t *testing.T) {
	var concrete *concreteTestTx
	var tx testConnection = concrete

	assertPanicValue(t, "store: transaction must not be nil", func() {
		store.WithTx(t.Context(), tx)
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

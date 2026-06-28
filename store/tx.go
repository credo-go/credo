package store

import "context"

// txKey is a generic context key type for storing transactions.
// Each Go type T gets its own zero-size key, so different TX types
// don't collide in the context.
type txKey[T any] struct{}

// TxScope isolates transactions that share the same Go type but belong to
// different logical connections.
//
// The marker field keeps TxScope non-zero-size on purpose: in Go, pointers to
// distinct zero-size structs may share an address, which would let two
// independent scopes collide as context keys. A non-empty field guarantees a
// unique address per NewTxScope call.
type TxScope struct {
	marker byte
}

// NewTxScope creates a unique transaction scope.
func NewTxScope() *TxScope {
	return &TxScope{marker: 1}
}

type scopedTxKey[T any] struct {
	scope *TxScope
}

// WithTx stores a transaction handle in the context.
// T is the transaction type (e.g., bun.Tx, bun.IDB).
func WithTx[T any](ctx context.Context, tx T) context.Context {
	return context.WithValue(ctx, txKey[T]{}, tx)
}

// WithTxInScope stores a transaction handle in the context for a specific
// logical connection scope.
// Panics if scope is nil — scopes are created once at wiring time via
// [NewTxScope], so a nil scope is a programming error.
func WithTxInScope[T any](ctx context.Context, scope *TxScope, tx T) context.Context {
	return context.WithValue(ctx, scopedTxKey[T]{scope: requireTxScope(scope)}, tx)
}

// GetTx retrieves a transaction handle from the context.
// Returns the zero value and false if no TX of type T is stored.
func GetTx[T any](ctx context.Context) (T, bool) {
	tx, ok := ctx.Value(txKey[T]{}).(T)
	return tx, ok
}

// GetTxInScope retrieves a scoped transaction handle from the context.
// Returns the zero value and false if no TX of type T is stored for the scope.
// Panics if scope is nil.
func GetTxInScope[T any](ctx context.Context, scope *TxScope) (T, bool) {
	tx, ok := ctx.Value(scopedTxKey[T]{scope: requireTxScope(scope)}).(T)
	return tx, ok
}

// Conn returns the transaction from context if present, otherwise
// returns the fallback connection. Repositories call this in every
// method for opt-in TX participation.
//
// Multi-DB caveat: GetTx/Conn are keyed by Go type T. If two databases
// use the same client type (e.g., two *bun.DB instances), context TX
// cannot distinguish between them. Use the explicit TX pattern (native
// ORM API) for multi-DB same-type scenarios.
func Conn[T any](ctx context.Context, fallback T) T {
	if tx, ok := GetTx[T](ctx); ok {
		return tx
	}
	return fallback
}

// ConnInScope returns the transaction from context for the given scope if
// present, otherwise returns the fallback connection.
// Panics if scope is nil.
func ConnInScope[T any](ctx context.Context, scope *TxScope, fallback T) T {
	if tx, ok := GetTxInScope[T](ctx, scope); ok {
		return tx
	}
	return fallback
}

func requireTxScope(scope *TxScope) *TxScope {
	if scope == nil {
		panic("store: tx scope must not be nil")
	}
	return scope
}

// --- TxScope methods (ergonomic scoped form) ---
//
// These are the method form of the WithTxInScope / GetTxInScope / ConnInScope
// free functions: s.WithTx[T](ctx, tx) reads better than the free-function
// call WithTxInScope[T](ctx, s, tx) and keeps the scope on the value the caller
// already holds. They are additive sugar — same keying, so a value stored
// through a method is readable through the matching free function and vice
// versa — and a nil scope panics identically.

// WithTx stores a transaction handle in the context for this scope.
// It is the method form of [WithTxInScope].
func (s *TxScope) WithTx[T any](ctx context.Context, tx T) context.Context {
	return WithTxInScope[T](ctx, s, tx)
}

// GetTx retrieves this scope's transaction handle from the context, returning
// the zero value and false when none is stored.
// It is the method form of [GetTxInScope].
func (s *TxScope) GetTx[T any](ctx context.Context) (T, bool) {
	return GetTxInScope[T](ctx, s)
}

// Conn returns this scope's transaction from the context if present, otherwise
// the fallback connection — the call repositories make for opt-in scoped TX
// participation. It is the method form of [ConnInScope].
func (s *TxScope) Conn[T any](ctx context.Context, fallback T) T {
	return ConnInScope[T](ctx, s, fallback)
}

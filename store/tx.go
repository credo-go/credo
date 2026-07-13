package store

import (
	"context"
	"errors"
	"reflect"
)

// txKey is a generic context key type for storing transactions.
// Each Go type T gets its own zero-size key, so different TX types
// don't collide in the context.
type txKey[T any] struct{}

// ErrTxMissing indicates that a transaction-required operation was called
// without a transaction in its context scope.
var ErrTxMissing = errors.New("store: transaction missing from context")

// TxScope isolates transactions that share the same Go type but belong to
// different logical connections. T is fixed when the scope is created, so a
// concrete transaction cannot be stored and then silently missed by reading
// the same scope through a different interface type.
//
// The marker field keeps TxScope non-zero-size on purpose: in Go, pointers to
// distinct zero-size structs may share an address, which would let two
// independent scopes collide as context keys. A non-empty field guarantees a
// unique address per NewTxScope call.
type TxScope[T any] struct {
	marker byte
}

// NewTxScope creates a unique transaction scope whose transaction type is T.
func NewTxScope[T any]() *TxScope[T] {
	return &TxScope[T]{marker: 1}
}

type scopedTxKey[T any] struct {
	scope *TxScope[T]
}

// WithTx stores a transaction handle in the context.
// T is the transaction type (e.g., bun.Tx, bun.IDB).
// Panics if tx is nil, including a typed-nil pointer stored in an interface T.
//
// Deprecated: create a typed [TxScope] with [NewTxScope] and use its WithTx
// method. A standalone type-keyed helper cannot prevent a producer and
// consumer from choosing different concrete/interface forms of the same
// transaction type.
func WithTx[T any](ctx context.Context, tx T) context.Context {
	return context.WithValue(ctx, txKey[T]{}, requireNonNilTx(tx))
}

// WithTxInScope stores a transaction handle in the context for a specific
// logical connection scope.
// Panics if scope is nil — scopes are created once at wiring time via
// [NewTxScope], so a nil scope is a programming error. Panics if tx is nil,
// including a typed-nil pointer stored in an interface T.
func WithTxInScope[T any](ctx context.Context, scope *TxScope[T], tx T) context.Context {
	return context.WithValue(ctx, scopedTxKey[T]{scope: requireTxScope(scope)}, requireNonNilTx(tx))
}

// GetTx retrieves a transaction handle from the context.
// Returns the zero value and false if no TX of type T is stored.
//
// Deprecated: create a typed [TxScope] with [NewTxScope] and use its GetTx
// method. A standalone type-keyed helper cannot prevent concrete/interface
// type mismatches across call sites.
func GetTx[T any](ctx context.Context) (T, bool) {
	tx, ok := ctx.Value(txKey[T]{}).(T)
	return tx, ok
}

// GetTxInScope retrieves a scoped transaction handle from the context.
// Returns the zero value and false if no TX of type T is stored for the scope.
// Panics if scope is nil.
func GetTxInScope[T any](ctx context.Context, scope *TxScope[T]) (T, bool) {
	tx, ok := ctx.Value(scopedTxKey[T]{scope: requireTxScope(scope)}).(T)
	return tx, ok
}

// RequireTxInScope retrieves a scoped transaction handle or returns
// [ErrTxMissing] when the scope has no transaction in ctx.
// Panics if scope is nil.
func RequireTxInScope[T any](ctx context.Context, scope *TxScope[T]) (T, error) {
	if tx, ok := GetTxInScope(ctx, scope); ok {
		return tx, nil
	}
	var zero T
	return zero, ErrTxMissing
}

// Conn returns the transaction from context if present, otherwise
// returns the fallback connection. Repositories call this in every
// method for opt-in TX participation.
//
// Deprecated: create a typed [TxScope] with [NewTxScope] and use its Conn
// method. A standalone type-keyed helper cannot isolate multiple logical
// connections that share T or prevent concrete/interface mismatches.
func Conn[T any](ctx context.Context, fallback T) T {
	if tx, ok := GetTx[T](ctx); ok {
		return tx
	}
	return fallback
}

// ConnInScope returns the transaction from context for the given scope if
// present, otherwise returns the fallback connection.
// Panics if scope is nil.
func ConnInScope[T any](ctx context.Context, scope *TxScope[T], fallback T) T {
	if tx, ok := GetTxInScope(ctx, scope); ok {
		return tx
	}
	return fallback
}

func requireTxScope[T any](scope *TxScope[T]) *TxScope[T] {
	if scope == nil {
		panic("store: tx scope must not be nil")
	}
	return scope
}

func requireNonNilTx[T any](tx T) T {
	v := reflect.ValueOf(tx)
	if !v.IsValid() {
		panic("store: transaction must not be nil")
	}
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			panic("store: transaction must not be nil")
		}
	}
	return tx
}

// --- TxScope methods (ergonomic scoped form) ---
//
// These are the method form of the WithTxInScope / GetTxInScope /
// RequireTxInScope / ConnInScope free functions. The scope fixes T once, so
// every operation uses the same transaction type. A value stored through a
// method is readable through the matching free function and vice versa. A nil
// scope panics identically.

// WithTx stores a transaction handle in the context for this scope.
// It is the method form of [WithTxInScope]. It panics for nil transaction
// values, including typed-nil pointers held by an interface T.
func (s *TxScope[T]) WithTx(ctx context.Context, tx T) context.Context {
	return WithTxInScope(ctx, s, tx)
}

// GetTx retrieves this scope's transaction handle from the context, returning
// the zero value and false when none is stored.
// It is the method form of [GetTxInScope].
func (s *TxScope[T]) GetTx(ctx context.Context) (T, bool) {
	return GetTxInScope(ctx, s)
}

// RequireTx retrieves this scope's transaction handle or returns
// [ErrTxMissing] when none is stored. It never falls back to a base connection.
// It is the method form of [RequireTxInScope].
func (s *TxScope[T]) RequireTx(ctx context.Context) (T, error) {
	return RequireTxInScope(ctx, s)
}

// Conn returns this scope's transaction from the context if present, otherwise
// the fallback connection — the call repositories make for opt-in scoped TX
// participation. It is the method form of [ConnInScope].
func (s *TxScope[T]) Conn(ctx context.Context, fallback T) T {
	return ConnInScope(ctx, s, fallback)
}

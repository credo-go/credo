package store

import (
	"context"
	"errors"
	"reflect"
)

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

// --- TxScope methods: the transaction scope API ---
//
// The scope fixes T once, so every operation uses the same transaction type.
// Each method panics on a nil scope: scopes are created once at wiring time
// with [NewTxScope], so a nil scope is a programming error.

// WithTx stores a transaction handle in the context for this scope. It panics
// for nil transaction values, including typed-nil pointers held by an
// interface T.
func (s *TxScope[T]) WithTx(ctx context.Context, tx T) context.Context {
	return context.WithValue(ctx, scopedTxKey[T]{scope: requireTxScope(s)}, requireNonNilTx(tx))
}

// GetTx retrieves this scope's transaction handle from the context, returning
// the zero value and false when none is stored.
func (s *TxScope[T]) GetTx(ctx context.Context) (T, bool) {
	tx, ok := ctx.Value(scopedTxKey[T]{scope: requireTxScope(s)}).(T)
	return tx, ok
}

// RequireTx retrieves this scope's transaction handle or returns
// [ErrTxMissing] when none is stored. It never falls back to a base connection.
func (s *TxScope[T]) RequireTx(ctx context.Context) (T, error) {
	if tx, ok := s.GetTx(ctx); ok {
		return tx, nil
	}
	var zero T
	return zero, ErrTxMissing
}

// Conn returns this scope's transaction from the context if present, otherwise
// the fallback connection — the call repositories make for opt-in scoped TX
// participation.
func (s *TxScope[T]) Conn(ctx context.Context, fallback T) T {
	if tx, ok := s.GetTx(ctx); ok {
		return tx
	}
	return fallback
}

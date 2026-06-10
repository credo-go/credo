package sqldb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/store"
)

// RunInTx starts a transaction, stores it in context via store.WithTx,
// executes fn, and commits on nil / rolls back on error.
// Nested calls use Bun's SAVEPOINT automatically.
// If fn panics, the transaction is rolled back and the panic is re-raised.
//
// [DB.InTx] is the method form of this function.
func RunInTx(ctx context.Context, db *DB, fn func(ctx context.Context) error) error {
	return RunInTxWith(ctx, db, nil, fn)
}

// InTx is the method form of [RunInTx]: it starts a transaction, stores it
// in the context passed to fn, and commits on nil / rolls back on error.
// Nested calls use Bun's SAVEPOINT automatically. If fn panics, the
// transaction is rolled back and the panic is re-raised.
//
// In a handler, call it with the request context:
//
//	err := db.InTx(ctx.Context(), func(ctx context.Context) error {
//	    // repos called with this ctx pick up the TX automatically
//	    return svc.Transfer(ctx, from, to, amount)
//	})
func (db *DB) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return RunInTxWith(ctx, db, nil, fn)
}

// InTxWith is like [DB.InTx] but accepts sql.TxOptions for configuring
// isolation level and read-only mode. It is the method form of [RunInTxWith].
func (db *DB) InTxWith(ctx context.Context, opts *sql.TxOptions, fn func(ctx context.Context) error) error {
	return RunInTxWith(ctx, db, opts, fn)
}

// RunInTxWith is like RunInTx but accepts sql.TxOptions for configuring
// isolation level and read-only mode.
//
// [DB.InTxWith] is the method form of this function.
func RunInTxWith(ctx context.Context, db *DB, opts *sql.TxOptions, fn func(ctx context.Context) error) error {
	if db == nil {
		return fmt.Errorf("sqldb: db must not be nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Use existing TX from context if present (creates SAVEPOINT),
	// otherwise use the raw DB (creates a new transaction).
	conn := db.conn(ctx)

	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		return mapError(fmt.Errorf("sqldb: begin tx: %w", err))
	}

	// Store TX in context so this DB's proxies and raw helpers pick it up.
	txCtx := store.WithTxInScope[bun.IDB](ctx, db.txScope, tx)

	// Execute with panic recovery.
	var fnErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				_ = tx.Rollback()
				panic(r)
			}
		}()
		fnErr = fn(txCtx)
	}()

	if fnErr != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return mapError(fmt.Errorf("sqldb: rollback: %w (original: %w)", rbErr, fnErr))
		}
		return mapError(fnErr)
	}

	if err := tx.Commit(); err != nil {
		return mapError(fmt.Errorf("sqldb: commit: %w", err))
	}
	return nil
}

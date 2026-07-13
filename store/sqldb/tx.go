package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"
)

// RunInTx starts a transaction, stores it in this DB's typed context scope,
// executes fn, and commits on nil / rolls back on error. Nested calls use a
// Bun savepoint. If fn panics, rollback is attempted and the original panic
// value is re-raised. A nil fn returns [ErrNilTxCallback] without starting a
// transaction.
//
// A callback error is an application value: after a successful rollback it is
// returned unchanged and is never passed through the SQL driver error mapper.
// Begin, rollback, and commit errors are mapped separately. A commit error can
// leave the database outcome unknown; callers must not blindly retry solely
// because Commit returned an error.
//
// Nested savepoint creation and cleanup are bounded and cancellation-aware. If
// their outcome becomes uncertain, the shared transaction state is marked
// rollback-only before the ambient SQL abort starts; an outer callback cannot
// swallow the nested error and commit. If a callback returns nil after its
// context was canceled, the savepoint is rolled back and the context error is
// returned.
//
// The callback is the transaction lifetime boundary. It must not retain txCtx
// or start transaction work (including nested InTx calls) that outlives its
// return.
//
// [DB.InTx] is the method form of this function.
func RunInTx(ctx context.Context, db *DB, fn func(ctx context.Context) error) error {
	return RunInTxWith(ctx, db, nil, fn)
}

// InTx is the method form of [RunInTx]: it starts a transaction, stores it
// in the context passed to fn, and commits on nil / rolls back on error.
// Nested calls use Bun's SAVEPOINT automatically. If fn panics, the
// transaction is rolled back and the original panic value is re-raised.
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

// InTxWith is like [DB.InTx] but accepts sql.TxOptions for configuring the
// outer transaction's isolation level and read-only mode. A nested call uses a
// savepoint; non-default options there return [ErrNestedTxOptions] instead of
// being silently ignored. Option support and enforcement remain driver-
// specific; Credo does not emulate an unsupported isolation level. The pinned
// modernc SQLite driver does not reliably enforce Isolation or ReadOnly, so use
// ordinary InTx for SQLite snapshot reads. It is the method form of
// [RunInTxWith].
func (db *DB) InTxWith(ctx context.Context, opts *sql.TxOptions, fn func(ctx context.Context) error) error {
	return RunInTxWith(ctx, db, opts, fn)
}

// RunInTxWith is like RunInTx but accepts sql.TxOptions for configuring the
// outer transaction's isolation level and read-only mode. Bun implements a
// nested transaction as a savepoint, which cannot apply new transaction
// options; a nested call with non-default options returns
// [ErrNestedTxOptions] before creating the savepoint or invoking fn.
// Outer option support remains driver-specific; unsupported levels must not be
// assumed to have taken effect merely because Begin succeeded.
//
// [DB.InTxWith] is the method form of this function.
func RunInTxWith(ctx context.Context, db *DB, opts *sql.TxOptions, fn func(ctx context.Context) error) error {
	if db == nil {
		return fmt.Errorf("sqldb: db must not be nil")
	}
	if fn == nil {
		return ErrNilTxCallback
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ambientTx, nested := db.txScope.GetTx(ctx)
	state := transactionStateFromContext(ctx, db)
	if state == nil {
		state = new(transactionState)
	}
	if nested && state.rollbackOnly.Load() {
		return ErrTxRollbackOnly
	}
	if nested && hasNonDefaultTxOptions(opts) {
		return ErrNestedTxOptions
	}
	if nested && ctx.Err() != nil {
		return ctx.Err()
	}

	// Use existing TX from context if present (creates SAVEPOINT),
	// otherwise use the raw DB (creates a new transaction).
	conn := db.conn(ctx)
	var cleanupCtx context.Context
	var cancelCleanup context.CancelCauseFunc
	var abortAmbient func() error
	if nested {
		// Bun stores the BeginTx context on the savepoint and reuses it for
		// RELEASE / ROLLBACK TO SAVEPOINT. Keep cleanup possible if the
		// callback context is canceled; queries still receive the original ctx.
		cleanupCtx, cancelCleanup = context.WithCancelCause(context.WithoutCancel(ctx))
		defer cancelCleanup(context.Canceled)
		abortAmbient = func() error {
			state.rollbackOnly.Store(true)
			return abortAmbientTransaction(ambientTx, db.txCleanupTimeout)
		}
	}

	var tx bun.Tx
	var err error
	if nested {
		var outcomeUnknown bool
		tx, err, outcomeUnknown = beginNestedTransaction(
			ctx,
			cleanupCtx,
			cancelCleanup,
			conn,
			opts,
			db.txCleanupTimeout,
		)
		if err != nil && outcomeUnknown {
			beginErr := db.mapError(ctx, fmt.Errorf("sqldb: begin tx: %w", err))
			return joinTransactionOperationError(
				ctx,
				db.family,
				beginErr,
				"abort ambient transaction",
				abortAmbient(),
			)
		}
	} else {
		tx, err = conn.BeginTx(ctx, opts)
	}
	if err != nil {
		return db.mapError(ctx, fmt.Errorf("sqldb: begin tx: %w", err))
	}

	// Store TX in context so this DB's proxies and raw helpers pick it up.
	txCtx := db.txScope.WithTx(ctx, bun.IDB(tx))
	txCtx = withTransactionState(txCtx, db, state)
	finalizer := txFinalizer(tx)
	if nested {
		finalizer = &boundedTxFinalizer{
			tx:      tx,
			cancel:  cancelCleanup,
			timeout: db.txCleanupTimeout,
		}
	}
	if ctx.Err() != nil {
		return finishTransaction(ctx, db.family, finalizer, ctx.Err(), abortAmbient)
	}

	// Execute with panic recovery.
	var fnErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				if rollbackErr := finalizer.Rollback(); rollbackErr != nil && abortAmbient != nil {
					_ = abortAmbient()
				}
				panic(r)
			}
		}()
		fnErr = fn(txCtx)
	}()

	if fnErr == nil && ctx.Err() != nil {
		// A callback that ignored cancellation must not release/commit its
		// savepoint. Roll back and surface the context operation error.
		fnErr = ctx.Err()
	}
	if fnErr == nil && state.rollbackOnly.Load() {
		return joinTransactionOperationError(
			ctx,
			db.family,
			ErrTxRollbackOnly,
			"abort rollback-only transaction",
			abortAmbientTransaction(tx, db.txCleanupTimeout),
		)
	}

	err = finishTransaction(ctx, db.family, finalizer, fnErr, abortAmbient)
	if err == nil && nested && ctx.Err() != nil {
		// Cancellation raced with a successful savepoint release. Abort the
		// ambient SQL transaction so the released work cannot be committed.
		return joinTransactionOperationError(
			ctx,
			db.family,
			ctx.Err(),
			"abort ambient transaction",
			abortAmbient(),
		)
	}
	return err
}

type txFinalizer interface {
	Commit() error
	Rollback() error
}

type transactionState struct {
	rollbackOnly atomic.Bool
}

type transactionStateKey struct {
	db *DB
}

func transactionStateFromContext(ctx context.Context, db *DB) *transactionState {
	state, _ := ctx.Value(transactionStateKey{db: db}).(*transactionState)
	return state
}

func withTransactionState(ctx context.Context, db *DB, state *transactionState) context.Context {
	return context.WithValue(ctx, transactionStateKey{db: db}, state)
}

type beginTxResult struct {
	tx  bun.Tx
	err error
}

func beginNestedTransaction(
	callbackCtx context.Context,
	cleanupCtx context.Context,
	cancelCleanup context.CancelCauseFunc,
	conn bun.IDB,
	opts *sql.TxOptions,
	timeout time.Duration,
) (bun.Tx, error, bool) {
	result := make(chan beginTxResult, 1)
	go func() {
		tx, err := conn.BeginTx(cleanupCtx, opts)
		result <- beginTxResult{tx: tx, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-result:
		return result.tx, result.err, false
	case <-callbackCtx.Done():
		cancelCleanup(callbackCtx.Err())
		return bun.Tx{}, callbackCtx.Err(), true
	case <-timer.C:
		cancelCleanup(context.DeadlineExceeded)
		return bun.Tx{}, fmt.Errorf(
			"sqldb: savepoint creation exceeded cleanup timeout %s: %w",
			timeout,
			context.DeadlineExceeded,
		), true
	}
}

type boundedTxFinalizer struct {
	tx      txFinalizer
	cancel  context.CancelCauseFunc
	timeout time.Duration
}

func (tx *boundedTxFinalizer) Commit() error {
	return tx.run("savepoint release", tx.tx.Commit)
}

func (tx *boundedTxFinalizer) Rollback() error {
	return tx.run("savepoint rollback", tx.tx.Rollback)
}

func (tx *boundedTxFinalizer) run(operation string, fn func() error) error {
	result := make(chan error, 1)
	go func() {
		result <- fn()
	}()

	timer := time.NewTimer(tx.timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		tx.cancel(context.DeadlineExceeded)
		return fmt.Errorf(
			"sqldb: %s exceeded cleanup timeout %s: %w",
			operation,
			tx.timeout,
			context.DeadlineExceeded,
		)
	}
}

func finishTransaction(
	ctx context.Context,
	family driverFamily,
	tx txFinalizer,
	callbackErr error,
	abortAmbient func() error,
) error {
	if callbackErr != nil {
		if err := tx.Rollback(); err != nil &&
			!(errors.Is(err, sql.ErrTxDone) && ctx.Err() != nil) {
			rollbackErr := mapError(ctx, family, fmt.Errorf("sqldb: rollback after callback error: %w", err))
			joined := errors.Join(callbackErr, rollbackErr)
			if abortAmbient != nil {
				return joinTransactionOperationError(
					ctx,
					family,
					joined,
					"abort ambient transaction",
					abortAmbient(),
				)
			}
			return joined
		}
		return callbackErr
	}

	if err := tx.Commit(); err != nil {
		commitErr := mapError(ctx, family, fmt.Errorf("sqldb: commit: %w", err))
		if abortAmbient != nil {
			return joinTransactionOperationError(
				ctx,
				family,
				commitErr,
				"abort ambient transaction",
				abortAmbient(),
			)
		}
		return commitErr
	}
	return nil
}

func abortAmbientTransaction(conn bun.IDB, timeout time.Duration) error {
	var sqlTx *sql.Tx
	switch tx := conn.(type) {
	case bun.Tx:
		sqlTx = tx.Tx
	case *bun.Tx:
		sqlTx = tx.Tx
	default:
		return fmt.Errorf("sqldb: ambient transaction has unsupported type %T", conn)
	}
	if sqlTx == nil {
		return fmt.Errorf("sqldb: ambient transaction is nil")
	}
	result := make(chan error, 1)
	go func() {
		result <- sqlTx.Rollback()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			return err
		}
		return nil
	case <-timer.C:
		return fmt.Errorf(
			"sqldb: ambient transaction rollback exceeded cleanup timeout %s: %w",
			timeout,
			context.DeadlineExceeded,
		)
	}
}

func joinTransactionOperationError(
	ctx context.Context,
	family driverFamily,
	base error,
	operation string,
	operationErr error,
) error {
	if operationErr == nil {
		return base
	}
	return errors.Join(base, mapError(ctx, family, fmt.Errorf("sqldb: %s: %w", operation, operationErr)))
}

func hasNonDefaultTxOptions(opts *sql.TxOptions) bool {
	return opts != nil && (opts.Isolation != sql.LevelDefault || opts.ReadOnly)
}

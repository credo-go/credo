package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/store"
)

type fakeTxFinalizer struct {
	commitErr   error
	rollbackErr error
	commits     int
	rollbacks   int
}

type contextBlockingFinalizer struct {
	ctx context.Context
}

type blockingRollbackConnector struct {
	release <-chan struct{}
}

func (c *blockingRollbackConnector) Connect(context.Context) (driver.Conn, error) {
	return &blockingRollbackConn{release: c.release}, nil
}

func (c *blockingRollbackConnector) Driver() driver.Driver {
	return blockingRollbackDriver{release: c.release}
}

type blockingRollbackDriver struct {
	release <-chan struct{}
}

func (d blockingRollbackDriver) Open(string) (driver.Conn, error) {
	return &blockingRollbackConn{release: d.release}, nil
}

type blockingRollbackConn struct {
	release <-chan struct{}
}

func (*blockingRollbackConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*blockingRollbackConn) Close() error                        { return nil }

func (c *blockingRollbackConn) Begin() (driver.Tx, error) {
	return &blockingDriverTx{release: c.release}, nil
}

func (c *blockingRollbackConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &blockingDriverTx{release: c.release}, nil
}

type blockingDriverTx struct {
	release <-chan struct{}
}

func (*blockingDriverTx) Commit() error { return nil }

func (tx *blockingDriverTx) Rollback() error {
	<-tx.release
	return nil
}

func (tx *contextBlockingFinalizer) Commit() error {
	<-tx.ctx.Done()
	return tx.ctx.Err()
}

func (tx *contextBlockingFinalizer) Rollback() error {
	<-tx.ctx.Done()
	return tx.ctx.Err()
}

func (tx *fakeTxFinalizer) Commit() error {
	tx.commits++
	return tx.commitErr
}

func (tx *fakeTxFinalizer) Rollback() error {
	tx.rollbacks++
	return tx.rollbackErr
}

func TestFinishTransaction_CallbackErrorIdentity(t *testing.T) {
	tx := &fakeTxFinalizer{}
	callbackErr := errors.New("domain duplicate key validation")

	got := finishTransaction(t.Context(), driverFamilyPostgres, tx, callbackErr, nil)
	if got != callbackErr { //nolint:errorlint // Exact callback identity is the contract under test.
		t.Fatalf("finishTransaction = %v (%T), want exact callback error %p", got, got, callbackErr)
	}
	if errors.Is(got, store.ErrDuplicate) {
		t.Fatal("domain callback error was incorrectly classified as ErrDuplicate")
	}
	if tx.rollbacks != 1 || tx.commits != 0 {
		t.Fatalf("rollback/commit calls = %d/%d, want 1/0", tx.rollbacks, tx.commits)
	}
}

func TestFinishTransaction_RollbackErrorPreservesBothCauses(t *testing.T) {
	callbackErr := errors.New("domain failure")
	rollbackErr := &mockSQLStateError{state: "23505", msg: "rollback driver failure"}
	tx := &fakeTxFinalizer{rollbackErr: rollbackErr}

	got := finishTransaction(t.Context(), driverFamilyPostgres, tx, callbackErr, nil)
	if !errors.Is(got, callbackErr) {
		t.Errorf("finishTransaction error does not preserve callback cause: %v", got)
	}
	if !errors.Is(got, rollbackErr) {
		t.Errorf("finishTransaction error does not preserve rollback cause: %v", got)
	}
	if !errors.Is(got, store.ErrDuplicate) {
		t.Errorf("finishTransaction error does not map rollback SQLSTATE: %v", got)
	}
	if tx.rollbacks != 1 || tx.commits != 0 {
		t.Fatalf("rollback/commit calls = %d/%d, want 1/0", tx.rollbacks, tx.commits)
	}
}

func TestFinishTransaction_DoesNotClassifyFromCallbackOnRollbackFailure(t *testing.T) {
	callbackErr := errors.New("domain duplicate key validation")
	rollbackErr := errors.New("network disconnected during rollback")
	tx := &fakeTxFinalizer{rollbackErr: rollbackErr}

	got := finishTransaction(t.Context(), driverFamilyPostgres, tx, callbackErr, nil)
	if !errors.Is(got, callbackErr) || !errors.Is(got, rollbackErr) {
		t.Fatalf("finishTransaction = %v, want both causes", got)
	}
	if errors.Is(got, store.ErrDuplicate) {
		t.Fatal("callback text incorrectly classified the rollback composite")
	}
}

func TestFinishTransaction_ErrTxDoneRollbackKeepsCallbackIdentity(t *testing.T) {
	tx := &fakeTxFinalizer{rollbackErr: sql.ErrTxDone}
	callbackErr := errors.New("domain failure")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got := finishTransaction(ctx, driverFamilyPostgres, tx, callbackErr, nil)
	if got != callbackErr { //nolint:errorlint // Exact callback identity is the contract under test.
		t.Fatalf("finishTransaction = %v, want exact callback error", got)
	}
}

func TestFinishTransaction_UnexpectedErrTxDoneIsPreserved(t *testing.T) {
	tx := &fakeTxFinalizer{rollbackErr: sql.ErrTxDone}
	callbackErr := errors.New("domain failure")

	got := finishTransaction(t.Context(), driverFamilyPostgres, tx, callbackErr, nil)
	if !errors.Is(got, callbackErr) || !errors.Is(got, sql.ErrTxDone) {
		t.Fatalf("finishTransaction = %v, want callback and unexpected ErrTxDone", got)
	}
}

func TestFinishTransaction_CommitErrorMappedAtCommitLayer(t *testing.T) {
	commitErr := &mockSQLStateError{state: "40001", msg: "serialization failure"}
	tx := &fakeTxFinalizer{commitErr: commitErr}

	got := finishTransaction(t.Context(), driverFamilyPostgres, tx, nil, nil)
	if !errors.Is(got, commitErr) {
		t.Errorf("finishTransaction error does not preserve commit cause: %v", got)
	}
	if !errors.Is(got, store.ErrSerialization) || !errors.Is(got, store.ErrConflict) {
		t.Errorf("finishTransaction error does not map commit SQLSTATE: %v", got)
	}
	if !strings.Contains(got.Error(), "sqldb: commit:") {
		t.Errorf("finishTransaction error = %q, want commit operation context", got)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("commit/rollback calls = %d/%d, want 1/0", tx.commits, tx.rollbacks)
	}
}

func TestFinishTransaction_NestedRollbackFailureAbortsAmbient(t *testing.T) {
	callbackErr := errors.New("domain failure")
	rollbackErr := errors.New("savepoint rollback failed")
	tx := &fakeTxFinalizer{rollbackErr: rollbackErr}
	abortCalls := 0

	got := finishTransaction(t.Context(), driverFamilyPostgres, tx, callbackErr, func() error {
		abortCalls++
		return nil
	})
	if !errors.Is(got, callbackErr) || !errors.Is(got, rollbackErr) {
		t.Fatalf("finishTransaction = %v, want callback and rollback causes", got)
	}
	if abortCalls != 1 {
		t.Fatalf("abort calls = %d, want 1", abortCalls)
	}
}

func TestFinishTransaction_NestedCommitFailureAbortsAmbient(t *testing.T) {
	commitErr := errors.New("savepoint release failed")
	abortErr := &mockSQLStateError{state: "40001", msg: "ambient rollback failed"}
	tx := &fakeTxFinalizer{commitErr: commitErr}
	abortCalls := 0

	got := finishTransaction(t.Context(), driverFamilyPostgres, tx, nil, func() error {
		abortCalls++
		return abortErr
	})
	if !errors.Is(got, commitErr) || !errors.Is(got, abortErr) {
		t.Fatalf("finishTransaction = %v, want commit and abort causes", got)
	}
	if !errors.Is(got, store.ErrSerialization) || !errors.Is(got, store.ErrConflict) {
		t.Fatalf("finishTransaction = %v, want mapped abort SQLSTATE", got)
	}
	if abortCalls != 1 {
		t.Fatalf("abort calls = %d, want 1", abortCalls)
	}
}

func TestAbortAmbientTransaction_RollsBackRootSQLTransaction(t *testing.T) {
	db, err := Open(&Config{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })

	if _, createErr := db.Client().NewRaw("CREATE TABLE tx_abort (id INTEGER)").Exec(t.Context()); createErr != nil {
		t.Fatalf("create table = %v", createErr)
	}
	tx, err := db.Client().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx = %v", err)
	}
	if _, err := tx.NewRaw("INSERT INTO tx_abort (id) VALUES (1)").Exec(t.Context()); err != nil {
		t.Fatalf("insert = %v", err)
	}

	if err := abortAmbientTransaction(tx, time.Second); err != nil {
		t.Fatalf("abortAmbientTransaction = %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("Commit after abort = %v, want sql.ErrTxDone", err)
	}
	var count int
	if err := db.Client().NewRaw("SELECT COUNT(*) FROM tx_abort").Scan(t.Context(), &count); err != nil {
		t.Fatalf("count = %v", err)
	}
	if count != 0 {
		t.Fatalf("count after abort = %d, want 0", count)
	}
}

func TestBoundedTxFinalizer_StopsBlockedCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 5 * time.Second
		ctx, cancel := context.WithCancelCause(context.WithoutCancel(t.Context()))
		defer cancel(context.Canceled)
		finalizer := &boundedTxFinalizer{
			tx:      &contextBlockingFinalizer{ctx: ctx},
			cancel:  cancel,
			timeout: timeout,
		}

		start := time.Now()
		err := finalizer.Rollback()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Rollback = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != timeout {
			t.Fatalf("Rollback elapsed = %s, want %s", elapsed, timeout)
		}
		if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatalf("cleanup context cause = %v, want DeadlineExceeded", context.Cause(ctx))
		}
		synctest.Wait()
	})
}

func TestAbortAmbientTransaction_StopsWaitingForBlockedDriver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 5 * time.Second
		release := make(chan struct{})
		sqlDB := sql.OpenDB(&blockingRollbackConnector{release: release})
		defer sqlDB.Close()
		sqlTx, err := sqlDB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatalf("BeginTx = %v", err)
		}

		start := time.Now()
		err = abortAmbientTransaction(bun.Tx{Tx: sqlTx}, timeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("abortAmbientTransaction = %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != timeout {
			t.Fatalf("abort elapsed = %s, want %s", elapsed, timeout)
		}
		if err := sqlTx.Commit(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("Commit after timed-out abort = %v, want sql.ErrTxDone", err)
		}

		close(release)
		synctest.Wait()
	})
}

func TestHasNonDefaultTxOptions(t *testing.T) {
	tests := []struct {
		name string
		opts *sql.TxOptions
		want bool
	}{
		{name: "nil", opts: nil, want: false},
		{name: "zero", opts: &sql.TxOptions{}, want: false},
		{name: "isolation", opts: &sql.TxOptions{Isolation: sql.LevelSerializable}, want: true},
		{name: "read-only", opts: &sql.TxOptions{ReadOnly: true}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasNonDefaultTxOptions(tt.opts); got != tt.want {
				t.Errorf("hasNonDefaultTxOptions(%+v) = %v, want %v", tt.opts, got, tt.want)
			}
		})
	}
}

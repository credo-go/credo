package sqldb

import "errors"

var (
	// ErrNilTxCallback indicates that a transaction was requested without a
	// callback. The transaction is not started when this error is returned.
	ErrNilTxCallback = errors.New("sqldb: transaction callback must not be nil")

	// ErrNestedTxOptions indicates that non-default sql.TxOptions were supplied
	// to a nested transaction. Bun implements nested transactions with a
	// savepoint, where isolation and read-only options cannot be applied.
	ErrNestedTxOptions = errors.New("sqldb: nested transaction options are not supported")

	// ErrTxRollbackOnly indicates that a nested savepoint operation had an
	// uncertain outcome and poisoned the ambient transaction. The outer
	// transaction is aborted instead of being allowed to commit.
	ErrTxRollbackOnly = errors.New("sqldb: transaction is rollback-only")
)

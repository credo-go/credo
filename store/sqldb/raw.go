package sqldb

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/store"
)

// conn returns the active transaction from context, or the DB as fallback.
func (db *DB) conn(ctx context.Context) bun.IDB {
	if ctx == nil {
		return db.db
	}
	return db.txScope.Conn(ctx, db.db)
}

// Conn returns the active transaction for this DB from ctx, or the underlying
// Bun DB when no transaction is active. It is the transaction-aware escape
// hatch for advanced Bun operations that are not covered by Credo's proxies:
//
//	err := db.InTx(ctx, func(txCtx context.Context) error {
//		return db.Conn(txCtx).NewSelect().Model(&users).Scan(txCtx)
//	})
//
// Conn does not lease a dedicated sql.Conn. The returned value is borrowed and
// must not escape the InTx callback when it represents a transaction. Queries
// executed through it use Bun directly and therefore bypass Credo error
// mapping. A nil context selects the underlying DB.
func (db *DB) Conn(ctx context.Context) bun.IDB {
	return db.conn(ctx)
}

// RequireTx returns the active transaction for this DB or [store.ErrTxMissing]
// when ctx is outside its transaction scope. Unlike [DB.Conn], it never falls
// back to the underlying DB. The returned transaction is borrowed and must not
// escape the InTx callback.
func (db *DB) RequireTx(ctx context.Context) (bun.IDB, error) {
	if ctx == nil {
		return nil, store.ErrTxMissing
	}
	return db.txScope.RequireTx(ctx)
}

// Exec executes a raw SQL query with TX injection and error mapping.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	conn := db.conn(ctx)
	res, err := conn.NewRaw(query, args...).Exec(ctx)
	return res, db.mapError(ctx, err)
}

// QueryRow executes a raw SQL query that returns a single row,
// with TX injection and error mapping. dest is scanned into.
func (db *DB) QueryRow(ctx context.Context, dest any, query string, args ...any) error {
	conn := db.conn(ctx)
	return db.mapError(ctx, conn.NewRaw(query, args...).Scan(ctx, dest))
}

// Query executes a raw SQL query that returns multiple rows,
// with TX injection and error mapping. dest should be a pointer to a slice.
func (db *DB) Query(ctx context.Context, dest any, query string, args ...any) error {
	conn := db.conn(ctx)
	return db.mapError(ctx, conn.NewRaw(query, args...).Scan(ctx, dest))
}

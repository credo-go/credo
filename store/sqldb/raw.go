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
	return store.ConnInScope[bun.IDB](ctx, db.txScope, db.db)
}

// Exec executes a raw SQL query with TX injection and error mapping.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	conn := db.conn(ctx)
	res, err := conn.NewRaw(query, args...).Exec(ctx)
	return res, mapError(err)
}

// QueryRow executes a raw SQL query that returns a single row,
// with TX injection and error mapping. dest is scanned into.
func (db *DB) QueryRow(ctx context.Context, dest any, query string, args ...any) error {
	conn := db.conn(ctx)
	return mapError(conn.NewRaw(query, args...).Scan(ctx, dest))
}

// Query executes a raw SQL query that returns multiple rows,
// with TX injection and error mapping. dest should be a pointer to a slice.
func (db *DB) Query(ctx context.Context, dest any, query string, args ...any) error {
	conn := db.conn(ctx)
	return mapError(conn.NewRaw(query, args...).Scan(ctx, dest))
}

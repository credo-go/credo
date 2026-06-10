package sqldb

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
)

// InsertQuery proxies bun.InsertQuery with TX injection and error mapping.
type InsertQuery struct {
	raw   *bun.InsertQuery
	state queryState
}

// --- Builder methods ---

// Model sets the model for the insert.
func (q *InsertQuery) Model(model any) *InsertQuery {
	q.raw = q.raw.Model(model)
	return q
}

// Column specifies columns to insert.
func (q *InsertQuery) Column(columns ...string) *InsertQuery {
	q.raw = q.raw.Column(columns...)
	return q
}

// Value sets a custom value expression for a column.
func (q *InsertQuery) Value(column string, expr string, args ...any) *InsertQuery {
	q.raw = q.raw.Value(column, expr, args...)
	return q
}

// On adds an ON CONFLICT clause.
func (q *InsertQuery) On(query string, args ...any) *InsertQuery {
	q.raw = q.raw.On(query, args...)
	return q
}

// Set adds a SET clause for ON CONFLICT DO UPDATE.
func (q *InsertQuery) Set(query string, args ...any) *InsertQuery {
	q.raw = q.raw.Set(query, args...)
	return q
}

// Returning adds a RETURNING clause.
func (q *InsertQuery) Returning(query string, args ...any) *InsertQuery {
	q.raw = q.raw.Returning(query, args...)
	return q
}

// Conn sets an explicit connection, bypassing context TX injection.
func (q *InsertQuery) Conn(db bun.IConn) *InsertQuery {
	q.state.markConnSet()
	q.raw = q.raw.Conn(db)
	return q
}

// --- Escape hatches ---

// Apply delegates to Bun's native Apply for advanced builder methods.
// Nil functions are filtered out.
func (q *InsertQuery) Apply(fns ...func(*bun.InsertQuery) *bun.InsertQuery) *InsertQuery {
	q.raw = applyFiltered(q.raw, fns...)
	return q
}

// Unwrap returns the underlying *bun.InsertQuery for builder-only use.
func (q *InsertQuery) Unwrap() *bun.InsertQuery {
	return q.raw
}

// --- Terminal methods ---

// Exec executes the insert query.
//
// Before TX injection the builder is shallow-copied so injecting the
// connection does not mutate the caller's builder. bun exposes Clone only on
// SelectQuery, so a deep copy is unavailable here; the shallow copy suffices
// because bun reads — never mutates — the builder while generating SQL.
//
// Driver errors are mapped to store.Err* sentinels. Unique-constraint
// violations become [store.ErrDuplicate]; foreign-key violations become
// [store.ErrConflict].
func (q *InsertQuery) Exec(ctx context.Context, dest ...any) (sql.Result, error) {
	raw := prepareQuery(ctx, q.raw, q.state, shallowCopy[bun.InsertQuery])
	res, err := raw.Exec(ctx, dest...)
	return res, mapError(err)
}

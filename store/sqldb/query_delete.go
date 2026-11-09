package sqldb

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
)

// DeleteQuery proxies bun.DeleteQuery with TX injection and error mapping.
type DeleteQuery struct {
	raw   *bun.DeleteQuery
	state queryState
}

// --- Builder methods ---

// Model sets the model for the delete.
func (q *DeleteQuery) Model(model any) *DeleteQuery {
	q.raw = q.raw.Model(model)
	return q
}

// Where adds a WHERE condition.
func (q *DeleteQuery) Where(query string, args ...any) *DeleteQuery {
	q.raw = q.raw.Where(query, args...)
	return q
}

// WherePK adds a WHERE clause for the primary key columns.
func (q *DeleteQuery) WherePK(cols ...string) *DeleteQuery {
	q.raw = q.raw.WherePK(cols...)
	return q
}

// Returning adds a RETURNING clause.
func (q *DeleteQuery) Returning(query string, args ...any) *DeleteQuery {
	q.raw = q.raw.Returning(query, args...)
	return q
}

// Conn sets an explicit connection, bypassing context TX injection.
func (q *DeleteQuery) Conn(db bun.IConn) *DeleteQuery {
	q.state.markConnSet()
	q.raw = q.raw.Conn(db)
	return q
}

// --- Escape hatches ---

// Apply delegates to Bun's native Apply for advanced builder methods.
// Nil functions are filtered out. A connection set inside fn is overridden by
// TX injection at the terminal; use Conn for an explicit connection.
func (q *DeleteQuery) Apply(fns ...func(*bun.DeleteQuery) *bun.DeleteQuery) *DeleteQuery {
	q.raw = applyFiltered(q.raw, fns...)
	return q
}

// ApplyFilter runs fn on a [Filter] over this query's WHERE clause, so a single
// predicate — tenant scoping, soft-delete filters, ownership checks — scopes a
// select, an update, and a delete alike. A nil fn is a no-op.
func (q *DeleteQuery) ApplyFilter(fn func(*Filter)) *DeleteQuery {
	applyFilter(q.raw.QueryBuilder(), fn)
	return q
}

// Unwrap returns the underlying *bun.DeleteQuery for builder-only use.
func (q *DeleteQuery) Unwrap() *bun.DeleteQuery {
	return q.raw
}

// --- Terminal methods ---

// Exec executes the delete query.
//
// Before TX injection the builder is shallow-copied so injecting the
// connection does not mutate the caller's builder. bun exposes Clone only on
// SelectQuery, so a deep copy is unavailable here; the shallow copy suffices
// because bun reads — never mutates — the builder while generating SQL.
//
// Driver errors are mapped to store.Err* sentinels. Callers should
// inspect the returned sql.Result to detect zero-row deletes; the
// proxy does not convert "no rows affected" into [store.ErrNotFound].
func (q *DeleteQuery) Exec(ctx context.Context, dest ...any) (sql.Result, error) {
	raw := prepareQuery(ctx, q.raw, q.state, shallowCopy[bun.DeleteQuery])
	res, err := raw.Exec(ctx, dest...)
	return res, q.state.db.mapError(ctx, err)
}

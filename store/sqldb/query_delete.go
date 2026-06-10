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
// Nil functions are filtered out.
func (q *DeleteQuery) Apply(fns ...func(*bun.DeleteQuery) *bun.DeleteQuery) *DeleteQuery {
	q.raw = applyFiltered(q.raw, fns...)
	return q
}

// ApplyQueryBuilder applies fn to Bun's shared [bun.QueryBuilder] — the
// builder-only interface (Where, WhereOr, WhereGroup, WherePK,
// WhereDeleted, WhereAllWithDeleted) common to select, update, and delete
// queries. It lets a single WHERE predicate be reused across all three
// query types instead of being duplicated per type via Apply.
//
// Conditions added through the builder land on this query, so the proxy's
// terminal methods still apply TX injection and error mapping; interceptors
// are preserved. A nil fn is a no-op. The builder's Unwrap() any remains a
// terminal-bypass escape, the same caveat as Unwrap.
func (q *DeleteQuery) ApplyQueryBuilder(fn func(bun.QueryBuilder) bun.QueryBuilder) *DeleteQuery {
	if fn == nil {
		return q
	}
	q.raw = fn(q.raw.QueryBuilder()).Unwrap().(*bun.DeleteQuery)
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
	return res, mapError(err)
}

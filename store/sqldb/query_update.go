package sqldb

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
)

// UpdateQuery proxies bun.UpdateQuery with TX injection and error mapping.
type UpdateQuery struct {
	raw   *bun.UpdateQuery
	state queryState
}

// --- Builder methods ---

// Model sets the model for the update.
func (q *UpdateQuery) Model(model any) *UpdateQuery {
	q.raw = q.raw.Model(model)
	return q
}

// Column specifies columns to update.
func (q *UpdateQuery) Column(columns ...string) *UpdateQuery {
	q.raw = q.raw.Column(columns...)
	return q
}

// Set adds a SET clause.
func (q *UpdateQuery) Set(query string, args ...any) *UpdateQuery {
	q.raw = q.raw.Set(query, args...)
	return q
}

// Where adds a WHERE condition.
func (q *UpdateQuery) Where(query string, args ...any) *UpdateQuery {
	q.raw = q.raw.Where(query, args...)
	return q
}

// WherePK adds a WHERE clause for the primary key columns.
func (q *UpdateQuery) WherePK(cols ...string) *UpdateQuery {
	q.raw = q.raw.WherePK(cols...)
	return q
}

// Returning adds a RETURNING clause.
func (q *UpdateQuery) Returning(query string, args ...any) *UpdateQuery {
	q.raw = q.raw.Returning(query, args...)
	return q
}

// OmitZero omits zero values from the update.
func (q *UpdateQuery) OmitZero() *UpdateQuery {
	q.raw = q.raw.OmitZero()
	return q
}

// Conn sets an explicit connection, bypassing context TX injection.
func (q *UpdateQuery) Conn(db bun.IConn) *UpdateQuery {
	q.state.markConnSet()
	q.raw = q.raw.Conn(db)
	return q
}

// --- Escape hatches ---

// Apply delegates to Bun's native Apply for advanced builder methods.
// Nil functions are filtered out. A connection set inside fn is overridden by
// TX injection at the terminal; use Conn for an explicit connection.
func (q *UpdateQuery) Apply(fns ...func(*bun.UpdateQuery) *bun.UpdateQuery) *UpdateQuery {
	q.raw = applyFiltered(q.raw, fns...)
	return q
}

// ApplyFilter runs fn on a [Filter] over this query's WHERE clause, so a single
// predicate — tenant scoping, soft-delete filters, ownership checks — scopes a
// select, an update, and a delete alike. A nil fn is a no-op.
func (q *UpdateQuery) ApplyFilter(fn func(*Filter)) *UpdateQuery {
	applyFilter(q.raw.QueryBuilder(), fn)
	return q
}

// Unwrap returns the underlying *bun.UpdateQuery for builder-only use.
func (q *UpdateQuery) Unwrap() *bun.UpdateQuery {
	return q.raw
}

// --- Terminal methods ---

// Exec executes the update query.
//
// Before TX injection the builder is shallow-copied so injecting the
// connection does not mutate the caller's builder. bun exposes Clone only on
// SelectQuery, so a deep copy is unavailable here; the shallow copy suffices
// because bun reads — never mutates — the builder while generating SQL.
//
// Driver errors are mapped to store.Err* sentinels. Callers should
// inspect the returned sql.Result to detect zero-row updates; the
// proxy does not convert "no rows affected" into [store.ErrNotFound].
func (q *UpdateQuery) Exec(ctx context.Context, dest ...any) (sql.Result, error) {
	raw := prepareQuery(ctx, q.raw, q.state, shallowCopy[bun.UpdateQuery])
	res, err := raw.Exec(ctx, dest...)
	return res, q.state.db.mapError(ctx, err)
}

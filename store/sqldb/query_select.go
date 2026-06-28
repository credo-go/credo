package sqldb

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/pagination"
)

// SelectQuery proxies bun.SelectQuery with TX injection and error mapping.
type SelectQuery struct {
	raw   *bun.SelectQuery
	state queryState
}

// --- Builder methods (curated subset) ---

// Model sets the model for the query.
func (q *SelectQuery) Model(model any) *SelectQuery {
	q.raw = q.raw.Model(model)
	return q
}

// Column adds columns to select.
func (q *SelectQuery) Column(columns ...string) *SelectQuery {
	q.raw = q.raw.Column(columns...)
	return q
}

// ColumnExpr adds a raw expression to the SELECT clause. Use for
// computed columns and aggregates that the model layer cannot express.
func (q *SelectQuery) ColumnExpr(query string, args ...any) *SelectQuery {
	q.raw = q.raw.ColumnExpr(query, args...)
	return q
}

// ExcludeColumn removes columns that the model would otherwise select.
// Use "*" to start from an empty set and add columns explicitly.
func (q *SelectQuery) ExcludeColumn(columns ...string) *SelectQuery {
	q.raw = q.raw.ExcludeColumn(columns...)
	return q
}

// TableExpr sets the FROM clause from a raw expression. Use for
// model-less queries (reporting, ad-hoc projections) where Model is
// not appropriate.
func (q *SelectQuery) TableExpr(query string, args ...any) *SelectQuery {
	q.raw = q.raw.TableExpr(query, args...)
	return q
}

// Join adds a JOIN clause. The join string is the full join expression
// including the join type and ON condition, e.g.
// "LEFT JOIN orders AS o ON o.user_id = u.id".
// For composing the ON clause separately, follow with JoinOn.
func (q *SelectQuery) Join(join string, args ...any) *SelectQuery {
	q.raw = q.raw.Join(join, args...)
	return q
}

// JoinOn appends an additional ON condition to the most recent Join,
// joined with AND.
func (q *SelectQuery) JoinOn(cond string, args ...any) *SelectQuery {
	q.raw = q.raw.JoinOn(cond, args...)
	return q
}

// JoinOnOr appends an additional ON condition to the most recent Join,
// joined with OR.
func (q *SelectQuery) JoinOnOr(cond string, args ...any) *SelectQuery {
	q.raw = q.raw.JoinOnOr(cond, args...)
	return q
}

// Where adds a WHERE condition.
func (q *SelectQuery) Where(query string, args ...any) *SelectQuery {
	q.raw = q.raw.Where(query, args...)
	return q
}

// WhereOr adds an OR WHERE condition.
func (q *SelectQuery) WhereOr(query string, args ...any) *SelectQuery {
	q.raw = q.raw.WhereOr(query, args...)
	return q
}

// WherePK adds a WHERE clause for the primary key columns.
func (q *SelectQuery) WherePK(cols ...string) *SelectQuery {
	q.raw = q.raw.WherePK(cols...)
	return q
}

// OrderExpr adds an ORDER BY expression.
func (q *SelectQuery) OrderExpr(query string, args ...any) *SelectQuery {
	q.raw = q.raw.OrderExpr(query, args...)
	return q
}

// Limit sets the LIMIT clause.
func (q *SelectQuery) Limit(n int) *SelectQuery {
	q.raw = q.raw.Limit(n)
	return q
}

// Offset sets the OFFSET clause.
func (q *SelectQuery) Offset(n int) *SelectQuery {
	q.raw = q.raw.Offset(n)
	return q
}

// Relation joins a related model.
func (q *SelectQuery) Relation(name string, apply ...func(*bun.SelectQuery) *bun.SelectQuery) *SelectQuery {
	q.raw = q.raw.Relation(name, apply...)
	return q
}

// Distinct adds a DISTINCT clause.
func (q *SelectQuery) Distinct() *SelectQuery {
	q.raw = q.raw.Distinct()
	return q
}

// GroupExpr adds a GROUP BY expression.
func (q *SelectQuery) GroupExpr(query string, args ...any) *SelectQuery {
	q.raw = q.raw.GroupExpr(query, args...)
	return q
}

// Having adds a HAVING clause.
func (q *SelectQuery) Having(query string, args ...any) *SelectQuery {
	q.raw = q.raw.Having(query, args...)
	return q
}

// Clone creates a deep copy of the query.
func (q *SelectQuery) Clone() *SelectQuery {
	return &SelectQuery{
		raw:   q.raw.Clone(),
		state: q.state,
	}
}

// Conn sets an explicit connection, bypassing context TX injection.
func (q *SelectQuery) Conn(db bun.IConn) *SelectQuery {
	q.state.markConnSet()
	q.raw = q.raw.Conn(db)
	return q
}

// --- Escape hatches ---

// Apply delegates to Bun's native Apply for advanced builder methods
// not in the curated proxy set. Nil functions are filtered out.
// Interceptors (TX injection, error mapping) are preserved on terminal
// methods.
func (q *SelectQuery) Apply(fns ...func(*bun.SelectQuery) *bun.SelectQuery) *SelectQuery {
	q.raw = applyFiltered(q.raw, fns...)
	return q
}

// ApplyQueryBuilder applies fn to Bun's shared [bun.QueryBuilder] — the
// builder-only interface (Where, WhereOr, WhereGroup, WherePK,
// WhereDeleted, WhereAllWithDeleted) common to select, update, and delete
// queries. Unlike Apply, which is typed per query, this lets a single
// predicate — tenant scoping, soft-delete filters, ownership checks — be
// reused across all three query types instead of being duplicated per type.
//
// Conditions added through the builder land on this query, so the proxy's
// terminal methods still apply TX injection and error mapping; interceptors
// are preserved, exactly like Apply. A nil fn is a no-op.
//
// The bun.QueryBuilder passed to fn also exposes Unwrap() any as a terminal
// escape; calling terminal methods on that unwrapped query bypasses Credo
// interceptors — the same caveat as Unwrap.
func (q *SelectQuery) ApplyQueryBuilder(fn func(bun.QueryBuilder) bun.QueryBuilder) *SelectQuery {
	if fn == nil {
		return q
	}
	q.raw = fn(q.raw.QueryBuilder()).Unwrap().(*bun.SelectQuery)
	return q
}

// Unwrap returns the underlying *bun.SelectQuery for builder-only use.
// Terminal methods on the unwrapped query bypass Credo interceptors
// (TX injection, error mapping). Use Apply for the recommended escape
// hatch that preserves interceptors.
func (q *SelectQuery) Unwrap() *bun.SelectQuery {
	return q.raw
}

// --- Terminal methods ---

// prepareTerminal clones the raw query and injects TX from context
// if no explicit Conn was set.
func (q *SelectQuery) prepareTerminal(ctx context.Context) *bun.SelectQuery {
	return prepareQuery(ctx, q.raw, q.state, func(raw *bun.SelectQuery) *bun.SelectQuery {
		return raw.Clone()
	})
}

// Scan executes the query and scans results into dest.
//
// Driver errors are mapped to store.Err* sentinels. In particular,
// [sql.ErrNoRows] is returned as [store.ErrNotFound], so callers can use
// [errors.Is](err, store.ErrNotFound) without importing database/sql.
func (q *SelectQuery) Scan(ctx context.Context, dest ...any) error {
	return mapError(q.prepareTerminal(ctx).Scan(ctx, dest...))
}

// Count executes the query and returns the count of matching rows.
// Driver errors are mapped to store.Err* sentinels.
func (q *SelectQuery) Count(ctx context.Context) (int, error) {
	n, err := q.prepareTerminal(ctx).Count(ctx)
	return n, mapError(err)
}

// Exists executes the query and returns true if at least one row matches.
// Driver errors are mapped to store.Err* sentinels.
func (q *SelectQuery) Exists(ctx context.Context) (bool, error) {
	ok, err := q.prepareTerminal(ctx).Exists(ctx)
	return ok, mapError(err)
}

// --- Typed terminal methods (generic) ---

// One executes the query and returns its first matching row as a value of T.
// T drives both the table and the scan destination, so the query is built
// model-less and One owns the destination:
//
//	user, err := db.Select().Where("id = ?", id).One[User](ctx)
//
// One applies LIMIT 1, so multiple matches are not an error — it returns the
// first row; add an OrderExpr for a deterministic choice. A missing row maps
// [sql.ErrNoRows] to [store.ErrNotFound], so callers branch with
// errors.Is(err, store.ErrNotFound); other driver errors map to the store.Err*
// sentinels. The receiver is not mutated: the query is cloned and the ambient
// transaction from ctx injected, exactly as for [SelectQuery.Scan].
func (q *SelectQuery) One[T any](ctx context.Context) (T, error) {
	var out T
	err := q.prepareTerminal(ctx).Model(&out).Limit(1).Scan(ctx)
	return out, mapError(err)
}

// All executes the query and returns every matching row as a []T. T drives
// both the table and the scan destination, so the query is built model-less
// and All owns the destination:
//
//	users, err := db.Select().Where("active = ?", true).OrderExpr("id").All[User](ctx)
//
// No matching rows yield an empty, non-nil slice and a nil error — unlike One,
// an empty result is not [store.ErrNotFound]. Driver errors map to the
// store.Err* sentinels. The receiver is not mutated: the query is cloned and
// the ambient transaction from ctx injected, exactly as for [SelectQuery.Scan].
func (q *SelectQuery) All[T any](ctx context.Context) ([]T, error) {
	out := []T{}
	err := q.prepareTerminal(ctx).Model(&out).Scan(ctx)
	return out, mapError(err)
}

// Page runs COUNT + SELECT with LIMIT/OFFSET and assembles the result as a
// *pagination.Page[T]. Like [SelectQuery.One] and [SelectQuery.All] it is a
// typed terminal: T drives both the table and the scan destination, so the
// query is built model-less and Page owns the result:
//
//	page, err := db.Select().
//		Where("tenant_id = ?", tenantID).
//		OrderExpr("created_at DESC").
//		Page[User](ctx, req)            // (*pagination.Page[User], error)
//
// req is read, never modified, and is assumed to be already normalized —
// BindQuery does this automatically via [pagination.PageRequest.Validate];
// otherwise call [pagination.PageRequest.Normalize] (or NormalizeWithMax)
// first. Page does not normalize: req.Page and req.PerPage are used as given
// and echoed into the returned Page. A nil req is the one rejected input and
// returns an error.
//
// COUNT runs first; when it reports zero rows the SELECT is skipped and the
// returned Page carries a non-nil empty Records slice with the requested page
// and per-page preserved. COUNT and SELECT are separate statements, so under
// concurrent writes the total and the page can drift — call Page inside
// [RunInTx] when a consistent snapshot matters. Both statements clone the
// query and join the ambient transaction from ctx exactly like
// [SelectQuery.Scan], so the receiver is never mutated.
//
// Page is the all-in-one terminal for flows that respond with the queried
// type directly. When records need a model→DTO mapping, T cannot be both the
// table model and the response type — build the Page from the typed terminals
// instead, so it is constructed once with the final DTO type:
//
//	total, err := q.Clone().Model((*Model)(nil)).Count(ctx)
//	if err != nil || total == 0 {
//		return pagination.NewPage([]DTO{}, int64(total), req.Page, req.PerPage), err
//	}
//	rows, err := q.Clone().Offset(req.Offset()).Limit(req.PerPage).All[Model](ctx)
//	// ... map rows []Model → dtos []DTO ...
//	page := pagination.NewPage(dtos, int64(total), req.Page, req.PerPage)
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error) {
	if req == nil {
		return nil, fmt.Errorf("sqldb: page request must not be nil")
	}

	// COUNT with T's table. T drives the table, so the query is built
	// model-less and the COUNT clone owns the model (like All owns it for the
	// SELECT below); the receiver is never mutated.
	total, err := q.Clone().Model((*T)(nil)).Count(ctx)
	if err != nil {
		return nil, err
	}

	// No rows — skip the SELECT, preserving the requested page/per-page.
	if total == 0 {
		return pagination.NewPage([]T{}, 0, req.Page, req.PerPage), nil
	}

	// SELECT on a clone so Offset/Limit never leak back into the receiver.
	records, err := q.Clone().Offset(req.Offset()).Limit(req.PerPage).All[T](ctx)
	if err != nil {
		return nil, err
	}
	return pagination.NewPage(records, int64(total), req.Page, req.PerPage), nil
}

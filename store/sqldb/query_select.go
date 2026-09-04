package sqldb

import (
	"context"
	"database/sql"
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

// Limit sets the LIMIT clause. Values outside Bun v1.2.18's signed-int32
// storage range record [ErrInvalidLimitOffset]; the terminal then fails before
// executing. Values inside that range retain Bun's zero/negative semantics.
func (q *SelectQuery) Limit(n int) *SelectQuery {
	if err := validateBunLimitOffset("limit", n); err != nil {
		q.raw = q.raw.Err(err)
		return q
	}
	q.raw = q.raw.Limit(n)
	return q
}

// Offset sets the OFFSET clause. Values outside Bun v1.2.18's signed-int32
// storage range record [ErrInvalidLimitOffset]; the terminal then fails before
// executing. Values inside that range retain Bun's zero/negative semantics.
func (q *SelectQuery) Offset(n int) *SelectQuery {
	if err := validateBunLimitOffset("offset", n); err != nil {
		q.raw = q.raw.Err(err)
		return q
	}
	q.raw = q.raw.Offset(n)
	return q
}

// Relation loads a named relation for a bound model. Use the model-bound Scan
// form for eager loading, for example:
//
//	var users []User
//	err := db.Select(&users).Relation("Orders").Scan(ctx)
//
// A relation cannot be deferred to One, All, or Page: typed terminals require
// a model-less query and return [ErrTypedTerminalModel] when a model is already
// bound.
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

// Clone returns a top-level query-builder fork while preserving execution
// state that Bun v1.2.18 omits: an explicit connection, builder errors, WherePK
// fields, soft-delete flags, and CTE materialization flags. It follows Bun's
// sharing semantics for nested values, including a bound destination and CTE
// or relation subqueries; do not mutate or scan source and clone concurrently
// when they share such values.
func (q *SelectQuery) Clone() *SelectQuery {
	return q.cloneQuery()
}

func (q *SelectQuery) cloneQuery() *SelectQuery {
	return &SelectQuery{
		raw:   cloneBunSelectQuery(q.raw),
		state: q.state,
	}
}

// Conn sets an explicit connection, bypassing context TX injection.
func (q *SelectQuery) Conn(db bun.IConn) *SelectQuery {
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

// prepareTerminal creates an execution snapshot and injects TX from context if
// no explicit Conn was set. The snapshot preserves Bun state without mutating
// the reusable receiver.
func (q *SelectQuery) prepareTerminal(ctx context.Context) *bun.SelectQuery {
	prepared := cloneBunSelectQuery(q.raw)
	conn, err := bunSelectQueryConn(q.raw)
	if err == nil && conn == nil {
		prepared = prepared.Conn(q.state.conn(ctx))
	}
	return prepared
}

// countLogicalRows counts the rows produced by the complete SELECT projection
// instead of asking Bun to replace that projection with count(*). The outer
// query is what makes ungrouped aggregates, Distinct, and grouped results obey
// one logical-result cardinality contract. model is used only by Page to bind
// T's table on the private source snapshot.
func (q *SelectQuery) countLogicalRows(ctx context.Context, model ...any) (int, error) {
	if len(model) > 1 {
		return 0, fmt.Errorf("sqldb: internal logical count accepts at most one model")
	}
	source := q.prepareTerminal(ctx)
	if len(model) == 1 {
		source = source.Model(model[0])
	}
	if err := bunSelectQueryError(source); err != nil {
		return 0, err
	}
	if err := validateCountQueryShape(source); err != nil {
		return 0, err
	}

	// Bun's native Count bypasses model SELECT hooks. Page's data statement
	// does not, so bypassing them here could make Total and Records represent
	// different predicates or projections. Run the same hook lifecycle around
	// the logical source on its private snapshot.
	afterSelect, err := runBunSelectHooksBefore(ctx, source)
	if err != nil {
		return 0, err
	}
	if queryErr := bunSelectQueryError(source); queryErr != nil {
		return 0, queryErr
	}
	if shapeErr := validateCountQueryShape(source); shapeErr != nil {
		return 0, shapeErr
	}

	countSource := cloneBunSelectQuery(source)
	if queryErr := bunSelectQueryError(countSource); queryErr != nil {
		return 0, queryErr
	}
	if prepareErr := prepareBunSelectCountSource(countSource); prepareErr != nil {
		return 0, prepareErr
	}
	renderedSource, err := renderBunSelectCountSource(q.state.db.db.QueryGen(), countSource)
	if err != nil {
		return 0, err
	}

	conn, err := bunSelectQueryConn(countSource)
	if err != nil {
		return 0, err
	}
	outer := q.state.db.db.NewSelect().
		TableExpr("(?) AS _credo_count_source", renderedSource)
	if countSource.GetModel() != nil {
		// Preserve QueryEvent.Model without binding the model table, relations,
		// or soft-delete state to the synthetic outer query. All model-aware SQL
		// lives in the complete policy-bearing source.
		if modelErr := setBunSelectQueryEventModel(outer, countSource.GetModel()); modelErr != nil {
			return 0, modelErr
		}
	}
	if conn != nil {
		outer = outer.Conn(conn)
	}
	total, err := outer.Count(ctx)
	if err != nil {
		return 0, wrapMySQLCountExecutionError(q.state.db.family, err)
	}
	if afterSelect != nil {
		if hookErr := afterSelect.AfterSelect(ctx, countSource); hookErr != nil {
			return 0, hookErr
		}
	}
	return total, nil
}

func (q *SelectQuery) validateTypedTerminal(terminal string) error {
	if err := bunSelectQueryError(q.raw); err != nil {
		return err
	}
	if q.raw.GetModel() != nil {
		return typedTerminalModelError(terminal)
	}
	return nil
}

// Scan executes the query and scans results into dest.
//
// Driver errors are mapped to store.Err* sentinels. In particular,
// [sql.ErrNoRows] is returned as [store.ErrNotFound], so callers can use
// [errors.Is](err, store.ErrNotFound) without importing database/sql.
func (q *SelectQuery) Scan(ctx context.Context, dest ...any) error {
	return q.state.db.mapError(ctx, q.prepareTerminal(ctx).Scan(ctx, dest...))
}

// Count executes the query and returns the count of matching logical result
// rows. Credo counts an outer derived-table source, so ungrouped aggregate
// projections, Group, Distinct, and Group+Having share the same cardinality
// contract. Direct compound queries and HAVING without GROUP BY return
// [ErrUnsupportedCountQuery] before execution. Driver errors are mapped to
// store.Err* sentinels. Bun model SELECT hooks run around the logical count
// source; a Page that reaches its data SELECT invokes them once for COUNT and
// once for SELECT.
// Count does not scan or mutate a bound model: successful AfterSelect hooks see
// its pre-count value. MySQL validates the generated derived-table output names
// during execution. ER_DUP_FIELDNAME (1060) from that logical COUNT is wrapped
// with ErrUnsupportedCountQuery while retaining the driver cause; use unique
// aliases to resolve colliding output names.
// Relation callbacks may shape predicates/projections, but cannot replace the
// model or add root ORDER/LIMIT/OFFSET/FOR or an unsupported count shape.
func (q *SelectQuery) Count(ctx context.Context) (int, error) {
	if err := bunSelectQueryError(q.raw); err != nil {
		return 0, err
	}
	if err := validateCountQueryShape(q.raw); err != nil {
		return 0, err
	}
	n, err := q.countLogicalRows(ctx)
	return n, q.state.db.mapError(ctx, err)
}

// Exists executes the query and returns true if at least one row matches.
// Driver errors are mapped to store.Err* sentinels.
func (q *SelectQuery) Exists(ctx context.Context) (bool, error) {
	ok, err := q.prepareTerminal(ctx).Exists(ctx)
	return ok, q.state.db.mapError(ctx, err)
}

// --- Typed terminal methods (generic) ---

// One executes the query and returns its first matching row as a value of T.
// T drives both the table and the scan destination, so the query is built
// model-less and One owns the destination. T may be a struct or a pointer to
// one (User or *User); both forms work:
//
//	user, err := db.Select().Where("id = ?", id).One[User](ctx)
//
// One applies LIMIT 1, so multiple matches are not an error — it returns the
// first row; add an OrderExpr for a deterministic choice. A missing row returns
// [store.ErrNotFound] (wrapping [sql.ErrNoRows]), so callers branch with
// errors.Is(err, store.ErrNotFound); other driver errors map to the store.Err*
// sentinels. The query must be model-less; a model bound through Select, Model,
// or Apply returns [ErrTypedTerminalModel] before execution. The receiver is
// not mutated: an internal execution snapshot receives the destination and the
// ambient transaction from ctx, exactly as for [SelectQuery.Scan].
func (q *SelectQuery) One[T any](ctx context.Context) (T, error) {
	var zero T
	if err := q.validateTypedTerminal("One"); err != nil {
		return zero, err
	}

	// Scan into a *[]T slice (with LIMIT 1), not a *T: bun strips the pointer in
	// a slice element's type, so a pointer T such as *Row works — a *T scan
	// destination would build a **Row that bun rejects. Mirrors the slice model
	// All and Page use; an empty slice means no row matched.
	var out []T
	if err := q.prepareTerminal(ctx).Model(&out).Limit(1).Scan(ctx); err != nil {
		return zero, q.state.db.mapError(ctx, err)
	}
	if len(out) == 0 {
		return zero, q.state.db.mapError(ctx, sql.ErrNoRows) // no row → store.ErrNotFound
	}
	return out[0], nil
}

// All executes the query and returns every matching row as a []T. T drives
// both the table and the scan destination, so the query is built model-less
// and All owns the destination:
//
//	users, err := db.Select().Where("active = ?", true).OrderExpr("id").All[User](ctx)
//
// No matching rows yield an empty, non-nil slice and a nil error — unlike One,
// an empty result is not [store.ErrNotFound]. Driver errors map to the
// store.Err* sentinels. The query must be model-less; a model bound through
// Select, Model, or Apply returns [ErrTypedTerminalModel] before execution. The
// receiver is not mutated: an internal execution snapshot receives the
// destination and ambient transaction from ctx, exactly as for
// [SelectQuery.Scan].
func (q *SelectQuery) All[T any](ctx context.Context) ([]T, error) {
	out := []T{}
	if err := q.validateTypedTerminal("All"); err != nil {
		return out, err
	}
	err := q.prepareTerminal(ctx).Model(&out).Scan(ctx)
	return out, q.state.db.mapError(ctx, err)
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
// req is copied once and never modified. BindQuery applies defaults through
// [pagination.PageRequest.Validate]; manually constructed requests may call
// [pagination.PageRequest.Normalize] (or NormalizeWithMax) first. Page itself
// never defaults or clamps: it strictly requires positive Page and PerPage
// values and an offset/limit representable by Bun v1.2.18. Nil, invalid, or
// overflowing requests return an error matching
// [pagination.ErrInvalidPageRequest] before COUNT or SELECT executes. Valid
// custom PerPage values above pagination.MaxPerPage remain unchanged.
//
// The query must be model-less; a model bound through Select, Model, or Apply
// returns [ErrTypedTerminalModel] before COUNT or SELECT executes. Use a bound
// model with [SelectQuery.Scan] when relations or a custom destination are
// required.
//
// Total is the number of logical result rows before ordering and pagination.
// An ungrouped aggregate projection counts as one row when it produces a row;
// Distinct counts selected projection tuples; Group counts groups; Group with
// Having counts the groups left after Having. Bun v1.2.18 cannot safely window
// standalone Having or a direct UNION/INTERSECT/EXCEPT query, so those shapes
// return [ErrUnsupportedCountQuery] before execution. Restructure compound
// input behind an outer derived-table/CTE source and compose explicit
// Count/Scan terminals when a custom source is required.
// On MySQL, ER_DUP_FIELDNAME (1060) returned by the COUNT statement is wrapped
// with ErrUnsupportedCountQuery after I/O while retaining the driver cause.
// Bun model SELECT hooks run around each statement's private snapshot, so a
// Page that reaches its data SELECT invokes the lifecycle once for COUNT and
// once for SELECT; hook-added predicates and projections affect both Total and
// Records.
// Keep hooks deterministic: transaction isolation cannot make a volatile
// projection or application-side hook choose the same result twice.
//
// COUNT runs first; when it reports zero rows the SELECT is skipped and the
// returned Page carries a non-nil empty Records slice with the requested page
// and per-page preserved. COUNT and SELECT are separate statements, so under
// concurrent writes the total and the page can drift. A normal Read Committed
// transaction does not make the two statements share a snapshot; use an
// isolation level with that guarantee when consistency is required. Both
// statements use internal execution snapshots and join the ambient transaction
// from ctx exactly like [SelectQuery.Scan], so the receiver is never mutated.
// Credo does not start an implicit transaction: PostgreSQL and MySQL/InnoDB
// callers normally select Repeatable Read on an outer [DB.InTxWith] call,
// while SQLite callers use an explicit [DB.InTx] read transaction. Always pass
// the callback's txCtx to Page; isolation support remains driver-specific.
//
// Page is the all-in-one terminal for flows that respond with the queried
// type directly. When records need a model→DTO mapping, run Page[Model] and
// map it with pagination's Page.Map, which carries the metadata over:
//
//	modelPage, err := q.Page[Model](ctx, req)
//	if err != nil {
//		return nil, err
//	}
//	dtoPage := modelPage.Map(func(m Model) DTO { return toDTO(m) })
//
// For a conversion that can fail, iterate modelPage.Records with ordinary
// error handling and build the DTO page with pagination.NewPage, carrying
// modelPage's Total, Page, and PerPage over.
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error) {
	if req == nil {
		return nil, fmt.Errorf("%w: request must not be nil", pagination.ErrInvalidPageRequest)
	}
	request := *req
	offset, err := validatedPageOffset(request)
	if err != nil {
		return nil, fmt.Errorf("sqldb: Page: %w", err)
	}
	if terminalErr := q.validateTypedTerminal("Page"); terminalErr != nil {
		return nil, terminalErr
	}
	if shapeErr := validateCountQueryShape(q.raw); shapeErr != nil {
		return nil, shapeErr
	}

	// COUNT with T's table. T drives the table, so the query is built
	// model-less and the COUNT injects the model on prepareTerminal's execution
	// snapshot — the same snapshot path One/All use, so the receiver is never
	// mutated.
	// The model is a *[]T slice, not a (*T)(nil) scalar: bun strips the pointer
	// in a slice element's type, so the table resolves for both a value T and a
	// pointer T such as *Row. A (*T)(nil) scalar would build (**Row)(nil), which
	// bun rejects — and this mirrors the slice model All uses for the SELECT.
	var model []T
	total, err := q.countLogicalRows(ctx, &model)
	if err != nil {
		return nil, q.state.db.mapError(ctx, err)
	}

	// No rows — skip the SELECT, preserving the requested page/per-page.
	if total == 0 {
		return pagination.NewPage([]T{}, 0, request.Page, request.PerPage), nil
	}

	// SELECT on a private structural copy so Offset/Limit never leak back into
	// the receiver. cloneQuery is deliberately internal here; Page does not
	// depend on the public Clone API as a composition step.
	records, err := q.cloneQuery().Offset(offset).Limit(request.PerPage).All[T](ctx)
	if err != nil {
		return nil, err
	}
	return pagination.NewPage(records, int64(total), request.Page, request.PerPage), nil
}

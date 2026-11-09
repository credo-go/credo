package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/pagination"
)

// SelectQuery proxies bun.SelectQuery with TX injection and error mapping.
type SelectQuery struct {
	raw   *bun.SelectQuery
	state queryState
	proof countProof
}

// countProof records, from the wrapper's own builder methods, what Count and
// Page need to know to prove that Bun's native count returns the number of
// rows the data query returns. The first culprit of each kind is kept for the
// error message.
type countProof struct {
	escape     string // the first escape hatch: Apply, Unwrap, or a Relation callback
	columnExpr string // the first raw ColumnExpr
	having     string // the first Having
	grouped    bool   // GroupExpr or Distinct
}

func (p *countProof) markEscape(culprit string) {
	if p.escape == "" {
		p.escape = culprit
	}
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
//
// Bun's count replaces the projection, so Count and Page cannot prove the
// total of a query with a raw ColumnExpr unless it also carries GroupExpr or
// Distinct; page such a query with [SelectQuery.PageWithCount].
func (q *SelectQuery) ColumnExpr(query string, args ...any) *SelectQuery {
	if q.proof.columnExpr == "" {
		q.proof.columnExpr = query
	}
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

// Limit sets the LIMIT clause. Zero and negative values keep Bun's semantics:
// the clause is omitted.
func (q *SelectQuery) Limit(n int) *SelectQuery {
	q.raw = q.raw.Limit(int64(n))
	return q
}

// Offset sets the OFFSET clause. Zero and negative values keep Bun's
// semantics: the clause is omitted.
func (q *SelectQuery) Offset(n int) *SelectQuery {
	q.raw = q.raw.Offset(int64(n))
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
// bound. A non-nil apply callback is an escape hatch: Count cannot prove the
// total of a query built with one.
func (q *SelectQuery) Relation(name string, apply ...func(*bun.SelectQuery) *bun.SelectQuery) *SelectQuery {
	for _, fn := range apply {
		if fn != nil {
			q.proof.markEscape(fmt.Sprintf("a Relation(%q) callback", name))
			break
		}
	}
	q.raw = q.raw.Relation(name, apply...)
	return q
}

// Distinct adds a DISTINCT clause.
func (q *SelectQuery) Distinct() *SelectQuery {
	q.proof.grouped = true
	q.raw = q.raw.Distinct()
	return q
}

// GroupExpr adds a GROUP BY expression.
func (q *SelectQuery) GroupExpr(query string, args ...any) *SelectQuery {
	q.proof.grouped = true
	q.raw = q.raw.GroupExpr(query, args...)
	return q
}

// Having adds a HAVING clause. Count and Page cannot prove the total of a
// query with Having unless it also carries GroupExpr or Distinct.
func (q *SelectQuery) Having(query string, args ...any) *SelectQuery {
	if q.proof.having == "" {
		q.proof.having = query
	}
	q.raw = q.raw.Having(query, args...)
	return q
}

// Clone returns a top-level query-builder fork through Bun's own Clone. It
// follows Bun's sharing semantics for nested values, including a bound
// destination and CTE or relation subqueries; do not mutate or scan source
// and clone concurrently when they share such values.
func (q *SelectQuery) Clone() *SelectQuery {
	return &SelectQuery{
		raw:   q.raw.Clone(),
		state: q.state,
		proof: q.proof,
	}
}

// Conn sets an explicit connection, bypassing context TX injection. Only a
// connection set here is kept: one set inside an Apply callback is overridden
// by transaction injection at the terminal.
func (q *SelectQuery) Conn(db bun.IConn) *SelectQuery {
	q.state.markConnSet()
	q.raw = q.raw.Conn(db)
	return q
}

// --- Escape hatches ---

// Apply delegates to Bun's native Apply for advanced builder methods
// not in the curated proxy set. Nil functions are filtered out.
// Interceptors (TX injection, error mapping) are preserved on terminal
// methods; a connection set inside fn is overridden by TX injection, so use
// [SelectQuery.Conn] for an explicit connection.
//
// The wrapper cannot see what fn changes, so Count and Page cannot prove the
// total of a query built with Apply; page it with [SelectQuery.PageWithCount].
func (q *SelectQuery) Apply(fns ...func(*bun.SelectQuery) *bun.SelectQuery) *SelectQuery {
	if len(filterApply(fns...)) > 0 {
		q.proof.markEscape("Apply")
	}
	q.raw = applyFiltered(q.raw, fns...)
	return q
}

// ApplyFilter runs fn on a [Filter] over this query's WHERE clause, so a single
// predicate — tenant scoping, soft-delete filters, ownership checks — scopes a
// select, an update, and a delete alike. fn adds predicates only, so a query
// built with it keeps its automatic total in Count and Page. A nil fn is a
// no-op.
func (q *SelectQuery) ApplyFilter(fn func(*Filter)) *SelectQuery {
	applyFilter(q.raw.QueryBuilder(), fn)
	return q
}

// Unwrap returns the underlying *bun.SelectQuery for builder-only use.
// Terminal methods on the unwrapped query bypass Credo interceptors
// (TX injection, error mapping). Use Apply for the recommended escape
// hatch that preserves interceptors. The wrapper cannot see what is done to
// the unwrapped query, so Count and Page cannot prove its total afterwards.
func (q *SelectQuery) Unwrap() *bun.SelectQuery {
	q.proof.markEscape("Unwrap")
	return q.raw
}

// --- Terminal methods ---

// prepareTerminal creates an execution snapshot through Bun's Clone and
// injects the TX from context unless Conn set an explicit connection. The
// snapshot leaves the reusable receiver unchanged.
func (q *SelectQuery) prepareTerminal(ctx context.Context) *bun.SelectQuery {
	return prepareQuery(ctx, q.raw, q.state, (*bun.SelectQuery).Clone)
}

// proveCount reports, before any I/O, whether Bun's native count over source
// returns the number of rows the data query returns. source is the terminal's
// snapshot, whose model is the one the data query would scan with.
func (q *SelectQuery) proveCount(terminal string, source *bun.SelectQuery) error {
	if q.proof.escape != "" {
		return unsupportedCountError(
			terminal,
			"it was built with "+q.proof.escape+", whose changes the wrapper cannot see",
		)
	}
	if hookType := beforeSelectHookType(source); hookType != "" {
		return unsupportedCountError(
			terminal,
			"its model type "+hookType+" implements BeforeSelectHook, which Bun runs for the data query and not for its count",
		)
	}
	if q.proof.grouped {
		return nil
	}
	if q.proof.columnExpr != "" {
		return unsupportedCountError(
			terminal,
			fmt.Sprintf(
				"its raw ColumnExpr(%q) without GroupExpr or Distinct can change how many rows the projection returns, and Bun's count replaces the projection",
				q.proof.columnExpr,
			),
		)
	}
	if q.proof.having != "" {
		return unsupportedCountError(
			terminal,
			fmt.Sprintf(
				"its Having(%q) without GroupExpr or Distinct filters a projection Bun's count replaces",
				q.proof.having,
			),
		)
	}
	return nil
}

// beforeSelectHookType returns the model type of source when it implements
// bun.BeforeSelectHook, as Bun resolves it for the data query, or "".
func beforeSelectHookType(source *bun.SelectQuery) string {
	tableModel, ok := source.GetModel().(bun.TableModel)
	if !ok {
		return ""
	}
	zero := tableModel.Table().ZeroIface
	if _, ok := zero.(bun.BeforeSelectHook); ok {
		return fmt.Sprintf("%T", zero)
	}
	return ""
}

// count runs Bun's native count on source, a proven terminal snapshot that
// already carries the query's connection. Bun's count drops ORDER BY, LIMIT,
// OFFSET and FOR, and runs no model hook.
func (q *SelectQuery) count(ctx context.Context, source *bun.SelectQuery) (int, error) {
	total, err := source.Count(ctx)
	if err != nil {
		return 0, q.state.db.mapError(ctx, wrapMySQLCountExecutionError(q.state.db.family, err))
	}
	if total > math.MaxInt || total < math.MinInt {
		return 0, fmt.Errorf("sqldb: count: total %d does not fit in int", total)
	}
	return int(total), nil
}

func (q *SelectQuery) validateTypedTerminal(terminal string) error {
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

// Count runs Bun's native count and returns the number of rows the query
// returns without its LIMIT and OFFSET. Bun's count drops ORDER BY, LIMIT,
// OFFSET and FOR, wraps a query with GroupExpr or Distinct so that it counts
// groups or distinct tuples, and runs no model hook.
//
// Count runs only on a query the wrapper can prove: one built without Apply,
// Unwrap or a Relation callback; whose model's type does not implement
// bun.BeforeSelectHook; and that either carries GroupExpr or Distinct or has
// no raw ColumnExpr and no Having. Every other query returns an error matching
// [ErrUnsupportedCountQuery] before I/O, naming the rule and its culprit.
//
// The count runs on the query's own connection: the one set with Conn, else
// the ambient transaction from ctx, else the pool. Driver errors are mapped to
// store.Err* sentinels; on MySQL, ER_DUP_FIELDNAME (1060) from the count is
// wrapped with ErrUnsupportedCountQuery while retaining the driver cause.
func (q *SelectQuery) Count(ctx context.Context) (int, error) {
	source := q.prepareTerminal(ctx)
	if err := q.proveCount("Count", source); err != nil {
		return 0, err
	}
	return q.count(ctx, source)
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

// pageWindow copies req once and returns it with its offset. Page and
// PageWithCount never default or clamp: a nil request, a non-positive Page or
// PerPage, or an offset that does not fit in int returns an error matching
// [pagination.ErrInvalidPageRequest] before any I/O.
func pageWindow(terminal string, req *pagination.PageRequest) (pagination.PageRequest, int, error) {
	if req == nil {
		return pagination.PageRequest{}, 0, fmt.Errorf("%w: request must not be nil", pagination.ErrInvalidPageRequest)
	}
	request := *req
	offset, err := request.Offset()
	if err != nil {
		return pagination.PageRequest{}, 0, fmt.Errorf("sqldb: %s: %w", terminal, err)
	}
	return request, offset, nil
}

// Page runs Bun's native count and the data query with LIMIT/OFFSET and
// assembles the result as a *pagination.Page[T]. Like [SelectQuery.One] and
// [SelectQuery.All] it is a typed terminal: T drives both the table and the
// scan destination, so the query is built model-less and Page owns the result:
//
//	page, err := db.Select().
//		ApplyFilter(tenantScope(tenantID)).
//		OrderExpr("created_at DESC").
//		Page[User](ctx, req)            // (*pagination.Page[User], error)
//
// req is copied once and never modified. BindQuery applies defaults through
// [pagination.PageRequest.Validate]; manually constructed requests may call
// [pagination.PageRequest.Normalize] (or NormalizeWithMax) first. Page itself
// never defaults or clamps: it strictly requires positive Page and PerPage
// values and an offset that fits in int. Nil, invalid, or overflowing requests
// return an error matching [pagination.ErrInvalidPageRequest] before any I/O.
// Valid custom PerPage values above pagination.MaxPerPage remain unchanged.
//
// The query must be model-less; a model bound through Select, Model, or Apply
// returns [ErrTypedTerminalModel] before any I/O. Use a bound model with
// [SelectQuery.Scan] when relations or a custom destination are required.
//
// Total is the number of rows the data query returns without its LIMIT and
// OFFSET, or an error, never another number. Page counts under the rule of
// [SelectQuery.Count], with T as the model: a query the wrapper cannot prove,
// including one whose T implements bun.BeforeSelectHook, returns an error
// matching [ErrUnsupportedCountQuery] before any I/O; page it with
// [SelectQuery.PageWithCount]. A query scoped through [SelectQuery.ApplyFilter]
// keeps its automatic total.
//
// The count runs first; when it reports zero rows the data query is skipped
// and the returned Page carries a non-nil empty Records slice with the
// requested page and per-page preserved. The count and the data query are
// separate statements, so under concurrent writes the total and the page can
// drift. A normal Read Committed transaction does not make the two statements
// share a snapshot; use an isolation level with that guarantee when
// consistency is required. Both statements run on the query's own connection
// and join the ambient transaction from ctx exactly like [SelectQuery.Scan],
// so the receiver is never mutated. Credo does not start an implicit
// transaction: PostgreSQL and MySQL/InnoDB callers normally select Repeatable
// Read on an outer [DB.InTxWith] call, while SQLite callers use an explicit
// [DB.InTx] read transaction. Always pass the callback's txCtx to Page;
// isolation support remains driver-specific.
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
	request, offset, err := pageWindow("Page", req)
	if err != nil {
		return nil, err
	}
	if terminalErr := q.validateTypedTerminal("Page"); terminalErr != nil {
		return nil, terminalErr
	}

	// The count binds T's table on prepareTerminal's snapshot, the model the
	// data query scans with, so the receiver is never mutated. The model is a
	// *[]T slice, not a (*T)(nil) scalar: bun strips the pointer in a slice
	// element's type, so the table resolves for both a value T and a pointer T
	// such as *Row, mirroring the slice model All uses for the data query.
	var model []T
	source := q.prepareTerminal(ctx).Model(&model)
	if proofErr := q.proveCount("Page", source); proofErr != nil {
		return nil, proofErr
	}
	total, err := q.count(ctx, source)
	if err != nil {
		return nil, err
	}

	// No rows — skip the data query, preserving the requested page/per-page.
	if total == 0 {
		return pagination.NewPage([]T{}, 0, request.Page, request.PerPage), nil
	}
	return q.pageRecords[T](ctx, request, offset, total)
}

// PageWithCount pages the query with the caller's count, for a query whose
// total Page cannot prove: one built with Apply, Unwrap or a Relation callback,
// with a raw ColumnExpr or Having and neither GroupExpr nor Distinct, or whose
// T implements bun.BeforeSelectHook. Another query's Count passes as it is:
//
//	page, err := db.Select().
//		Apply(withReportColumns).
//		OrderExpr("created_at DESC").
//		PageWithCount[Report](ctx, req, db.Select((*Report)(nil)).ApplyFilter(scope).Count)
//
// PageWithCount validates req as [SelectQuery.Page] does, and requires a
// model-less query, both before any I/O. It then runs count, and the data
// query with the page's window, and builds the page with
// [pagination.NewPage]: Total is the count's result, unchanged. An error from
// count is returned as it is. The count's sort and lock clauses, its
// connection and its consistency with the data query are the caller's.
func (q *SelectQuery) PageWithCount[T any](
	ctx context.Context,
	req *pagination.PageRequest,
	count func(context.Context) (int, error),
) (*pagination.Page[T], error) {
	request, offset, err := pageWindow("PageWithCount", req)
	if err != nil {
		return nil, err
	}
	if count == nil {
		return nil, fmt.Errorf("sqldb: PageWithCount: count must not be nil; pass a function such as another query's Count")
	}
	if terminalErr := q.validateTypedTerminal("PageWithCount"); terminalErr != nil {
		return nil, terminalErr
	}
	total, err := count(ctx)
	if err != nil {
		return nil, err
	}
	return q.pageRecords[T](ctx, request, offset, total)
}

// pageRecords runs the data query on a private copy with the page's window,
// so Offset and Limit never leak back into the receiver.
func (q *SelectQuery) pageRecords[T any](
	ctx context.Context,
	request pagination.PageRequest,
	offset int,
	total int,
) (*pagination.Page[T], error) {
	records, err := q.Clone().Offset(offset).Limit(request.PerPage).All[T](ctx)
	if err != nil {
		return nil, err
	}
	return pagination.NewPage(records, int64(total), request.Page, request.PerPage), nil
}

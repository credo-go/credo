# Pagination Spec

**Status**: Offset pagination implemented; cursor design accepted with implementation gate deferred **Package**: `pagination/` (core), typed terminal in `store/sqldb/` **Depends on**: Root package (`BindQuery` tag support), `store/sqldb/` (adapter layer)

---

## Canonical Source

Implementation-level details for Credo's pagination abstraction are defined in this file. Other documents should keep only high-level references and link here.

---

## Overview

The `pagination/` package provides generic, ORM-agnostic types and utilities for paginated API responses. Actual query execution (COUNT + LIMIT/OFFSET) lives in the ORM-specific adapter (the `SelectQuery.Page[T]` terminal in `store/sqldb/`), keeping the core free of external dependencies.

Key design properties:

- **ORM-agnostic core** — `Page[T]`, `Meta`, `PageRequest` have zero ORM dependencies. Only types, normalization, and sort validation.
- **Adapter-level execution** — the `SelectQuery.Page[T]` terminal in `store/sqldb/` runs the COUNT + LIMIT/OFFSET queries via Bun and returns a ready `*Page[T]`.
- **Exact logical totals** — `Page.Total` is the number of complete logical projection rows before ordering and the page window: plain projection rows, ungrouped aggregate rows, distinct projection tuples, groups, or groups left after `Having`. Unknown totals are not encoded in `Page`.
- **Metadata computed once, carried across mapping** — a `Page[T]`'s pagination metadata (`Total`, `Page`, `PerPage`, `TotalPages`) is computed once, by `Page[T]` or `NewPage`, and never recomputed or hand-copied. When the response carries the queried type, `Page[T]` returns it directly; for a model→DTO response, build `Page[Model]` and `Map` it to `Page[DTO]` — `Page.Map` carries the metadata over.
- **Request binding** — `PageRequest` and `SortRequest` are embeddable structs that work with `BindQuery` via `query:"..."` tags.
- **SQL injection prevention** — `SortRequest.ValidateSort` whitelist-based sort field validation. Only pre-approved DB columns can appear in ORDER BY.
- **Cursor API not exported yet** — the forward-only contract is designed below, but its consumer, Bun-hook, and real-database gates remain open.

---

## Goals

1. **ORM-agnostic types**: `Page[T]`, `Meta`, `PageRequest` import only stdlib. No GORM, Bun, or other ORM types leak into the core.
2. **Metadata computed once, preserved across mapping**: a `Page`'s metadata (`Total`, `Page`, `PerPage`, `TotalPages`) is computed once — by `SelectQuery.Page[T]` or `NewPage` — and never recomputed or hand-copied. When the queried type is the response type, `Page[T]` returns it directly; for a model→DTO response, build `Page[Model]` then `Map` it to `Page[DTO]`, which carries the metadata over mechanically. (This replaces the earlier "single construction, never an intermediate `Page[Model]`" rule: the invariant being protected was always *metadata is computed once, never hand-juggled* — `Map` enforces it by construction, so building an intermediate `Page[Model]` is now safe and idiomatic.)
3. **BindQuery integration**: `PageRequest` and `SortRequest` use `query:"..."` tags for automatic request binding via `ctx.Request().BindQuery(&filter)`.
4. **Defaults and execution safety are separate**: `Normalize()` converts zero/negative values to defaults and caps `PerPage` at `MaxPerPage` (50). `NormalizeWithMax(n)` applies a custom cap per endpoint (shadow `Validate` on the embedding struct to use it with BindQuery). These are forgiving, mutating input-policy operations. `Offset()` and adapter terminals are strict, non-mutating execution boundaries: invalid values return `ErrInvalidPageRequest` rather than being silently normalized into a different query.
5. **Sort safety**: `SortRequest.ValidateSort` rejects unknown sort fields, preventing SQL injection via ORDER BY. Falls back to configured defaults silently.

---

## Architecture

### Two Layers

```text
┌───────────────────────────────────────────────────────┐
│  Application Code                                      │
│                                                        │
│  Controller: ctx.Request().BindQuery(&filter)          │
│  Repository: query.Page[Model](ctx, &filter.PageReq)   │
│  Service:    modelPage.Map(toResponseDTO)              │
│  Controller: page.ToDataMeta() → JSON response         │
└────────────────────┬──────────────────────────────────┘
                     │ uses types
┌────────────────────▼──────────────────────────────────┐
│  pagination/  (core — zero dependencies)              │
│  Page[T], Meta, PageRequest, SortRequest              │
│  NewPage(), Normalize(), ValidateSort(), Offset()     │
└───────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────┐
│  Adapter  (ORM-specific query execution)              │
│  (*SelectQuery).Page[T](ctx, req) → *Page[T]          │
│  (queried-type flows; model→DTO: Page[Model].Map)     │
└───────────────────────────────────────────────────────┘
```

### Data Flow

```text
HTTP Request                Repository              Service                 Controller
────────────                ──────────              ───────                 ──────────
GET /products               ListByFilter()          ListByFilter()          List()
?page=2&per_page=20         │                       │                       │
?sort_by=name               │                       │                       │
                            │                       │                       │
BindQuery(&filter)          │                       │                       │
  ↓                         │                       │                       │
PageRequest{2, 20}    ───►  query.Page[Product]()   │                       │
SortRequest{"name","asc"}   → *Page[Product]   ───► page.Map(toDTO)         │
                            │                       → *Page[DTO]       ───► ToDataMeta()
                            │                       │                       → JSON response
```

---

## Core Package: `pagination/`

### Types

```go
// PageRequest is an embeddable struct for pagination query parameters.
// Works with BindQuery via query tags.
type PageRequest struct {
    Page    int `query:"page"`
    PerPage int `query:"per_page"`
}

// SortRequest is an embeddable struct for sort query parameters.
type SortRequest struct {
    SortBy    string `query:"sort_by"`
    SortOrder string `query:"sort_order"`
}

// Page is a generic paginated result. Its metadata is computed once and
// carried over by Map, so model→DTO is Page[Model] → Map → Page[DTO].
type Page[T any] struct {
    Records    []T
    Total      int64
    Page       int
    PerPage    int
    TotalPages int64
}

// Meta is pagination metadata for JSON serialization.
type Meta struct {
    Total      int64 `json:"total_count"`
    Page       int   `json:"page"`
    PerPage    int   `json:"per_page"`
    TotalPages int64 `json:"total_pages"`
    HasNext    bool  `json:"has_next"`
    HasPrev    bool  `json:"has_prev"`
}

// SortConfig defines allowed sort fields for SQL injection prevention.
type SortConfig struct {
    DefaultField  string
    DefaultOrder  string            // "ASC" or "DESC"
    AllowedFields map[string]string // API field name → DB column name
}
```

### Functions

```go
// ErrInvalidPageRequest identifies a request that cannot safely produce a
// pagination window.
var ErrInvalidPageRequest = errors.New("pagination: invalid page request")

// NewPage creates a Page from raw values. Its ceiling division does not
// overflow when total is near math.MaxInt64.
func NewPage[T any](records []T, total int64, page, perPage int) *Page[T]

// NewEmpty creates an empty Page with default pagination values.
func NewEmpty[T any]() *Page[T]

// Normalize applies forgiving pagination defaults and limits in place.
// Zero/negative → defaults, PerPage capped at max.
func (r *PageRequest) Normalize()

// Validate implements validation.Validatable so that BindQuery
// automatically applies the same normalization policy after decoding.
func (r *PageRequest) Validate() error

// Offset strictly computes the zero-based offset without mutating, defaulting,
// or clamping. Non-positive values and native-int overflow wrap
// ErrInvalidPageRequest.
func (r PageRequest) Offset() (int, error)

// ValidateSort validates sort parameters against allowed fields.
// Returns (dbColumn, order). Invalid input → defaults silently.
// A nil receiver is safe to call.
func (r *SortRequest) ValidateSort(cfg *SortConfig) (column, order string)

// HasNext reports whether there is a page after the current one.
func (p *Page[T]) HasNext() bool

// HasPrev reports whether there is a page before the current one.
func (p *Page[T]) HasPrev() bool

// ToDataMeta splits Page into records slice + Meta for JSON response.
// Meta includes HasNext and HasPrev fields.
func (p *Page[T]) ToDataMeta() ([]T, *Meta)

// Map returns a new Page[U] with each record transformed by fn, carrying
// the pagination metadata over unchanged. fn must be pure; a nil fn panics.
// This is the canonical model→DTO step: Page[Model] → Map → Page[DTO].
func (p *Page[T]) Map[U any](fn func(T) U) *Page[U]
```

### Constants

```go
DefaultPage    = 1
DefaultPerPage = 50
MinPerPage     = 1
MaxPerPage     = 50
```

---

## Adapter: `SelectQuery.Page[T]`

Lives in `store/sqldb/` — the Bun wrapper package. `Page[T]` is a typed terminal alongside `One[T]` / `All[T]` (see the [store spec](store.md)), so `T` drives both the table and the result and the query is built model-less. `T` must be the actual table model; `TableExpr` is not a typed projection override. A model bound through `Select`, `Model`, or `Apply` returns `sqldb.ErrTypedTerminalModel` before COUNT or SELECT executes. Use `TableExpr(...).Scan(ctx, &dest)` for projections and the model-bound `Model(&dest).Relation(...).Scan(ctx)` form for relation loading. Add a stable `ORDER BY` with a unique tie-breaker whenever page membership must be deterministic, for example `created_at DESC, id DESC` rather than `created_at DESC` alone.

```go
// Page runs COUNT + a LIMIT/OFFSET SELECT and assembles a *pagination.Page[T].
// BindQuery can apply Validate's forgiving defaults/clamp policy. Page does not
// repeat that policy: it snapshots req and strictly validates nil, positivity,
// native offset overflow, and Bun v1.2.18's signed-int32 LIMIT/OFFSET range
// before COUNT. Violations wrap pagination.ErrInvalidPageRequest. On zero rows
// SELECT is skipped and the page keeps the snapshot's page/per-page. The caller's
// request and query receiver are not mutated. COUNT and SELECT remain separate
// statements; database visibility is determined by the caller's transaction
// and database-specific isolation. Total is complete logical projection-row
// cardinality: an ungrouped aggregate normally contributes one row, Distinct
// counts selected tuples, Group counts groups, and Group+Having counts the
// remaining groups. Standalone Having and direct compound roots return
// sqldb.ErrUnsupportedCountQuery before database I/O.
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error)
```

### Implementation

```go
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error) {
    if req == nil {
        return nil, fmt.Errorf("%w: request must not be nil", pagination.ErrInvalidPageRequest)
    }
    request := *req
    offset, err := validatedPageOffset(request) // int overflow + Bun int32 range
    if err != nil {
        return nil, fmt.Errorf("sqldb: Page: %w", err)
    }
    if err := q.validateTypedTerminal("Page"); err != nil {
        return nil, err
    }
    if err := validateCountQueryShape(q.raw); err != nil {
        return nil, err
    }

    // COUNT complete logical projection rows with T's table on the source.
    var model []T
    total, err := q.countLogicalRows(ctx, &model)
    if err != nil {
        return nil, q.state.db.mapError(ctx, err)
    }

    // No rows — skip the SELECT, preserving the requested page/per-page.
    if total == 0 {
        return pagination.NewPage([]T{}, 0, request.Page, request.PerPage), nil
    }

    // SELECT on a private structural copy so Offset/Limit do not leak.
    records, err := q.cloneQuery().Offset(offset).Limit(request.PerPage).All[T](ctx)
    if err != nil {
        return nil, err
    }
    return pagination.NewPage(records, int64(total), request.Page, request.PerPage), nil
}
```

`validatedPageOffset` first calls the strict `PageRequest.Offset()` contract,
then rejects `PerPage` or the computed offset above `math.MaxInt32`. This second
bound is adapter-specific: Bun v1.2.18 accepts `int` in its public methods but
stores LIMIT and OFFSET in signed `int32` fields. The division guard runs before
multiplication, so the check is safe on both 32-bit and 64-bit Go targets. A Bun
upgrade must re-run the range conformance test before changing the bound.

The same narrowing risk exists when callers use the curated
`SelectQuery.Limit(int)` or `Offset(int)` methods directly. Those methods accept
the full signed-int32 range so Bun's existing zero/negative behavior is
unchanged, but an `int` outside that range records `sqldb.ErrInvalidLimitOffset`;
the terminal returns it before database execution. This is a proxy guarantee,
not a rewrite of Bun: `Apply` and `Unwrap` expose raw Bun builders and retain
Bun's own conversion semantics.

### Logical total and supported count shapes

`Page.Total` counts complete projection rows in the logical query result before
`ORDER BY` and the Page-owned LIMIT/OFFSET window. Credo clones the query,
removes root ORDER/LIMIT/OFFSET/FOR state, and counts the universal outer
`_credo_count_source` derived table:

| Query shape | `Total` |
| --- | --- |
| Plain filtered projection | Projection rows |
| Ungrouped aggregate projection | Normally one row, including `COUNT(*)` over empty input |
| `Distinct` projection | Distinct selected projection tuples |
| `GroupExpr` | Groups |
| `GroupExpr` + `Having` | Groups left after `Having` |

Both the behavior and generated outer SQL are pinned by conformance tests. Two
root shapes do not have a safe Count+window contract:

- `Having` without `GroupExpr`
- a direct `UNION`/`INTERSECT`/`EXCEPT` query introduced through `Apply`

`Count` and `Page` reject both with `sqldb.ErrUnsupportedCountQuery` before any
database operation. To count a compound query, use it as the source of an outer
derived-table/CTE query. When that source also requires custom result scanning,
compose an explicit count query and data query, then construct
`pagination.NewPage`; typed `Page[T]` does not become a general custom-source or
projection terminal.

MySQL also requires every output name in a derived table to be unique. After
model hooks run, Credo renders the count source once and lets the server apply
its real naming and `sql_mode` rules. Wildcards, implicit aliases, and unaliased
expressions are valid when MySQL derives unique names. If the logical COUNT
returns `ER_DUP_FIELDNAME` (1060 / SQLSTATE `42S21`), Count/Page wraps
`sqldb.ErrUnsupportedCountQuery` after I/O and preserves the driver cause; add
explicit unique aliases to fix the collision. The mapping is Count/Page-local,
so raw and other non-count 1060 errors pass through unchanged. Real tests cover
normal mode and `NO_BACKSLASH_ESCAPES`. PostgreSQL and SQLite are not narrowed.
This follows MySQL's [derived-table output-name rule](https://dev.mysql.com/doc/mysql/en/derived-tables.html).

Bun applies relation callbacks while rendering SQL. Credo therefore renders
the private count source exactly once, rechecks the post-render builder, and
executes those validated bytes. Relation predicates/projections are supported;
a callback that replaces the model, reintroduces root ORDER/LIMIT/OFFSET/FOR,
or creates standalone `Having`/a compound root returns
`sqldb.ErrUnsupportedCountQuery` before I/O.

The universal count source evaluates the complete projection. That makes
aggregate and set-returning cardinality exact, but a costly or volatile
expression may execute once for COUNT and again for the data SELECT. Such
repositories should use a deliberately equivalent cheaper count query.

The count source also runs the model SELECT hook lifecycle on its private
snapshot: `BeforeSelect`, `BeforeAppendModel`, and `AfterSelect` after a
successful count. When Page proceeds to its data SELECT, Bun invokes the hooks
again for that statement. Hook-added predicates/projections therefore affect
both `Total` and `Records`. The outer count keeps the model identity in
`QueryEvent.Model` for observability, while soft-delete policy is applied only
inside `_credo_count_source` and is not duplicated by the outer query. Count
does not scan or mutate a bound model; its successful `AfterSelect` sees the
model's pre-count value and exists to close the query lifecycle.

Hooks must be deterministic. Repeatable Read can stabilize database rows, but
it cannot make a volatile projection or application-side hook decision return
the same value in two executions. Those cases can still make `Total` and
`Records` differ and should use an explicit, deliberately equivalent count/data
composition.

Credo deliberately has no custom-count callback or strategy. Shared predicates
can be applied to separate count and data builders with `ApplyQueryBuilder`, and
advanced builders remain available through `Apply`; combine their results with
`NewPage`. A first-class abstraction is reconsidered only after two real
consumers repeat the same pattern. An API cannot itself prove that divergent
count and data queries describe the same logical set.

### COUNT + SELECT snapshot consistency

`Page` runs two statements and never starts an implicit transaction. Without an
explicit transaction, the pool may execute them on different connections and
snapshots. Inside one transaction, visibility is still database-specific. The
transaction owner must pass the callback's `txCtx` to `Page`; passing the outer
context would execute outside that transaction.

For PostgreSQL and InnoDB deployments that require one snapshot, establish a
read-only Repeatable Read transaction at the outermost boundary:

```go
err := db.InTxWith(ctx, &sql.TxOptions{
    Isolation: sql.LevelRepeatableRead,
    ReadOnly:  true,
}, func(txCtx context.Context) error {
    var err error
    page, err = db.Select().
        Where("tenant_id = ?", tenantID).
        OrderExpr("created_at DESC, id DESC").
        Page[User](txCtx, req)
    return err
})
```

Non-default options cannot be introduced by a nested savepoint;
`InTxWith` returns `sqldb.ErrNestedTxOptions` there. Database behavior is:

| Database | Relevant behavior |
| --- | --- |
| PostgreSQL | Default Read Committed takes a new snapshot per statement, so COUNT and SELECT can drift. Repeatable Read uses the snapshot established by the first non-control statement. |
| MySQL/InnoDB | Default Repeatable Read makes ordinary nonlocking consistent reads share the first-read snapshot. Server configuration can change the default; other engines, locking reads, and Read Committed differ, so request Repeatable Read explicitly. |
| SQLite | A plain explicit transaction keeps the snapshot established by its first read. WAL allows a concurrent writer while the reader retains it; rollback-journal mode may block the writer. Shared cache plus `PRAGMA read_uncommitted=ON` is the documented exception. |

The pinned modernc SQLite driver does not reliably enforce
`sql.TxOptions.Isolation` or `ReadOnly`; use plain `InTx` for SQLite's explicit
snapshot boundary. Fail-loud driver capability validation is deferred. Primary
references: [PostgreSQL transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html),
[InnoDB transaction isolation](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html),
[InnoDB consistent reads](https://dev.mysql.com/doc/refman/8.4/en/innodb-consistent-read.html),
[SQLite isolation](https://www.sqlite.org/isolation.html), and
[SQLite transactions](https://www.sqlite.org/lang_transaction.html).

### Exact-total `Page` versus a total-free slice

`Page` always carries an exact `Total` and `TotalPages`; `HasNext` derives from
that metadata. Unknown totals are not represented as zero, `-1`, a pointer, or
an omitted JSON field. A total-free offset query can fetch `PerPage+1` records
to determine `HasNext`, but its response uses the separate working name
`Slice[T]` until its own design gate. Keyset pagination uses the distinct reserved
`CursorPage[T]` shape described below. There is no `WithCount(false)` mode that
changes the meaning of an existing `Page`.

### Model→DTO responses

`Page[T]` answers with the queried type directly. When records must be mapped to a response DTO, run the query as `Page[Model]` and map it with `Page.Map` — the metadata is carried over, so the intermediate `Page[Model]` needs no hand-juggling:

```go
modelPage, err := q.Page[Model](ctx, req)
if err != nil {
    return nil, err
}
dtoPage := modelPage.Map(func(m Model) DTO { return toDTO(m) }) // *Page[DTO]
```

`Map` takes a pure `func(Model) DTO`. When the conversion itself can fail, fetch a model page first, map its records with ordinary error handling, and build the DTO page with `NewPage`. The query remains model-less and public `Clone` is not needed:

```go
modelPage, err := q.Page[Model](ctx, req)
if err != nil {
    return nil, err
}
dtos := make([]DTO, len(modelPage.Records))
for i, m := range modelPage.Records {
    dtos[i], err = toDTO(m) // fallible conversion
    if err != nil {
        return nil, err
    }
}
page := pagination.NewPage(
    dtos, modelPage.Total, modelPage.Page, modelPage.PerPage,
)
```

---

## Usage Example

This end-to-end walkthrough wires `BindQuery`, the `pagination/` core, and the `sqldb` typed terminals into the canonical Controller → Service → Repository layout. Because the response is a DTO (`ProductResponse`, not the `Product` table model), the repository returns `Page[*Product]` from the `Page[T]` terminal and the service maps it to `Page[*ProductResponse]` with `Page.Map`; when the response carries the queried type, return `Page[T]` directly without the map. Domain types (`Product`, `ProductResponse`) and column names are illustrative; only the Credo imports are framework APIs.

### Filter struct

Embed `PageRequest` and `SortRequest` so a single `BindQuery` call decodes pagination, sort, and filter parameters at once. `PageRequest.Validate()` applies forgiving page / per_page defaults and clamping because it implements `validation.Validatable` — `BindQuery` invokes it after decode, so no manual `Normalize()` call is needed. This does not replace the terminal's strict, non-mutating execution validation.

```go
type ProductFilter struct {
    pagination.PageRequest         // page, per_page
    pagination.SortRequest         // sort_by, sort_order
    SearchTerm string `query:"search_term"`
}
```

### Sort whitelist

`SortConfig` maps API field names to DB column names. `ValidateSort` returns the configured default when the request asks for a field that isn't in the whitelist — this is the SQL-injection guard for `ORDER BY`.

```go
var productSortConfig = &pagination.SortConfig{
    DefaultField: "created_at",
    DefaultOrder: "DESC",
    AllowedFields: map[string]string{
        "name":       "name",
        "created_at": "created_at",
        "price":      "price",
    },
}
```

### Repository

```go
type productRepo struct {
    db *sqldb.DB
}

func (r *productRepo) ListByFilter(ctx context.Context, filter *ProductFilter) (*pagination.Page[*Product], error) {
    query := r.db.Select() // model-less: the terminal owns the model via T

    if filter.SearchTerm != "" {
        query = query.Where("name ILIKE ?", "%"+filter.SearchTerm+"%")
    }

    column, order := filter.SortRequest.ValidateSort(productSortConfig)
    query = query.OrderExpr(column + " " + order)

    page, err := query.Page[*Product](ctx, &filter.PageRequest)
    if err != nil {
        return nil, fmt.Errorf("list products: %w", err)
    }
    return page, nil
}
```

### Service

```go
type productService struct {
    repo *productRepo
}

func (s *productService) ListByFilter(ctx context.Context, filter *ProductFilter) (*pagination.Page[*ProductResponse], error) {
    page, err := s.repo.ListByFilter(ctx, filter)
    if err != nil {
        return nil, err
    }
    return page.Map(toProductResponse), nil
}
```

### Controller

```go
type ProductHandler struct {
    service *productService
}

func (h *ProductHandler) List(ctx *credo.Context) error {
    var filter ProductFilter
    if err := ctx.Request().BindQuery(&filter); err != nil {
        return err // RFC 7807 problem details on bind/validation failure
    }

    page, err := h.service.ListByFilter(ctx.Context(), &filter)
    if err != nil {
        return err // framework classifies, logs, and renders the response
    }

    data, meta := page.ToDataMeta()
    return ctx.Response().JSON(http.StatusOK, map[string]any{
        "data": data,
        "meta": meta,
    })
}
```

### Wiring

Register the repo, service, and handler in the DI container; the constructor parameters drive resolution.

```go
app.Provide[*productRepo](func(infra credo.Infra, db *sqldb.DB) *productRepo {
    return &productRepo{db: db}
})
app.Provide[*productService](func(infra credo.Infra, repo *productRepo) *productService {
    return &productService{repo: repo}
})
app.Provide[*ProductHandler](func(infra credo.Infra, svc *productService) *ProductHandler {
    return &ProductHandler{service: svc}
})

handler := app.Resolve[*ProductHandler]()
app.GET("/products", handler.List)
```

`Product`, `ProductResponse`, and `toProductResponse` are deliberately left to the application — pagination is orthogonal to domain modelling.

---

## Cursor/Keyset Design Gate

The design direction is accepted, but no cursor types or terminals are exported
yet. `CursorPage[T]` is reserved for keyset pagination; `Slice[T]` is only the
working name for a future total-free **offset** window and gets its own design
gate before export. They do not weaken `Page[T]`'s exact total contract and are
not aliases of one another.

### Reserved forward-only shape

The first cursor delivery is deliberately one-way:

```text
request:  after=<opaque cursor>&per_page=50

response:
{
  "data": [],
  "meta": {
    "per_page": 50,
    "has_next": false,
    "next_cursor": null
  }
}
```

The planned core names are `CursorRequest`, `CursorPage[T]`, and `CursorMeta`;
the sqldb terminal is `SelectQuery.CursorPage[T]`. Records are always a non-nil
slice. The final window uses JSON `null` for `next_cursor`. There is no
`before`, previous cursor, arbitrary direction parameter, page number,
`total_count`, `total_pages`, or `WithTotal` switch in the first delivery.
Repositories that need a total run a separate explicit `Count` and put it in an
application-owned envelope. Cursor execution itself never runs COUNT.

`CursorRequest.Validate` will mirror `PageRequest`'s input-policy role by
defaulting/clamping `per_page`; the terminal will snapshot the request and
strictly reject nil, non-positive, overflowed, or adapter-unrepresentable
execution values without mutating it. Because Bun v1.2.18 stores LIMIT as a
signed int32 and the terminal adds one, strict execution requires
`per_page <= math.MaxInt32 - 1` and checks native-int addition before building
SQL.

### Terminal-owned keyset

The terminal owns ordering through an immutable adapter-level keyset spec. The
first delivery accepts only a model-less default full-model SELECT plus curated
`Where` or `ApplyQueryBuilder` predicates whose top-level separator is AND.
`WherePK` is excluded because Bun resolves it when the model-less builder is
composed, before the typed terminal supplies `T`, and records an unrecoverable
builder error. A top-level `WhereOr` is also rejected: appending the cursor
ladder could otherwise produce `A OR (B AND cursor)` instead of
`(A OR B) AND cursor`. An OR filter must first be enclosed in one Bun
`WhereGroup` through `ApplyQueryBuilder`; the resulting group itself joins the
root with AND. The terminal validates this final WHERE tree before I/O. It also
rejects custom `Column`/`ColumnExpr`/`ExcludeColumn`/`TableExpr`/`Apply`, joins, root
ORDER/LIMIT/OFFSET/lock, and distinct, aggregate, grouped, compound, or
otherwise non-row-shaped sources. Full-model projection ensures every cursor
key was actually scanned into `T`. A join can multiply one model row, so a
root-table unique id is not necessarily unique in the SQL result. Supporting
joins or projections requires a later logical-row uniqueness/projection proof.
This fail-loud boundary prevents the token's order and the executed SQL order
from diverging.

The first delivery also rejects model types that implement Bun SELECT,
append-model, or row/result scan hooks. A pre-query hook can replace the model
or alter query shape/order/window; `AfterScanRow` or `AfterSelect` can mutate a
cursor key after SQL ordering but before token generation. Bun v1.2.18 exposes
no public seam that lets Credo apply and verify terminal-owned state after all
of those hooks, so accepting hook-capable models would make fail-loud behavior
unprovable.

The terminal otherwise inherits the existing typed-terminal invariants: the
query starts model-less, execution uses a private snapshot, the reusable
receiver is not mutated, explicit connections win over ambient transaction
injection, ambient transactions still propagate, and database failures retain
the current store-error mapping.

Every keyset component must be:

- a quoted database column identifier, not request-provided SQL;
- selected into the model and encoded with its original type;
- immutable and `NOT NULL` for the lifetime of a cursor walk; and
- backed by a matching composite index where performance matters.

The final component is a separately declared unique tie-breaker. This makes the
order total even when an earlier value repeats. ASC and DESC components may be
mixed, but each direction is fixed by the keyset spec rather than the request.

For keys `(a ASC, b DESC, id ASC)`, an `after` boundary expands to the portable
lexicographic ladder:

```sql
a > :a
OR (a = :a AND b < :b)
OR (a = :a AND b = :b AND id > :id)
```

The complete ladder is appended to the already-validated filter root as one
parenthesized AND condition. Column identifiers are quoted, and decoded values go through Bun's
typed SQL formatter; token bytes are never concatenated into SQL. Bun v1.2.18
renders the final SQL rather than passing driver bind parameters, so the
contract is formatter safety, not a claim about driver placeholders.

The terminal fetches `per_page + 1`, trims the extra row, derives `has_next`,
and encodes the last included record as `next_cursor`. It never uses OFFSET.
The expanded predicate is preferred over a row constructor because it handles
mixed directions uniformly and follows MySQL's documented optimization advice
for fuller composite-index use. See
[MySQL row-constructor optimization](https://dev.mysql.com/doc/refman/8.0/en/row-constructor-optimization.html).
NULL keys are excluded: comparison truth and default NULL placement differ
across supported databases. See
[PostgreSQL ordering](https://www.postgresql.org/docs/current/queries-order.html),
[MySQL NULL ordering](https://dev.mysql.com/doc/refman/8.0/en/null-values.html),
and [SQLite type ordering](https://www.sqlite.org/datatype3.html).

### Cursor integrity and privacy

There is no implicit unsigned or process-local-secret default. The planned
public-HTTP codec requires an explicit HMAC-SHA256 keyring. Each key is at least
32 cryptographically random bytes generated by a CSPRNG; the constructor can
enforce length, while entropy remains a deployment/configuration contract.
Exactly one key signs, older keys are verify-only, key-id lookup is direct, and
MAC comparison is constant-time. The first delivery caps the ring
at eight keys, the key tuple at eight components, the decoded payload at 1 KiB,
and the complete token at 2 KiB before parsing. Rotation retention is fixed
only together with the consumer's expiry policy; without an expiry horizon, a
verification key cannot be retired safely. Its signed input binds the
following; the construction follows
[RFC 2104](https://www.rfc-editor.org/rfc/rfc2104.html):

- token-format version;
- endpoint/query identity and canonical keyset order;
- normalized filter scope; and
- tenant/authorization scope.

The wire format is versioned, key-id-addressed, base64url-safe, and
integrity-protected, but its exact byte framing is deliberately not frozen yet.
Before implementation, the consumer must drive canonical tuple encoding,
key-id grammar, padding rules, external-scope canonicalization, MAC framing,
and cross-implementation golden vectors. The MAC uses a fixed Credo cursor
domain separator and covers the version, key id, payload, and external scope
binding. Key material is copied into the codec, never serialized, logged, or
included in an error. Tuple members retain explicit types and fixed arity;
decoding does not coerce JSON numbers into a generic floating-point value.
Expiry and rotation-retention durations are intentionally not fixed without a
consumer's request-lifetime requirements.

A token from one scope therefore fails closed in another. Decode has a strict
size limit, exact version/arity/type validation, no unknown fields or trailing
data, and errors never echo token contents or key values.

Malformed, tampered, unknown-key, scope-mismatched, and—when an expiry policy is
configured—expired client tokens share one public invalid-cursor error and map
to HTTP 400 without revealing the reason. Configuration or cursor-encoding
failures are internal errors; database failures keep the existing `store.Error`
mapping. Shipping this distinction also requires the framework's
transport-neutral taxonomy to gain a general invalid-argument kind rather than
teaching the HTTP layer about pagination.

A cursor is not an authorization capability. Every request re-runs normal
authentication, authorization, tenant predicates, and normalized filters;
signed scope binding is defense in depth against cross-query replay, not a
replacement for those checks. “Opaque” means clients must not interpret or
construct the token. Its signed payload is still visible and therefore not
confidential.

Signing provides integrity, not confidentiality. The payload remains visible,
so the first delivery permits only non-sensitive keys. Encryption (AEAD),
server-side cursors, expiry policy, and a deliberately named trusted-only
unsigned codec remain separate decisions driven by a real consumer.

### Mutation contract

Cursor pages are usually separate requests and do not share a transaction
snapshot. With immutable ordering keys:

- inserting or deleting a row strictly before the boundary does not shift the
  next window;
- inserting a row after the boundary may make it appear later;
- deleting an unseen row removes it from later windows; and
- changing an ordering key may duplicate or skip that row and is unsupported.

The conformance suite must demonstrate these cases and the corresponding
offset duplicate/skip behavior, while proving no duplicate keyset row appears.

### Gate-opening conditions

Implementation remains deferred until all of these are true:

1. a concrete consumer contributes its real model, filters, order, tenant
   binding, and key-rotation requirements;
2. sqldb either pre-I/O rejects hook-capable models or can prove model, query
   shape, order/window, scanned key values, and token values remain aligned
   across every Bun model/scan hook;
3. real PostgreSQL, MySQL, and SQLite jobs pass generated-SQL, NULL/collation,
   index-plan, insert/delete, and typed-value round-trip tests—including signed
   integers above 2^53, timestamp precision/timezone, deterministic string
   collation, and consumer-specific UUID/decimal types; floats/NaN have no
   default support;
4. the transport-neutral fault taxonomy maps invalid cursor input through the
   root renderer, built-in observer, and access-log middleware as HTTP 400; and
5. canonical wire framing and cross-implementation golden vectors are fixed.

Until then, a repository may own its keyset SQL and cursor codec explicitly,
but Credo will not freeze a speculative public generic abstraction.

---

## Design Decisions

| Decision | Rationale |
| --- | --- |
| Core has zero ORM deps | Consistent with store adapter pattern |
| `Page[T]` terminal returns `*Page[T]`; model→DTO maps via `Page.Map` | The all-in-one terminal is ergonomic when the response is the queried type. For a DTO mapping, run `Page[Model]` and `Map` to `Page[DTO]`; `Map` carries the metadata over, so the intermediate `Page[Model]` costs only a slice transform, not hand-copied metadata |
| Typed Page queries are model-less and `T` is the table model | The terminal owns model selection. A pre-bound model returns `sqldb.ErrTypedTerminalModel`; projections and relations use explicit-destination `Scan` |
| COUNT and SELECT are separate statements | `Page` starts no implicit transaction. Database visibility comes from the caller's outer transaction and database-specific isolation; PostgreSQL Read Committed can drift, while an appropriate first-read snapshot can keep both statements stable |
| `Total` is complete logical projection cardinality | Credo counts a universal outer source after removing root ORDER/LIMIT/OFFSET/FOR, covering ungrouped aggregates, distinct tuples, groups, and post-`Having` groups; model SELECT hooks run on the private source; unsafe standalone `Having` and direct compound roots fail pre-I/O, while MySQL logical-count 1060 is wrapped with `ErrUnsupportedCountQuery` after I/O and preserves its driver cause |
| No custom-count strategy yet | Existing explicit count + data query + `NewPage` composition covers advanced sources; wait for two real consumers before committing another public API |
| No unknown total in `Page` | Total-free offset/cursor responses have different metadata and remain separate future types |
| Cursor and total-free offset names stay separate | `CursorPage[T]` is the reserved keyset result; `Slice[T]` is only the working name for a future total-free offset result with its own design gate |
| Cursor implementation gate remains closed | A real consumer, a fail-loud Bun hook boundary, invalid-argument transport mapping, canonical wire vectors, and PostgreSQL/MySQL/SQLite conformance are required before public symbols ship |
| Normalize policy is separate from execution validation | `Normalize`/`Validate` mutate the input to apply defaults and caps. `Offset` and `SelectQuery.Page` never normalize or clamp: they reject unsafe values with `ErrInvalidPageRequest`, preserving a valid custom `PerPage` above 50 |
| `Offset()` returns `(int, error)` | A plain `int` result could silently wrap. The pre-v1 signature break makes arithmetic failure explicit and keeps invalid LIMIT/OFFSET state from reaching adapters |
| Bun Page windows are bounded to signed int32 | Bun v1.2.18 narrows its public `int` LIMIT/OFFSET inputs internally. `sqldb` validates that adapter-specific range before COUNT and rechecks it on Bun upgrades |
| `PageRequest` uses non-pointer `int` | `Normalize()` handles absence/zero as the default-input policy. Strict execution validation runs only after policy has been applied; callers constructing requests manually must supply positive values |
| `NewPage` uses quotient + remainder ceiling division | `(total + perPage - 1) / perPage` can overflow near `math.MaxInt64`; quotient plus a non-zero-remainder increment produces the same ceiling safely |
| `ValidateSort` as method on `SortRequest` | SQL injection prevention is ORM-agnostic logic; method is more idiomatic than free function |
| `Page.Map[U]` for model→DTO (Go 1.27 generic method) | Reverses the earlier "no convert function" stance: the metadata-integrity invariant that justified it (compute once, never hand-copy) is now *enforced* by `Map` rather than by forbidding intermediate pages. A generic method (not a free function) keeps the call fluent on the page the caller already holds, and stays in the ORM-agnostic core |
| `SortConfig` whitelist approach | Only pre-approved fields can reach ORDER BY. Safer than blacklist or regex |

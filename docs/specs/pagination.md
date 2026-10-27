# Pagination Spec

**Status:** Implemented (beta); cursor/keyset pagination deferred **Package:** `pagination/`, `store/sqldb/` (`SelectQuery.Page[T]`) **ADR:** [ADR-015](../adr/015-data-access.md) **Guide:** [Data access guide](../guides/data-access.md)

This spec owns offset pagination (the `pagination` core and the `SelectQuery.Page[T]` terminal), the logical-count contract that `SelectQuery.Count` and `Page.Total` share, and the cursor/keyset design gate. Query builders, transactions and error mapping are in the [store spec](store.md).

## Overview

The `pagination/` package provides generic, ORM-agnostic types and utilities for paginated API responses. Query execution (COUNT + LIMIT/OFFSET) lives in the ORM-specific adapter: the `SelectQuery.Page[T]` terminal in `store/sqldb/` (which imports `pagination`) runs both queries via Bun and returns a ready `*Page[T]`.

Key design properties:

- **ORM-agnostic core** — `Page[T]`, `Meta`, `PageRequest`, `SortRequest` and `SortConfig` import only the standard library: types, normalization, and sort validation.
- **Exact logical totals** — `Page.Total` is the number of complete logical projection rows before ordering and the page window ([logical total](#logical-total-and-supported-count-shapes)). Unknown totals are not encoded in `Page`.
- **Metadata computed once, carried across mapping** — a `Page[T]`'s pagination metadata (`Total`, `Page`, `PerPage`, `TotalPages`) is computed once, by `Page[T]` or `NewPage`, and never recomputed or hand-copied. When the response carries the queried type, `Page[T]` returns it directly; for a model→DTO response, build `Page[Model]` and `Map` it to `Page[DTO]` — `Page.Map` carries the metadata over.
- **Forgiving input policy, strict execution** — `Normalize()` converts zero/negative values to defaults and caps `PerPage` at `MaxPerPage` (50); `NormalizeWithMax(n)` applies a custom cap per endpoint. Both mutate the request. `Offset()` and adapter terminals are strict, non-mutating execution boundaries: invalid values return `ErrInvalidPageRequest` rather than being silently normalized into a different query, and a valid custom `PerPage` above 50 is kept.
- **Request binding** — `PageRequest` and `SortRequest` are embeddable structs that work with `ctx.Request().BindQuery(&filter)` via `query:"..."` tags.
- **SQL injection prevention** — `SortRequest.ValidateSort` validates sort fields against a whitelist. Only pre-approved DB columns can appear in ORDER BY; unknown fields fall back to the configured default silently.
- **Cursor API not exported yet** — the forward-only contract is designed below, but its consumer, Bun-hook, and real-database gates remain open.

## Architecture

### Two Layers

```text
┌──────────────────────────────────────────────────────────┐
│  Application code                                        │
│                                                          │
│  Controller: ctx.Request().BindQuery(&filter)            │
│  Repository: query.Page[Model](ctx, &filter.PageRequest) │
│  Service:    modelPage.Map(toResponseDTO)                │
│  Controller: page.ToDataMeta() → JSON response           │
└────────────────────────────┬─────────────────────────────┘
                             │ uses types
┌────────────────────────────▼─────────────────────────────┐
│  pagination/  (core, stdlib only)                        │
│  Page[T], Meta, PageRequest, SortRequest, SortConfig     │
│  NewPage(), Normalize(), Offset(), ValidateSort(), Map() │
└──────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────┐
│  store/sqldb  (adapter, ORM-specific execution)          │
│  (*SelectQuery).Page[T](ctx, req) → *Page[T]             │
└──────────────────────────────────────────────────────────┘
```

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
// ErrInvalidPageRequest reports page/per-page values that cannot safely
// produce a pagination window.
var ErrInvalidPageRequest = errors.New("pagination: invalid page request")

// NewPage creates a Page from raw values; a nil records slice becomes an
// empty slice. TotalPages is the quotient plus one for a non-zero
// remainder, so it does not overflow when total is near math.MaxInt64.
func NewPage[T any](records []T, total int64, page, perPage int) *Page[T]

// NewEmpty creates an empty Page with default pagination values.
func NewEmpty[T any]() *Page[T]

// Normalize applies forgiving pagination defaults and limits in place.
// Zero/negative → defaults, PerPage clamped to [MinPerPage, MaxPerPage].
func (r *PageRequest) Normalize()

// NormalizeWithMax is Normalize with a custom PerPage cap; values <= 0
// fall back to MaxPerPage.
func (r *PageRequest) NormalizeWithMax(maxPerPage int)

// Validate implements validation.Validatable so that BindQuery
// automatically applies Normalize after decoding.
func (r *PageRequest) Validate() error

// Offset strictly computes the zero-based offset without mutating, defaulting,
// or clamping. Non-positive values and native-int overflow wrap
// ErrInvalidPageRequest. The overflow guard divides before it multiplies,
// so it is safe on 32-bit and 64-bit targets.
func (r PageRequest) Offset() (int, error)

// ValidateSort validates sort parameters against allowed fields and
// returns (dbColumn, order) with order uppercased. Invalid input falls back
// to the configured defaults silently. A nil cfg returns "", ""; a nil
// receiver is safe to call.
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

## Adapter: `SelectQuery.Page[T]`

`Page[T]` lives in `store/sqldb/` and is a typed terminal alongside `One[T]` / `All[T]`, with the same model-less rules (see the [store spec](store.md#query-builders)): `T` drives both the table and the result, a model bound through `Select`, `Model`, or `Apply` returns `sqldb.ErrTypedTerminalModel` before COUNT or SELECT executes, and `T` must be the actual table model (`TableExpr` is not a typed projection override; projections and relations use an explicit-destination `Scan`). Offset pagination requires a stable total order: repositories must end `ORDER BY` with a unique tie-breaker, for example `created_at DESC, id DESC` rather than `created_at DESC` alone, so rows with equal sort values cannot move between pages.

```go
// Page runs COUNT + a LIMIT/OFFSET SELECT and assembles a *pagination.Page[T].
// req is copied once; neither req nor the query receiver is mutated. Before
// any I/O, nil, non-positive values or offset overflow wrap
// pagination.ErrInvalidPageRequest, a bound model returns
// ErrTypedTerminalModel, and an unsupported count shape returns
// ErrUnsupportedCountQuery. A zero total skips the SELECT and keeps the
// requested page and per-page with a non-nil empty Records slice.
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error)
```

`Page` does not repeat the forgiving input policy: its strict check is `PageRequest.Offset()`, run before COUNT. No narrower adapter bound applies, since Bun v1.3.0 stores LIMIT and OFFSET as `int64`.

`ErrInvalidPageRequest` has no HTTP classification: returned unchanged from a handler, it renders as 500 and is logged as a server error. `BindQuery` does not rule it out, because `Normalize` does not cap `Page`: a client-supplied `page` large enough that the offset overflows `int` passes binding and fails in `Page`. A handler that serves client input maps it to 400 itself, as the [Controller](#controller) example does.

### Logical total and supported count shapes

`SelectQuery.Count` and `Page.Total` count complete projection rows in the logical query result before `ORDER BY` and the LIMIT/OFFSET window. Credo clones the query, removes root ORDER/LIMIT/OFFSET/FOR state, and counts the universal outer `_credo_count_source` derived table:

| Query shape | `Total` |
| --- | --- |
| Plain filtered projection | Projection rows |
| Ungrouped aggregate projection | Normally one row, including `COUNT(*)` over empty input |
| `Distinct` projection | Distinct selected projection tuples |
| `GroupExpr` | Groups |
| `GroupExpr` + `Having` | Groups left after `Having` |

Both the behavior and generated outer SQL are pinned by conformance tests. Two root shapes do not have a safe Count+window contract:

- `Having` without `GroupExpr`
- a direct `UNION`/`INTERSECT`/`EXCEPT` query introduced through `Apply`

`Count` and `Page` reject both with `sqldb.ErrUnsupportedCountQuery` before any database operation. To count a compound query, use it as the source of an outer derived-table/CTE query. When that source also requires custom result scanning, compose an explicit count query and data query, then construct `pagination.NewPage`; typed `Page[T]` does not become a general custom-source or projection terminal.

MySQL also requires every output name in a derived table to be unique. After model hooks run, Credo renders the count source once and lets the server apply its real naming and `sql_mode` rules. Wildcards, implicit aliases, and unaliased expressions are valid when MySQL derives unique names. If the logical COUNT returns `ER_DUP_FIELDNAME` (1060 / SQLSTATE `42S21`), Count/Page wraps `sqldb.ErrUnsupportedCountQuery` after I/O and preserves the driver cause; add explicit unique aliases to fix the collision. The server does not say which derived-table level produced the error, so a 1060 from a caller-supplied nested derived table inside the count source is wrapped the same way. The retained driver cause is diagnostic and must not be rendered directly to HTTP. The mapping is Count/Page-local, so raw and other non-count 1060 errors pass through unchanged. Real MySQL tests cover normal mode and `NO_BACKSLASH_ESCAPES`. PostgreSQL and SQLite are not narrowed. This follows MySQL's [derived-table output-name rule](https://dev.mysql.com/doc/mysql/en/derived-tables.html).

Bun applies relation callbacks while rendering SQL. Credo therefore renders the private count source exactly once, rechecks the post-render builder, and executes those validated bytes. Relation predicates/projections are supported; a callback that replaces the model, reintroduces root ORDER/LIMIT/OFFSET/FOR, or creates standalone `Having`/a compound root returns `sqldb.ErrUnsupportedCountQuery` before I/O.

The universal count source evaluates the complete projection. That makes aggregate and set-returning cardinality exact, but a costly or volatile expression may execute once for COUNT and again for the data SELECT. Such repositories should use a deliberately equivalent cheaper count query.

The count source also runs the model SELECT hook lifecycle on its private snapshot: `BeforeSelect`, `BeforeAppendModel`, and `AfterSelect` after a successful count. When Page proceeds to its data SELECT, Bun invokes the hooks again for that statement. Hook-added predicates/projections therefore affect both `Total` and `Records`. The outer count keeps the model identity in `QueryEvent.Model` for observability, while soft-delete policy is applied only inside `_credo_count_source` and is not duplicated by the outer query. Count does not scan or mutate a bound model; its successful `AfterSelect` sees the model's pre-count value and exists to close the query lifecycle.

Hooks must be deterministic. Repeatable Read can stabilize database rows, but it cannot make a volatile projection or application-side hook decision return the same value in two executions. Those cases can still make `Total` and `Records` differ and should use an explicit, deliberately equivalent count/data composition.

Credo deliberately has no custom-count callback or strategy. Shared predicates can be applied to separate count and data builders with `ApplyQueryBuilder`, and advanced builders remain available through `Apply`; combine their results with `NewPage`. A first-class abstraction is reconsidered only after two real consumers repeat the same pattern. An API cannot itself prove that divergent count and data queries describe the same logical set.

### COUNT + SELECT snapshot consistency

`Page` runs two statements and never starts an implicit transaction. Without an explicit transaction, the pool may execute them on different connections and snapshots. Inside one transaction, visibility is still database-specific. The transaction owner must pass the callback's `txCtx` to `Page`; passing the outer context would execute outside that transaction.

For PostgreSQL and InnoDB deployments that require one snapshot, establish a read-only Repeatable Read transaction at the outermost boundary:

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

Non-default options cannot be introduced by a nested savepoint; `InTxWith` returns `sqldb.ErrNestedTxOptions` there. Database behavior is:

| Database | Relevant behavior |
| --- | --- |
| PostgreSQL | Default Read Committed takes a new snapshot per statement, so COUNT and SELECT can drift. Repeatable Read uses the snapshot established by the first non-control statement. |
| MySQL/InnoDB | Default Repeatable Read makes ordinary nonlocking consistent reads share the first-read snapshot. Server configuration can change the default; other engines, locking reads, and Read Committed differ, so request Repeatable Read explicitly. |
| SQLite | A plain explicit transaction keeps the snapshot established by its first read. WAL allows a concurrent writer while the reader retains it; rollback-journal mode may block the writer. Shared cache plus `PRAGMA read_uncommitted=ON` is the documented exception. |

The pinned modernc SQLite driver does not reliably enforce `sql.TxOptions.Isolation` or `ReadOnly`; use plain `InTx` for SQLite's explicit snapshot boundary. Fail-loud driver capability validation is deferred. Primary references: [PostgreSQL transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html), [InnoDB transaction isolation](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html), [InnoDB consistent reads](https://dev.mysql.com/doc/refman/8.4/en/innodb-consistent-read.html), [SQLite isolation](https://www.sqlite.org/isolation.html), and [SQLite transactions](https://www.sqlite.org/lang_transaction.html).

### Model→DTO responses

`Page[T]` answers with the queried type directly. When records must be mapped to a response DTO, run the query as `Page[Model]` and map it with `Page.Map`, which carries the metadata over. `Map` takes a pure `func(Model) DTO`; when the conversion itself can fail, map the model page's records with ordinary error handling and build the DTO page with `NewPage`:

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

## Usage Example

This end-to-end walkthrough wires `BindQuery`, the `pagination/` core, and the `Page[T]` terminal into the Controller → Service → Repository layout: the repository returns `Page[*Product]` and the service maps it to `Page[*ProductResponse]` with `Page.Map`. Domain types (`Product`, `ProductResponse`), `toProductResponse` and column names are illustrative; only the Credo imports are framework APIs. Registering the three types and routing `GET /products` to `ProductHandler.List` is ordinary composition (see the [dependency injection guide](../guides/dependency-injection.md)).

### Filter struct

Embed `PageRequest` and `SortRequest` so a single `BindQuery` call decodes pagination, sort, and filter parameters at once. `PageRequest.Validate()` applies the page / per_page defaults and the cap because it implements `validation.Validatable` — `BindQuery` invokes it after decode, so no manual `Normalize()` call is needed. A filter that defines its own `Validate` shadows the promoted `PageRequest.Validate`, so it calls `f.PageRequest.Validate()` — or `f.NormalizeWithMax(n)` for a per-endpoint cap — itself.

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
    query = query.OrderExpr(column + " " + order).OrderExpr("id " + order) // unique tie-breaker

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
        return err // default error envelope on bind/validation failure
    }

    page, err := h.service.ListByFilter(ctx.Context(), &filter)
    if errors.Is(err, pagination.ErrInvalidPageRequest) {
        return credo.ErrBadRequest.WithInternal(err) // e.g. a page whose offset overflows int
    }
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

## Cursor/Keyset Design Gate

The design direction is accepted, but no cursor types or terminals are exported yet. `Page` always carries an exact `Total` and `TotalPages`, and `HasNext` derives from that metadata: an unknown total is not represented as zero, `-1`, a pointer, or an omitted JSON field, and there is no `WithCount(false)` mode that changes the meaning of an existing `Page`. Total-free responses therefore get their own types. `CursorPage[T]` is reserved for keyset pagination; `Slice[T]` is only the working name for a future total-free **offset** window (which can fetch `PerPage+1` records to determine `HasNext`) and gets its own design gate before export. They do not weaken `Page[T]`'s exact total contract and are not aliases of one another.

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

The planned core names are `CursorRequest`, `CursorPage[T]`, and `CursorMeta`; the sqldb terminal is `SelectQuery.CursorPage[T]`. Records are always a non-nil slice. The final window uses JSON `null` for `next_cursor`. There is no `before`, previous cursor, arbitrary direction parameter, page number, `total_count`, `total_pages`, or `WithTotal` switch in the first delivery. Repositories that need a total run a separate explicit `Count` and put it in an application-owned envelope. Cursor execution itself never runs COUNT.

`CursorRequest.Validate` will mirror `PageRequest`'s input-policy role by defaulting/clamping `per_page`; the terminal will snapshot the request and strictly reject nil, non-positive, overflowed, or adapter-unrepresentable execution values without mutating it. Because the terminal adds one to `per_page`, strict execution checks native-int addition before building SQL.

### Terminal-owned keyset

The terminal owns ordering through an immutable adapter-level keyset spec. The first delivery accepts only a model-less default full-model SELECT plus curated `Where` or `ApplyQueryBuilder` predicates whose top-level separator is AND. `WherePK` is excluded because Bun resolves it when the model-less builder is composed, before the typed terminal supplies `T`, and records an unrecoverable builder error. A top-level `WhereOr` is also rejected: appending the cursor ladder could otherwise produce `A OR (B AND cursor)` instead of `(A OR B) AND cursor`. An OR filter must first be enclosed in one Bun `WhereGroup` through `ApplyQueryBuilder`; the resulting group itself joins the root with AND. The terminal validates this final WHERE tree before I/O. It also rejects custom `Column`/`ColumnExpr`/`ExcludeColumn`/`TableExpr`/`Apply`, joins, root ORDER/LIMIT/OFFSET/lock, and distinct, aggregate, grouped, compound, or otherwise non-row-shaped sources. Full-model projection ensures every cursor key was actually scanned into `T`. A join can multiply one model row, so a root-table unique id is not necessarily unique in the SQL result. Supporting joins or projections requires a later logical-row uniqueness/projection proof. This fail-loud boundary prevents the token's order and the executed SQL order from diverging.

The first delivery also rejects model types that implement Bun SELECT, append-model, or row/result scan hooks. A pre-query hook can replace the model or alter query shape/order/window; `AfterScanRow` or `AfterSelect` can mutate a cursor key after SQL ordering but before token generation. Bun v1.3.0 exposes no public seam that lets Credo apply and verify terminal-owned state after all of those hooks, so accepting hook-capable models would make fail-loud behavior unprovable.

The terminal otherwise inherits the existing typed-terminal invariants: the query starts model-less, execution uses a private snapshot, the reusable receiver is not mutated, explicit connections win over ambient transaction injection, ambient transactions still propagate, and database failures retain the current store-error mapping.

Every keyset component must be:

- a quoted database column identifier, not request-provided SQL;
- selected into the model and encoded with its original type;
- immutable and `NOT NULL` for the lifetime of a cursor walk; and
- backed by a matching composite index where performance matters.

The final component is a separately declared unique tie-breaker. This makes the order total even when an earlier value repeats. ASC and DESC components may be mixed, but each direction is fixed by the keyset spec rather than the request.

For keys `(a ASC, b DESC, id ASC)`, an `after` boundary expands to the portable lexicographic ladder:

```sql
a > :a
OR (a = :a AND b < :b)
OR (a = :a AND b = :b AND id > :id)
```

The complete ladder is appended to the already-validated filter root as one parenthesized AND condition. Column identifiers are quoted, and decoded values go through Bun's typed SQL formatter; token bytes are never concatenated into SQL. Bun v1.3.0 renders the final SQL rather than passing driver bind parameters, so the contract is formatter safety, not a claim about driver placeholders.

The terminal fetches `per_page + 1`, trims the extra row, derives `has_next`, and encodes the last included record as `next_cursor`. It never uses OFFSET. The expanded predicate is preferred over a row constructor because it handles mixed directions uniformly and follows MySQL's documented optimization advice for fuller composite-index use. See [MySQL row-constructor optimization](https://dev.mysql.com/doc/refman/8.0/en/row-constructor-optimization.html). NULL keys are excluded: row comparisons become unknown around NULL ([PostgreSQL row comparison](https://www.postgresql.org/docs/current/functions-comparisons.html)), and default NULL placement differs across supported databases — PostgreSQL places NULL last for ascending order by default, while MySQL and SQLite place it first. See [PostgreSQL ordering](https://www.postgresql.org/docs/current/queries-order.html), [MySQL NULL ordering](https://dev.mysql.com/doc/refman/8.0/en/null-values.html), and [SQLite type ordering](https://www.sqlite.org/datatype3.html).

### Cursor integrity and privacy

There is no implicit unsigned or process-local-secret default. The planned public-HTTP codec requires an explicit HMAC-SHA256 keyring. Each key is at least 32 cryptographically random bytes generated by a CSPRNG; the constructor can enforce length, while entropy remains a deployment/configuration contract. Exactly one key signs, older keys are verify-only, key-id lookup is direct, and MAC comparison is constant-time. The first delivery caps the ring at eight keys, the key tuple at eight components, the decoded payload at 1 KiB, and the complete token at 2 KiB before parsing; decoding then requires the exact version, arity and types, accepts no unknown fields or trailing data, and its errors never echo token contents or key values. Rotation retention is fixed only together with the consumer's expiry policy; without an expiry horizon, a verification key cannot be retired safely. Its signed input binds the following; the construction follows [RFC 2104](https://www.rfc-editor.org/rfc/rfc2104.html):

- token-format version;
- endpoint/query identity and canonical keyset order;
- normalized filter scope; and
- tenant/authorization scope.

A token from one scope therefore fails closed in another.

The wire format is versioned, key-id-addressed, base64url-safe, and integrity-protected, but its exact byte framing is deliberately not frozen yet. Before implementation, the consumer must drive canonical tuple encoding, key-id grammar, padding rules, external-scope canonicalization, MAC framing, and cross-implementation golden vectors. The MAC uses a fixed Credo cursor domain separator, which prevents cross-protocol MAC reuse, and covers the version, key id, payload, and external scope binding. Key material is copied into the codec, never serialized, logged, or included in an error. Tuple members retain explicit types and fixed arity; decoding does not coerce JSON numbers into a generic floating-point value.

Malformed, tampered, unknown-key, scope-mismatched, and—when an expiry policy is configured—expired client tokens share one public invalid-cursor error and map to HTTP 400 without revealing the reason. Configuration or cursor-encoding failures are internal errors; database failures keep the existing `store.Error` mapping. Shipping this distinction also requires the framework's transport-neutral taxonomy to gain a general invalid-argument kind rather than teaching the HTTP layer about pagination.

A cursor is not an authorization capability. Every request re-runs normal authentication, authorization, tenant predicates, and normalized filters; signed scope binding is defense in depth against cross-query replay, not a replacement for those checks. “Opaque” means clients must not interpret or construct the token. Its signed payload is still visible: signing provides integrity, not confidentiality, so the first delivery permits only non-sensitive keys. Encryption (AEAD), server-side cursors, expiry policy, and a deliberately named trusted-only unsigned codec remain separate decisions driven by a real consumer.

### Mutation contract

Cursor pages are usually separate requests and do not share a transaction snapshot. With immutable ordering keys:

- inserting or deleting a row strictly before the boundary does not shift the next window;
- inserting a row after the boundary may make it appear later;
- deleting an unseen row removes it from later windows; and
- changing an ordering key may duplicate or skip that row and is unsupported.

The conformance suite must demonstrate these cases and the corresponding offset duplicate/skip behavior, while proving no duplicate keyset row appears.

### Gate-opening conditions

Implementation remains deferred until all of these are true:

1. a concrete consumer contributes its real model, filters, order, tenant binding, and key-rotation requirements;
2. sqldb rejects hook-capable models before I/O, as [Terminal-owned keyset](#terminal-owned-keyset) requires;
3. real PostgreSQL, MySQL, and SQLite jobs pass generated-SQL, NULL/collation, index-plan, insert/delete, and typed-value round-trip tests—including signed integers above 2^53, timestamp precision/timezone, deterministic string collation, and consumer-specific UUID/decimal types; floats/NaN have no default support;
4. the transport-neutral fault taxonomy (`fault.Kind`) gains an invalid-argument kind that the error pipeline classifies as HTTP 400, so the rendered response and the access-log status agree; and
5. canonical wire framing and cross-implementation golden vectors are fixed.

Until then, a repository may own its keyset SQL and cursor codec explicitly, but Credo will not freeze a speculative public generic abstraction.

## Design Decisions

The rationale for the adapter, count, snapshot and cursor decisions is in [ADR-015](../adr/015-data-access.md) and in the sections above. The core decisions:

- **Core has zero ORM dependencies**, consistent with the store adapter pattern.
- **`Offset()` returns `(int, error)`**: a plain `int` result could silently wrap; returning the error makes arithmetic failure explicit and keeps invalid LIMIT/OFFSET state from reaching adapters.
- **`PageRequest` uses non-pointer `int`**: `Normalize()` handles absence/zero as the default-input policy. Strict execution validation runs only after policy has been applied; callers constructing requests manually must supply positive values.
- **`NewPage` uses quotient + remainder ceiling division**: `(total + perPage - 1) / perPage` can overflow near `math.MaxInt64`; quotient plus a non-zero-remainder increment produces the same ceiling safely.
- **`ValidateSort` is a method on `SortRequest`**: SQL injection prevention is ORM-agnostic logic, and a method is more idiomatic than a free function.
- **`Page.Map[U]` is a generic method** (Go 1.27): `Map` enforces compute-once metadata by construction. A generic method (not a free function) keeps the call fluent on the page the caller holds and stays in the ORM-agnostic core.
- **`SortConfig` is a whitelist**: only pre-approved fields can reach ORDER BY, which is safer than a blacklist or a regex.

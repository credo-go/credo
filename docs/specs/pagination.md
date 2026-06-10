# Pagination Spec

**Status**: Planned
**Package**: `pagination/` (core), adapter helper in `store/sqldb/`
**Depends on**: Root package (`BindQuery` tag support), `store/sqldb/` (adapter layer)

---

## Canonical Source

Implementation-level details for Credo's pagination abstraction are defined
in this file. Other documents should keep only high-level references and link
here.

---

## Overview

The `pagination/` package provides generic, ORM-agnostic types and utilities
for paginated API responses. Actual query execution (COUNT + LIMIT/OFFSET)
lives in the ORM-specific adapter (`sqldb.Paginate` in `store/sqldb/`), keeping
the core free of external dependencies.

Key design properties:

- **ORM-agnostic core** — `Page[T]`, `Meta`, `PageRequest` have zero ORM
  dependencies. Only types, normalization, and sort validation.
- **Adapter-level execution** — `sqldb.Paginate` / `sqldb.PaginateRequest`
  in `store/sqldb/` handle the actual COUNT + LIMIT/OFFSET queries via Bun.
- **No intermediate wrapper** — Adapter `Paginate` fills a `dest` slice and
  returns `total int64`. `Page[T]` is constructed only once in the service
  layer with the final DTO type. No model→DTO conversion step at the
  pagination level. (`sqldb.PaginateRequest` is the deliberate exception
  for flows that respond with the queried type directly — it returns a
  ready `*Page[T]` built from a `PageRequest`.)
- **Request binding** — `PageRequest` and `SortRequest` are embeddable structs
  that work with `BindQuery` via `query:"..."` tags.
- **SQL injection prevention** — `SortRequest.ValidateSort` whitelist-based
  sort field validation. Only pre-approved DB columns can appear in ORDER BY.

---

## Goals

1. **ORM-agnostic types**: `Page[T]`, `Meta`, `PageRequest` import only stdlib.
   No GORM, Bun, or other ORM types leak into the core.
2. **Single construction**: `Page[T]` is built once with the final response
   type (DTO), never with intermediate model types. Adapters return raw
   slices + total count, not wrapped pages.
3. **BindQuery integration**: `PageRequest` and `SortRequest` use `query:"..."`
   tags for automatic request binding via `ctx.Request().BindQuery(&filter)`.
4. **Safe defaults**: `Normalize()` converts zero/negative values to defaults
   and caps `PerPage` at `MaxPerPage` (50). `NormalizeWithMax(n)` applies a
   custom cap per endpoint (shadow `Validate` on the embedding struct to use
   it with BindQuery). Invalid input never causes panics or unbounded queries.
5. **Sort safety**: `SortRequest.ValidateSort` rejects unknown sort fields,
   preventing SQL injection via ORDER BY. Falls back to configured defaults
   silently.

---

## Architecture

### Two Layers

```text
┌───────────────────────────────────────────────────────┐
│  Application Code                                      │
│                                                        │
│  Controller: ctx.Request().BindQuery(&filter)          │
│  Service:    pagination.NewPage(dtos, total, p, pp)    │
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
│  sqldb.Paginate[T](ctx, q, page, perPage, &dest)     │
│  sqldb.PaginateRequest[T](ctx, q, req) → *Page[T]    │
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
PageRequest{2, 20}    ───►  sqldb.Paginate()        │                       │
SortRequest{"name","asc"}   → []Model + total  ───► model→DTO loop         │
                            │                       NewPage(dtos,total,2,20)│
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

// Page is a generic paginated result.
// Constructed once with the final response type (DTO), not with model types.
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
// NewPage creates a Page from raw values.
func NewPage[T any](records []T, total int64, page, perPage int) *Page[T]

// NewEmpty creates an empty Page with default pagination values.
func NewEmpty[T any]() *Page[T]

// Normalize validates and normalizes pagination values.
// Zero/negative → defaults, PerPage capped at max.
func (r *PageRequest) Normalize()

// Validate implements validation.Validatable so that BindQuery
// automatically normalizes pagination values after decoding.
func (r *PageRequest) Validate() error

// Offset returns the zero-based offset for SQL LIMIT/OFFSET queries.
func (r *PageRequest) Offset() int

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
```

### Constants

```go
DefaultPage    = 1
DefaultPerPage = 50
MinPerPage     = 1
MaxPerPage     = 50
```

---

## Adapter: `sqldb.Paginate` / `sqldb.PaginateRequest`

Lives in `store/sqldb/` — the Bun wrapper package.

```go
// Paginate executes COUNT + SELECT with LIMIT/OFFSET on the given query.
// The query should already have model, filters, and ordering applied.
// Paginate clones the query internally — the original is not mutated.
func Paginate[T any](ctx context.Context, q *SelectQuery, page, perPage int, dest *[]T) (int64, error)

// PaginateRequest is the all-in-one variant: it runs Paginate with the
// values from req and assembles a ready *pagination.Page[T]. req is
// assumed normalized (BindQuery does this via Validate) and is never
// modified. Use it when the response carries the queried type directly;
// for model→DTO flows prefer Paginate + NewPage so the Page is built
// once with the DTO type.
func PaginateRequest[T any](ctx context.Context, q *SelectQuery, req *pagination.PageRequest) (*pagination.Page[T], error)
```

### Implementation

```go
// Paginate uses Clone, Count, Offset, Limit, and Scan — all part of
// the query builder proxy's curated method set (see store spec).
func Paginate[T any](ctx context.Context, q *SelectQuery, page, perPage int, dest *[]T) (int64, error) {
    // COUNT query (on cloned query — no mutation)
    total, err := q.Clone().Count(ctx)
    if err != nil {
        return 0, err
    }

    // No results — skip the SELECT query
    if total == 0 {
        *dest = []T{}
        return 0, nil
    }

    // SELECT with LIMIT/OFFSET (on cloned query).
    // Note: callers can also use PageRequest.Offset() for this calculation.
    offset := (page - 1) * perPage
    err = q.Clone().Offset(offset).Limit(perPage).Scan(ctx, dest)
    if err != nil {
        return 0, err
    }

    return int64(total), nil
}
```

---

## Usage Example

This end-to-end walkthrough wires `BindQuery`, the `pagination/` core, and
the `sqldb.Paginate` adapter into the canonical Controller → Service →
Repository layout. Domain types (`Product`, `ProductResponse`) and column
names are illustrative; only the Credo imports are framework APIs.

### Filter struct

Embed `PageRequest` and `SortRequest` so a single `BindQuery` call decodes
pagination, sort, and filter parameters at once. `PageRequest.Validate()`
normalizes page / per_page automatically because it implements
`validation.Validatable` — `BindQuery` invokes it after decode, so no
manual `Normalize()` call is needed.

```go
type ProductFilter struct {
    pagination.PageRequest         // page, per_page
    pagination.SortRequest         // sort_by, sort_order
    SearchTerm string `query:"search_term"`
}
```

### Sort whitelist

`SortConfig` maps API field names to DB column names. `ValidateSort` returns
the configured default when the request asks for a field that isn't in the
whitelist — this is the SQL-injection guard for `ORDER BY`.

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

func (r *productRepo) ListByFilter(ctx context.Context, filter *ProductFilter) ([]*Product, int64, error) {
    var products []*Product
    query := r.db.NewSelect().Model(&products)

    if filter.SearchTerm != "" {
        query = query.Where("name ILIKE ?", "%"+filter.SearchTerm+"%")
    }

    column, order := filter.SortRequest.ValidateSort(productSortConfig)
    query = query.OrderExpr(column + " " + order)

    total, err := sqldb.Paginate(ctx, query, filter.Page, filter.PerPage, &products)
    if err != nil {
        return nil, 0, fmt.Errorf("product pagination: %w", err)
    }
    return products, total, nil
}
```

### Service

```go
type productService struct {
    repo *productRepo
}

func (s *productService) ListByFilter(ctx context.Context, filter *ProductFilter) (*pagination.Page[*ProductResponse], error) {
    products, total, err := s.repo.ListByFilter(ctx, filter)
    if err != nil {
        return nil, err
    }

    dtos := make([]*ProductResponse, len(products))
    for i, p := range products {
        dtos[i] = toProductResponse(p)
    }
    return pagination.NewPage(dtos, total, filter.Page, filter.PerPage), nil
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

Register the repo, service, and handler in the DI container; the
constructor parameters drive resolution.

```go
credo.Provide[*productRepo](app, func(infra credo.Infra, db *sqldb.DB) *productRepo {
    return &productRepo{db: db}
})
credo.Provide[*productService](app, func(infra credo.Infra, repo *productRepo) *productService {
    return &productService{repo: repo}
})
credo.Provide[*ProductHandler](app, func(infra credo.Infra, svc *productService) *ProductHandler {
    return &ProductHandler{service: svc}
})

handler := credo.Resolve[*ProductHandler](app)
app.GET("/products", handler.List)
```

`Product`, `ProductResponse`, and `toProductResponse` are deliberately
left to the application — pagination is orthogonal to domain modelling.

---

## Future: Cursor-Based Pagination

Planned but not yet designed. Will use a separate type (`CursorPage[T]`) since
the response shape differs (no `total_count`, has `next_cursor`). The core
`pagination/` package will be extended when needed.

---

## Design Decisions

| Decision | Rationale |
|----------|-----------|
| Core has zero ORM deps | Consistent with store adapter pattern |
| Adapter returns `(total, error)` not `Page[T]` | `Page[T]` would be an intermediate wrapper — always discarded after model→DTO conversion. Single construction with final type is cleaner |
| `PageRequest` uses non-pointer `int` | `Normalize()` handles zero→default conversion. Pointer fields (`*int`) are supported by `BindQuery` but unnecessary here since zero is always normalized |
| `ValidateSort` as method on `SortRequest` | SQL injection prevention is ORM-agnostic logic; method is more idiomatic than free function |
| No `Convert[T,U]` function | Unnecessary — `Page[T]` is constructed once with final DTO type. Model→DTO conversion is the service's responsibility, not pagination's |
| `SortConfig` whitelist approach | Only pre-approved fields can reach ORDER BY. Safer than blacklist or regex |

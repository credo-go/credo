# Pagination Spec

**Status**: Planned **Package**: `pagination/` (core), adapter helper in `store/sqldb/` **Depends on**: Root package (`BindQuery` tag support), `store/sqldb/` (adapter layer)

---

## Canonical Source

Implementation-level details for Credo's pagination abstraction are defined in this file. Other documents should keep only high-level references and link here.

---

## Overview

The `pagination/` package provides generic, ORM-agnostic types and utilities for paginated API responses. Actual query execution (COUNT + LIMIT/OFFSET) lives in the ORM-specific adapter (the `SelectQuery.Page[T]` terminal in `store/sqldb/`), keeping the core free of external dependencies.

Key design properties:

- **ORM-agnostic core** — `Page[T]`, `Meta`, `PageRequest` have zero ORM dependencies. Only types, normalization, and sort validation.
- **Adapter-level execution** — the `SelectQuery.Page[T]` terminal in `store/sqldb/` runs the COUNT + LIMIT/OFFSET queries via Bun and returns a ready `*Page[T]`.
- **Single construction** — `Page[T]` is built once with the final response type. When the response carries the queried type directly, `Page[T]` builds it; for a model→DTO response the service builds it once with the DTO type from the `Count` + `All[Model]` terminals + `NewPage`, so there is no intermediate `Page[Model]` to discard.
- **Request binding** — `PageRequest` and `SortRequest` are embeddable structs that work with `BindQuery` via `query:"..."` tags.
- **SQL injection prevention** — `SortRequest.ValidateSort` whitelist-based sort field validation. Only pre-approved DB columns can appear in ORDER BY.

---

## Goals

1. **ORM-agnostic types**: `Page[T]`, `Meta`, `PageRequest` import only stdlib. No GORM, Bun, or other ORM types leak into the core.
2. **Single construction**: `Page[T]` is built once with the final response type (DTO), never with intermediate model types. Adapters return raw slices + total count, not wrapped pages.
3. **BindQuery integration**: `PageRequest` and `SortRequest` use `query:"..."` tags for automatic request binding via `ctx.Request().BindQuery(&filter)`.
4. **Safe defaults**: `Normalize()` converts zero/negative values to defaults and caps `PerPage` at `MaxPerPage` (50). `NormalizeWithMax(n)` applies a custom cap per endpoint (shadow `Validate` on the embedding struct to use it with BindQuery). Invalid input never causes panics or unbounded queries.
5. **Sort safety**: `SortRequest.ValidateSort` rejects unknown sort fields, preventing SQL injection via ORDER BY. Falls back to configured defaults silently.

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
│  (*SelectQuery).Page[T](ctx, req) → *Page[T]          │
│  (queried-type flows; model→DTO uses Count + All[T])  │
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
PageRequest{2, 20}    ───►  Count + All[Model]()    │                       │
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

## Adapter: `SelectQuery.Page[T]`

Lives in `store/sqldb/` — the Bun wrapper package. `Page[T]` is a typed terminal alongside `One[T]` / `All[T]` (see the [store spec](store.md)), so `T` drives both the table and the result and the query is built model-less.

```go
// Page runs COUNT + a LIMIT/OFFSET SELECT and assembles a *pagination.Page[T].
// req is assumed already normalized (BindQuery does this via Validate); Page
// does not re-normalize and rejects only a nil req. On zero rows the SELECT is
// skipped and the page keeps the requested page/per-page. Both statements clone
// the query and join the ambient transaction, so the receiver is not mutated.
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error)
```

### Implementation

```go
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error) {
    if req == nil {
        return nil, fmt.Errorf("sqldb: page request must not be nil")
    }

    // COUNT with T's table — model-less query, the COUNT clone owns the model.
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
```

### Model→DTO responses

`Page[T]` answers with the queried type directly. When records must be mapped to a response DTO, `T` cannot be both the table model and the DTO — build the page from the `Count` + `All[Model]` terminals so it is constructed once with the final type (`Count` takes `Model((*Model)(nil))` because the query is model-less, exactly as `Page` does internally):

```go
total, err := q.Clone().Model((*Model)(nil)).Count(ctx)
if err != nil || total == 0 {
    return pagination.NewPage([]DTO{}, int64(total), req.Page, req.PerPage), err
}
rows, err := q.Clone().Offset(req.Offset()).Limit(req.PerPage).All[Model](ctx)
if err != nil {
    return nil, err
}
dtos := make([]DTO, len(rows))
for i, m := range rows {
    dtos[i] = toDTO(m)
}
page := pagination.NewPage(dtos, int64(total), req.Page, req.PerPage)
```

---

## Usage Example

This end-to-end walkthrough wires `BindQuery`, the `pagination/` core, and the `sqldb` typed terminals into the canonical Controller → Service → Repository layout. Because the response is a DTO (`ProductResponse`, not the `Product` table model), it uses the model→DTO recipe (`Count` + `All[Model]` + `NewPage`) rather than the all-in-one `Page[T]` terminal; reach for `Page[T]` directly when the response carries the queried type. Domain types (`Product`, `ProductResponse`) and column names are illustrative; only the Credo imports are framework APIs.

### Filter struct

Embed `PageRequest` and `SortRequest` so a single `BindQuery` call decodes pagination, sort, and filter parameters at once. `PageRequest.Validate()` normalizes page / per_page automatically because it implements `validation.Validatable` — `BindQuery` invokes it after decode, so no manual `Normalize()` call is needed.

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

func (r *productRepo) ListByFilter(ctx context.Context, filter *ProductFilter) ([]*Product, int64, error) {
    query := r.db.Select() // model-less: the terminals own the model via T

    if filter.SearchTerm != "" {
        query = query.Where("name ILIKE ?", "%"+filter.SearchTerm+"%")
    }

    column, order := filter.SortRequest.ValidateSort(productSortConfig)
    query = query.OrderExpr(column + " " + order)

    total, err := query.Clone().Model((*Product)(nil)).Count(ctx)
    if err != nil {
        return nil, 0, fmt.Errorf("count products: %w", err)
    }
    if total == 0 {
        return nil, 0, nil
    }

    products, err := query.Offset(filter.Offset()).Limit(filter.PerPage).All[*Product](ctx)
    if err != nil {
        return nil, 0, fmt.Errorf("list products: %w", err)
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

## Future: Cursor-Based Pagination

Planned but not yet designed. Will use a separate type (`CursorPage[T]`) since the response shape differs (no `total_count`, has `next_cursor`). The core `pagination/` package will be extended when needed.

---

## Design Decisions

| Decision | Rationale |
| --- | --- |
| Core has zero ORM deps | Consistent with store adapter pattern |
| `Page[T]` terminal returns `*Page[T]`; model→DTO builds from `Count` + `All[Model]` | The all-in-one terminal is ergonomic when the response is the queried type. When a DTO mapping is needed, building from the lower-level terminals keeps `Page[T]` constructed once with the final type — no intermediate `Page[Model]` to allocate and discard |
| `PageRequest` uses non-pointer `int` | `Normalize()` handles zero→default conversion. Pointer fields (`*int`) are supported by `BindQuery` but unnecessary here since zero is always normalized |
| `ValidateSort` as method on `SortRequest` | SQL injection prevention is ORM-agnostic logic; method is more idiomatic than free function |
| No `Convert[T,U]` function | Unnecessary — `Page[T]` is constructed once with final DTO type. Model→DTO conversion is the service's responsibility, not pagination's |
| `SortConfig` whitelist approach | Only pre-approved fields can reach ORDER BY. Safer than blacklist or regex |

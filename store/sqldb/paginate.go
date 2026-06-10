package sqldb

import (
	"context"
	"fmt"

	"github.com/credo-go/credo/pagination"
)

// Paginate executes COUNT + SELECT with LIMIT/OFFSET on the given query.
// The query should already have model, filters, and ordering applied.
// Paginate clones the query internally — the original is not mutated.
//
// Both queries join the ambient transaction from ctx (see [RunInTx]) like
// any other query. Because COUNT and SELECT are two separate statements,
// the total and the page can drift apart under concurrent writes — call
// Paginate inside [RunInTx] when a consistent snapshot matters.
//
// Returns the total count and any error. If total is zero, dest is set to
// an empty slice and the SELECT query is skipped.
func Paginate[T any](ctx context.Context, q *SelectQuery, page, perPage int, dest *[]T) (int64, error) {
	if q == nil {
		return 0, fmt.Errorf("sqldb: paginate query must not be nil")
	}
	if dest == nil {
		return 0, fmt.Errorf("sqldb: paginate destination must not be nil")
	}
	if page < 1 {
		return 0, fmt.Errorf("sqldb: paginate page must be >= 1, got %d", page)
	}
	if perPage < 1 {
		return 0, fmt.Errorf("sqldb: paginate perPage must be >= 1, got %d", perPage)
	}

	// COUNT query (on cloned query — no mutation).
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return 0, err
	}

	// No results — skip the SELECT query.
	if total == 0 {
		*dest = []T{}
		return 0, nil
	}

	// SELECT with LIMIT/OFFSET (on cloned query).
	offset := (page - 1) * perPage
	err = q.Clone().Offset(offset).Limit(perPage).Scan(ctx, dest)
	if err != nil {
		return 0, err
	}

	return int64(total), nil
}

// PaginateRequest executes COUNT + SELECT for the given page request and
// assembles the result as a [pagination.Page]. It is the all-in-one
// variant of [Paginate] for handlers and repositories that respond with
// the queried type directly:
//
//	var filter ListFilter                     // embeds pagination.PageRequest
//	if err := ctx.Request().BindQuery(&filter); err != nil { ... }
//	page, err := sqldb.PaginateRequest[User](ctx, query, &filter.PageRequest)
//
// req is read, never modified, and is assumed to be normalized —
// BindQuery does this automatically via Validate; otherwise call
// [pagination.PageRequest.Normalize] (or NormalizeWithMax) first.
// Query semantics are those of [Paginate]: the query is cloned, both
// statements join the ambient transaction from ctx, and the SELECT is
// skipped when the total is zero.
//
// When records need a model→DTO mapping, prefer [Paginate] +
// [pagination.NewPage] so the Page is built once with the final DTO type.
func PaginateRequest[T any](ctx context.Context, q *SelectQuery, req *pagination.PageRequest) (*pagination.Page[T], error) {
	if req == nil {
		return nil, fmt.Errorf("sqldb: paginate request must not be nil")
	}

	var records []T
	total, err := Paginate(ctx, q, req.Page, req.PerPage, &records)
	if err != nil {
		return nil, err
	}
	return pagination.NewPage(records, total, req.Page, req.PerPage), nil
}

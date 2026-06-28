// Package pagination provides generic, ORM-agnostic types and utilities
// for paginated API responses.
//
// The package defines request binding structs, a generic result type, and
// sort validation — all with zero external dependencies. Actual query
// execution (COUNT + LIMIT/OFFSET) lives in ORM-specific adapters such as
// the [github.com/credo-go/credo/store/sqldb.SelectQuery.Page] terminal.
//
// # Request Binding
//
// [PageRequest] and [SortRequest] are embeddable structs that work with
// BindQuery via query tags:
//
//	type BranchFilter struct {
//	    pagination.PageRequest
//	    pagination.SortRequest
//	    Search string `query:"search"`
//	}
//
// # Page Construction and Mapping
//
// A [Page] carries pagination metadata (Total, Page, PerPage, TotalPages) that
// is computed once — by [NewPage] or by a query terminal such as sqldb's
// SelectQuery.Page — and never recomputed or hand-copied afterward:
//
//	page := pagination.NewPage(dtos, total, filter.Page, filter.PerPage)
//
// To turn a page of database models into a page of response DTOs, use
// [Page.Map]; it transforms the records and carries the metadata over:
//
//	dtoPage := modelPage.Map(func(m Model) DTO { return toDTO(m) })
//
// # JSON Response
//
// [Page.ToDataMeta] splits the page into a data slice and a [Meta] struct
// suitable for JSON serialization:
//
//	data, meta := page.ToDataMeta()
//
// # Sort Safety
//
// [SortRequest.ValidateSort] rejects unknown sort fields via a whitelist,
// preventing SQL injection through ORDER BY clauses:
//
//	col, ord := filter.SortRequest.ValidateSort(sortCfg)
package pagination

// Package pagination provides generic, ORM-agnostic types and utilities
// for paginated API responses.
//
// The package defines request binding structs, a generic result type, and
// sort validation — all with zero external dependencies. Actual query
// execution (COUNT + LIMIT/OFFSET) lives in ORM-specific adapters such as
// [github.com/credo-go/credo/store/sqldb.Paginate].
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
// # Page Construction
//
// [Page] is constructed once with the final response type (DTO), not with
// intermediate model types:
//
//	page := pagination.NewPage(dtos, total, filter.Page, filter.PerPage)
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

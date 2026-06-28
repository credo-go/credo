package pagination

import "strings"

// Default pagination constants.
const (
	DefaultPage    = 1
	DefaultPerPage = 50
	MinPerPage     = 1
	MaxPerPage     = 50
)

// PageRequest is an embeddable struct for pagination query parameters.
// It works with BindQuery via query tags.
type PageRequest struct {
	Page    int `query:"page"`
	PerPage int `query:"per_page"`
}

// SortRequest is an embeddable struct for sort query parameters.
type SortRequest struct {
	SortBy    string `query:"sort_by"`
	SortOrder string `query:"sort_order"`
}

// Page is a generic paginated result. Its pagination metadata (Total, Page,
// PerPage, TotalPages) is computed once — by [NewPage] or by a query terminal
// such as sqldb's SelectQuery.Page — and carried unchanged through any later
// reshaping: [Page.Map] turns a Page of database models into a Page of response
// DTOs while preserving that metadata, so it is never recomputed or hand-copied.
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
// AllowedFields maps API field names to DB column names.
type SortConfig struct {
	DefaultField  string
	DefaultOrder  string            // "ASC" or "DESC"
	AllowedFields map[string]string // API field name → DB column name
}

// Normalize validates and normalizes pagination values in place.
// Zero or negative values are replaced with defaults. PerPage is clamped
// to [MinPerPage, MaxPerPage]. For a custom upper bound, use
// [PageRequest.NormalizeWithMax].
func (r *PageRequest) Normalize() {
	r.NormalizeWithMax(MaxPerPage)
}

// NormalizeWithMax is like [PageRequest.Normalize] but clamps PerPage to
// the given maximum instead of the package default [MaxPerPage].
// maxPerPage values <= 0 fall back to [MaxPerPage].
//
// PageRequest's own Validate — and therefore BindQuery's auto-validation —
// always applies the package default. To raise the cap for a specific
// endpoint, shadow Validate on the embedding struct:
//
//	type ListQuery struct {
//		pagination.PageRequest
//	}
//
//	func (q *ListQuery) Validate() error {
//		q.NormalizeWithMax(200)
//		return nil
//	}
func (r *PageRequest) NormalizeWithMax(maxPerPage int) {
	if maxPerPage <= 0 {
		maxPerPage = MaxPerPage
	}
	if r.Page <= 0 {
		r.Page = DefaultPage
	}
	if r.PerPage <= 0 {
		r.PerPage = DefaultPerPage
	}
	if r.PerPage > maxPerPage {
		r.PerPage = maxPerPage
	}
	// Defensive: unreachable with current constants (MinPerPage=1,
	// DefaultPerPage=50) but guards against future constant changes.
	if r.PerPage < MinPerPage {
		r.PerPage = MinPerPage
	}
}

// Validate implements [validation.Validatable] so that BindQuery
// automatically normalizes pagination values after decoding.
func (r *PageRequest) Validate() error {
	r.Normalize()
	return nil
}

// Offset returns the zero-based offset for SQL LIMIT/OFFSET queries.
// It assumes Normalize has been called (or Validate via BindQuery).
func (r *PageRequest) Offset() int {
	return (r.Page - 1) * r.PerPage
}

// NewPage creates a [Page] from raw values. TotalPages is computed
// automatically. A nil records slice is replaced with an empty slice
// for safe JSON serialization ([] instead of null).
func NewPage[T any](records []T, total int64, page, perPage int) *Page[T] {
	if records == nil {
		records = []T{}
	}
	var totalPages int64
	if perPage > 0 {
		totalPages = (total + int64(perPage) - 1) / int64(perPage)
	}
	return &Page[T]{
		Records:    records,
		Total:      total,
		Page:       page,
		PerPage:    perPage,
		TotalPages: totalPages,
	}
}

// NewEmpty creates an empty [Page] with default pagination values.
func NewEmpty[T any]() *Page[T] {
	return &Page[T]{
		Records:    []T{},
		Total:      0,
		Page:       DefaultPage,
		PerPage:    DefaultPerPage,
		TotalPages: 0,
	}
}

// ValidateSort validates sort parameters against allowed fields.
// It returns the DB column name and normalized sort order (uppercase).
// Unknown sort fields fall back to cfg.DefaultField. Invalid sort orders
// fall back to cfg.DefaultOrder. If cfg is nil, both return values are
// empty strings. A nil receiver is safe to call.
func (r *SortRequest) ValidateSort(cfg *SortConfig) (column, order string) {
	if cfg == nil {
		return "", ""
	}

	// Resolve column from whitelist.
	if r != nil && r.SortBy != "" {
		if col, ok := cfg.AllowedFields[r.SortBy]; ok {
			column = col
		}
	}
	if column == "" {
		column = cfg.DefaultField
	}

	// Resolve and normalize order.
	order = normalizeOrder(reqOrder(r))
	if order == "" {
		order = normalizeOrder(cfg.DefaultOrder)
	}

	return column, order
}

// HasNext reports whether there is a page after the current one.
func (p *Page[T]) HasNext() bool { return int64(p.Page) < p.TotalPages }

// HasPrev reports whether there is a page before the current one.
func (p *Page[T]) HasPrev() bool { return p.Page > 1 }

// ToDataMeta splits the page into a records slice and a [Meta] struct
// for JSON response serialization.
func (p *Page[T]) ToDataMeta() ([]T, *Meta) {
	return p.Records, &Meta{
		Total:      p.Total,
		Page:       p.Page,
		PerPage:    p.PerPage,
		TotalPages: p.TotalPages,
		HasNext:    p.HasNext(),
		HasPrev:    p.HasPrev(),
	}
}

// Map returns a new Page[U] whose records are produced by applying fn to each
// record of p, carrying the pagination metadata (Total, Page, PerPage,
// TotalPages) over unchanged. It is the canonical way to turn a page of
// database models into a page of response DTOs:
//
//	modelPage, err := db.Select().Where(...).Page[Model](ctx, req)
//	if err != nil {
//		return err
//	}
//	dtoPage := modelPage.Map(func(m Model) DTO { return toDTO(m) })
//
// fn is applied once per record, in order, and must be pure: for a conversion
// that can fail, map in the service layer and build the page with [NewPage].
// fn must not be nil — a nil mapping is always a programming error, so Map
// panics on a nil fn even when the page is empty, rather than silently
// returning an empty Page[U].
func (p *Page[T]) Map[U any](fn func(T) U) *Page[U] {
	if fn == nil {
		panic("pagination: Page.Map called with nil fn")
	}
	records := make([]U, len(p.Records))
	for i, r := range p.Records {
		records[i] = fn(r)
	}
	return &Page[U]{
		Records:    records,
		Total:      p.Total,
		Page:       p.Page,
		PerPage:    p.PerPage,
		TotalPages: p.TotalPages,
	}
}

func reqOrder(req *SortRequest) string {
	if req == nil {
		return ""
	}
	return req.SortOrder
}

func normalizeOrder(s string) string {
	switch strings.ToUpper(s) {
	case "ASC":
		return "ASC"
	case "DESC":
		return "DESC"
	default:
		return ""
	}
}

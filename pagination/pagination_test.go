package pagination_test

import (
	"fmt"
	"testing"

	"github.com/credo-go/credo/pagination"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name        string
		in          pagination.PageRequest
		wantPage    int
		wantPerPage int
	}{
		{"zero values", pagination.PageRequest{}, pagination.DefaultPage, pagination.DefaultPerPage},
		{"negative page", pagination.PageRequest{Page: -5, PerPage: 10}, pagination.DefaultPage, 10},
		{"negative per_page", pagination.PageRequest{Page: 2, PerPage: -1}, 2, pagination.DefaultPerPage},
		{"exceeds max", pagination.PageRequest{Page: 1, PerPage: 999}, 1, pagination.MaxPerPage},
		{"valid values", pagination.PageRequest{Page: 3, PerPage: 25}, 3, 25},
		{"per_page exactly min", pagination.PageRequest{Page: 1, PerPage: pagination.MinPerPage}, 1, pagination.MinPerPage},
		{"per_page exactly max", pagination.PageRequest{Page: 1, PerPage: pagination.MaxPerPage}, 1, pagination.MaxPerPage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.in
			r.Normalize()
			if r.Page != tt.wantPage {
				t.Errorf("Page = %d, want %d", r.Page, tt.wantPage)
			}
			if r.PerPage != tt.wantPerPage {
				t.Errorf("PerPage = %d, want %d", r.PerPage, tt.wantPerPage)
			}
		})
	}
}

func TestNormalizeWithMax(t *testing.T) {
	tests := []struct {
		name        string
		in          pagination.PageRequest
		max         int
		wantPerPage int
	}{
		{"raised cap allows larger pages", pagination.PageRequest{Page: 1, PerPage: 120}, 200, 120},
		{"raised cap still clamps", pagination.PageRequest{Page: 1, PerPage: 999}, 200, 200},
		{"lower cap clamps below default", pagination.PageRequest{Page: 1, PerPage: 30}, 20, 20},
		{"default fill then clamp to low cap", pagination.PageRequest{Page: 1}, 20, 20},
		{"zero max falls back to MaxPerPage", pagination.PageRequest{Page: 1, PerPage: 999}, 0, pagination.MaxPerPage},
		{"negative max falls back to MaxPerPage", pagination.PageRequest{Page: 1, PerPage: 999}, -1, pagination.MaxPerPage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.in
			r.NormalizeWithMax(tt.max)
			if r.PerPage != tt.wantPerPage {
				t.Errorf("PerPage = %d, want %d", r.PerPage, tt.wantPerPage)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Run("normalizes and returns nil", func(t *testing.T) {
		r := pagination.PageRequest{Page: -1, PerPage: 999}
		err := r.Validate()
		if err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if r.Page != pagination.DefaultPage {
			t.Errorf("Page = %d, want %d", r.Page, pagination.DefaultPage)
		}
		if r.PerPage != pagination.MaxPerPage {
			t.Errorf("PerPage = %d, want %d", r.PerPage, pagination.MaxPerPage)
		}
	})

	t.Run("valid values unchanged", func(t *testing.T) {
		r := pagination.PageRequest{Page: 3, PerPage: 20}
		err := r.Validate()
		if err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if r.Page != 3 {
			t.Errorf("Page = %d, want 3", r.Page)
		}
		if r.PerPage != 20 {
			t.Errorf("PerPage = %d, want 20", r.PerPage)
		}
	})
}

func TestOffset(t *testing.T) {
	tests := []struct {
		name       string
		page       int
		perPage    int
		wantOffset int
	}{
		{"first page", 1, 10, 0},
		{"second page", 2, 10, 10},
		{"third page with 25 per page", 3, 25, 50},
		{"large page", 100, 50, 4950},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := pagination.PageRequest{Page: tt.page, PerPage: tt.perPage}
			if got := r.Offset(); got != tt.wantOffset {
				t.Errorf("Offset() = %d, want %d", got, tt.wantOffset)
			}
		})
	}
}

func TestNewPage(t *testing.T) {
	t.Run("basic construction", func(t *testing.T) {
		records := []string{"a", "b", "c"}
		p := pagination.NewPage(records, 100, 2, 10)

		if len(p.Records) != 3 {
			t.Fatalf("Records len = %d, want 3", len(p.Records))
		}
		if p.Total != 100 {
			t.Errorf("Total = %d, want 100", p.Total)
		}
		if p.Page != 2 {
			t.Errorf("Page = %d, want 2", p.Page)
		}
		if p.PerPage != 10 {
			t.Errorf("PerPage = %d, want 10", p.PerPage)
		}
		if p.TotalPages != 10 {
			t.Errorf("TotalPages = %d, want 10", p.TotalPages)
		}
	})

	t.Run("total pages rounds up", func(t *testing.T) {
		p := pagination.NewPage([]int{1}, 11, 1, 5)
		if p.TotalPages != 3 {
			t.Errorf("TotalPages = %d, want 3 (ceil(11/5))", p.TotalPages)
		}
	})

	t.Run("nil records becomes empty slice", func(t *testing.T) {
		p := pagination.NewPage[string](nil, 0, 1, 10)
		if p.Records == nil {
			t.Fatal("Records is nil, want empty slice")
		}
		if len(p.Records) != 0 {
			t.Errorf("Records len = %d, want 0", len(p.Records))
		}
	})

	t.Run("zero total", func(t *testing.T) {
		p := pagination.NewPage([]int{}, 0, 1, 10)
		if p.TotalPages != 0 {
			t.Errorf("TotalPages = %d, want 0", p.TotalPages)
		}
	})

	t.Run("zero perPage", func(t *testing.T) {
		p := pagination.NewPage([]int{}, 10, 1, 0)
		if p.TotalPages != 0 {
			t.Errorf("TotalPages = %d, want 0 for zero perPage", p.TotalPages)
		}
	})
}

func TestNewEmpty(t *testing.T) {
	p := pagination.NewEmpty[string]()

	if p.Records == nil {
		t.Fatal("Records is nil, want empty slice")
	}
	if len(p.Records) != 0 {
		t.Errorf("Records len = %d, want 0", len(p.Records))
	}
	if p.Total != 0 {
		t.Errorf("Total = %d, want 0", p.Total)
	}
	if p.Page != pagination.DefaultPage {
		t.Errorf("Page = %d, want %d", p.Page, pagination.DefaultPage)
	}
	if p.PerPage != pagination.DefaultPerPage {
		t.Errorf("PerPage = %d, want %d", p.PerPage, pagination.DefaultPerPage)
	}
	if p.TotalPages != 0 {
		t.Errorf("TotalPages = %d, want 0", p.TotalPages)
	}
}

func TestHasNextHasPrev(t *testing.T) {
	tests := []struct {
		name     string
		page     *pagination.Page[int]
		wantNext bool
		wantPrev bool
	}{
		{
			"first of many pages",
			pagination.NewPage([]int{1}, 30, 1, 10),
			true, false,
		},
		{
			"middle page",
			pagination.NewPage([]int{1}, 30, 2, 10),
			true, true,
		},
		{
			"last page",
			pagination.NewPage([]int{1}, 30, 3, 10),
			false, true,
		},
		{
			"single page",
			pagination.NewPage([]int{1}, 5, 1, 10),
			false, false,
		},
		{
			"empty page",
			pagination.NewEmpty[int](),
			false, false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.page.HasNext(); got != tt.wantNext {
				t.Errorf("HasNext() = %v, want %v", got, tt.wantNext)
			}
			if got := tt.page.HasPrev(); got != tt.wantPrev {
				t.Errorf("HasPrev() = %v, want %v", got, tt.wantPrev)
			}
		})
	}
}

func TestValidateSort(t *testing.T) {
	cfg := &pagination.SortConfig{
		DefaultField:  "created_at",
		DefaultOrder:  "DESC",
		AllowedFields: map[string]string{"name": "name", "date": "created_at"},
	}

	tests := []struct {
		name      string
		req       *pagination.SortRequest
		cfg       *pagination.SortConfig
		wantCol   string
		wantOrder string
	}{
		{
			"valid field and order",
			&pagination.SortRequest{SortBy: "name", SortOrder: "asc"},
			cfg,
			"name", "ASC",
		},
		{
			"unknown field falls back to default",
			&pagination.SortRequest{SortBy: "unknown", SortOrder: "asc"},
			cfg,
			"created_at", "ASC",
		},
		{
			"invalid order falls back to default",
			&pagination.SortRequest{SortBy: "name", SortOrder: "random"},
			cfg,
			"name", "DESC",
		},
		{
			"empty sort_by falls back to default field",
			&pagination.SortRequest{SortBy: "", SortOrder: "asc"},
			cfg,
			"created_at", "ASC",
		},
		{
			"nil request falls back to defaults",
			nil,
			cfg,
			"created_at", "DESC",
		},
		{
			"nil config returns empty",
			&pagination.SortRequest{SortBy: "name", SortOrder: "asc"},
			nil,
			"", "",
		},
		{
			"case insensitive order",
			&pagination.SortRequest{SortBy: "name", SortOrder: "DESC"},
			cfg,
			"name", "DESC",
		},
		{
			"no default field and unknown sort_by",
			&pagination.SortRequest{SortBy: "unknown", SortOrder: "asc"},
			&pagination.SortConfig{AllowedFields: map[string]string{"name": "col"}},
			"", "ASC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col, ord := tt.req.ValidateSort(tt.cfg)
			if col != tt.wantCol {
				t.Errorf("column = %q, want %q", col, tt.wantCol)
			}
			if ord != tt.wantOrder {
				t.Errorf("order = %q, want %q", ord, tt.wantOrder)
			}
		})
	}
}

func TestToDataMeta(t *testing.T) {
	t.Run("populated page", func(t *testing.T) {
		p := pagination.NewPage([]string{"a", "b"}, 50, 3, 10)
		data, meta := p.ToDataMeta()

		if len(data) != 2 {
			t.Fatalf("data len = %d, want 2", len(data))
		}
		if data[0] != "a" || data[1] != "b" {
			t.Errorf("data = %v, want [a b]", data)
		}
		if meta.Total != 50 {
			t.Errorf("meta.Total = %d, want 50", meta.Total)
		}
		if meta.Page != 3 {
			t.Errorf("meta.Page = %d, want 3", meta.Page)
		}
		if meta.PerPage != 10 {
			t.Errorf("meta.PerPage = %d, want 10", meta.PerPage)
		}
		if meta.TotalPages != 5 {
			t.Errorf("meta.TotalPages = %d, want 5", meta.TotalPages)
		}
		if !meta.HasNext {
			t.Error("meta.HasNext = false, want true")
		}
		if !meta.HasPrev {
			t.Error("meta.HasPrev = false, want true")
		}
	})

	t.Run("empty page", func(t *testing.T) {
		p := pagination.NewEmpty[int]()
		data, meta := p.ToDataMeta()

		if len(data) != 0 {
			t.Errorf("data len = %d, want 0", len(data))
		}
		if meta.Total != 0 {
			t.Errorf("meta.Total = %d, want 0", meta.Total)
		}
		if meta.HasNext {
			t.Error("meta.HasNext = true, want false")
		}
		if meta.HasPrev {
			t.Error("meta.HasPrev = true, want false")
		}
	})

	t.Run("first page has next but no prev", func(t *testing.T) {
		p := pagination.NewPage([]string{"a"}, 20, 1, 10)
		_, meta := p.ToDataMeta()

		if !meta.HasNext {
			t.Error("meta.HasNext = false, want true")
		}
		if meta.HasPrev {
			t.Error("meta.HasPrev = true, want false")
		}
	})

	t.Run("last page has prev but no next", func(t *testing.T) {
		p := pagination.NewPage([]string{"a"}, 20, 2, 10)
		_, meta := p.ToDataMeta()

		if meta.HasNext {
			t.Error("meta.HasNext = true, want false")
		}
		if !meta.HasPrev {
			t.Error("meta.HasPrev = false, want true")
		}
	})
}

func TestMap(t *testing.T) {
	t.Run("maps records and preserves metadata", func(t *testing.T) {
		src := pagination.NewPage([]int{1, 2, 3}, 30, 2, 10) // TotalPages = 3
		got := src.Map(func(n int) string { return fmt.Sprintf("#%d", n) })

		want := []string{"#1", "#2", "#3"}
		if len(got.Records) != len(want) {
			t.Fatalf("Records len = %d, want %d", len(got.Records), len(want))
		}
		for i := range want {
			if got.Records[i] != want[i] {
				t.Errorf("Records[%d] = %q, want %q", i, got.Records[i], want[i])
			}
		}
		if got.Total != 30 || got.Page != 2 || got.PerPage != 10 || got.TotalPages != 3 {
			t.Errorf("metadata = {Total:%d Page:%d PerPage:%d TotalPages:%d}, want {30 2 10 3}",
				got.Total, got.Page, got.PerPage, got.TotalPages)
		}
	})

	t.Run("empty page maps to non-nil empty slice", func(t *testing.T) {
		src := pagination.NewPage([]int{}, 0, 1, 10)
		got := src.Map(func(n int) string { return "" })

		if got.Records == nil {
			t.Fatal("Records is nil, want empty non-nil slice")
		}
		if len(got.Records) != 0 {
			t.Errorf("Records len = %d, want 0", len(got.Records))
		}
	})

	t.Run("source page is not mutated", func(t *testing.T) {
		src := pagination.NewPage([]int{1, 2}, 2, 1, 10)
		_ = src.Map(func(n int) int { return n * 10 })

		if len(src.Records) != 2 || src.Records[0] != 1 || src.Records[1] != 2 {
			t.Errorf("source Records = %v, want [1 2] (Map must not mutate the source)", src.Records)
		}
	})

	t.Run("nil fn panics even for an empty page", func(t *testing.T) {
		src := pagination.NewPage([]int{}, 0, 1, 10)
		defer func() {
			if r := recover(); r == nil {
				t.Error("Map(nil) did not panic, want panic")
			}
		}()
		src.Map[string](nil)
	})
}

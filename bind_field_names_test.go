package credo_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

// SourcePaging is embedded, so its fields are named at the outer level.
type SourcePaging struct {
	PerPage int `json:"perPage" query:"per_page" form:"per-page"`
}

// sourceFilter is a nested value whose fields carry the tags of every source.
type sourceFilter struct {
	MaxPrice int `json:"maxPrice" query:"max_price" form:"max-price"`
}

func (f *sourceFilter) Validate() error {
	return validation.ValidateStruct(f,
		validation.Field(&f.MaxPrice, validation.Min(1)),
	)
}

// sourceInput names each field differently in JSON, in a query and in a form,
// so a violation's field shows which tag named it. Untagged has no tag at all.
type sourceInput struct {
	SourcePaging
	MinPrice int          `json:"minPrice" query:"min_price" form:"min-price" xml:"min"`
	Filter   sourceFilter `json:"filterBy" query:"filter" form:"filter-by"`
	Untagged string
}

func (in *sourceInput) Validate() error {
	return validation.ValidateStruct(in,
		validation.Field(&in.PerPage, validation.Min(1)),
		validation.Field(&in.MinPrice, validation.Min(1)),
		validation.Field(&in.Filter),
		validation.Field(&in.Untagged, validation.Required[string]()),
	)
}

// serveSourceInput binds a request into a sourceInput, by BindQuery for a GET
// and by BindBody otherwise, and returns the violations' fields of the 422.
func serveSourceInput(t *testing.T, r *http.Request) []string {
	t.Helper()
	app := mustNew(t)
	handler := func(c *credo.Context) error {
		var in sourceInput
		if c.Request().Method == http.MethodGet {
			return c.Request().BindQuery(&in)
		}
		return c.Request().BindBody(&in)
	}
	app.GET("/in", handler)
	app.POST("/in", handler)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", w.Code, w.Body.String())
	}
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	fields := make([]string, len(body.Error.Violations))
	for i, v := range body.Error.Violations {
		fields[i] = v.Field
	}
	return fields
}

func TestBind_ViolationsNameFieldsByTheirSource(t *testing.T) {
	multipartBody := func() (*bytes.Buffer, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.WriteField("min-price", "0"); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		return &buf, mw.FormDataContentType()
	}

	tests := []struct {
		name string
		req  func() *http.Request
		want []string
	}{
		{
			name: "BindQuery names fields by the query tag",
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/in?per_page=0&min_price=0", nil)
			},
			want: []string{"per_page", "min_price", "filter.max_price", "Untagged"},
		},
		{
			name: "a form body names fields by the form tag",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/in", strings.NewReader("per-page=0&min-price=0"))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return r
			},
			want: []string{"per-page", "min-price", "filter-by.max-price", "Untagged"},
		},
		{
			name: "a multipart body names fields by the form tag",
			req: func() *http.Request {
				body, contentType := multipartBody()
				r := httptest.NewRequest(http.MethodPost, "/in", body)
				r.Header.Set("Content-Type", contentType)
				return r
			},
			want: []string{"per-page", "min-price", "filter-by.max-price", "Untagged"},
		},
		{
			name: "a JSON body names fields by the json tag",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/in", strings.NewReader(`{"minPrice":0}`))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
			want: []string{"perPage", "minPrice", "filterBy.maxPrice", "Untagged"},
		},
		{
			name: "an XML body keeps the json names",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/in", strings.NewReader(`<sourceInput><min>0</min></sourceInput>`))
				r.Header.Set("Content-Type", "application/xml")
				return r
			},
			want: []string{"perPage", "minPrice", "filterBy.maxPrice", "Untagged"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serveSourceInput(t, tt.req()); !slices.Equal(got, tt.want) {
				t.Fatalf("fields = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSourceFieldPath(t *testing.T) {
	type item struct {
		Name string `json:"name" form:"item-name"`
	}
	type target struct {
		Items  []item         `json:"items" form:"lines"`
		Array  [2]*item       `json:"array" form:"pair"`
		ByKey  map[string]any `json:"byKey" form:"by-key"`
		Any    any            `json:"any" form:"anything"`
		Plain  string
		Hidden string `json:"-" form:"-"`
	}
	typ := reflect.TypeFor[*target]()
	tests := []struct{ path, want string }{
		{"items[0].name", "lines[0].item-name"},
		{"items[12]", "lines[12]"},
		{"array[1].name", "pair[1].item-name"},
		{"byKey[k].name", "by-key[k].name"},
		{"any.name", "anything.name"},
		{"Plain", "Plain"},
		{"Hidden", "Hidden"},
		{"unknown.name", "unknown.name"},
		{"items[0", "lines[0"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := credo.SourceFieldPath(typ, tt.path, "form"); got != tt.want {
			t.Errorf("SourceFieldPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestSourceFieldNames_KeepsOtherErrorsAndTheEngineNames(t *testing.T) {
	target := &sourceInput{}
	single := &validation.ValidationError{Field: "minPrice", Code: "min"}

	got := credo.SourceFieldNames(single, target, "query")
	ve, ok := got.(*validation.ValidationError) //nolint:errorlint // The rename returns a new top-level violation.
	if !ok || ve.Field != "min_price" {
		t.Fatalf("SourceFieldNames(*ValidationError) = %v, want field min_price", got)
	}
	if single.Field != "minPrice" {
		t.Fatalf("the original violation was renamed in place: %q", single.Field)
	}

	errs := validation.Errors{{Field: "minPrice", Code: "min"}}
	if got := credo.SourceFieldNames(errs, target, "json"); !reflect.DeepEqual(got, errs) {
		t.Fatalf("SourceFieldNames(json) = %v, want the engine's names", got)
	}
	_ = credo.SourceFieldNames(errs, target, "form")
	if errs[0].Field != "minPrice" {
		t.Fatalf("the original Errors were renamed in place: %q", errs[0].Field)
	}

	internal := http.ErrBodyNotAllowed
	if got := credo.SourceFieldNames(internal, target, "query"); got != internal { //nolint:errorlint // Any other error must come back as the same value.
		t.Fatalf("SourceFieldNames(internal) = %v, want it unchanged", got)
	}
}

// orderLine is a slice element with a pointer-receiver Validate.
type orderLine struct {
	SKU string `json:"sku"`
}

func (l *orderLine) Validate() error {
	return validation.ValidateStruct(l, validation.Field(&l.SKU, validation.Required[string]()))
}

// orderInput has a slice of structs and an optional nested value behind a
// presence rule.
type orderInput struct {
	Lines   []orderLine    `json:"lines"`
	Billing *nestedAddress `json:"billing"`
}

func (o *orderInput) Validate() error {
	return validation.ValidateStruct(o,
		validation.Field(&o.Lines, validation.NotEmptySlice[orderLine]()),
		validation.Field(&o.Billing, validation.NotNil[nestedAddress]()),
	)
}

func TestBindBody_SliceElementsAndNestedValuesBehindRulesFailWith422(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"an invalid element of a slice of structs", `{"lines":[{"sku":"a"},{"sku":""}],"billing":{"city":"Ankara"}}`, []string{"lines[1].sku"}},
		{"a nested value failing its Validate behind NotNil", `{"lines":[{"sku":"a"}],"billing":{"city":" "}}`, []string{"billing.city"}},
		{"a nil nested value reports NotNil only", `{"lines":[{"sku":"a"}]}`, []string{"billing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.POST("/orders", func(c *credo.Context) error {
				var in orderInput
				return c.Request().BindBody(&in)
			})
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			app.ServeHTTP(w, r)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body %s", w.Code, w.Body.String())
			}
			var body credo.ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
			}
			got := make([]string, len(body.Error.Violations))
			for i, v := range body.Error.Violations {
				got[i] = v.Field
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("fields = %q, want %q", got, tt.want)
			}
		})
	}
}

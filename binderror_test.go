package credo_test

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// bindProblem mirrors the RFC 7807 fields asserted by bind error tests.
type bindProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Errors []struct {
		Field   string         `json:"field"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Params  map[string]any `json:"params"`
	} `json:"errors"`
}

func decodeBindProblem(t *testing.T, w *httptest.ResponseRecorder) bindProblem {
	t.Helper()
	var pr bindProblem
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode problem response: %v (body %q)", err, w.Body.String())
	}
	return pr
}

// assertBindReason asserts the standard shape of a bind error response:
// 400, binding type URI, and a single errors[] entry with the given
// reason code and field.
func assertBindReason(t *testing.T, w *httptest.ResponseRecorder, code, field string) bindProblem {
	t.Helper()
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %q)", w.Code, w.Body.String())
	}
	pr := decodeBindProblem(t, w)
	if pr.Type != "https://credo.dev/errors/binding" {
		t.Errorf("type = %q, want binding type URI", pr.Type)
	}
	if pr.Status != 400 {
		t.Errorf("problem status = %d, want 400", pr.Status)
	}
	if len(pr.Errors) != 1 {
		t.Fatalf("len(errors) = %d, want 1 (body %q)", len(pr.Errors), w.Body.String())
	}
	if pr.Errors[0].Code != code {
		t.Errorf("errors[0].code = %q, want %q", pr.Errors[0].Code, code)
	}
	if pr.Errors[0].Field != field {
		t.Errorf("errors[0].field = %q, want %q", pr.Errors[0].Field, field)
	}
	return pr
}

func TestBindBody_JSON_SingleValue(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"single value", `{"name":"a"}`, 200},
		{"trailing whitespace", "{\"name\":\"a\"}\n\t ", 200},
		{"second document", `{"name":"a"}{"name":"b"}`, 400},
		{"trailing garbage", `{"name":"a"}xyz`, 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.POST("/bind", func(ctx *credo.Context) error {
				var v struct {
					Name string `json:"name"`
				}
				if err := ctx.Request().BindBody(&v); err != nil {
					return err
				}
				return ctx.Response().NoContent(200)
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/bind", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			app.ServeHTTP(w, r)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus == 400 {
				assertBindReason(t, w, "trailing_data", "")
			}
		})
	}
}

func TestBindBody_JSON_ProblemShape(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(`{"name":"a"} extra`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	pr := assertBindReason(t, w, "trailing_data", "")
	if pr.Title != "Malformed Request" {
		t.Errorf("title = %q, want %q", pr.Title, "Malformed Request")
	}
	if pr.Errors[0].Message == "" {
		t.Error("errors[0].message is empty, want default English message")
	}
}

func TestBindBody_JSON_EmptyBody_Reason(t *testing.T) {
	for _, body := range []string{"", "   \n"} {
		app := mustNew(t)
		app.POST("/bind", func(ctx *credo.Context) error {
			var v struct{}
			return ctx.Request().BindBody(&v)
		})

		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/bind", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(w, r)

		assertBindReason(t, w, "empty_body", "")
	}
}

func TestBindBody_JSON_Syntax_Reason(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantOffset bool
	}{
		{"invalid token", `{"name":}`, true},
		{"truncated body", `{"name":"a"`, false},
		{"not json", "not json", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.POST("/bind", func(ctx *credo.Context) error {
				var v struct {
					Name string `json:"name"`
				}
				return ctx.Request().BindBody(&v)
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/bind", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			app.ServeHTTP(w, r)

			pr := assertBindReason(t, w, "syntax", "")
			offset, hasOffset := pr.Errors[0].Params["offset"]
			if tt.wantOffset {
				if !hasOffset {
					t.Fatalf("params.offset missing (params %v)", pr.Errors[0].Params)
				}
				if n, ok := offset.(float64); !ok || n <= 0 {
					t.Errorf("params.offset = %v, want positive number", offset)
				}
			}
		})
	}
}

func TestBindBody_JSON_TypeMismatch_Reason(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"top-level field", `{"age":"x"}`, "age"},
		{"nested field", `{"user":{"age":"x"}}`, "user.age"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.POST("/bind", func(ctx *credo.Context) error {
				var v struct {
					Age  int `json:"age"`
					User struct {
						Age int `json:"age"`
					} `json:"user"`
				}
				return ctx.Request().BindBody(&v)
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/bind", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			app.ServeHTTP(w, r)

			pr := assertBindReason(t, w, "type_mismatch", tt.wantField)
			if got := pr.Errors[0].Params["expected"]; got != "integer" {
				t.Errorf("params.expected = %v, want %q", got, "integer")
			}
		})
	}
}

func TestBindBody_JSON_DuplicateField_Reason(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"top-level", `{"name":"a","name":"b"}`, "name"},
		{"nested", `{"user":{"age":1,"age":2}}`, "user.age"},
		{"case-variant", `{"name":"a","Name":"b"}`, "Name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.POST("/bind", func(ctx *credo.Context) error {
				var v struct {
					Name string `json:"name"`
					User struct {
						Age int `json:"age"`
					} `json:"user"`
				}
				return ctx.Request().BindBody(&v)
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/bind", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			app.ServeHTTP(w, r)

			assertBindReason(t, w, "duplicate_field", tt.wantField)
		})
	}
}

func TestBindBody_JSON_CaseInsensitiveMatch(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(`{"Name":"alice"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if w.Body.String() != "alice" {
		t.Errorf("bound value = %q, want %q (case-insensitive member matching)", w.Body.String(), "alice")
	}
}

func TestBindBody_JSON_ArrayFieldPath(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v struct {
			Items []struct {
				N int `json:"n"`
			} `json:"items"`
		}
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(`{"items":[{"n":1},{"n":"x"}]}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	assertBindReason(t, w, "type_mismatch", "items[1].n")
}

func TestBindBody_JSON_InvalidValue_Reason(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v struct {
			When time.Time `json:"when"`
		}
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(`{"when":"notatime"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	assertBindReason(t, w, "invalid_value", "when")
}

func TestBindBody_NilTarget(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		return ctx.Request().BindBody(nil)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestBindBody_Form_TypeMismatch_Reason(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var f formInput
		return ctx.Request().BindBody(&f)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader("name=Alice&age=notanumber"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, r)

	pr := assertBindReason(t, w, "type_mismatch", "age")
	if got := pr.Errors[0].Params["expected"]; got != "integer" {
		t.Errorf("params.expected = %v, want %q", got, "integer")
	}
}

func TestBindQuery_TypeMismatch_Reason(t *testing.T) {
	app := mustNew(t)
	app.GET("/bind", func(ctx *credo.Context) error {
		var q struct {
			Page int `query:"page"`
		}
		return ctx.Request().BindQuery(&q)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/bind?page=abc", nil)
	app.ServeHTTP(w, r)

	assertBindReason(t, w, "type_mismatch", "page")
}

func TestBindQuery_InvalidValue_Reason(t *testing.T) {
	app := mustNew(t)
	app.GET("/bind", func(ctx *credo.Context) error {
		var q struct {
			Since time.Time `query:"since"`
		}
		return ctx.Request().BindQuery(&q)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/bind?since=notatime", nil)
	app.ServeHTTP(w, r)

	assertBindReason(t, w, "invalid_value", "since")
}

func TestBindBody_XML_EmptyBody_Reason(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v xmlInput
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/xml")
	app.ServeHTTP(w, r)

	assertBindReason(t, w, "empty_body", "")
}

func TestBindBody_XML_Syntax_Reason(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v xmlInput
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader("<unclosed"))
	r.Header.Set("Content-Type", "application/xml")
	app.ServeHTTP(w, r)

	assertBindReason(t, w, "syntax", "")
}

func TestBindError_AsType(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v struct {
			Age int `json:"age"`
		}
		err := ctx.Request().BindBody(&v)
		be, ok := errors.AsType[*credo.BindError](err)
		if !ok {
			t.Fatalf("BindBody error = %v, want *credo.BindError", err)
		}
		if be.Reason != credo.BindReasonTypeMismatch {
			t.Errorf("Reason = %q, want %q", be.Reason, credo.BindReasonTypeMismatch)
		}
		if be.Field != "age" {
			t.Errorf("Field = %q, want %q", be.Field, "age")
		}
		if be.Expected != "integer" {
			t.Errorf("Expected = %q, want %q", be.Expected, "integer")
		}
		if be.Offset <= 0 {
			t.Errorf("Offset = %d, want > 0", be.Offset)
		}
		if be.Unwrap() == nil {
			t.Error("Unwrap() = nil, want underlying decoder error")
		}
		return ctx.Response().NoContent(204)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(`{"age":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	if w.Code != 204 {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

func TestBindError_ErrorString(t *testing.T) {
	be := &credo.BindError{
		Reason:   credo.BindReasonTypeMismatch,
		Field:    "age",
		Expected: "integer",
		Offset:   7,
		Internal: errors.New("boom"),
	}

	got := be.Error()
	for _, want := range []string{"type_mismatch", `"age"`, "integer", "offset 7", "boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to contain %q", got, want)
		}
	}
	if be.HTTPStatus() != 400 {
		t.Errorf("HTTPStatus() = %d, want 400", be.HTTPStatus())
	}
}

// strictBindApp builds an app with strict bodies and a /bind route decoding
// into a two-level struct, returning the recorder for body.
func strictBindPost(t *testing.T, body string, opts ...credo.Option) *httptest.ResponseRecorder {
	t.Helper()
	app := mustNew(t, opts...)
	app.POST("/bind", func(ctx *credo.Context) error {
		var v struct {
			Name    string `json:"name"`
			Address struct {
				Zip string `json:"zip"`
			} `json:"address"`
		}
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	return w
}

// TestBindBody_JSON_UnknownField_LenientDefault locks the default posture:
// unknown members are ignored unless strict bodies is enabled.
func TestBindBody_JSON_UnknownField_LenientDefault(t *testing.T) {
	w := strictBindPost(t, `{"name":"alice","extra":1}`)
	if w.Code != 200 || w.Body.String() != "alice" {
		t.Fatalf("lenient default: status=%d body=%q, want 200 alice", w.Code, w.Body.String())
	}
}

func TestBindBody_JSON_UnknownField_Strict(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"top-level", `{"name":"alice","extra":1}`, "extra"},
		{"nested", `{"name":"alice","address":{"zipp":"1"}}`, "address.zipp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := strictBindPost(t, tt.body, credo.WithStrictBodies())
			pr := assertBindReason(t, w, "unknown_field", tt.wantField)
			if off, ok := pr.Errors[0].Params["offset"].(float64); !ok || off <= 0 {
				t.Errorf("params.offset = %v, want > 0", pr.Errors[0].Params["offset"])
			}
		})
	}
}

// TestBindBody_JSON_UnknownField_Precedence pins the interplay with the
// other member-level checks: case-insensitive matching happens before the
// unknown decision, and a duplicate of a known member is reported as
// duplicate_field, not unknown_field.
func TestBindBody_JSON_UnknownField_Precedence(t *testing.T) {
	w := strictBindPost(t, `{"Name":"alice"}`, credo.WithStrictBodies())
	if w.Code != 200 || w.Body.String() != "alice" {
		t.Fatalf("case-variant known member: status=%d body=%q, want 200 alice", w.Code, w.Body.String())
	}

	w = strictBindPost(t, `{"name":"a","Name":"b"}`, credo.WithStrictBodies())
	assertBindReason(t, w, "duplicate_field", "Name")
}

// TestBindBody_StrictBodies_OtherDecodersUnchanged verifies strict bodies
// only affects JSON: XML and form decoders keep ignoring extra input.
func TestBindBody_StrictBodies_OtherDecodersUnchanged(t *testing.T) {
	tests := []struct {
		name string
		ct   string
		body string
	}{
		{"xml", "application/xml", `<v><name>alice</name><extra>1</extra></v>`},
		{"form", "application/x-www-form-urlencoded", `name=alice&extra=1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t, credo.WithStrictBodies())
			app.POST("/bind", func(ctx *credo.Context) error {
				var v struct {
					XMLName struct{} `xml:"v" json:"-" form:"-"`
					Name    string   `xml:"name" form:"name"`
				}
				if err := ctx.Request().BindBody(&v); err != nil {
					return err
				}
				return ctx.Response().Text(200, v.Name)
			})
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/bind", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", tt.ct)
			app.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.String() != "alice" {
				t.Fatalf("status=%d body=%q, want 200 alice", w.Code, w.Body.String())
			}
		})
	}
}

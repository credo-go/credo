package credo_test

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// xmlOrder is the target of the XML contract tests: a scalar field, a slice
// field and a nested element.
type xmlOrder struct {
	XMLName struct{} `xml:"order"`
	ID      string   `xml:"id"`
	Lines   []string `xml:"line"`
	Buyer   struct {
		Name string `xml:"name"`
	} `xml:"buyer"`
}

// xmlBind posts body as XML to a handler that binds an xmlOrder and writes
// what it decoded. The handler records the bind error in bindErr.
func xmlBind(t *testing.T, contentType, body string, bindErr *error, opts ...credo.Option) *httptest.ResponseRecorder {
	t.Helper()
	app := mustNew(t, opts...)
	app.POST("/orders", func(c *credo.Context) error {
		var v xmlOrder
		err := c.Request().BindBody(&v)
		if bindErr != nil {
			*bindErr = err
		}
		if err != nil {
			return err
		}
		return c.Response().Text(200, fmt.Sprintf("id=%s lines=%s buyer=%s", v.ID, strings.Join(v.Lines, ","), v.Buyer.Name))
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/orders", strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	app.ServeHTTP(w, r)
	return w
}

// TestBindBody_XML_SingleDocument pins what may follow the root element of an
// XML body: white space, comments and processing instructions, as XML's own
// grammar allows. Anything else is refused with the trailing-data reason, as
// content after a JSON body's first value is, and the refusal names what the
// body must be (the client's remedy) and, for the log, what was found.
func TestBindBody_XML_SingleDocument(t *testing.T) {
	const doc = `<order><id>7</id></order>`
	tests := []struct {
		name string
		body string
		// found is what the logged cause names; empty when the body is accepted.
		found string
	}{
		{"document alone", doc, ""},
		{"prolog before the root", `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + doc, ""},
		{"white space after the root", doc + " \t\r\n", ""},
		{"comment after the root", doc + "\n<!-- sent by the batch job -->\n", ""},
		{"processing instruction after the root", doc + `<?audit id="3"?>`, ""},
		{"second document", doc + `<order><id>8</id></order>`, "the element <order> after its root element"},
		{"another element", doc + `<note/>`, "the element <note> after its root element"},
		{"text", doc + "junk", "character data after its root element"},
		{"text after white space", doc + "\n  7", "character data after its root element"},
		{"non-breaking space is not XML white space", doc + " ", "character data after its root element"},
		{"directive", doc + `<!DOCTYPE order>`, "a directive after its root element"},
		{"stray end tag", doc + `</order>`, "unexpected end element </order>"},
		{"truncated markup", doc + `<`, "unexpected EOF"},
	}
	for _, ct := range []string{"application/xml", "text/xml; charset=utf-8"} {
		for _, tt := range tests {
			t.Run(ct+"/"+tt.name, func(t *testing.T) {
				var bindErr error
				w := xmlBind(t, ct, tt.body, &bindErr)
				if tt.found == "" {
					if w.Code != 200 || w.Body.String() != "id=7 lines= buyer=" {
						t.Fatalf("status = %d, body = %q, want the document bound", w.Code, w.Body.String())
					}
					return
				}
				pr := assertBindReason(t, w, "trailing_data", "")
				if got, want := pr.Error.Violations[0].Message, "request body must contain a single XML document"; got != want {
					t.Errorf("violations[0].message = %q, want %q", got, want)
				}
				be, ok := errors.AsType[*credo.BindError](bindErr)
				if !ok {
					t.Fatalf("BindBody error = %v, want a *credo.BindError", bindErr)
				}
				if be.Reason != credo.BindReasonTrailingData || be.Field != "" {
					t.Errorf("BindError = %+v, want the trailing-data reason and no field", be)
				}
				if be.Internal == nil || !strings.Contains(be.Internal.Error(), tt.found) {
					t.Errorf("BindError.Internal = %v, want it to name %q", be.Internal, tt.found)
				}
				if strings.Contains(w.Body.String(), "after its root element") {
					t.Errorf("the logged cause reached the client: %s", w.Body.String())
				}
			})
		}
	}
}

// TestBindBody_XML_TrailingData_JSONMessageUnchanged: the JSON refusal keeps
// its own wording.
func TestBindBody_XML_TrailingData_JSONMessageUnchanged(t *testing.T) {
	app := mustNew(t)
	app.POST("/bind", func(c *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		return c.Request().BindBody(&v)
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/bind", strings.NewReader(`{"name":"a"}{"name":"b"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	pr := assertBindReason(t, w, "trailing_data", "")
	if got, want := pr.Error.Violations[0].Message, "request body must contain a single JSON value"; got != want {
		t.Errorf("violations[0].message = %q, want %q", got, want)
	}
}

// TestBindBody_XML_TrailingData_OverLimit: a body that passes the size limit
// after its root element is a 413, as it is in the JSON branch, not a
// trailing-data 400 and not an accepted request.
func TestBindBody_XML_TrailingData_OverLimit(t *testing.T) {
	body := `<order><id>7</id></order>` + strings.Repeat(" ", 256)
	w := xmlBind(t, "application/xml", body, nil, credo.WithMaxBodyBytes(64))
	if w.Code != 413 {
		t.Fatalf("status = %d, want 413 (body %q)", w.Code, w.Body.String())
	}
}

// TestBindBody_XML_RepeatedElement pins that a repeated element keeps
// encoding/xml's meaning and is never a duplicate-field failure: repetition
// is how XML spells a list, so every occurrence is appended to a slice field,
// and the last occurrence sets a field of another kind.
func TestBindBody_XML_RepeatedElement(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"slice field takes every occurrence", `<order><line>a</line><line>b</line><line>c</line></order>`, "id= lines=a,b,c buyer="},
		{"scalar field takes the last occurrence", `<order><id>7</id><id>8</id></order>`, "id=8 lines= buyer="},
		{"nested element merges its occurrences", `<order><buyer><name>Ada</name></buyer><buyer/></order>`, "id= lines= buyer=Ada"},
	}
	for _, strict := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("strict=%v/%s", strict, tt.name), func(t *testing.T) {
				var opts []credo.Option
				if strict {
					opts = append(opts, credo.WithStrictBodies())
				}
				w := xmlBind(t, "application/xml", tt.body, nil, opts...)
				if w.Code != 200 || w.Body.String() != tt.want {
					t.Fatalf("status = %d, body = %q, want 200 %q", w.Code, w.Body.String(), tt.want)
				}
			})
		}
	}
}

// TestBindBody_XML_UnknownIgnored pins that an element or attribute the
// target has no field for is ignored, with and without strict bodies: the
// strict switch decides JSON members only.
func TestBindBody_XML_UnknownIgnored(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown element", `<order><id>7</id><coupon>X</coupon></order>`},
		{"unknown element with children", `<order><id>7</id><gift><wrap>true</wrap></gift></order>`},
		{"unknown attribute", `<order channel="web"><id>7</id></order>`},
		{"unknown element inside a known one", `<order><id>7</id><buyer><vat>1</vat></buyer></order>`},
	}
	for _, strict := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("strict=%v/%s", strict, tt.name), func(t *testing.T) {
				var opts []credo.Option
				if strict {
					opts = append(opts, credo.WithStrictBodies())
				}
				w := xmlBind(t, "application/xml", tt.body, nil, opts...)
				if w.Code != 200 || w.Body.String() != "id=7 lines= buyer=" {
					t.Fatalf("status = %d, body = %q, want the unknown input ignored", w.Code, w.Body.String())
				}
			})
		}
	}
}

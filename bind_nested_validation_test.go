package credo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

// nestedAddress reads its fields in a pointer-receiver Validate (a call on a
// nil receiver panics) and normalizes City.
type nestedAddress struct {
	City string `json:"city"`
}

func (a *nestedAddress) Validate() error {
	a.City = strings.TrimSpace(a.City)
	return validation.ValidateStruct(a,
		validation.Field(&a.City, validation.Required[string]()),
	)
}

// nestedOrder is a bind target with a required nested struct and an optional
// nested pointer, both validated by their own Validate.
type nestedOrder struct {
	Shipping nestedAddress  `json:"shipping"`
	Billing  *nestedAddress `json:"billing"`
}

func (o *nestedOrder) Validate() error {
	return validation.ValidateStruct(o,
		validation.Field(&o.Shipping),
		validation.Field(&o.Billing),
	)
}

// serveNestedOrder binds body into a nestedOrder and echoes the bound value.
func serveNestedOrder(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	app := mustNew(t)
	app.POST("/orders", func(ctx *credo.Context) error {
		var in nestedOrder
		if err := ctx.Request().BindBody(&in); err != nil {
			return err
		}
		return ctx.Response().JSON(http.StatusOK, in)
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	return w
}

func TestBindBody_NestedNilPointerIsValid(t *testing.T) {
	w := serveNestedOrder(t, `{"shipping":{"city":"Ankara"}}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an omitted optional object; body %s", w.Code, w.Body.String())
	}
	var got nestedOrder
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	if got.Billing != nil {
		t.Fatalf("billing = %+v, want nil", got.Billing)
	}
}

func TestBindBody_NestedPointerSetIsValidated(t *testing.T) {
	w := serveNestedOrder(t, `{"shipping":{"city":"Ankara"},"billing":{"city":"  "}}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", w.Code, w.Body.String())
	}
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	if body.Error.Code != "validation_failed" || len(body.Error.Violations) != 1 {
		t.Fatalf("body = %+v, want one violation", body)
	}
	if v := body.Error.Violations[0]; v.Field != "billing.city" || v.Code != "required" {
		t.Fatalf("violation = %+v, want required on billing.city", v)
	}
}

func TestBindBody_NestedPointerReceiverKeepsNormalization(t *testing.T) {
	w := serveNestedOrder(t, `{"shipping":{"city":"  Ankara  "},"billing":{"city":" Izmir "}}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var got nestedOrder
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	if got.Shipping.City != "Ankara" {
		t.Fatalf("shipping.city = %q, want the value Validate normalized, %q", got.Shipping.City, "Ankara")
	}
	if got.Billing == nil || got.Billing.City != "Izmir" {
		t.Fatalf("billing = %+v, want city %q", got.Billing, "Izmir")
	}
}

// Package validation provides a programmatic, type-safe validation engine
// using Go generics.
//
// # Overview
//
// Validation in Credo uses generic [Rule] interfaces and pointer-based field
// references for compile-time type safety. No struct tags are used for rule
// definition. Reflection is limited to field name extraction (cached after
// first use per struct type).
//
// # Quick Start
//
//	import v "github.com/credo-go/credo/validation"
//
//	type CreateUserInput struct {
//	    Name  string `json:"name"`
//	    Email string `json:"email"`
//	    Age   int    `json:"age"`
//	}
//
//	func (c *CreateUserInput) Validate() error {
//	    return v.ValidateStruct(c,
//	        v.Field(&c.Name, v.Required[string](), v.Length(2, 100)),
//	        v.Field(&c.Email, v.Required[string](), v.Email()),
//	        v.Field(&c.Age, v.Required[int](), v.Min(18)),
//	    )
//	}
//
// When the struct implements [Validatable], the Validate method is called
// automatically by BindBody and BindQuery. In debug mode, a warning is logged
// if the target does not implement Validatable.
//
// # Nested Structs
//
// A [Field] validates a field that implements [Validatable] by calling its
// Validate once the field's rules pass, and prefixes the failures with the
// field's name (address.city); a field whose rules fail reports their
// violations only. A slice or array of such values, structs whose pointer
// implements Validatable included, has each element validated, its failures
// prefixed with the field's name and the index (items[0].name). A struct
// field's or element's Validate runs on the value itself, so what a
// pointer-receiver Validate normalizes is kept. A nil pointer or nil interface
// is valid and its Validate is not called, so an optional nested object may
// be omitted, and a mandatory one takes [NotNil]:
//
//	func (o *CreateOrderInput) Validate() error {
//	    return v.ValidateStruct(o,
//	        v.Field(&o.Shipping),                     // Address: always validated
//	        v.Field(&o.Billing),                      // *Address: validated when set
//	        v.Field(&o.Contact, v.NotNil[Contact]()), // *Contact: must be set, then validated
//	        v.Field(&o.Items),                        // []LineItem: each element validated
//	    )
//	}
//
// A violation names a field by its json tag, or the Go field name when the
// tag gives none. BindBody and BindQuery rename the violations a target's
// Validate returns to the tag of the source they decoded: form for a form or
// multipart body, query for a query.
//
// # Error Format
//
// Validation errors are returned as [Violations] ([]Violation), designed
// for centralized error-envelope integration. Each [Violation] includes
// Field, Code, Message, and optional Params for i18n template rendering. A
// custom rule builds one with [NewViolation].
//
// # PATCH Support
//
// For partial updates with pointer fields, use [NilSafe]:
//
//	v.Field(&u.Name, v.NilSafe(v.Length(2, 100)))
//
// # Adapted From
//
// API design adapted from ozzo-validation (MIT). Generic [Rule] architecture
// inspired by govy (design only, no code adapted). See NOTICES file for
// full attribution.
//
// Maturity: beta
package validation

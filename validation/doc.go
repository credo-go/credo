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
// A [Field] with no rules validates a field that implements [Validatable] by
// calling its Validate, and prefixes the failures with the field's name
// (address.city). A struct field's Validate runs on the field itself, so what
// a pointer-receiver Validate normalizes is kept. A nil pointer or nil
// interface is valid and its Validate is not called, so an optional nested
// object may be omitted. A field with rules runs its rules only, so a
// mandatory nested pointer takes [NotNil] in a Field of its own:
//
//	func (o *CreateOrderInput) Validate() error {
//	    return v.ValidateStruct(o,
//	        v.Field(&o.Shipping),                     // Address: always validated
//	        v.Field(&o.Billing),                      // *Address: validated when set
//	        v.Field(&o.Contact, v.NotNil[Contact]()), // *Contact: must be set
//	        v.Field(&o.Contact),                      // and is then validated
//	    )
//	}
//
// # Error Format
//
// Validation errors are returned as [Errors] ([]ValidationError), designed
// for centralized error-envelope integration. Each error includes Field, Code,
// Message, and optional Params for i18n template rendering.
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

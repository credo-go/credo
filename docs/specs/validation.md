# Validation Spec

**Status:** Implemented (beta) **Package:** `validation/` **ADR:** [ADR-011](../adr/011-validation-strategy.md) **Guide:** [Getting started](../guides/getting-started.md#validation), [Error handling guide](../guides/error-handling.md#validation-and-bind-failures) **Sources:** ozzo-validation (MIT), Goyave (MIT, topic-based file grouping), govy (design inspiration only, no code adapted)

## Overview

Credo uses **programmatic-only** validation with **generic type-safe rules** and compile-time safe pointer-based field references. No struct tags for rule definition. Reflection limited to field name extraction (cached). Integrated with Context via the "Parse, don't validate" pattern.

This spec owns the `validation` package contract: the rule interface, the built-in rules, the rule-error contract and the error types. Package `validation` depends only on the standard library; the root package depends on it. The error pipeline that classifies a validation error as 422 is [ADR-009](../adr/009-handler-and-error-handling.md)'s; message-key resolution is in the [i18n spec](i18n.md).

## Philosophy: Parse, Don't Validate

Validation is part of parsing, not a separate step. When the target of `ctx.Request().BindBody(&input)` or `BindQuery` implements `Validatable`, `Validate()` runs right after decoding, so the handler only receives data that has already been validated ([ADR-008](../adr/008-context-design.md#parse-dont-validate)):

```go
// package validation
type Validatable interface {
    Validate() error
}
```

## API Surface

### Rule Interface (generic foundation)

```go
// validation/rule.go
type Rule[T any] interface {
    Validate(value T) error
}
```

`Rule[T]` is generic — the type parameter `T` matches the field type. This provides compile-time type safety: applying a `Rule[int]` to a `*string` field is a compiler error, not a runtime panic.

### FieldRules (type-erased container)

```go
// validation/rule.go
type FieldRules interface {
    validate(structPtr any) error
}
```

`FieldRules` is a non-generic interface returned by `Field[T]`. It erases the type parameter so that `ValidateStruct` can accept fields of different types in a single variadic call.

### ValidateStruct + Field (ozzo-inspired, pointer-based field refs)

```go
// validation/rule.go
func ValidateStruct(structPtr any, fields ...FieldRules) error

func Field[T any](fieldPtr *T, rules ...Rule[T]) FieldRules
```

`ValidateStruct` treats a nil pointer as valid and panics on a non-pointer or a pointer to a non-struct (a programming error).

**Two layers of compile-time safety**:

1. `&c.Name` — Go compiler checks the field exists on the struct.
2. `Rule[T]` — Go compiler checks the rule type matches the field type.

```go
type CreateUserInput struct {
    Name  string `json:"name"`
    Email string `json:"email"`
    Age   int    `json:"age"`
}

func (c *CreateUserInput) Validate() error {
    return validation.ValidateStruct(c,
        validation.Field(&c.Name, validation.Required[string](), validation.Length(2, 100)),
        validation.Field(&c.Email, validation.Required[string](), validation.Email()),
        validation.Field(&c.Age, validation.Required[int](), validation.Min(18)),
    )
}

// COMPILER ERRORS — caught before runtime:
validation.Field(&c.Name, validation.Min(18))   // ✗ *string ≠ Rule[int]
validation.Field(&c.Age, validation.Email())    // ✗ *int ≠ Rule[string]
```

### Built-in Rules (topic-based grouping)

```
validation/
├── rule.go             Rule[T] interface, FieldRules, ValidateStruct, Field[T], Validatable
├── errors.go           ValidationError, Errors (JSON marshaling, Unwrap), NewError
├── string_rules.go     Required[T], Email, URL, UUID, Regex, Length
├── numeric_rules.go    Min, Max, Between
├── collection_rules.go NotEmptySlice, NotEmptyMap, Each, When, NilSafe
├── date_rules.go       DateBefore, DateAfter
├── file_rules.go       MaxFileSize, AllowedMimeTypes, AllowedExtensions
├── common_rules.go     In, NotNil, By (inline custom)
└── doc.go
```

**Rule semantics**: the format rules (`Email`, `URL`, `UUID`, `Regex`) and `Length` pass an empty string, the date rules pass the zero time, and the file rules pass a nil file header, so presence comes from `Required` (or `NotEmptySlice`/`NotEmptyMap`, `NotNil`). `Min`, `Max` and `Between` compare every value, zero included. `Length` counts runes, not bytes. Every rule of a field runs, and each failure is a separate violation: in the `CreateUserInput` example above, an age of 0 yields both `required` and `min`. Per-rule detail is in the godoc.

**Single package model**: All types, functions, and rules live in the `validation` package. There is no `validation/rule` sub-package. For concise examples, use an import alias:

```go
import v "github.com/credo-go/credo/validation"

v.ValidateStruct(c,
    v.Field(&c.Name, v.Required[string](), v.Length(2, 100)),
)
```

**Required vs NotEmpty**: `Required[T]` accepts any comparable type and fails on its zero value. Its `comparable` constraint excludes slices and maps by language rules. `NotEmptySlice[E]()` and `NotEmptyMap[K, V]()` are their counterparts: they fail when the collection is nil or has length zero (error code `not_empty`). The check is `len`-based — no reflection, consistent with the zero-reflection rule execution policy.

### Custom Rules

A custom rule reports a client-visible failure with `validation.NewError(code, message string) *ValidationError`; the field path is filled in by `Field`, as for the built-in rules.

```go
// Option 1: Implement Rule[T] interface
type CountryCode struct{}

func (r CountryCode) Validate(value string) error {
    if !isValidCountryCode(value) {
        return validation.NewError("country_code", "must be a valid country code")
    }
    return nil
}

// Usage: validation.Field(&c.Country, validation.Required[string](), CountryCode{})

// Option 2: Inline with validation.By[T]()
validation.Field(&c.Code, validation.By(func(code string) error {
    if len(code) != 2 {
        return validation.NewError("country_code", "must be a 2-letter code")
    }
    return nil
}))
```

### Rule Errors

The type of a rule's error decides whether the client sees it. The type is found the way the error pipeline finds it, through the error's chain, so a `*ValidationError` wrapped with `fmt.Errorf("…: %w", …)` is still a violation:

| A rule returns | Validation | Response (default pipeline) |
| --- | --- | --- |
| `*ValidationError` or `validation.Errors` | continues; the error is a violation of the field | 422 `validation_failed`, the rule's code and message in `violations[]` |
| any other error | stops at once; the error leaves `ValidateStruct`, `Validate` and `BindBody`/`BindQuery` unchanged | classified like any handler error ([ADR-009](../adr/009-handler-and-error-handling.md)): a plain error is a 500 with the default message, its text logged, not rendered; a fault keeps its mapped status (`store`'s unavailable kind → 503) |

An internal error stops validation everywhere it can arise: `ValidateStruct` returns it without collecting the remaining fields, and nested `Validatable` fields, `Each`, `When` and `NilSafe` pass it on unchanged, so a rule's internal failure never turns into a violation at any depth. Rationale and the rejected alternative: [ADR-011](../adr/011-validation-strategy.md#rule-errors-client-messages-and-internal-failures).

### Validation Boundary — Stateless Only

`Validate()` handles **all validation that can be determined from the struct fields alone** — this includes both format checks and pure business rules:

- **Format**: required, email, length, regex, UUID, URL
- **Cross-field business rules**: "end date must be after start date", "card number required when payment method is card", "discount <= 50%"

These are all **stateless** — they depend only on the payload, require no external state, and are deterministic.

**Stateful validation** (anything requiring I/O) belongs in the **service layer**:

- Uniqueness checks (DB query)
- Referential integrity (does this product ID exist?)
- Balance/quota verification (DB query)
- External service calls (payment gateway, API key validation)

This separation ensures:

- `Validate()` is fast, deterministic, and testable without mocks.
- Service-layer validation has access to repositories and context.
- Error handling is cleanly separated (422 for validation, 409/403 for stateful).

```go
// ✗ WRONG — I/O in Validate()
func (c *CreateUserInput) Validate() error {
    // Don't do this — stateful checks belong in the service layer
    db.QueryRow("SELECT EXISTS(...)", c.Email)
}

// ✓ CORRECT — stateless validation in Validate()
type Order struct {
    Email     string    `json:"email"`
    Quantity  int       `json:"quantity"`
    StartDate time.Time `json:"start_date"`
    EndDate   time.Time `json:"end_date"`
}

func (o *Order) Validate() error {
    return validation.ValidateStruct(o,
        validation.Field(&o.Email, validation.Required[string](), validation.Email()),
        validation.Field(&o.Quantity, validation.Min(1)),
        // Cross-field business rule — pure, no I/O
        validation.Field(&o.EndDate,
            validation.When(!o.StartDate.IsZero(), validation.DateAfter(o.StartDate)),
        ),
    )
}

// Stateful checks belong in the service layer
func (s *OrderService) Create(ctx context.Context, input Order) error {
    exists, err := s.repo.EmailExists(ctx, input.Email)
    if err != nil {
        return fmt.Errorf("check email: %w", err)
    }
    if exists {
        return credo.NewHTTPError(http.StatusConflict, "email_exists")
    }
    // ...
}
```

### Conditional Validation

```go
func (p *Payment) Validate() error {
    return validation.ValidateStruct(p,
        validation.Field(&p.CardNumber,
            validation.When(p.Method == "card",
                validation.Required[string](), validation.Length(13, 19),
            ),
        ),
        validation.Field(&p.IBAN,
            validation.When(p.Method == "bank",
                validation.Required[string](), validation.Length(15, 34),
            ),
        ),
    )
}
```

`When`'s rules are built eagerly: Go evaluates them before `When` sees its condition, so a rule argument must not dereference what the condition guards (`When(o.StartDate != nil, DateAfter(*o.StartDate))` panics on a nil `StartDate`); use a value field or check inside a `validation.By` closure.

### PATCH / Partial Update Support

For PATCH requests, input structs use pointer fields to distinguish "not sent" (nil) from "sent as empty". `NilSafe` wraps rules to skip validation when the pointer is nil:

```go
type UpdateUserInput struct {
    Name  *string `json:"name"`   // pointer = optional field
    Email *string `json:"email"`
}

func (u *UpdateUserInput) Validate() error {
    return validation.ValidateStruct(u,
        validation.Field(&u.Name, validation.NilSafe(validation.Length(2, 100))),
        validation.Field(&u.Email, validation.NilSafe(validation.Email())),
    )
}
```

`NilSafe[T](rules ...Rule[T]) Rule[*T]` — when the pointer is nil, validation is skipped. When non-nil, the value is unwrapped and inner rules execute.

### Nested Struct Validation

```go
type Address struct {
    Street string `json:"street"`
    City   string `json:"city"`
}

func (a *Address) Validate() error {
    return validation.ValidateStruct(a,
        validation.Field(&a.Street, validation.Required[string]()),
        validation.Field(&a.City, validation.Required[string]()),
    )
}

type CreateUserInput struct {
    Name    string  `json:"name"`
    Address Address `json:"address"`
}

func (c *CreateUserInput) Validate() error {
    return validation.ValidateStruct(c,
        validation.Field(&c.Name, validation.Required[string]()),
        validation.Field(&c.Address), // Address.Validate() called automatically
    )
}
```

When a field has no rules but implements `Validatable`, `ValidateStruct` auto-calls `Validate()` on it, enabling recursive nested validation.

## Error Format

Validation errors are classified as 422 by the framework's internal error pipeline and rendered in the [default error envelope](../adr/009-handler-and-error-handling.md#default-error-envelope) (or by a configured `ErrorRenderer`):

```json
{
    "success": false,
    "error": {
        "code": "validation_failed",
        "message": "Validation Failed",
        "violations": [
            {"field": "name", "code": "length", "message": "must be between 2 and 100 characters", "params": {"min": 2, "max": 100}},
            {"field": "email", "code": "email", "message": "must be a valid email address"},
            {"field": "age", "code": "min", "message": "must be at least 18", "params": {"min": 18}}
        ]
    }
}
```

An explicitly constructed `HTTPError` wins over validation errors wrapped inside it: `credo.NewHTTPError(409, "tenant_conflict").WithInternal(vErrs)` renders 409 `tenant_conflict`, not 422 `validation_failed`; only validation errors that no `HTTPError` wraps take the 422 above. See [ADR-009](../adr/009-handler-and-error-handling.md#an-explicit-status-wins).

### Error Types

```go
// validation/errors.go

// ValidationError represents a single field validation failure.
type ValidationError struct {
    Field      string         `json:"field"`
    Code       string         `json:"code"`
    Message    string         `json:"message"`
    MessageKey string         `json:"-"`              // optional exact presentation key
    Params     map[string]any `json:"params,omitempty"` // sent to clients
}

func (e *ValidationError) Error() string

// NewError builds a client-visible failure for a custom rule or a By function.
func NewError(code, message string) *ValidationError

// Errors is a collection of validation errors.
type Errors []ValidationError

func (e Errors) Error() string  // implements error interface
func (e Errors) MarshalJSON() ([]byte, error)
func (e Errors) MarshalJSONTo(enc *jsontext.Encoder) error
func (e Errors) Unwrap() []error
```

`Code` is the stable rule identity. `MessageKey`, when present, is an exact presentation key and is never serialized. `Params` provides template data and is sent to clients, so a custom rule never puts internal or sensitive values in it. `Errors.Unwrap` returns the individual failures, which lets `errors.As` find a single `*ValidationError` through any wrapping.

`Field` is the stable technical path on the wire, for frontend form-input matching: the field's `json` tag name, or the Go field name when the tag gives none; nested fields join with `.` and `Each` adds `[i]` (`address.city`, `items[0]`). Translation happens in the error pipeline, never in the validation engine, and never changes the wire `field`; see the i18n spec's [message-key resolution](i18n.md#message-key-resolution) and [field display names](i18n.md#validation-bind-and-fields).

## Design Decisions

Rationale for programmatic-only rules, generic `Rule[T]`, pointer field references, auto-validation through `Validatable`, topic-based grouping and the absence of `ValidateBody`/`ValidateQuery` on Route is in [ADR-011](../adr/011-validation-strategy.md).

- **i18n-ready `ValidationError`** — `Code` + `Params` fields enable translation without breaking changes. Default English messages work out of the box. Translation strategy: [ADR-013](../adr/013-internationalization.md).
- **No fluent builder** — `Rule[T]` is the substrate; helpers a fluent layer would make obsolete (such as a `RequiredField` shorthand) are not added ([ADR-011](../adr/011-validation-strategy.md#no-fluent-builder-deferred)).

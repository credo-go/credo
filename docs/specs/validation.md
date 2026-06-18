# Validation Spec

**Status**: Approved **Package**: `validation/` **Sources**: ozzo-validation (MIT, API design), Goyave (MIT, organization), govy (architecture inspiration only, no code adapted) **Depends on**: Root package (`Validatable` interface) **ADR**: [011-validation-strategy](../adr/011-validation-strategy.md)

---

## Overview

Credo uses **programmatic-only** validation with **generic type-safe rules** and compile-time safe pointer-based field references. No struct tags for rule definition. Reflection limited to field name extraction (cached). Integrated with Context via the "Parse, don't validate" pattern.

---

## Philosophy: Parse, Don't Validate

Validation is NOT a separate step. It is part of parsing:

```
Request → BindBody(&input) → [decode] → [validate] → guaranteed valid output
                                                    ↘ error → RFC 7807
```

The `Validatable` interface is the bridge between binding and validation:

```go
// Root package
type Validatable interface {
    Validate() error
}
```

When `ctx.Request().BindBody(&input)` is called and `input` implements `Validatable`, `Validate()` is called automatically after decoding. The handler only receives data that has already been validated.

---

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
    fieldName() string
}
```

`FieldRules` is a non-generic interface returned by `Field[T]`. It erases the type parameter so that `ValidateStruct` can accept fields of different types in a single variadic call.

### ValidateStruct + Field (ozzo-inspired, pointer-based field refs)

```go
// validation/rule.go
func ValidateStruct(structPtr any, fields ...FieldRules) error

func Field[T any](fieldPtr *T, rules ...Rule[T]) FieldRules
```

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
├── rule.go             Rule[T] interface, FieldRules, ValidateStruct, Field[T]
├── errors.go           ValidationError, Errors type, RFC 7807 serialization
├── string_rules.go     Required[T], Email, URL, UUID, Regex, Length
├── numeric_rules.go    Min, Max, Between
├── collection_rules.go NotEmptySlice, NotEmptyMap, Each, When, NilSafe
├── date_rules.go       Date, DateBefore, DateAfter
├── file_rules.go       MaxFileSize, AllowedMimeTypes, AllowedExtensions
├── common_rules.go     In, NotNil, By (inline custom)
└── doc.go
```

**Single package model**: All types, functions, and rules live in the `validation` package. There is no `validation/rule` sub-package. For concise examples, use an import alias:

```go
import v "github.com/credo-go/credo/validation"

v.ValidateStruct(c,
    v.Field(&c.Name, v.Required[string](), v.Length(2, 100)),
)
```

**Note**: `Required[T]` lives in `string_rules.go` because it is most commonly used with strings, but it is generic and works with any comparable type.

**Required vs NotEmpty**: `Required[T]`'s `comparable` constraint excludes slices and maps by language rules. `NotEmptySlice[E]()` and `NotEmptyMap[K, V]()` are their counterparts: they fail when the collection is nil or has length zero (error code `not_empty`). The check is `len`-based — no reflection, consistent with the zero-reflection rule execution policy.

### Custom Rules

```go
// Option 1: Implement Rule[T] interface
type CountryCode struct{}

func (r CountryCode) Validate(value string) error {
    if !isValidCountryCode(value) {
        return errors.New("invalid country code")
    }
    return nil
}

// Usage: validation.Field(&c.Country, validation.Required[string](), CountryCode{})

// Option 2: Inline with validation.By[T]()
validation.Field(&c.Code, validation.By(func(code string) error {
    if len(code) != 2 {
        return errors.New("must be a 2-letter code")
    }
    return nil
}))
```

Note: `validation.By` uses type inference — no explicit type parameter needed when the closure signature makes the type clear.

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
func (o *Order) Validate() error {
    return validation.ValidateStruct(o,
        validation.Field(&o.Email, validation.Required[string](), validation.Email()),
        validation.Field(&o.Quantity, validation.Required[int](), validation.Min(1)),
        // Cross-field business rule — pure, no I/O
        validation.Field(&o.EndDate,
            validation.When(o.StartDate != nil, validation.DateAfter(*o.StartDate)),
        ),
        validation.Field(&o.CardNumber,
            validation.When(o.PaymentMethod == "card", validation.Required[string]()),
        ),
    )
}

// Stateful checks belong in the service layer
func (s *OrderService) Create(ctx context.Context, input Order) error {
    if exists, _ := s.repo.EmailExists(ctx, input.Email); exists {
        return credo.NewHTTPError(409, "email already in use")
    }
    // ...
}
```

### Conditional Validation

```go
func (o *Order) Validate() error {
    return validation.ValidateStruct(o,
        validation.Field(&o.CardNumber,
            validation.When(o.PaymentMethod == "card",
                validation.Required[string](), validation.CreditCard(),
            ),
        ),
        validation.Field(&o.IBAN,
            validation.When(o.PaymentMethod == "bank",
                validation.Required[string](), validation.IBAN(),
            ),
        ),
    )
}
```

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

---

## Error Format — RFC 7807

Validation errors are classified by the framework's internal error pipeline and rendered as RFC 7807 Problem Details by the default renderer (or a custom `ErrorRenderer` if configured):

```json
{
    "type": "https://credo.dev/errors/validation",
    "title": "Validation Failed",
    "status": 422,
    "detail": "One or more fields failed validation.",
    "errors": [
        {"field": "name", "code": "length", "message": "must be between 2 and 100 characters", "params": {"min": 2, "max": 100}},
        {"field": "email", "code": "email", "message": "must be a valid email address"},
        {"field": "age", "code": "min", "message": "must be at least 18", "params": {"min": 18}}
    ]
}
```

### Error Types

```go
// validation/errors.go

// ValidationError represents a single field validation failure.
type ValidationError struct {
    Field   string         `json:"field"`           // Field path: "name", "address.city"
    Code    string         `json:"code"`            // Rule identifier / i18n key: "required", "email"
    Message string         `json:"message"`         // Default English message
    Params  map[string]any `json:"params,omitempty"` // Template params: {min: 2, max: 100}
}

// Errors is a collection of validation errors.
type Errors []ValidationError

func (e Errors) Error() string  // implements error interface
func (e Errors) MarshalJSON() ([]byte, error)
```

The `Code` field enables i18n translation in Phase 3 without changing the error type. `Params` provides template variables for localized messages (e.g., `"must be between {{min}} and {{max}} characters"`).

**Field names are NOT translated** — `Field` is a stable technical identifier for frontend form-input matching. See [ADR-013](../adr/013-internationalization.md).

**Translation trigger** — Translation is triggered automatically by the framework's internal error handling when i18n is configured (via `app.UseI18n()`). Error types implementing `TranslationKeyer` provide their own lookup key. Translation never happens inside the validation engine. See [ADR-013](../adr/013-internationalization.md).

---

## Design Decisions

1. **Programmatic only — no struct tags** — Struct tags for validation require reflection and are not compile-time safe. See [ADR-011](../adr/011-validation-strategy.md).

2. **Generic `Rule[T]` over non-generic `Rule`** — ozzo's `Rule.Validate(any)` loses type information. Generic rules catch type mismatches at compile time. See [ADR-011](../adr/011-validation-strategy.md).

3. **Pointer field refs (ozzo-style)** — `validation.Field(&c.Name, ...)` is familiar and compile-time safe for field existence. Reflection is used only for field name extraction and is cached per struct type.

4. **Validatable interface for auto-validation** — `Bind*` methods auto-call `Validate()` on target structs. Handlers never need a separate validation step.

5. **No `ValidateBody`/`ValidateQuery` on Route** — The Bind pattern handles this cleanly in the handler.

6. **Topic-based rule grouping** — Rules grouped by domain (`string_rules.go`, `numeric_rules.go`, etc.) rather than one file per rule. Reduces file count while keeping related rules together.

7. **i18n-ready `ValidationError`** — `Code` + `Params` fields enable translation without breaking changes. Default English messages work out of the box. Translation strategy: [ADR-013](../adr/013-internationalization.md).

8. **NilSafe for PATCH support** — `NilSafe[T]` wrapper provides first-class partial update support via pointer fields, without requiring separate rule sets for create vs update.

9. **Fluent API targeted at v1 (Go 1.27)** — the long-term direction is a fluent builder built on generic methods (golang/go#77273, Go 1.27). The current rule set is the substrate that any fluent shape will wrap, so v0 additions stay deliberately small and fully portable (`NotEmptySlice`/`NotEmptyMap` are `len`-based rules the fluent layer can reuse as-is). Convenience helpers that a fluent API would obsolete (e.g. a `RequiredField` shorthand) are intentionally not added — shipping an API that is born deprecated helps no one.

---

## Comparison with Alternatives

| Criteria | go-playground | ozzo | govy | Goyave | **Credo** |
| --- | --- | --- | --- | --- | --- |
| Rule definition | Tag strings | Programmatic | Programmatic | Programmatic | **Programmatic** |
| Field reference | Reflection | Pointer | Getter func | Path string | **Pointer** |
| Compile-time safe | No | Partial | Full | No | **Full (per field)** |
| Generics | No | No | Yes (core) | No | **Yes (Rule[T])** |
| Auto-validate | No | No | No | Route MW | **BindBody/Query** |
| Custom rules | Tag func | `Rule` interface | `Rule[T]` | Validator | **`Rule[T]`** |
| PATCH support | Awkward | Good (nil ptr) | OmitEmpty | Separate rules | **NilSafe[T]** |
| Error format | Custom | map[string]error | Structured | Nested JSON | **RFC 7807** |
| i18n | Translator | No | Templates | Built-in | **Code + Params** |
| Reflection | Heavy (cached) | Moderate | None | Minimal | **Minimal (cached)** |

---

## Implementation Phase

- **Phase 2.3**: Core validation engine (`validation/` package)
- **Phase 2.4**: RFC 7807 error integration with `ErrorRenderer`
- **Phase 1.3** (completed): `Validatable` interface + `BindBody`/`BindQuery` (Context methods, auto-call Validate)

# ADR-011: Validation Strategy

**Status:** Accepted
**Date:** 2026-03-01
**Depends on:** ADR-008

## Context

Validation is essential for enterprise applications (ADR-001). Go's
ecosystem offers two main approaches:

1. **Struct tags** (go-playground/validator): Declarative but limited —
   string-based rules, no type safety, complex rules require custom
   validators with reflection-heavy registration.

2. **Programmatic** (ozzo-validation): Code-based rules with pointer
   field references. More verbose but type-safe, composable, and
   testable.

Credo chooses programmatic validation to align with the explicit-first
philosophy (ADR-001) and to leverage Go generics for type-safe rules.

## Decision

### Programmatic Only — No Struct Tags

Validation rules are defined in code, not in struct tags. This is a
deliberate, permanent decision:

```go
// YES — programmatic
func (r *CreateUserRequest) Validate() error {
    return validation.ValidateStruct(r,
        validation.Field(&r.Name, validation.Required[string](), validation.Length(2, 100)),
        validation.Field(&r.Email, validation.Required[string](), validation.Email()),
        validation.Field(&r.Age, validation.Min(18), validation.Max(150)),
    )
}

// NO — no struct tag validation
type CreateUserRequest struct {
    Name  string `validate:"required,min=2,max=100"` // NOT supported
}
```

**Why no struct tags:**
- Struct tags are strings — no compile-time type checking
- Complex rules (cross-field, conditional) are awkward in tags
- Tags mix validation with serialization concerns
- Programmatic rules are testable as regular Go code

### Generic Rule[T] Interface

```go
type Rule[T any] interface {
    Validate(value T) error
}
```

Rules are type-parameterized. `Required[string]()` only accepts string
fields. `Min[int](18)` only accepts numeric fields. Type mismatches are
caught at compile time.

### Pointer Field References (ozzo-style)

```go
validation.Field(&r.Name, rules...)
```

`Field` takes a pointer to the struct field. This gives:
- Compile-time field existence checking
- Runtime field name extraction (via reflection, cached)
- No string-based field names that can drift from struct

### Validatable Interface

```go
type Validatable interface {
    Validate() error
}
```

Structs implementing `Validatable` are auto-validated by `BindBody()`
and `BindQuery()` after decoding. This is the "parse, don't validate"
integration point (ADR-008).

### Topic-Based Rule Grouping

Rules are organized by topic, not by implementation detail:

| File | Rules |
|------|-------|
| `common_rules.go` | `Required`, `NotNil`, `In`, `By` (custom) |
| `string_rules.go` | `Length`, `Email`, `URL`, `UUID`, `Regex` |
| `numeric_rules.go` | `Min`, `Max`, `Between` |
| `date_rules.go` | `DateBefore`, `DateAfter` |
| `collection_rules.go` | `Each`, `When`, `NilSafe` |

### Error Format

Validation errors return as `validation.Errors` (a slice of
`ValidationError`), each with field name, rule code, and message.
The framework's internal error handler classifies these as 422
Unprocessable Entity and passes them to the `ErrorRenderer` (or the
default RFC 7807 JSON renderer if none is configured) via `ErrorInfo`
(ADR-009).

### Rejected Alternatives

| Alternative | Reason |
|-------------|--------|
| Struct tags (go-playground/validator) | String-based, no type safety, reflection-heavy |
| govy library code | MPL-2.0 copyleft — incompatible with MIT |
| ValidateBody/ValidateQuery on Route | Couples validation to routing; "parse, don't validate" is cleaner |

## Consequences

**Positive:**
- Type-safe rules via generics — errors caught at compile time
- Composable — rules combine naturally (`When`, `Each`, `NilSafe`)
- Testable — rules are regular Go values, testable in isolation
- Auto-validation via `Validatable` — no manual validation calls needed
- RFC 7807 error output — machine-readable, consistent

**Negative:**
- More verbose than struct tags for simple cases
- Pointer field refs use reflection for name extraction (cached, cold path)
- No declarative overview of validation rules on the struct itself

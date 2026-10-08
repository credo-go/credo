# ADR-011: Validation Strategy

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-008 — rule errors are internal unless they are violations (v0.24.0)

## Context

Validation is essential for enterprise applications (ADR-001). Go's ecosystem offers two main approaches:

1. **Struct tags** (go-playground/validator): Declarative but limited — string-based rules, no type safety, complex rules require custom validators with reflection-heavy registration.

2. **Programmatic** (ozzo-validation): Code-based rules with pointer field references. More verbose but type-safe, composable, and testable.

Credo chooses programmatic validation to keep validation boundaries typed and reviewable (ADR-001) and to leverage Go generics for type-safe rules.

## Decision

### Programmatic Only — No Struct Tags

Validation rules are defined in code, not in struct tags. This is a deliberate, permanent decision:

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

Rules are type-parameterized. `Required[string]()` only accepts string fields. `Min[int](18)` only accepts numeric fields. Type mismatches are caught at compile time.

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

Structs implementing `Validatable` are auto-validated by `BindBody()` and `BindQuery()` after decoding. This is the "parse, don't validate" integration point (ADR-008).

### Topic-Based Rule Grouping

Rules are organized by topic, not by implementation detail:

| File                  | Rules                                     |
| --------------------- | ----------------------------------------- |
| `common_rules.go`     | `Required`, `NotNil`, `In`, `By` (custom) |
| `string_rules.go`     | `Length`, `Email`, `URL`, `UUID`, `Regex` |
| `numeric_rules.go`    | `Min`, `Max`, `Between`                   |
| `date_rules.go`       | `DateBefore`, `DateAfter`                 |
| `collection_rules.go` | `Each`, `When`, `NilSafe`                 |

### Error Format

Validation errors return as `validation.Errors` (a slice of `ValidationError`), each with field name, rule code, message, optional exact message key, and params. The internal handler classifies these as 422 and passes normalized data to `ErrorRenderer`, or writes the default Credo envelope (ADR-009).

### Rule Errors: Client Messages and Internal Failures

A rule error has two possible meanings: a message meant for the client ("must be a 2-letter code") or a failure the client did not cause (a lookup inside a rule that could not reach its database). One channel cannot carry both: converting every error into an `invalid` violation that carries the error's text puts internal text, such as `pq: connection to 10.1.2.3:5432 refused`, into a 422 body, where it reaches the client unless an i18n catalog happens to translate the generic code. The type of the returned error therefore decides its meaning:

- **Client-visible.** A rule that wants a client-visible message returns a `*ValidationError` (or a `validation.Errors`), or builds one with the small constructor `validation.NewError(code, message string) *ValidationError`. It becomes a violation of the field the rule validates and renders as 422 `validation_failed`. `By`'s documented example returns one.
- **Internal.** Any other error is internal. Validation stops — `ValidateStruct` returns that error without collecting further violations, and nested `Validate` calls, `Each`, `When` and `NilSafe` pass it on unchanged — and the error leaves `Validate`, and `BindBody`/`BindQuery`, as it would leave a handler. The error pipeline classifies it like any other handler error (ADR-009): a plain error is a 500 whose text is logged, not rendered, and an error that carries a fault keeps its mapped status, so `store`'s unavailable kind is a 503.

This keeps the stateless boundary honest without policing it: a rule that does I/O anyway and fails reports a server failure, not invalid input.

Rejected: rendering such an error as a generic 422 violation with a fixed, safe message. It hides the text but still tells the client that its input was wrong when the server failed, and a client that trusts the status does not retry a request that would succeed later.

Together with the precedence of an explicit `HTTPError` over a wrapped validation error ([ADR-009](009-handler-and-error-handling.md)), this is one of the error model's final rules: later changes to the error model keep it.

### Rejected Alternatives

| Alternative | Reason |
| --- | --- |
| Struct tags (go-playground/validator) | String-based, no type safety, reflection-heavy |
| govy library code | MPL-2.0 copyleft — incompatible with MIT |
| ValidateBody/ValidateQuery on Route | Couples validation to routing; "parse, don't validate" is cleaner |

## Consequences

**Positive:**

- Type-safe rules via generics — errors caught at compile time
- Composable — rules combine naturally (`When`, `Each`, `NilSafe`)
- Testable — rules are regular Go values, testable in isolation
- Auto-validation via `Validatable` — no manual validation calls needed
- Stable machine-readable default envelope; RFC 9457 remains opt-in

**Negative:**

- More verbose than struct tags for simple cases
- Pointer field refs use reflection for name extraction (cached, cold path)
- No declarative overview of validation rules on the struct itself

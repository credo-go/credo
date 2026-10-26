# ADR-011: Validation Strategy

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-008, ADR-009 **Related:** ADR-001, ADR-002, ADR-013 **Specification:** [Validation spec](../specs/validation.md) **Guide:** [Getting started](../guides/getting-started.md#validation), [Error handling guide](../guides/error-handling.md#validation-and-bind-failures)

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

Rules are generic over the field type: a rule is a `Rule[T]` whose single method validates a `T` ([API surface](../specs/validation.md#api-surface)). `Required[string]()` only accepts a string field, and `Min(18)` yields a `Rule[int]` that only an int field accepts, so a type mismatch is a compile error. Unlike ozzo's `Rule.Validate(any)`, which loses the type, rule execution needs neither a type assertion nor reflection.

### Pointer Field References (ozzo-style)

```go
validation.Field(&r.Name, rules...)
```

`Field` takes a pointer to the struct field. This gives:

- Compile-time field existence checking
- Runtime field name extraction (via reflection, cached)
- No string-based field names that can drift from struct

### Validatable Interface

A type that implements `validation.Validatable` (`Validate() error`) is validated by `BindBody()` and `BindQuery()` right after decoding. This is the "parse, don't validate" integration point ([ADR-008](008-context-design.md#parse-dont-validate)). The interface lives in package `validation`, which depends only on the standard library; the root package depends on it, not the reverse.

### Topic-Based Rule Grouping

Rules are grouped by topic (string, numeric, date, collection, file, common), one file per topic rather than one per rule, following Goyave's organization; the inventory is in the [validation spec](../specs/validation.md#built-in-rules-topic-based-grouping).

### Error Format

Validation errors return as `validation.Errors` (a slice of `ValidationError`), each with field name, rule code, message, optional exact message key, and params. The internal handler classifies these as 422 and passes normalized data to `ErrorRenderer`, or writes the [default error envelope](009-handler-and-error-handling.md#default-error-envelope). `Code` and `Params` let the error pipeline translate a message without changing the wire shape ([ADR-013](013-internationalization.md)).

### Rule Errors: Client Messages and Internal Failures

A rule error has two possible meanings: a message meant for the client ("must be a 2-letter code") or a failure the client did not cause (a lookup inside a rule that could not reach its database). One channel cannot carry both: treating every rule error as a violation would put internal text, such as `pq: connection to 10.1.2.3:5432 refused`, into a 422 body sent to the client. The type of the returned error therefore decides its meaning:

- **Client-visible.** A `*ValidationError` or a `validation.Errors` — `validation.NewError(code, message)` builds one — is a violation of the field the rule validates and renders as 422 `validation_failed`.
- **Internal.** Any other error stops validation and leaves `Validate`, and `BindBody`/`BindQuery`, unchanged, as it would leave a handler. The error pipeline classifies it like any other handler error ([ADR-009](009-handler-and-error-handling.md)): a plain error is a 500 whose text is logged, not rendered, and an error that carries a fault keeps its mapped status, so `store`'s unavailable kind is a 503.

The [validation spec](../specs/validation.md#rule-errors) states the contract at every depth. This keeps the stateless boundary honest without policing it: a rule that does I/O anyway and fails reports a server failure, not invalid input.

Rejected: rendering such an error as a generic 422 violation with a fixed, safe message. It hides the text but still tells the client that its input was wrong when the server failed, and a client that trusts the status does not retry a request that would succeed later.

Together with the precedence of an explicit `HTTPError` over a wrapped validation error ([ADR-009](009-handler-and-error-handling.md#an-explicit-status-wins)), this rule is final: later changes to the error model keep it.

### No Fluent Builder (Deferred)

A fluent builder over generic methods (available since Go 1.27, [golang/go#77273](https://github.com/golang/go/issues/77273)) is deliberately deferred. `Rule[T]` is the substrate any fluent layer would wrap, so the rule set grows only by small, portable rules (`NotEmptySlice`/`NotEmptyMap` are `len`-based and reusable as-is). Convenience helpers that a fluent API would make obsolete — a `RequiredField` shorthand, for example — are not added: an API born deprecated helps no one.

## Rejected Alternatives

The libraries considered compare as follows:

| Criteria | go-playground | ozzo | govy | Goyave | **Credo** |
| --- | --- | --- | --- | --- | --- |
| Rule definition | Tag strings | Programmatic | Programmatic | Programmatic | **Programmatic** |
| Field reference | Reflection | Pointer | Getter func | Path string | **Pointer** |
| Compile-time safe | No | Partial | Full | No | **Full (per field)** |
| Generics | No | No | Yes (core) | No | **Yes (Rule[T])** |
| Auto-validate | No | No | No | Route MW | **BindBody/Query** |
| Custom rules | Tag func | `Rule` interface | `Rule[T]` | Validator | **`Rule[T]`** |
| PATCH support | Awkward | Good (nil ptr) | OmitEmpty | Separate rules | **NilSafe[T]** |
| Error format | Custom | map[string]error | Structured | Nested JSON | **Default error envelope; RFC 9457 opt-in** |
| i18n | Translator | No | Templates | Built-in | **Code + Params** |
| Reflection | Heavy (cached) | Moderate | None | Minimal | **Minimal (cached)** |

| Alternative | Reason |
| --- | --- |
| Struct tags (go-playground/validator) | See [Why no struct tags](#programmatic-only--no-struct-tags) |
| govy library code | MPL-2.0 (file-level copyleft): adapted files would stay MPL, outside the MIT/BSD/ISC/Apache-2.0 adaptation policy ([ADR-002](002-code-acquisition-strategy.md)); govy informs the design only |
| ValidateBody/ValidateQuery on Route | Couples validation to routing; "parse, don't validate" is cleaner |

## Consequences

**Positive:**

- Type-safe rules via generics — errors caught at compile time
- Testable — rules are regular Go values, testable in isolation
- Stable machine-readable default envelope; RFC 9457 remains opt-in

**Negative:**

- More verbose than struct tags for simple cases
- Pointer field refs use reflection for name extraction (cached, cold path)
- No declarative overview of validation rules on the struct itself

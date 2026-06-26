# ADR-004: Dependency Injection & credo.Infra

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001, ADR-003

## Context

Dependency injection is a fundamental need for enterprise applications (ADR-001, ADR-003). Credo's DI mechanism must address two distinct needs:

1. **Business dependencies** (DB, repo, service): Each service requires different combinations. Must be explicit, mockable, and type-safe.

2. **Infrastructure dependencies** (Logger, Metrics, Tracer): Nearly every service needs them. Passing 3-4 infra parameters to every constructor is verbose, but implicit injection (auto-populate) in Go conflicts with Go's philosophy.

### Why Not Implicit Base?

An embeddable `Base` struct auto-populated via reflection (similar to Spring's `@Autowired`) may seem attractive at first glance, but it has serious problems in Go:

- **Implicit**: Not visible in the constructor signature — it's unclear what is being injected
- **Reflection**: Requires scanning struct fields via reflect
- **God object tendency**: Logger + Config + Metrics + Tracer all in one struct
- **Testing difficulty**: Requires special mechanisms for mocking
- **Conflicts with Go philosophy**: Not idiomatic in Go

## Decision

### Container: Generics-Based DI

The DI container implementation lives in the `internal/di` package. Type-safe generic functions are exposed through the root package:

```go
app.Provide[T](constructor)  // Register
app.Resolve[T]()               // Resolve
```

The container is a Credo-specific component — it is not intended for standalone use as an independent DI library. For this reason, `internal/di` is preferred over a public `container/` package.

- **Lifecycle**: Singleton
- Reflection is used at registration time (constructor inspection) and once per singleton during first construction (`reflect.Value.Call`). Subsequent resolves are pure cache lookups — zero reflection.

### Interface Alias

Interface alias via `Alias[I, T]()` creates an alias so `Resolve[I]` returns T's singleton. Contract: I is an interface, T implements I, and T is already registered via `Provide`.

```go
app.Provide[*UserRepo](NewUserRepo)
app.Alias[UserRepository, *UserRepo]()  // Resolve[UserRepository] returns *UserRepo
```

### Ordered Interface Collections

Some application components need an ordered set of implementations rather than one default implementation: notification senders, hooks, subscribers, policy evaluators, or plugin chains.

Credo supports this via `BindMany[I, T]()` and `ResolveAll[I]()`:

```go
app.Provide[*EmailSender](NewEmailSender)
app.Provide[*InAppSender](NewInAppSender)

app.BindMany[Sender, *EmailSender]()
app.BindMany[Sender, *InAppSender]()

senders := app.MustResolveAll[Sender]()
```

The same ordered collection is also injectable via constructor parameters of type `[]I`:

```go
func NewSenderRegistry(senders []Sender) *SenderRegistry {
    return NewSenderRegistryWithSenders(senders...)
}
```

Rules:

1. `I` must be an interface type
2. `T` must already be registered via `Provide` or `ProvideValue`
3. `T` must be a concrete type and implement `I`
4. Binding order is preserved
5. `ResolveAll[I]` and `[]I` injection return an empty slice when no bindings exist
6. `Alias` and `BindMany` are independent; one does not imply the other

### Finalize Phase

`app.Finalize()` freezes the container and validates the dependency graph. After Finalize, `Provide`, `ProvideFactory`, `ProvideValue`, `Replace`, `Alias`, and `BindMany` calls are rejected. `Run()` and `RunContext()` call Finalize implicitly. `Resolve` is allowed both before and after Finalize (bootstrap phase supports `Resolve`-if-missing-`Provide` patterns). Credo's recommended usage keeps `Resolve` in bootstrap/composition-root code; runtime `Resolve` remains available but is not the preferred application pattern. After a failed Finalize, `Resolve` returns the error.

### credo.Infra: Explicit Infrastructure Carrier

`credo.Infra` is a fixed struct defined by the framework. It carries framework-managed infrastructure. Today that is the service-scoped Logger; the observability release (Phase 3.5, aligned with the v1 / Go 1.27 window) extends the same carrier with metrics and tracing, designed against real OpenTelemetry and Prometheus adapters rather than speculative placeholders:

```go
// Defined by the framework, not extensible by the user.
type Infra struct {
    _ struct{} // forces keyed literals so new fields (metrics, tracing) land compatibly

    Logger *slog.Logger
}
```

The `_ struct{}` keyed-literal guard is deliberate: it lets Phase 3.5 add the metrics and tracing fields without breaking existing `credo.Infra{Logger: ...}` construction sites.

When the container sees the `credo.Infra` type as a constructor parameter, it runs a special code path:

1. Resolves the Logger from the container (or uses the framework default)
2. Scopes the Logger with a `service=<name>` attribute
3. Places the produced `Infra` value into the parameter

```go
// Model 1: Infra as first parameter in the constructor (convention)
func NewUserService(infra credo.Infra, repo UserRepo) *UserService {
    infra.Logger.Info("user service initialized")
    return &UserService{infra: infra, repo: repo}
}
```

### Container Detection Logic

The container automatically determines which injection model is being used by inspecting the constructor signature:

1. If the first parameter type is `credo.Infra` -> **Model 1**: Produce Infra specially, resolve remaining parameters normally
2. Otherwise -> **Pure constructor injection**: All parameters resolved normally (no Infra magic)

The developer chooses on a per-service basis.

### Infra Design Decisions

| Decision | Rationale |
| --- | --- |
| **Fixed struct, not extensible** | Fields are known, no field-scan/tag needed |
| **Always available** | Like `context.Context` — no need to register, container knows how to produce it |
| **Default fallback** | If no Logger is registered, the framework default logger is used — no panic |
| **Scoped Logger** | Each service gets a logger scoped with its own name |
| **First parameter convention** | Like Go's `context.Context` convention, Infra is always the first parameter |
| **Reflection constrained to cold path** | Constructor inspection at registration + `reflect.Call` once per singleton first construction; subsequent resolves are cache lookups |
| **Config not included** | Config is a separate concern — distributed via DI as typed struct (ADR-005) |
| **Immutable** | Cannot be changed after production — snapshot semantics |

### Considered and Rejected

| Alternative | Reason for rejection |
| --- | --- |
| Implicit Base (auto-populate) | Conflicts with Go philosophy, reflection-based field population, implicit |
| Container as parameter (service locator) | Dependencies not visible in signature, unclear what to mock in tests |
| Struct tag injection (`credo:"inject"`) | Tag typos not caught at compile time, visual noise, field-scan reflection |
| Setter injection (`SetLogger`, `SetTracer`) | Object returned from constructor is half-initialized, lifecycle problem, implicit |
| Pure constructor params (each infra separate) | 6-7 parameters are verbose, Infra consolidates them into a single parameter |
| Container as separate public package | No standalone usage scenario, Credo-specific — internal is sufficient |
| RequestScoped lifecycle | Go's `context.Context` + middleware pattern provides sufficient request-scoped dependency management without DI container complexity |
| Model 3: Hybrid Embed (struct with embedded `credo.Infra` + resolved fields) | Reflective field population hides application boundaries. Model 1 with visible constructor parameters is clearer and sufficient |

## Consequences

**Positive:**

- Every dependency is visible in the constructor signature — explicit, reviewable
- `credo.Infra` consolidates infra boilerplate into a single parameter — not verbose
- Reflection is limited to constructor/infra metadata inspection; resolve hot path uses cached mappings
- Easy to mock in tests — provide your own Infra struct
- Scoped logger is automatic — each service logs with its own name
- Always available — no registration dependency
- Immutable — snapshot semantics, no race conditions
- `Alias[I, T]()` enables programming to interfaces without duplicate registrations
- `BindMany[I, T]()` / `ResolveAll[I]` support ordered plugin-style composition without manual registry bootstrapping
- `Finalize()` catches dependency graph errors at startup, not at first request

**Negative:**

- `credo.Infra` parameter must be added to every constructor (minimal boilerplate)
- Infra is not extensible — adding a new infra type requires a framework change
- Special code path in container for `credo.Infra` — but it's a simple type switch
- `BindMany` adds ordering and empty-collection semantics that must be documented clearly
- Container is in `internal/di` — cannot be used as a standalone DI library

## ProvideFactory

`Provide`'s `constructor` parameter is necessarily `any` — Go cannot express "a function with arbitrary parameters returning `T`" in the type system — so signature mistakes surface as registration-time errors, not compile errors. `ProvideFactory[T](app, fn func(*App) (T, error))` is the fully compiler-checked factory alternative: `fn`'s signature is enforced (with `T` inferred), and `fn` resolves its own dependencies via `Resolve` inside the closure. The factory name is intentional: the container cannot inspect its dependency graph. The trade-off is that `fn` is opaque to the container: its dependencies do not participate in `Finalize` graph validation or cycle detection, and `credo.Infra` is not auto-injected (`app.NewInfra` replaces Model 1 inside `fn`). Plain `Provide` with a named constructor remains the recommended default. See `docs/specs/container.md` for details.

# ADR-004: Dependency Injection & credo.Infra

**Status:** Accepted **Date:** 2026-03-01 **Amended:** 2026-09-05 by [ADR-022](022-bootstrap-and-di-ownership.md) (phase-gated resolution, adoption, replacement ownership, dependency-ordered teardown) **Depends on:** ADR-001, ADR-003

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

### ProvideValue Preflight and Protected Bindings

`app.CanProvideValue[T]() error` performs the same local frozen-container and direct duplicate-`T` checks that `ProvideValue[T]` applies, without registering or reserving anything. It exists for composition helpers such as `store.Register`, which should reject predictable local conflicts before doing network I/O.

The result is only a point-in-time observation. Another goroutine may register `T` or finalize the container immediately afterward, so a nil result is not a promise that a later regular or protected value publication will succeed. The final `ProvideValue` or `ProvideProtectedValue` call remains authoritative and callers must handle its error.

Two low-level methods support integrations whose DI binding is coupled to external lifecycle, health, or registration state:

- `app.ProvideProtectedValue[T](value)` registers a pre-built singleton and marks its direct binding as protected from `app.Replace[T]`.
- `app.ProtectBinding[T](expected ...T)` protects an existing direct binding. With no expected value it is idempotent and does not resolve T. With one expected value it performs a CAS-style compare-and-protect: under the same lock used by `Replace`, it verifies that the already-resolved singleton is comparable and still equal to expected before protecting the binding. An unresolved, non-comparable, or changed value returns an error without adding protection. More than one expected value is rejected. Both forms require T to be registered and must run before Finalize.

`Replace[T]` returns an error for a protected binding. This prevents an integration from continuing to monitor or shut down one value while DI starts resolving another. Protection affects binding replacement only; it does not by itself create lifecycle ownership, health wiring, aliases, or collection bindings. Normal application and test bindings should continue to use override-friendly `ProvideValue`; the protected variants are intended for framework/integration registration paths such as `store.Register`.

### Registration-Time Reads and Replacement Ownership

Constructors run only after Finalize, so the registration phase has no general `Resolve`. Two observation/adoption operations cover what integrations need:

- `app.Has[T]() bool` reports registration presence (direct or alias) without constructing, adopting or protecting. It is a snapshot, not a reservation.
- `app.AdoptValue[T](validate func(T) error) (T, error)` reads an existing pre-built binding, validates it and atomically compare-and-protects that same binding. Protection is a consequence of successful validation, never of the read, so a rejected value (a typed-nil Registry) stays repairable through `Replace`. A replacement or phase change during validation makes the adoption fail rather than protect a stale instance. A constructor binding is rejected with an explanatory error and is never invoked; `store.Register` and `worker.Register` rely on this to refuse a Registry or Pool registered through `Provide`.

`Replace[T](value) (old T, existed bool, err error)` transfers ownership explicitly: on success the container owns the new value and returns any already-created superseded instance to the caller, who assumes its cleanup responsibility. `existed` means exactly "a previously created instance existed": a superseded constructor binding that never ran yields zero and false, and Replace never constructs an old provider merely to return it. A superseded `Shutdowner` is logged at Warn as a reminder, not as the transfer mechanism. A rejected replacement changes neither the binding nor ownership. `MustReplace` returns the same information and panics on error.

### Finalize Phase

`app.Finalize()` freezes the container and validates the dependency graph. After Finalize, `Provide`, `ProvideValue`, `ProvideProtectedValue`, `ProtectBinding`, `AdoptValue`, `Replace`, `Alias`, and `BindMany` calls are rejected. `Resolve` is admitted only after Finalize: constructors never run during registration, so the composition root finishes every DI write (store/worker registration included), calls Finalize and handles its error, and only then resolves controllers and binds routes. `Run()`, `RunContext()`, `ServeContext()` and the first direct `ServeHTTP` call Finalize implicitly as an idempotent safeguard, which cannot precede a Resolve the composition root has already executed. Finalize is DI-only; HTTP registration stays open until the App prepares to serve (ADR-006). Credo's recommended usage keeps `Resolve` in bootstrap/composition-root code; runtime `Resolve` remains available but is not the preferred application pattern, and lifecycle hooks capture their dependencies instead of resolving. After a failed Finalize, `Resolve` returns the error.

Construction completes once per singleton with a terminal result — value, error or panic — shared by every waiter; nothing is retried. A constructor panic is returned as `*credo.DIPanicError` (type, phase, original value, stack) and `MustResolve` panics with that error as its payload. Once shutdown reaches DI teardown the container is closing: new resolution, cached results included, returns an error wrapping `credo.ErrDIClosed`, while instances created by already-admitted builds remain owned and cleaned up by the container.

### Shutdown

`Container.Shutdown(ctx)` tears down live singletons in dependency order — consumers before the singletons they were constructed from — with a Kahn ready queue over the static graph (constructor parameters, aliases, collection edges; value bindings are vertices, dependencies hidden inside pre-built values are not) and reverse registration order as the tie-break. Each `Shutdowner` gets at most one sequential attempt bounded by the shared context through a helper goroutine; a hung callback keeps its dependencies blocked and is reported, never retried or closed around. Only construction completing after the context ended receives one separate fixed five-second best-effort attempt. Failure or incompleteness returns `*credo.DIShutdownError`, an immutable per-vertex snapshot that unwraps its causes; shutdown panics are isolated as `DIPanicError`. The [container spec](../specs/container.md#shutdown) and the [bootstrap contract](../specs/bootstrap-and-di-lifecycle.md) carry the full rules.

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

1. If any parameter is of type `credo.Infra` -> **Model 1**: the container produces that parameter (a scoped Infra) and resolves the rest normally. Placing it first is the recommended convention (above), but detection is position-independent.
2. Otherwise -> **Pure constructor injection**: All parameters resolved normally (no Infra magic)

The developer chooses on a per-service basis.

### Infra Design Decisions

| Decision | Rationale |
| --- | --- |
| **Fixed struct, not extensible** | Fields are known, no field-scan/tag needed |
| **Always available** | Like `context.Context` — no need to register, container knows how to produce it |
| **Default fallback** | If no Logger is registered, the framework default logger is used — no panic |
| **Scoped Logger** | Each service gets a logger scoped with its own name |
| **First parameter convention** | Like Go's `context.Context` convention, Infra is placed first by convention; the container still detects it at any parameter position |
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
| Closure factory (`fn func(*App) (T, error)` resolving its own dependencies) | Compiler-checked signature, but the dependencies resolved inside the closure are invisible to Finalize validation, cycle detection and dependency-ordered shutdown; typed constructors keep the graph static. Capturing `app.Resolve` inside a constructor is unsupported for the same reason |
| General early Resolve/Peek during registration; protect-on-read | Would run constructors against an unvalidated graph or freeze an invalid binding before validation. `AdoptValue` (validate, then atomically protect) and `Has` cover integration needs |
| Reverse registration order for shutdown | Ignores the dependency graph: a service registered before its DB closed after it. Dependency order with reverse-registration tie-break keeps the old order where the graph does not decide |

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
- `Finalize()` catches dependency graph errors at startup, not at first request, and is the only gate before any constructor runs
- Shutdown follows observable dependencies; cancellation bounds waiting and the report says what did not complete and why
- Replacement ownership is explicit: the caller receives the superseded instance instead of leaking it

**Negative:**

- `credo.Infra` parameter must be added to every constructor (minimal boilerplate)
- Infra is not extensible — adding a new infra type requires a framework change
- Special code path in container for `credo.Infra` — but it's a simple type switch
- `BindMany` adds ordering and empty-collection semantics that must be documented clearly
- Container is in `internal/di` — cannot be used as a standalone DI library
- The composition root needs an explicit, error-checked `Finalize` between registration and the first `Resolve`

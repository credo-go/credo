# ADR-016: Health Checks

**Status:** Accepted; a component that reports `degraded` itself deferred **Date:** 2026-03-07 **Depends on:** ADR-006, ADR-015, ADR-024 **Related:** ADR-004, ADR-010, ADR-023 **Specification:** [Health spec](../specs/health.md) **Guide:** [Getting started: Health Checks](../guides/getting-started.md#health-checks), [Deployment guide](../guides/deployment.md#shutdown-and-readiness)

## Context

Enterprise applications deployed to Kubernetes (and similar orchestrators) require liveness and readiness probes. Liveness probes detect deadlocked or unrecoverable processes; readiness probes gate traffic until all dependencies are available.

Stores report typed connection health; the health system must include the databases an application binds, without resolving anything from the DI container per request, while remaining usable for applications that have none. Probes are polled often, by several callers at once, on endpoints that are usually unauthenticated: a hung dependency must not hold a probe past its deadline or accumulate a goroutine per request, diagnostic text must not leak connection targets, and probe traffic must not flood the access log. A draining instance must leave rotation before its drain without being killed for it.

## Decision

### Engine

Credo writes the health engine itself. The engine lives in `internal/health` (`Engine`, `Probe`, name validation); the root package owns the endpoints, the public registration API and the response and logging policy — the same split as i18n (`internal/i18n` + `app.UseI18n`).

Each check owns a stable `Probe`: overlapping requests join one in-flight run instead of starting a callback per request, the per-check deadline is enforced even for a callback that ignores cancellation (at most one blocked callback is retained per check), and panics are isolated per check, so one check cannot abort its siblings or the readiness handler. Application and component checks run concurrently through this one runner. With no checks registered liveness is up: a responding server proves the process alive. The mechanics are in the [health spec](../specs/health.md#check-execution).

Rejected: adapting alexliesenfeld/health. Its API surface — interceptors, middleware chains, asynchronous checkers with caching — is much larger than Credo needs; Credo needs a small bounded runner, and writing it from scratch carries no attribution overhead.

### Public API (root package)

`app.UseHealth(cfg...)` mounts both probes with zero configuration, and `AddLivenessCheck`/`AddReadinessCheck` add the application's named checks; the signatures are in the [health spec](../specs/health.md#usehealth-and-healthconfig).

**Checks are collected; `UseHealth` mounts.** A check is accepted whenever it is registered — before `UseHealth`, after it, or on an App that never calls it — and an endpoint runs whatever is registered when a request arrives. Group middleware, route meta, the routed tier and DI bindings are collected and resolved later, so the order of two registrations never decides whether one of them is valid; health follows the same rule. A module can therefore add its readiness check without knowing whether the composition root has mounted the endpoints yet. An App that has checks and never calls `UseHealth` starts, and its start logs one warning naming their count: the checks are harmless, and a missing mount is more likely an omission than a decision. Rejected:

- **A panic for a check registered before `UseHealth`:** it makes a registration's validity depend on call order where the order carries no meaning, and it turns a module's check into a constraint on where the composition root calls `UseHealth`.
- **A failed start for an App that has checks and no `UseHealth`:** a module's check would then force every application that uses the module to mount health endpoints.

**A check is a function.** `AddLivenessCheck` and `AddReadinessCheck` take a `func(context.Context) error`, the contract of a component's `Readier.Ready`; a value with a `Check` method is passed as `v.Check`. Rejected: a `HealthChecker` interface with a `HealthCheckFunc` adapter — the one-method interface names a second time what `Ready` already is, and the adapter exists only to turn the function a caller has into that interface, so the common call carries a conversion that says nothing.

There is no store bridge: a bound database is a component, and its `Ready` is its readiness entry ([Readiness from the Kernel's Registries](#readiness-from-the-kernels-registries)). `AddReadinessCheck(name, check, opts...)` accepts `credo.OptionalReadiness()`, its one option, so a check of a cache or of a third-party API is reported without gating readiness, as an optional binding's entry is ([Optional Readiness](#optional-readiness)); any other option panics, naming it and the one accepted. The variadic parameter keeps every existing call compiling. `AddLivenessCheck` takes no option: liveness decides a restart, and a check that cannot fail it decides nothing.

### HealthConfig

- Errors are masked by default: a failing entry reports only its status, and its cause goes to the application log. Causes often contain hostnames and connection targets that must not reach an unauthenticated probe endpoint; `ExposeErrors` opts in, for a network-restricted endpoint only.
- Probe traffic is silent in the access log by default: orchestrators poll the probes, often once per second per replica. `LogRequests` re-enables the records, overriding a group that silenced access logging ([ADR-010](010-middleware-architecture.md)).
- The probes can be mounted on a `Group`, so the group's prefix and middleware, an IP restriction for example, apply to them.
- The three switches are plain bools named for what they turn off — `Disabled`, `DisableLiveness`, `DisableReadiness` — as `RestartPolicy.Disabled` and `RecoverConfig.DisableStackTrace` spell a default-on switch. The zero value is the default, so a zero `HealthConfig` mounts both endpoints, and an inverted bool tells "not set" from an explicit choice with no third state. Rejected: `*bool` fields `Enabled`, `Liveness` and `Readiness` with nil meaning true — the pointer buys the same distinction at the price of `new(false)` or a helper at every call site, and of a field whose zero value does not read as its default.
- A `Group` of another App is refused before the switches are read, so the mistake shows in every deployment and not only in the one that turns health on.

The fields and their defaults are in the [health spec](../specs/health.md#usehealth-and-healthconfig).

### Response Format

Liveness answers its status only; readiness reports each entry with its status, `up` or `down`, and its `latency`. The top-level readiness status comes from the gating entries: `up`, answered with 200, when every entry is up; `degraded`, answered with 200, when every gating entry is up and an [optional](#optional-readiness) one is not; `down`, answered with 503, when a gating entry is down. During shutdown readiness answers 503 `shutting_down` whatever its entries show. The bodies are in the [health spec](../specs/health.md#response-format).

Every readiness entry carries its latency — a component's, a database's and an application check's alike — measured from the start of the check's run to its result, and the failure log carries it too. Rejected: latency on database entries only — a slow dependency is what a probe dashboard looks for first, whatever its kind.

### Graceful Shutdown

When shutdown begins, `/ready` answers 503 `shutting_down` at once — before in-flight requests drain — so load balancers stop routing to the instance. Liveness (`/health`) keeps answering: the process is alive and draining, and must not be killed mid-drain. The sequence is in the [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers).

### Readiness from the Kernel's Registries

`/ready` reads two sources the App itself holds — the components the start walk built and the application's checks — and resolves nothing from the DI container per request. A bound database is one of the components: its binding's type shows `Ready`, so its readiness entry is planned like any other component's, under the component's name (`*sqldb.DB`, or its `credo.ComponentName` name). Each probe is built once: a component's when the App enters running, from the value the start walk built (after every override, so an override is the value reported), an application check's at its registration. Registration order between `UseHealth`, the checks and the components therefore does not matter. A worker reports through its component's `Ready` like any other component ([ADR-023](023-worker-system.md)).

A resource has one readiness entry, however many registrations hold it: a raw `*sqldb.DB` and a wrapper that embeds it, bound together, give one entry — its holder of record's ([ADR-024](024-lifecycle-components.md)) — whose `Ready` is asked, and their registrations agree on `credo.OptionalReadiness()` and on a `credo.ComponentName` name. A wrapper with readiness of its own holds the handle in a named field. Rejected: an entry per binding of one resource — no documented wrapper overrides the `Ready` it embeds, and two entries ping one database twice on every probe and leave `credo.OptionalReadiness()` on one of them without effect.

Readiness fails closed: a name that a component and an application check both report produces a down entry and 503 instead of one result silently overwriting the other, and cause text is captured inside the probe's bounded run. There is no status allowlist or result normalization to apply: a component's `Ready` and an application check return an error or nil.

Rejected: resolving a readiness seam from the container on each request — the service-locator access [ADR-004](004-dependency-injection-and-infra.md) rejects for applications; it would also have to treat every resolution error as "no entries", so a broken seam would make dependencies silently vanish from readiness instead of failing.

### Optional Readiness

The optional/critical decision is taken for readiness. Whether a dependency gates readiness is a fact only the application knows, so it is a registration option, the sibling of `credo.Borrowed()`: `credo.OptionalReadiness()` reports an entry in `/ready` without letting it gate readiness. It applies to a `Provide`, `ProvideValue` or `Manage` registration that plans a readiness entry — its type shows `Ready`, and it is a component or borrowed — and to a check added with `AddReadinessCheck`, whose one option it is; it panics on a registration that plans no entry. An optional entry's `Ready` is still asked under the same deadline, the entry shows its own status — `down` when its check fails — and its latency, and a failure is logged like any failing check. While every gating entry is up and an optional one is not, the top-level status is `degraded`, answered with 200.

The option changes readiness only. `Start` and `Shutdown` are unchanged, so an unreachable database whose readiness is optional still fails the start. A database whose outage must not stop the start is opened with `sqldb.WithOptionalStart()`, an option of the module, which alone knows that its pool is usable after a failed ping ([ADR-015](015-data-access.md#a-database-is-a-component)).

**Gating stays the default, and it has a cost.** A database's `Ready` is its ping, which goes through its pool: with `MaxOpenConns` set, as the pool warning asks, a pool whose connections are all busy holds the ping in its queue until the check's deadline, and a process that is merely busy leaves rotation and moves its load to the others. The pool's size and `credo.OptionalReadiness()` are the answers. The default is still gating, because a process that has lost its database while the others have not should leave rotation.

**Deferred:**

- A readiness ping on a connection outside the pool, which would separate "busy" from "unreachable".
- A component that reports `degraded` itself; its form, whether a typed error from `Ready` or a status method beside it, is decided with it. No component produces such a status — `sqldb` derives no DEGRADED, which would need an SLO, windowed deltas and hysteresis ([ADR-015](015-data-access.md#connection-pool-policy)) — and the capability can be added later without a break.
- A component in general whose failed `Start` does not fail the start: it would need a reconnect contract, which `sqldb.WithOptionalStart()` is for one module.

**Rejected:**

- **A database that gates readiness only when the application asks**, through `AddReadinessCheck` or an option: Spring Boot's default, for the shared outage that `credo.OptionalReadiness()` answers per binding. As the default it gives a database's `Ready` a rule no other component's follows, adds a registration step beside the binding, and keeps in rotation a process that has lost its database while the others have not.
- **Leaving a non-gating entry out of `/ready`:** an operator could no longer see it fail.
- **A failing optional entry that reports `degraded` instead of `down`:** the entry would hide that its dependency is down; `degraded` is the top-level status only.
- **The name `credo.NonCritical()`:** it names criticality in general, so it promises that a failing dependency does not stop the start either, while the option changes readiness only.
- **The name `credo.WithoutReadiness()`:** `With` and `Without` name the options of `credo.New`.
- **A wrapper that holds a database in a named field and does not forward `Ready`**, to keep it from gating readiness: the field hides `Start`, `Shutdown` and `ResourceIdentity` too, and a forgotten forward is silent.
- **A module option that makes `Ready` succeed:** an entry that reports up while its database is down.
- **A start forgiven for every registration whose readiness is optional:** the App cannot know whether a component is usable after its failed `Start`, which is why the forgiving option is the module's.
- **An optional component whose dependants are refused at `Finalize` and whose failed `Start` lets the App continue:** unusable for a database that repositories use, and the database stays closed after its failed `Start`.

## Consequences

**Positive:**

- Zero-config K8s probes: `app.UseHealth()` is all that's needed.
- A bound database appears in `/ready` with no call beyond its binding, under the name its component carries.
- Every entry shows its latency, and an optional dependency is visible in `/ready` without taking the instance out of rotation.
- Small implementation, with no store bridge to keep stable.
- No external dependencies or attribution obligations.

**Negative:**

- Gating by default can take a busy instance out of rotation when its bounded pool holds the readiness ping; the pool's size and `credo.OptionalReadiness()` answer it, and a ping outside the pool is deferred.
- No result cache — a completed probe is run again by the next request. Overlapping requests share only the current in-flight execution.
- No detailed liveness response body (only status). Keeps it minimal per K8s best practices.

**Risks:**

- Go cannot forcibly stop a non-cooperative callback. Credo returns at the configured deadline and prevents overlapping executions, but one callback goroutine can remain blocked indefinitely until application code releases it.
- Probe execution is detached from an individual HTTP waiter's cancellation so concurrent waiters can share it. A callback can therefore outlive that request (and, if non-cooperative, application drain); adapters must honor the finite probe context. The default five-second probe budget is shorter than the default thirty-second shutdown drain budget.

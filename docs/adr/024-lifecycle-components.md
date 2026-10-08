# ADR-024: Lifecycle Components

**Status:** Accepted, implemented in v0.24.0 **Date:** 2026-10-07 **Depends on:** ADR-004, ADR-006, ADR-022 **Related:** ADR-015, ADR-016, ADR-019, ADR-020, ADR-023 **Specification:** [Lifecycle spec](../specs/lifecycle.md), [Container spec](../specs/container.md) **Guide:** [Deployment guide](../guides/deployment.md), [Dependency injection guide](../guides/dependency-injection.md)

## Problem

An App had five teardown mechanisms, each with its own order and its own deadline rule:

- `OnPreDrain` hooks, run concurrently before the lifecycle context was cancelled, as a hard barrier that was never abandoned, even past the deadline;
- `OnDrain` hooks, run concurrently with the HTTP drain and before DI teardown;
- DI `Shutdowner`s, shut down by the container in dependency order;
- `OnShutdown` hooks, run LIFO after DI teardown;
- the cancellation of the lifecycle context, which goroutines launched from `OnStart` hooks watched.

It had three readiness paths: `AddReadinessCheck`, and two internal seams — one for stores, one for worker readiness — that the health engine resolved from the DI container on every `/ready` request.

Each in-tree integration mixed these differently. `websocket` started in `OnStart` and drained in `OnDrain`. `worker` started in `OnStart`, drained in `OnDrain` and was also a `Shutdowner`, so its `Shutdown` was reached twice — idempotent, but two mechanisms for one resource. `store` relied on DI teardown, and kept its own ledger of resource identities to refuse mixed ownership. `OnPreDrain` was used by nothing in the repository. ADR-006 deferred a lifecycle abstraction until several in-tree consumers needed one; the HTTP server, the worker pool, the WebSocket server and store registrations did.

The mix produced defects that no single mechanism could fix:

- **`OnShutdown` ran after DI teardown.** A hook named like the general-purpose shutdown hook could never safely use a DI singleton, because every `Shutdowner` was already closed when it ran.
- **A worker's context was cancelled before the HTTP drain.** The pool ran its workers on the lifecycle context, which was cancelled before the HTTP drain began. A job that a handler enqueued during the drain reached a worker that had already stopped, so the last work an HTTP request handed to a worker was lost even though the database it writes to was still open.
- **A pointer held by two bindings was shut down twice, the first time too early.** The container called `Shutdown` once per binding that held a value. Observed against v0.23.0: with a `*Producer` binding, an `*OrderService` that takes it, and an adapter constructor registered after it that returns the same pointer as an `EventPublisher`, the shutdown order is `producer → orderService → producer` — twice, the first time before its direct consumer. Without the adapter the order is `orderService → producer`.
- **A shutdown during startup did not interrupt it.** `Shutdown` refused an App that was starting; a signal or a cancelled `RunContext` context waited until every `OnStart` hook had finished, after which "server started" was logged, the server began accepting and the drain followed.
- **An App served through `ServeHTTP` started nothing.** An external `http.Server` or an `httptest` server never ran `OnStart` hooks, silently. With an abstraction that starts workers and checks pools, that silence would have become a worker that never runs.

## Decision

The App has one lifecycle abstraction, the **component**: a value whose teardown the App owns, ordered by the dependency graph, started when it says it can be started, and asked for readiness when it says it can answer. Every former teardown mechanism became a component or a hook that is one. Workers, stores, the WebSocket server and mounted child Apps attach as components ([ADR-023](023-worker-system.md), [ADR-015](015-data-access.md), [ADR-019](019-websocket-integration-and-drain.md)).

### The type and its capabilities

```go
type Component interface {
    Shutdown(ctx context.Context) error
}

type Starter interface {
    Start(ctx context.Context) error
}

type Readier interface {
    Ready(ctx context.Context) error
}

type ResourceIdentifier interface {
    ResourceIdentity() any
}
```

`Component` took the place of the former `Shutdowner` interface with the same method set, so every type that implemented it is a component without a change. `Shutdown` is the identity because ordered teardown is what most resources need: a connection pool, a cache client, a storage provider — none of them has a start step. The name says what the registry manages rather than its one method, because every capability added later would make a method's name less accurate.

`Component` never gains a method: discovery is structural, so a type without a new method would leave the graph without a compile error. It grows through optional capabilities, as `database/sql/driver` grows through `Pinger` and `SessionResetter` and `net/http` through `Flusher`. Each capability has a small exported interface named after its method — `Starter`, `Readier`, `ResourceIdentifier` — and a documented default when it is absent. `Readier` follows that rule rather than `ReadinessChecker`, because `HealthChecker` already names the `Check` shape that `AddReadinessCheck` takes, and two "checker" interfaces with different methods would be confused. A capability added after v1 takes a Credo-owned type in its signature, so no method an application already has matches it by accident on an upgrade; `Start` and `Ready` keep plain signatures because they arrive with the model, and the one accidental match that costs something, a double start, is documented (Consequences). A capability reports through `error`, so a richer report — degraded readiness, once health's optional/critical policy lands — is a typed error, not a second method.

`*sqldb.DB` implements `ResourceIdentifier` by returning itself, and a wrapper that embeds it inherits the method.

The root exports `credo.Tier` with `TierIngress` and `TierInternal`, for configuration fields and reports that must spell a component's tier; the zero `Tier` is unset and means the registration's default, while `TierIngress` and `TierInternal` are non-zero and encode as the text `"ingress"` and `"internal"`.

### Registration and discovery

Planning and teardown are decided at different moments, because the value exists for one and not for the other.

- **The start walk is planned at registration, from the binding's type.** `Start`, `Ready` and the tier come from the type the binding shows, so the plan never constructs anything early. A `Start` or `Ready` that only the built value has is neither called nor asked, as with any value seen through an interface in Go. A type that has only `Start` is not a component, so an unrelated `Start()` is never called by accident.
- **The teardown follows ownership.** A DI singleton the App owns is a component when it has `Shutdown` — its binding's type shows the method at registration, or its value has it once built — because the binding's type is the view its consumers need, not an ownership switch; `credo.Borrowed()` is that switch. The container decided a `Shutdowner`'s teardown the same way before components. Releasing is the owner's obligation, whereas starting and readiness are capabilities the binding hands the App.
- **A value behind an interface that an `OnStart` hook starts keeps working**: the App shuts it down and never starts it a second time. To have the App start or ask a value, provide the concrete type and `Alias` the interface the application uses.
- **A component found only on its built value is internal.** `Finalize` cannot see it, so if it depends on an ingress component its construction fails with the dependency path and the two remedies ([Two tiers](#two-tiers)), before the value is handed out.
- **`app.Manage(v, opts...)`** adds a component that is not a DI binding: a value, or a constructor over DI parameters that the start walk builds and that never becomes a binding — a component that should not be injectable, such as a DI-provided worker, gets its edges and its tier without a binding in the application's container. `credo.Named("…")` names it, defaulting to the type name; a duplicate name panics. `credo.Ingress()` places it in the ingress tier. A value handed to `Manage` has no edges; a constructor handed to it has its parameters' edges.

### Resource identity: one resource, one teardown

Within one App the registry keys components by resource identity: the token a value's `ResourceIdentity()` returns; otherwise the value itself when it is comparable — a pointer, or a struct over one; otherwise its binding alone. A `ResourceIdentity()` that panics or returns a token that is nil, not comparable or not equal to itself panics at the `ProvideValue` or `Manage` call and fails a constructor's construction.

Values that share an identity are one component with one teardown: one pointer under several bindings, the interface view an adapter constructor returns, a wrapper and the handle it embeds, two wrappers over one handle, a value that a constructor returns after `Manage` received it. They are shut down once, when the last holder retires, so the consumers of every holder stop first. They are started once, as they are shut down once: the start walk calls `Start` through the first holder it reaches whose binding plans a `Start`, and through none when a holder borrows the resource.

Grouping holders keeps the consumers' order. A holder that obtains the resource through one of its consumers — an interface view that a constructor reads from a service using the resource — depends on that consumer only to reach a value it shares, so the consumer stops first and the resource after it; without that rule the group and the consumer would each wait for the other, and neither would stop. Only such a route is dropped: a dependency that a holder which reached the resource on its own contributes — a service's dependency on a pool, when the service is itself a group with an interface view — always holds.

The application chooses that teardown, not the order of construction:

- The holders that have a teardown agree on its kind: `Shutdown`, or `Close` through `credo.Closer()`. A disagreement panics at the later `ProvideValue` call when both values are given, and otherwise fails the construction of the holder built later. The message names both bindings and both remedies: drop `credo.Closer()`, or bind the holder that has `Shutdown` under a type that shows `Close` and not `Shutdown`, with `credo.Closer()`.
- Among holders of one kind, the one registered first runs the teardown, whichever is built first: registration order is fixed by the code, while a lazily built holder makes the build order unpredictable.
- A holder without a teardown — a view of a `credo.Closer()` client under a type without `Shutdown` — takes no part in the choice, and the resource still waits for its consumers.
- A value that a constructor built and the registry then refuses — its identity is unusable, or it would be an internal component depending on an ingress one — fails its construction and is still the App's when no other holder carries it, so the drain releases it in order. A value refused because its holders disagree on the kind or the owner is the shared resource itself; its owner releases it, and the App does not.

Kinds are all the registry can compare: Go does not tell a `Shutdown` that a wrapper inherits from one it declares, so refusing two holders of one kind with different types would refuse the wrapper and the handle it embeds. The choice therefore rests on a contract: only a value without state of its own to release may share an identity. A wrapper that embeds a handle which identifies itself inherits that identity, so a wrapper that releases state of its own holds the handle in a named field instead; a wrapper that forwards an identity promises that it keeps none. This is what lets several instances of one type be wrapper types ([ADR-004](004-dependency-injection-and-infra.md)) while each database is still closed exactly once.

`Manage` of a value whose resource a `ProvideValue` binding holds panics (the resource is already managed through DI), and so does a resource handed to `Manage` twice. This rule replaced `store`'s ledger and keeps its refusal of mixed ownership.

### Ownership and registration options

Handing a value to DI and handing over its teardown are separate decisions. A component that is discovered or handed to `Manage` is owned by the App, which starts it and shuts it down. Four registration options act on components; [ADR-004](004-dependency-injection-and-infra.md) records their DI surface, and each is checked at the call, panicking on misuse:

- **`credo.Ingress()`** places a component in the ingress tier. On `Provide` and `ProvideValue` it panics on a binding that is not a component at registration — whose type shows no `Shutdown` and that carries no `credo.Closer()` — since the tier is planned before the value exists.
- **`credo.Borrowed()`**, on `ProvideValue` only, keeps the binding and the value's readiness contribution and leaves starting and shutting it down to the caller, through every holder of the resource — for a pool that two Apps in one process share, or a fixture a test suite reuses across the Apps it builds. Identity cannot express that, because it is unique only inside one App. A borrowed resource is torn down through none of its holders: a holder that claims the teardown — a component bound with `ProvideValue`, a value handed to `Manage`, a binding with `credo.Closer()` — disagrees on the owner and panics at the later call or fails its construction, while a value a constructor builds over a borrowed resource without `credo.Closer()` stays the caller's even when it has `Shutdown`, since a `Shutdown` found on a value is no claim. `Borrowed` is accepted on a binding whose type does not show `Shutdown`: that is how a caller keeps the teardown of a value that has the method behind an interface.
- **`credo.Closer()`** makes a binding whose type has a `Close` method — `Close() error`, `Close()` or `Close(ctx) error`, the last receiving the drain deadline — a component the App closes after its consumers, abandoned at the deadline like any `Shutdown`. It panics on a type with none of the three, on a type that is already a component, and beside `credo.Borrowed()`. It decides its binding's teardown: a built value that also has `Shutdown` is closed once, with `Close`, and a holder of the same resource whose teardown is `Shutdown` is refused. `Close` is never discovered on its own: the method is too common to mean that the App owns the value.
- **`credo.Override()`** replaces an earlier binding of the same type before `Finalize` and panics when there is none, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves. `testutil.WithOverride` builds on it and panics the same way; adding a binding is `WithWiring`'s job.

A teardown that is not a `Close` — a client's `Disconnect(ctx)`, a `Drain` that returns at once and must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself. Consumers depend on that type, so its edges are right, and its constructor's parameters give the teardown's own dependencies theirs; `sqldb.DB` wraps Bun this way. No new API is needed.

### Hooks

`OnStart(fn, opts...)` and `OnStop(fn, opts...)` are anonymous components of a tier, internal unless `credo.Ingress()` says otherwise. Within a tier, start hooks run FIFO after the tier's components have started, and stop hooks run LIFO before they stop, so a hook can use what the components provide and never outlives it. A hook is a leaf action: anything with its own teardown — a value built outside DI included — is a component, discovered or handed to `Manage`, so that the graph or the registration order places it. Process-level cleanup that must outlive every component, such as flushing a log sink, belongs after `Run` returns. A hook is abandoned at the deadline like any component. The lifecycle hooks' slots and their interaction with the rest of the drain are [ADR-006](006-application-lifecycle.md)'s.

### Two tiers

**Ingress is where work enters the process**: the HTTP listener, the WebSocket server, a scheduled worker that originates its runs, a consumer of an external queue. **Internal** is everything else — infrastructure and in-process consumers alike. The tier is declared per registration, with no marker interface and no third tier.

- **Start** mirrors the drain: the internal tier starts in dependency order, then the ingress tier, then the listener accepts.
- **Shutdown**: the ingress tier stops first, concurrently with the HTTP drain, and its components that no edge orders stop concurrently with each other, so a WebSocket drain that waits for its peers does not spend the budget of the components beside it. The internal tier then stops in reverse dependency order — a Kahn order over the dependency graph — with values that have no edges in reverse registration order. A worker that drains a queue fed by HTTP handlers therefore stops after the handlers have finished and before the database it depends on.
- **The one edge the DI graph cannot see** is a producer component handing work to a consumer component. The rule is a constructor dependency — the producer takes the consumer, or a handle the consumer owns — and "consumers before dependencies" then stops the producer first. There is no separate ordering API.
- **No dependency crosses the tiers backwards.** A component of the internal tier that depends on one of the ingress tier, directly or through bindings that are not components, fails `Finalize` with the path and both remedies: declare the dependent ingress, or split the ingress component so that what internal components use is an internal part the ingress component depends on. With that rule the dependency order holds for every component — a component's `Start` runs after its dependencies' and its `Shutdown` before theirs — and the tiers order only components that no dependency path connects. The case that seemed to need an exception does not: an in-process consumer that broadcasts over WebSockets depends on the application's own connection registry, an internal component that the server's handlers fill and its drain empties, and what it sends after the ingress drain finds no peers. A controller that is not a component creates no such edge, so injecting the WebSocket server into controllers is unaffected.

### Start

Work that needs a running dependency belongs in `Start`, never in a constructor. A constructor wires: it receives its dependencies and builds the value, and it never relies on a dependency having started, because the documented bootstrap order ([ADR-022](022-bootstrap-and-di-ownership.md)) resolves controllers — and most of the graph with them — to register routes before `Run`. Starts follow the dependency order without exception.

- A component with `Start` or `Ready` that is still unbuilt when the start phase begins is built by it, in its place in the walk; otherwise `/ready` would skip a component nothing resolved. A constructor error there is a start failure. So is every constructor handed to `Manage`. A component with neither stays lazy and is shut down only if it was built, as a lazily built `Shutdowner` was; singletons that are not components stay lazy too.
- **`Start` returns once the component is usable** — a pool reachable, settings read, a loop launched — or with an error after releasing what it opened. It never blocks for the component's lifetime. Work that outlives the call runs on a goroutine that `Start` launches, and `Shutdown` ends that work and waits for it.
- **Every context lives as long as its call.** `Start`'s bounds the start work, `Shutdown`'s carries the drain deadline, `Ready`'s belongs to the probe request. Credo cancels `Start`'s context when `Start` returns, or earlier when a shutdown is requested during the start phase. A goroutine that needs the context's values derives its own from `context.WithoutCancel(ctx)` with its own cancel, which `Shutdown` calls, so `Shutdown` is the one stop signal; a loop that keeps `Start`'s context stops at once, in the first test rather than as a broken drain order in production. An `OnStart` hook receives the same kind of context.
- **A failure after `Start` has returned is reported through `Ready`**, and a worker's also follows its restart policy. No component ends the App, except the App's own listeners, without which it serves nothing. This is a deliberate policy, not a recovery guarantee: a failing readiness probe takes the instance out of rotation and restarts nothing, so a component that has stopped for good leaves the process alive, unready and never restarted. An application that wants a restart ties a liveness check to the component itself, knowing that one failing because of a shared dependency restarts every replica. No fatal-error API is added.

A worked shape: a provider whose settings live in a database table, read through a parameter service, cannot be built at registration. As a component it needs no resolve-then-provide step: it takes the parameter service and the raw configuration in its constructor, reads the settings and opens its backend in `Start`, reports a degraded start through `Ready` instead of failing, and closes in `Shutdown`. Its I/O runs after the `Start` of everything it depends on, and nothing registers after `Finalize`.

### A failed start

A failed start stops, in reverse dependency order, every component that was built — a component without `Start` is owned the moment it is built, and one that was built but never started is stopped too, so `Shutdown` tolerates a component that never started — except the one whose `Start` failed: a `Start` that returns an error has released what it opened, the rule a constructor already follows, and the framework does not call `Shutdown` on a half-initialized value. A `Start` that panics is reported, and the rollback of the others continues. The App ends stopped.

### A shutdown during the start phase

A shutdown requested during the start phase — the first signal in `Run`, the context of `RunContext` or `ServeContext`, or `Shutdown`, which is accepted while the App is starting and waits for the rollback within its own context — cancels the running `Start`'s context, starts nothing further and rolls back what was built under the drain deadline counted from the request. A `Start` that ignores the cancellation is abandoned like a `Shutdown` that misses the deadline: its dependencies are kept open and it is reported; a second signal still kills the process. A requested shutdown is not a start failure: `Run` returns nil after a clean rollback and logs the interrupted `Start`'s error, "server started" is never logged, and the listener never accepts. `App.Start`'s own context interrupts it the same way, and an interrupted `App.Start` returns an error rather than nil, because its caller goes on to serve the App and must not mistake an interrupted start for a started one.

### Shutdown

The drain order is the tiers' ([Two tiers](#two-tiers)), under one deadline that both tiers share and spend in order. The rules are the ones the container applied to `Shutdowner`s before components, stated for every component:

- A `Shutdown` that has not returned at the deadline is abandoned: its dependencies are **not** stopped — nothing closes a database under a component that may still use it — and the abandoned component and every component it kept open are reported.
- A `Shutdown` that returns an error promptly is reported, and its dependencies are still stopped.
- Every `Start` and `Shutdown` is panic-isolated.
- A construction still running when the drain reaches it blocks its dependencies and is shut down in order when it completes; one that completes after the drain deadline gets a single late attempt bounded by a fixed five seconds, logged and not reported — for a component its binding's type shows and for one found only on its built value alike.

### One error

`Run`, `RunContext`, `ServeContext`, `App.Start` and `Shutdown` report a start or shutdown failure as one `*credo.LifecycleError`, naming each component — by its `credo.Named` name or its type name, and a hook as `OnStart[i]` or `OnStop[i]` — with its tier, its phase (`start`, or `shutdown` for the drain and for the rollback of a start) and its outcome: failed, panicked, abandoned at the deadline, kept open by an abandoned consumer. Only those outcomes appear; a component that started and stopped cleanly has no entry. Each `LifecycleEntry` carries `Name`, `Tier`, `Phase`, `Outcome`, `Err`, `KeptOpenBy` (the abandoned consumers of a kept-open component) and `Duration`, and `Cause` holds the context error that ended a phase before it completed. It generalizes the former `*DIShutdownError`: an immutable snapshot taken at the boundary — a call that returns later, and a late cleanup, are logged and never written back — with `Unwrap() []error` over the entries' errors and the cause, obtained with `errors.AsType[*credo.LifecycleError]`. A panicking `Start` is a `*credo.DIPanicError` with the phase `DIPanicStart`, beside `DIPanicConstruction`, `DIPanicShutdown` and `DIPanicLateCleanup`. The name says that it covers the start phase as well as the drain, which a "shutdown error" would not. A worker is named `worker:<name>`.

### Readiness

`/ready` aggregates three sources, with no DI resolution per request: the components' `Ready` capabilities, under the components' names, from the values built when the App entered running; the kernel's store registry — each registered store's typed health, from the value the start phase built and pinged once ([ADR-015](015-data-access.md#registration)); and the application's checks. A borrowed value's `Ready` is aggregated although the App neither starts nor stops it. During the drain `/ready` returns 503 `shutting_down` ([ADR-016](016-health-checks.md)). A worker's readiness conditions reach `/ready` this way, through its component's `Ready` under `worker:<name>`.

### Every way of serving starts the components

`Run`, `RunContext` and `ServeContext` run the start walk. `App.Start(ctx)` prepares the App and runs the start walk without a listener, for an App served through `ServeHTTP` by an external `http.Server` or in a test; `App.Shutdown` stops what it started. Its godoc opens with "Start runs the start phase without a listener; Run, RunContext and ServeContext serve.", because `Start(addr)` listens in other frameworks. `testutil.Start(tb, app)` starts the App and leaves its shutdown to the end of the test.

- An App that has anything to start — a component with `Start` or `Ready`, a constructor handed to `Manage`, a start hook, a store registration, the i18n catalogs `UseI18n` reads — refuses to serve until the start phase has completed successfully: before `App.Start` is called or while it runs, `ServeHTTP` panics with a message naming `App.Start`, `testutil.Start`, the serving entry points and `parent.Manage(child)`, as it does for a stored preparation error, so a handler never runs against dependencies that were not started. An App with nothing to start serves through `ServeHTTP` as before, preparing on its first request.
- A failed `App.Start` rolls back and leaves the App stopped; a stopped App answers with the default 503 envelope without touching DI, since the caller already has the error.
- `App.Start` is accepted only while the App is building: a second `App.Start`, or `Run`, `RunContext` or `ServeContext` after it, returns the state error, since the App is single-use.
- Whoever serves the App through `ServeHTTP` owns that server's admission and drain and completes them before `App.Shutdown`. The rule carries the order: the internal tier stops after the HTTP drain only if that drain has happened.
- `*App` has `Shutdown(ctx) error` and `Start(ctx) error`, so it is a startable component. A child App mounted with `Mount` is handed to `parent.Manage(child)`: it starts in the parent's start walk before the listener accepts and stops after the parent's HTTP drain — the internal default is the right tier, since that drain first finishes the requests that run the child's handlers. That is how a mounted child meets the `ServeHTTP` rule and the owner's drain, the parent's server being the owner; a mounted child with start work that its parent does not manage panics on its first request, which the parent's recovery turns into a logged 500. `parent.Manage(child)` composes independent Apps; an application's own modules share one App and its DI graph.

### Scope

Components are start-once; restart remains a worker concern, and supervisor trees are out of scope. Reload stays outside the component model ([ADR-020](020-reload-and-partial-config-reload.md)). The word "component" names only this abstraction in Credo's documentation and godoc; the log attribute `component` keeps its meaning — the subsystem that logs — because log queries depend on it.

## Removes

- `OnPreDrain`, with its hard barrier. A component that truly needs a barrier documents it as its own property.
- `OnDrain`: what drained beside the HTTP server is an ingress component; what drained before infrastructure is an internal component ordered by its edges.
- `OnShutdown` and its slot after DI teardown. `OnStop` runs before its tier's components stop, under a new name, so the moved slot is a compile error rather than a silent change.
- `Shutdowner` as a separate name: `Component` has its method set.
- The DI container's per-binding shutdown, which calls `Shutdown` once per holder of a value. Teardown belongs to the component registry, keyed by resource identity; the container keeps the dependency graph that orders it.
- The role of `store`'s ownership ledger — `WithCallerOwnedLifecycle` and the identity reservation — replaced by `credo.Borrowed()` and the resource-identity rule; `store.LifecycleIdentityProvider` became `credo.ResourceIdentifier` ([ADR-015](015-data-access.md#registration)).
- The store and worker readiness seams and their DI resolution on every `/ready` request.
- The lifecycle context as a contract: `OnStart` receives a context that ends with the call, and the session context the App keeps for reload is internal.
- The worker pool, its DI binding and its double shutdown path ([ADR-023](023-worker-system.md)): each worker is a component of its own, stopped only by its `Shutdown`.
- `Shutdown`'s refusal of an App that is starting.
- `*DIShutdownError`, generalized into `*LifecycleError`.
- `testutil.WithOverride` adding a binding when none exists.
- The deferred lifecycle `Service` abstraction.

## Consequences

One abstraction replaces five mechanisms, and the order a resource stops in follows from the graph rather than from which hook an integration chose. The last job an HTTP handler hands to an internal component before the drain reaches the database, and a failed start stops exactly the components that were built, except the one whose `Start` failed. A resource is shut down once, after the consumers of every holder.

**Costs.**

- **One drain deadline, spent in order.** Both tiers share the deadline, and a correct order spends it sequentially, so a long HTTP drain leaves less for the internal tier. The cost is documented, not hidden.
- **The ingress tier is a new concept**, and choosing it wrongly has asymmetric effects: an external consumer wrongly left internal merely stops later, while an in-process consumer wrongly placed in ingress loses the work handlers enqueue during the drain. That is why a continuous worker defaults to internal ([ADR-023](023-worker-system.md)).
- **The documented double start.** Where a binding's type shows both `Start` and `Shutdown`, an `OnStart` hook that started the value before v0.24.0 makes it start twice, unless the hook goes; behind an interface that hides either method, the hook alone starts it.
- **Readiness has a stated limit.** No component ends the App, and a failing readiness probe restarts nothing: a component that has stopped for good leaves an alive, unready process that is never restarted. The deployment guide states this, and the liveness alternative with its cost.
- **Changes that compile unchanged but behave differently**, which the release notes put first: `OnStart`'s context ends with the call, so a background loop it launched stops at once and becomes a component; a shutdown during startup interrupts the start; the double start above; a resource several bindings hold is shut down once, through the holder registered first and after the consumers of all of them; workers stop in their tier's turn, after the HTTP drain for a continuous worker, where the pool used to stop every worker beside the HTTP drain; `ServeHTTP` refuses an App with anything to start until `App.Start` has succeeded; an internal component that depends on an ingress one fails `Finalize`; `testutil.WithOverride` panics without an earlier binding.
- **Changes that break the build:** `Shutdowner` becomes `Component`; `OnShutdown` becomes `OnStop`, which runs before its tier's components — a hook that closed a resource becomes a component, a `credo.Closer()` binding or code after `Run`; `OnPreDrain` and `OnDrain` become components (`credo.Ingress()` for what drains beside the listener) or `OnStop`.

**Deferred.** A constructor returning `(T, func(context.Context) error, error)` — the cleanup function of Wire's providers — waits for a type that is costly to wrap: a third-party API needed raw across many consumers. Adding it later breaks nothing, because the container rejects a three-value constructor; removing it after v1 would. A policy by which a terminal component failure stops the App is deferred, as are further capabilities — staged reload, degraded readiness, a signal for a failure after `Start`, introspection — each waiting for its owner.

## Rejected alternatives

- **Requiring both `Start` and `Shutdown` for discovery.** It would keep a singleton with an unrelated `Start()` from being started by accident, but that holds anyway, since a type with only `Start` is not a component; and it would cost every resource without a start step — the class the former `Shutdowner` covered and most resources fall in — its ordered teardown, or force a no-op `Start` on it.
- **Discovering every `Close`.** The method is too common to mean that the App owns the value, and `Close() error` takes no deadline. `credo.Closer()` states the ownership explicitly.
- **A teardown-function option**, `credo.ShutdownWith(func(ctx, T) error)`. Go cannot tie the option's type parameter to `Provide`'s at compile time, so a mismatch would surface only as a registration panic, and the dependencies the function uses would have no edges, so the graph could close them first; its non-generic form, `func(ctx) error`, would fit `ProvideValue` only.
- **Promising that a component is constructed only after its dependencies have started.** The start walk could keep that promise only for what nothing resolved before `Run`, and the documented bootstrap order resolves most of the graph to register routes.
- **A value with no edges that resolves its dependencies in `Start`.** The order it needs would become a registration-order convention; `Manage` accepts a constructor instead, which gives the component its edges.
- **A `Start` that blocks for the component's lifetime**, the `Run(ctx)` shape. The start walk could not tell a started component from a running one, so neither the dependency order nor the rollback would have a signal to act on.
- **A per-component lifetime context**, cancelled in order just before the component's `Shutdown`. It makes `go loop(ctx)` correct, but gives one resource two stop signals — the double path this decision removes from the worker — and could not bound the start work, because a deadline cannot be removed from a context once set.
- **A start that no shutdown can interrupt**, as `OnStart` was. An orchestrator kills the process after its grace period anyway, so the guarantee never held, and a cancelled start rolls back where a killed one cannot. A step that must finish says so with `context.WithoutCancel`.
- **Letting the tier win over a dependency that crosses it backwards**, so that the consumer starts before the ingress component it depends on and stops after it. Safe for one broadcast implementation, but as a framework rule it breaks every component whose `Start` needs its ingress dependency started or whose `Shutdown` still uses it, and it contradicts the dependency order stated for every component. **Moving the dependent into the ingress tier implicitly** was rejected too: the declared tier would no longer say where the component stops.
- **An `After`/`Before` API, a marker interface and a third tier.** Constructor dependencies already order components, and the one edge DI cannot see — producer to consumer — is expressed as a constructor dependency. The tier belongs to each registration, not to a type: one worker type can be registered in either tier. Two tiers capture the one distinction that matters — whether work enters the process there.
- **`Name`, `Tier`, ordering, restart, liveness, metrics and tracing as methods of a component.** The name is `credo.Named`, the tier `credo.Ingress()`, ordering the constructor's dependencies; restart and pause are a worker concern; a component in the liveness check would turn a dependency's outage into a restart of every replica; metrics and tracing belong to the observability release. Each would also be a method `Component` could never add without silently dropping types from the graph.
- **The name `Boot` for `App.Start`.** In Laravel and Symfony it names the container's initialization phase, closer to `Finalize`, and it would break the pairing with `OnStart` and `Starter`. `Start` pairs with `App.Shutdown` and has the meaning fx's `App.Start` and .NET's `IHost.StartAsync` give it, with `Run` as start, wait and stop.
- **The App counting in-flight `ServeHTTP` calls and waiting for them in `Shutdown`.** An atomic operation on every request for a guarantee it still could not give, since the owner's listener keeps admitting requests; `http.Server.Shutdown` already stops admission and waits.
- **Deciding a component's teardown from its binding's static type.** An interface that omits `Shutdown` would silently end the teardown of a value the App built — a producer with what it buffers. A warning, in debug mode or always, reports that loss without preventing it.
- **Calling `Shutdown` once per binding that holds a value**, as the v0.23.0 container did. It shuts one resource down several times, the first time while consumers of another holder may still run. Calling `Start` once per binding, as v0.24.0 did, starts one resource several times, and starts a borrowed resource that a wrapper over it shows `Start` for.
- **Keeping every holder's dependencies on the shared resource.** A holder that reached the resource through a consumer would make the resource wait for that consumer while the consumer waits for the resource, and neither would be shut down, as in v0.24.0. Dropping every non-first holder's dependencies instead would let what a wrapper's own fields use stop before the wrapper's consumers, and dropping every edge on the cycle would drop a real dependency too when two groups meet: the service group's dependency on the pool group would go with the pool view's route through the service. Only an edge that a holder which reached the resource through another holder contributes, and that closes a cycle, is a route rather than a dependency.
- **Releasing every value the registry refuses**, or none. A value refused for a holder conflict is the shared resource, so releasing it closes what its owner — possibly the caller of `credo.Borrowed()` — still holds; releasing none, as v0.24.0 did, leaks a value only the App ever held.
- **Keying components by pointer alone, with "register pointers" as the rule.** It misses the value-shaped wrapper that several instances of one type require — `type PrimaryDB struct{ *sqldb.DB }` — whose interface view and raw handle would each be shut down again, silently closing the database twice.
- **Choosing among the teardowns that the holders of one resource declare by build order**, which a lazily built holder makes unpredictable and which may pick a holder without a teardown; or **by a fixed precedence of `credo.Closer()` over `Shutdown`**, which would skip a teardown the application wrote, a wrapper's flush, without a word.
- **Refusing a component whose value has a `Start` or `Ready` that its binding's type hides.** It guards no loss, since no earlier release looks for either method; it would break a value that an `OnStart` hook starts behind an interface; and its failure could first surface at a request for a lazily built value. **A warning in its place** would fire on that documented pattern.
- **Moving `OnShutdown` hooks before DI teardown in a patch.** The order would change in a patch and again under the component model.
- **The restartable `Service` abstraction** that the lifecycle spec once deferred — a `Run(ctx)`/`Name()` seam with a restartable/start-once taxonomy. Its `Run(ctx)` is the blocking start rejected above, its name is a registration option, and restart stays a worker concern; components are start-once.

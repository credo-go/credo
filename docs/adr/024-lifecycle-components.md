# ADR-024: Lifecycle Components

**Status:** Accepted **Date:** 2026-10-07 **Depends on:** ADR-004, ADR-006, ADR-022 **Related:** ADR-015, ADR-016, ADR-019, ADR-020, ADR-023 **Specification:** [Lifecycle spec](../specs/lifecycle.md), [Container spec](../specs/container.md) **Guide:** [Deployment guide](../guides/deployment.md), [Dependency injection guide](../guides/dependency-injection.md)

## Context

An App holds resources whose teardown must follow the work that uses them, and work enters it from several places: the HTTP listener, WebSocket peers, scheduled jobs, external queues. The HTTP server, workers, the WebSocket server and store registrations all need a start step, readiness and an ordered teardown. Separate mechanisms for them — hooks around the HTTP drain, a container teardown, a context that background work watches, a readiness seam per integration — each bring their own order and deadline rule, and leave failure modes that no single mechanism can close:

- **A resource must stop once, after every consumer**, including consumers reached through another binding of the same resource. A `*Producer` binding, an `*OrderService` that takes it, and an adapter constructor that returns the same pointer as an `EventPublisher` are one resource; shut down once per binding, it stops twice, the first time before its direct consumer.
- **Background work fed by requests must stop after the HTTP drain and before what it writes to.** A worker cancelled before the HTTP drain loses the job that a handler enqueues during the drain, although the database it writes to is still open.
- **A stop hook must still see its tier's singletons.** A hook that runs after the container's teardown can never safely use a singleton.
- **A shutdown during startup must interrupt it.** A start that finishes every start step before it honours a signal or a cancelled context logs "server started", accepts, and only then drains.
- **An App served by an external server must still start its components.** An external `http.Server` or an `httptest` server that starts nothing does so silently, and a worker it should have started never runs.
- **Ownership must be visible.** Whether the App or its caller releases a value — a pool two Apps in one process share — is a decision the registration states, not a ledger each integration keeps.
- **Readiness must come from what the start phase built.** A probe that resolves its sources from the container on every request can build a value that nothing started.

## Decision

The App has one lifecycle abstraction, the **component**: a value whose teardown the App owns, ordered by the dependency graph, started when it says it can be started, and asked for readiness when it says it can answer; a start or stop hook is a component as well. Workers, stores, the WebSocket server and mounted child Apps attach as components ([ADR-023](023-worker-system.md), [ADR-015](015-data-access.md), [ADR-019](019-websocket-integration-and-drain.md)).

### The type and its capabilities

The [lifecycle spec](../specs/lifecycle.md#component-types) lists the types: `Component`, whose one method is `Shutdown(ctx) error`, and the optional capabilities `Starter` (`Start`), `Readier` (`Ready`) and `ResourceIdentifier` (`ResourceIdentity`).

`Shutdown` is the identity because ordered teardown is what most resources need: a connection pool, a cache client, a storage provider — none of them has a start step. The name says what the registry manages rather than its one method, because every capability added later would make a method's name less accurate.

`Component` never gains a method: discovery is structural, so a type without a new method would leave the graph without a compile error. It grows through optional capabilities, as `database/sql/driver` grows through `Pinger` and `SessionResetter` and `net/http` through `Flusher`. Each capability has a small exported interface named after its method — `Starter`, `Readier`, `ResourceIdentifier` — and a documented default when it is absent. `Readier` follows that rule rather than `ReadinessChecker`, because `HealthChecker` already names the `Check` shape that `AddReadinessCheck` takes, and two "checker" interfaces with different methods would be confused. A capability added after v1 takes a Credo-owned type in its signature, so no method an application already has matches it by accident on an upgrade; `Start` and `Ready` keep plain signatures because they arrive with the model, and the one accidental match that costs something, a double start, is documented (Consequences). A capability reports through `error`, so a richer report — degraded readiness, once health's optional/critical policy lands — is a typed error, not a second method.

The root exports `credo.Tier`, with `TierIngress` and `TierInternal`, for configuration fields and reports that must spell a component's tier.

### Registration and discovery

Planning and teardown are decided at different moments, because the value exists for one and not for the other.

- **The start walk is planned at registration, from the binding's type.** `Start`, `Ready` and the tier come from the type the binding shows, so the plan never constructs anything early. A `Start` or `Ready` that only the built value has is neither called nor asked, as with any value seen through an interface in Go. A type that has only `Start` is not a component, so an unrelated `Start()` is never called by accident.
- **The teardown follows ownership.** A DI singleton the App owns is a component when it has `Shutdown` — its binding's type shows the method at registration, or its value has it once built — because the binding's type is the view its consumers need, not an ownership switch; `credo.Borrowed()` is that switch. Releasing is the owner's obligation, whereas starting and readiness are capabilities the binding hands the App. A component found only on its built value is internal, since `Finalize` cannot see it to check its tier.
- **`app.Manage(v, opts...)`** adds a component that is not a DI binding, so a component that should not be injectable, such as a DI-provided worker, gets its edges and its tier without a binding in the application's container ([lifecycle spec](../specs/lifecycle.md#appmanagev-opts)).

The [container spec](../specs/container.md#registration-options) states which bindings are components and the consequences for a value behind an interface.

### Resource identity: one resource, one teardown

Within one App the registry keys components by resource identity — the token a value's `ResourceIdentity()` returns, else the value itself when it is comparable — rather than by binding. Values that share an identity are one component with one teardown: one pointer under several bindings, the interface view an adapter constructor returns, a wrapper and the handle it embeds. They are shut down once, when the last holder retires, so the consumers of every holder stop first, and started once, as they are shut down once. The [container spec](../specs/container.md#resource-identity-and-teardown) states the identity order, the misuse panics and what happens to a value the registry refuses.

Grouping holders keeps the consumers' order. A holder that obtains the resource through one of its consumers — an interface view that a constructor reads from a service using the resource — depends on that consumer only to reach a value it shares, so the consumer stops first and the resource after it; without that rule the group and the consumer would each wait for the other, and neither would stop. Only such a route is dropped: a dependency that a holder which reached the resource on its own contributes — a service's dependency on a pool, when the service is itself a group with an interface view — always holds.

The application chooses that teardown, not the order of construction. The holders that have a teardown must agree on its kind, `Shutdown` or `Close` through `credo.Closer()`, and among holders of one kind the one registered first runs it, whichever is built first: registration order is fixed by the code, while a lazily built holder makes the build order unpredictable.

Kinds are all the registry can compare: Go does not tell a `Shutdown` that a wrapper inherits from one it declares, so refusing two holders of one kind with different types would refuse the wrapper and the handle it embeds. The choice therefore rests on a contract: only a value without state of its own to release may share an identity. A wrapper that embeds a handle which identifies itself inherits that identity, so a wrapper that releases state of its own holds the handle in a named field instead; a wrapper that forwards an identity promises that it keeps none. This is what lets several instances of one type be wrapper types ([ADR-004](004-dependency-injection-and-infra.md)) while each database is still closed exactly once.

One resource has one owner: `Manage` of a resource that a `ProvideValue` binding already holds panics, and so does a resource handed to `Manage` twice.

### Ownership and registration options

Handing a value to DI and handing over its teardown are separate decisions. A component that is discovered or handed to `Manage` is owned by the App, which starts it and shuts it down. Four registration options act on components, each checked at the call; [ADR-004](004-dependency-injection-and-infra.md) records their DI surface and the [container spec](../specs/container.md#registration-options) their rules:

- **`credo.Ingress()`** places a component in the ingress tier. On `Provide` and `ProvideValue` the binding must be a component at registration, since the tier is planned before the value exists.
- **`credo.Borrowed()`**, on `ProvideValue` only, keeps the binding and the value's readiness contribution and leaves starting and shutting it down to the caller, through every holder of the resource — for a pool that two Apps in one process share, or a fixture a test suite reuses across the Apps it builds. Identity cannot express that, because it is unique only inside one App.
- **`credo.Closer()`** makes a binding whose type has a `Close` method a component the App closes after its consumers, abandoned at the deadline like any `Shutdown`. `Close` is never discovered on its own: the method is too common to mean that the App owns the value.
- **`credo.Override()`** replaces an earlier binding of the same type before `Finalize` and panics when there is none, so a stale override — one whose binding the wiring dropped — fails instead of adding a binding nothing resolves.

A teardown that is not a `Close` — a client's `Disconnect(ctx)`, a `Drain` that returns at once and must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself. Consumers depend on that type, so its edges are right, and its constructor's parameters give the teardown's own dependencies theirs; `sqldb.DB` wraps Bun this way. No new API is needed.

### Hooks

`OnStart` and `OnStop` are anonymous components of a tier; a hook is a leaf action, and anything with its own teardown is a component ([ADR-006](006-application-lifecycle.md#lifecycle-hooks)).

### Two tiers

**Ingress is where work enters the process**: the HTTP listener, the WebSocket server, a scheduled worker that originates its runs, a consumer of an external queue. **Internal** is everything else — infrastructure and in-process consumers alike. The tier is declared per registration, with no marker interface and no third tier.

- **Start** mirrors the drain: the internal tier starts in dependency order, then the ingress tier, then the listener accepts.
- **Shutdown**: the ingress tier stops first, concurrently with the HTTP drain, and its components that no edge orders stop concurrently with each other, so a WebSocket drain that waits for its peers does not spend the budget of the components beside it. The internal tier then stops in reverse dependency order. A worker that drains a queue fed by HTTP handlers therefore stops after the handlers have finished and before the database it depends on.
- **The one edge the DI graph cannot see** is a producer component handing work to a consumer component. The rule is a constructor dependency — the producer takes the consumer, or a handle the consumer owns — and "consumers before dependencies" then stops the producer first. There is no separate ordering API.
- **No dependency crosses the tiers backwards.** A component of the internal tier that depends on one of the ingress tier, directly or through bindings that are not components, fails `Finalize` with the path and both remedies: declare the dependent ingress, or split the ingress component so that what internal components use is an internal part the ingress component depends on. With that rule the dependency order holds for every component — a component's `Start` runs after its dependencies' and its `Shutdown` before theirs — and the tiers order only components that no dependency path connects. The WebSocket broadcast case needs no exception ([ADR-019](019-websocket-integration-and-drain.md#drain-websockets-before-infrastructure)).

### Start

Work that needs a running dependency belongs in `Start`, never in a constructor. A constructor wires: it receives its dependencies and builds the value, and it never relies on a dependency having started, because the documented bootstrap order ([ADR-022](022-bootstrap-and-di-ownership.md#sequential-bootstrap-and-three-error-phases)) resolves controllers — and most of the graph with them — to register routes before `Run`. Starts follow the dependency order without exception.

- A component with `Start` or `Ready` that is still unbuilt when the start phase begins is built by it, in its place in the walk; otherwise `/ready` would skip a component nothing resolved. A component with neither stays lazy and is shut down only if it was built.
- **`Start` returns once the component is usable** — a pool reachable, settings read, a loop launched — or with an error after releasing what it opened. It never blocks for the component's lifetime. Work that outlives the call runs on a goroutine that `Start` launches, and `Shutdown` ends that work and waits for it.
- **Every context lives as long as its call** ([ADR-006](006-application-lifecycle.md#call-scoped-contexts)): `Start`'s context bounds the start work, so a goroutine that outlives it derives its own context with `context.WithoutCancel` and has `Shutdown` as its one stop signal.
- **A failure after `Start` has returned is reported through `Ready`**, and a worker's also follows its restart policy. No component ends the App, except the App's own listeners, without which it serves nothing. This is a deliberate policy, not a recovery guarantee: a failing readiness probe takes the instance out of rotation and restarts nothing, so a component that has stopped for good leaves the process alive, unready and never restarted. An application that wants a restart ties a liveness check to the component itself, knowing that one failing because of a shared dependency restarts every replica. No fatal-error API is added.

A worked shape: a provider whose settings live in a database table, read through a parameter service, cannot be built at registration. As a component it needs no resolve-then-provide step: it takes the parameter service and the raw configuration in its constructor, reads the settings and opens its backend in `Start`, reports a degraded start through `Ready` instead of failing, and closes in `Shutdown`. Its I/O runs after the `Start` of everything it depends on, and nothing registers after `Finalize`.

### A failed start

A failed start stops, in reverse dependency order, every component that was built — a component without `Start` is owned the moment it is built, and one that was built but never started is stopped too, so `Shutdown` tolerates a component that never started — except the one whose `Start` failed: a `Start` that returns an error has released what it opened, the rule a constructor already follows, and the framework does not call `Shutdown` on a half-initialized value. The App ends stopped ([lifecycle spec](../specs/lifecycle.md#a-failed-start)).

### A shutdown during the start phase

A shutdown requested during the start phase — a signal, the entry point's context, or `Shutdown`, which is accepted while the App is starting — cancels the running `Start`'s context, starts nothing further and rolls back what was built. A `Start` that ignores the cancellation is abandoned like a `Shutdown` that misses the deadline. A requested shutdown is not a start failure, so `Run` returns nil after a clean rollback; an interrupted `App.Start` returns an error rather than nil, because its caller goes on to serve the App and must not mistake an interrupted start for a started one. The [lifecycle spec](../specs/lifecycle.md#a-shutdown-during-the-start-phase) states the outcomes at each point of the phase.

### Shutdown

The drain order is the tiers' ([Two tiers](#two-tiers)), under one deadline that both tiers share and spend in order. A `Shutdown` that has not returned at the deadline is abandoned and its dependencies are **not** stopped — nothing closes a database under a component that may still use it — while one that returns an error promptly has completed, so its dependencies are still stopped. Every `Start` and `Shutdown` is panic-isolated, so one component's panic never ends the drain. The [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers) states the rules, including the late attempt for a construction that completes after the deadline.

### One error

`Run`, `RunContext`, `ServeContext`, `App.Start` and `Shutdown` report a start or shutdown failure as one `*credo.LifecycleError`, naming each component that failed, panicked, was abandoned at the deadline or was kept open by an abandoned consumer, with its tier and phase; a component that started and stopped cleanly has no entry. It is an immutable snapshot taken at the boundary, so a call that returns later is logged and never changes a result the caller already holds. The name says that it covers the start phase as well as the drain, which a "shutdown error" would not. The [lifecycle spec](../specs/lifecycle.md#the-lifecycle-error) states its fields and text.

### Readiness

A component's `Ready` is one of `/ready`'s sources, asked from the value the start phase built rather than resolved per request ([health spec](../specs/health.md#readiness-aggregation)).

### Every way of serving starts the components

`Run`, `RunContext` and `ServeContext` run the start walk, and `App.Start(ctx)` runs it without a listener, for an App served through `ServeHTTP` by an external `http.Server` or in a test. An App that has anything to start refuses `ServeHTTP` until the start phase has succeeded, with the treatment a stored preparation error gets: a handler served against dependencies that were not started is developer misuse, not an availability outcome ([lifecycle spec](../specs/lifecycle.md#the-start-gate-on-servehttp)).

- Whoever serves the App through `ServeHTTP` owns that server's admission and drain and completes them before `App.Shutdown`. The rule carries the order: the internal tier stops after the HTTP drain only if that drain has happened.
- `*App` has `Shutdown(ctx) error` and `Start(ctx) error`, so it is a startable component, and a child App mounted with `Mount` is handed to `parent.Manage(child)`. The internal default is the right tier, since the parent's HTTP drain first finishes the requests that run the child's handlers; the parent's server is the owner whose drain the child relies on ([lifecycle spec](../specs/lifecycle.md#mounted-apps)).

### Scope

Components are start-once; restart remains a worker concern, and supervisor trees are out of scope. Reload stays outside the component model ([ADR-020](020-reload-and-partial-config-reload.md)). The word "component" names only this abstraction in Credo's documentation and godoc. The log attribute `component` names the logging subsystem in the `net/http` bridge and the store registration, and the component in the lifecycle's own records.

## Rejected Alternatives

- **Requiring both `Start` and `Shutdown` for discovery.** It would keep a singleton with an unrelated `Start()` from being started by accident, but that holds anyway, since a type with only `Start` is not a component; and it would cost every resource without a start step — the class most resources fall in — its ordered teardown, or force a no-op `Start` on it.
- **Discovering every `Close`.** The method is too common to mean that the App owns the value, and `Close() error` takes no deadline. `credo.Closer()` states the ownership explicitly.
- **A teardown-function option**, `credo.ShutdownWith(func(ctx, T) error)`. Go cannot tie the option's type parameter to `Provide`'s at compile time, so a mismatch would surface only as a registration panic, and the dependencies the function uses would have no edges, so the graph could close them first; its non-generic form, `func(ctx) error`, would fit `ProvideValue` only.
- **Promising that a component is constructed only after its dependencies have started.** The start walk could keep that promise only for what nothing resolved before `Run`, and the documented bootstrap order resolves most of the graph to register routes.
- **A value with no edges that resolves its dependencies in `Start`.** The order it needs would become a registration-order convention; `Manage` accepts a constructor instead, which gives the component its edges.
- **A `Start` that blocks for the component's lifetime**, the `Run(ctx)` shape of a restartable `Service` abstraction with a restartable/start-once taxonomy. The start walk could not tell a started component from a running one, so neither the dependency order nor the rollback would have a signal to act on; a `Service`'s name is a registration option, and restart stays a worker concern — components are start-once.
- **A per-component lifetime context**, cancelled in order just before the component's `Shutdown`. It makes `go loop(ctx)` correct, but gives one resource two stop signals, the context and its `Shutdown`, and could not bound the start work, because a deadline cannot be removed from a context once set.
- **A start that no shutdown can interrupt.** An orchestrator kills the process after its grace period anyway, so the guarantee never held, and a cancelled start rolls back where a killed one cannot. A step that must finish says so with `context.WithoutCancel`, and migrations already require transactional or idempotent work.
- **Letting the tier win over a dependency that crosses it backwards**, so that the consumer starts before the ingress component it depends on and stops after it. Safe for one broadcast implementation, but as a framework rule it breaks every component whose `Start` needs its ingress dependency started or whose `Shutdown` still uses it, and it contradicts the dependency order stated for every component. **Moving the dependent into the ingress tier implicitly** was rejected too: the declared tier would then not say where the component stops.
- **An `After`/`Before` API, a marker interface and a third tier.** Constructor dependencies already order components, and the one edge DI cannot see — producer to consumer — is expressed as a constructor dependency. The tier belongs to each registration, not to a type: one worker type can be registered in either tier. Two tiers capture the one distinction that matters — whether work enters the process there.
- **`Name`, `Tier`, ordering, restart, liveness, metrics and tracing as methods of a component.** The name is `credo.Named`, the tier `credo.Ingress()`, ordering the constructor's dependencies; restart and pause are a worker concern; a component in the liveness check would turn a dependency's outage into a restart of every replica; metrics and tracing belong to the observability release. Each would also be a method `Component` could never add without silently dropping types from the graph.
- **The name `Boot` for `App.Start`.** In Laravel and Symfony it names the container's initialization phase, closer to `Finalize`, and it would break the pairing with `OnStart` and `Starter`. `Start` pairs with `App.Shutdown` and has the meaning fx's `App.Start` and .NET's `IHost.StartAsync` give it, with `Run` as start, wait and stop. Because `Start(addr)` listens in other frameworks, its godoc opens by saying that it runs the start phase without a listener.
- **The App counting in-flight `ServeHTTP` calls and waiting for them in `Shutdown`.** An atomic operation on every request for a guarantee it still could not give, since the owner's listener keeps admitting requests; `http.Server.Shutdown` already stops admission and waits.
- **Deciding a component's teardown from its binding's static type.** An interface that omits `Shutdown` would silently end the teardown of a value the App built — a producer with what it buffers. A warning, in debug mode or always, reports that loss without preventing it.
- **Calling `Shutdown` once per binding that holds a value.** It shuts one resource down several times, the first time while consumers of another holder may still run. Calling `Start` once per binding starts one resource several times, and starts a borrowed resource that a wrapper over it shows `Start` for.
- **Keeping every holder's dependencies on the shared resource.** A holder that reached the resource through a consumer would make the resource wait for that consumer while the consumer waits for the resource, and neither would be shut down. Dropping every non-first holder's dependencies instead would let what a wrapper's own fields use stop before the wrapper's consumers, and dropping every edge on the cycle would drop a real dependency too when two groups meet: the service group's dependency on the pool group would go with the pool view's route through the service. Only an edge that a holder which reached the resource through another holder contributes, and that closes a cycle, is a route rather than a dependency.
- **Releasing every value the registry refuses**, or none. A value refused for a holder conflict is the shared resource, so releasing it closes what its owner — possibly the caller of `credo.Borrowed()` — still holds; releasing none leaks a value only the App ever held.
- **Keying components by pointer alone, with "register pointers" as the rule.** It misses the value-shaped wrapper that several instances of one type require — `type PrimaryDB struct{ *sqldb.DB }` — whose interface view and raw handle would each be shut down again, silently closing the database twice.
- **Choosing among the teardowns that the holders of one resource declare by build order**, which a lazily built holder makes unpredictable and which may pick a holder without a teardown; or **by a fixed precedence of `credo.Closer()` over `Shutdown`**, which would skip a teardown the application wrote, a wrapper's flush, without a word.
- **Refusing a component whose value has a `Start` or `Ready` that its binding's type hides.** It guards no loss — unlike a hidden `Shutdown`, a hidden `Start` or `Ready` leaves nothing unreleased; it would break a value that an `OnStart` hook starts behind an interface; and its failure could first surface at a request for a lazily built value. **A warning in its place** would fire on that documented pattern.

## Consequences

The order a resource stops in follows from the graph rather than from which hook an integration chose: the last job an HTTP handler hands to an internal component is processed before the drain reaches the database; a failed start stops exactly the components that were built, except the one whose `Start` failed; and a resource is shut down once, after the consumers of every holder.

**Costs.**

- **One drain deadline, spent in order.** Both tiers share the deadline, and a correct order spends it sequentially, so a long HTTP drain leaves less for the internal tier. The cost is documented, not hidden.
- **The ingress tier is one more concept**, and choosing it wrongly has asymmetric effects ([ADR-023](023-worker-system.md#each-worker-is-a-component-the-supervisor-is-a-registry)).
- **The documented double start.** Where a binding's type shows both `Start` and `Shutdown`, an `OnStart` hook that also starts such a value makes it start twice; behind an interface that hides either method, the hook alone starts it.
- **Readiness has a stated limit** ([Start](#start)): a component that has stopped for good leaves an alive, unready process that is never restarted. The deployment guide states this, and the liveness alternative with its cost.

**Deferred.** A constructor returning `(T, func(context.Context) error, error)` — the cleanup function of Wire's providers — waits for a type that is costly to wrap: a third-party API needed raw across many consumers. Adding it later breaks nothing, because the container rejects a three-value constructor; removing it after v1 would. A policy by which a terminal component failure stops the App is deferred, as are further capabilities — staged reload, degraded readiness, a signal for a failure after `Start`, introspection — each waiting for its owner.

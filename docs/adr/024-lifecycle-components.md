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

> **Current until v0.25.0 (W11):** the next item's second sentence: a `Start` or `Ready` that only the built value has is skipped without a word. The paragraph after the item replaces it.

- **The start walk is planned at registration, from the binding's type.** `Start`, `Ready` and the tier come from the type the binding shows, so the plan never constructs anything early. A `Start` or `Ready` that only the built value has is neither called nor asked, as with any value seen through an interface in Go. A type that has only `Start` is not a component, so an unrelated `Start()` is never called by accident.

> **Pending v0.25.0 (W11):** not implemented yet; until then the item above holds.

**A capability the binding's type hides is refused.** A component whose binding's type hides its value's `Start` or `Ready`, or the ingress tier its type declares ([Two tiers](#two-tiers)), is refused instead of being run without it: a value handed in at the call (`ProvideValue`) panics there, and a value a constructor builds fails the start once it is built, where the container already finds a `Shutdown` the type hides. The plan still comes from the type alone — what is refused is a value that contradicts it — and the remedy is to bind the concrete type and bind the interface with `Alias`. `credo.Borrowed()` is exempt, since the App neither starts nor stops the value, and a hidden `Shutdown` still makes the value a component whose teardown follows it (below). The values a worker constructor and a constructor handed to `Manage` build follow the same rule.

- **The teardown follows ownership.** A DI singleton the App owns is a component when it has `Shutdown` — its binding's type shows the method at registration, or its value has it once built — because the binding's type is the view its consumers need, not an ownership switch; `credo.Borrowed()` is that switch. Releasing is the owner's obligation, whereas starting and readiness are capabilities the binding hands the App. A component found only on its built value is internal, since `Finalize` cannot see it to check its tier.
- **`app.Manage(v, opts...)`** adds a component that is not a DI binding, so a component that should not be injectable, such as a DI-provided worker, gets its edges and its tier without a binding in the application's container ([lifecycle spec](../specs/lifecycle.md#appmanagev-opts)).

The [container spec](../specs/container.md#registration-options) states which bindings are components and the consequences for a value behind an interface.

### What makes a value a component

> **Pending v0.25.0 (W8):** until W8 ships, `middleware.NewRateLimiter` returns a `*RateLimiter` that implements `Component` and is handed to `app.Manage` to close its store, although its function form owns the same store without one.

A value is a component when it owns something the App must release or prepare: a goroutine, a connection it opened, a buffer to flush, state it loads at start. Being middleware is a reason neither way: a middleware that owns such a thing is a component like any other value, and one that owns nothing is not made one so that every middleware looks alike. A rate limit owns nothing of that kind — its in-memory store runs no goroutine and sweeps expired buckets inline, so closing it would only release memory early, and a store passed in through its configuration stays its owner's component — so it is a plain middleware value ([ADR-010](010-middleware-architecture.md) records the rejected alternatives).

### Resource identity: one resource, one teardown

Within one App the registry keys components by resource identity — the token a value's `ResourceIdentity()` returns, else the value itself when it is comparable — rather than by binding. Values that share an identity are one component with one teardown: one pointer under several bindings, the interface view an adapter constructor returns, a wrapper and the handle it embeds. They are shut down once, when the last holder retires, so the consumers of every holder stop first, and started once, as they are shut down once. The [container spec](../specs/container.md#resource-identity-and-teardown) states the identity order, the misuse panics and what happens to a value the registry refuses.

Grouping holders keeps the consumers' order. A holder that obtains the resource through one of its consumers — an interface view that a constructor reads from a service using the resource — depends on that consumer only to reach a value it shares, so the consumer stops first and the resource after it; without that rule the group and the consumer would each wait for the other, and neither would stop. Only such a route is dropped: a dependency that a holder which reached the resource on its own contributes — a service's dependency on a pool, when the service is itself a group with an interface view — always holds.

The application chooses that teardown, not the order of construction. The holders that have a teardown must agree on its kind, `Shutdown` or `Close` through `credo.Closer()`, and among holders of one kind the one registered first runs it, whichever is built first: registration order is fixed by the code, while a lazily built holder makes the build order unpredictable.

Kinds are all the registry can compare: Go does not tell a `Shutdown` that a wrapper inherits from one it declares, so refusing two holders of one kind with different types would refuse the wrapper and the handle it embeds. The choice therefore rests on a contract: only a value without state of its own to release may share an identity. A wrapper that embeds a handle which identifies itself inherits that identity, so a wrapper that releases state of its own holds the handle in a named field instead; a wrapper that forwards an identity promises that it keeps none. This is what lets several instances of one type be wrapper types ([ADR-004](004-dependency-injection-and-infra.md)) while each database is still closed exactly once.

One resource has one owner: `Manage` of a resource that a `ProvideValue` binding already holds panics, and so does a resource handed to `Manage` twice.

#### The holder of record

> **Pending v0.25.0 (W5):** not implemented yet. Until W5 ships, the holder registered first among those with a teardown runs it, whether a registration holds the resource or a value a constructor built; a constructor handed to `Manage` claims what its value holds, so one whose value embeds a handle bound with `ProvideValue` fails the start; and the first registered holder's tier decides the teardown's tier without a check.

Only a registration claims a resource. A binding, or a value handed to `Manage`, that holds a resource is its **holder of record** — the first registered of them when several do — and the resource's readiness entry and teardown belong to it. A value a constructor builds — a worker's, or one from a constructor handed to `Manage` — that holds the resource too **defers** to that registration: it adds no readiness entry of its own and never runs the teardown, so `/ready` shows the registration's entry, with its name and its options, and the teardown carries the registration's name and tier, even when the constructor was registered first. Without such a registration the first registered holder is the holder of record, so two worker constructors that return one value they did not create give it one entry and one teardown. A built value claims no ownership either: a worker that embeds a bound `*sqldb.DB`, or two constructors that return one singleton, share the resource's one teardown, run by its holder of record after the consumers of all of them, and a borrowed resource is torn down by none of its holders.

The rule follows from what each call states. A registration says where its value stops and how it is reported; a constructor says only how a value is built, and the value it returns may be a binding's own or a wrapper over one. Letting the first registered constructor decide would let the order of two calls move a database's teardown and rename its readiness entry. `Manage` of a constructor whose value embeds a handle bound with `ProvideValue`, with or without `credo.Borrowed()`, therefore starts, and a value handed to `Manage` is still refused beside a `ProvideValue` binding or another `Manage` of one resource.

**Registrations of one resource agree on it.** The registrations that hold one resource — bindings, and a value handed to `Manage` — agree on its tier, and the registry checks it beside the owner and kind checks: the admission of whichever value becomes known second fails, naming both registrations, their tiers and the remedies — give them one tier, or, when one of them has a teardown of its own to run in its tier, hold the handle in a named field, so that it is a resource of its own. That is a panic at registration for a value handed in (`ProvideValue`, `Manage`) and a failed start for a value a binding's constructor builds. Without the check, a raw handle bound internal and a wrapper that embeds it bound with `credo.Ingress()` give the database the tier of whichever is registered first, and with the wrapper first it is torn down beside the HTTP drain while handlers that use the raw handle still run. A borrowed resource is exempt, since the App neither starts nor tears it down. A value a constructor builds takes no part: one that defers carries no options (below), and one that is the holder of record holds a resource no registration holds.

**A value that defers takes no registration options.** A `Manage` call's options apply to the value it builds — `credo.ComponentName` to its name, `credo.Ingress()` to its tier, `credo.OptionalReadiness()` to its readiness entry — and a value that defers has no entry and no teardown of its own for them to shape. A constructor handed to `Manage` with any of the three whose value defers fails the start once the value is built, naming the holder it defers to and the remedies: move the option to that holder's registration, whose entry and teardown are the resource's, or, when the value has a readiness or a teardown of its own, hold the handle in a named field. Without options such a call starts.

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships, every holder whose type shows `Ready` adds a readiness entry of its own.

**One resource, one readiness entry.** A resource has one readiness entry, its holder of record's, whose `Ready` is asked, as it is started once and torn down once. The registrations that plan an entry for the resource agree on the options that shape it: `credo.OptionalReadiness()` is on all of them or on none, and the names given with `credo.ComponentName` are one name, which the entry and the teardown carry — the holder of record's type name without one. A disagreement fails as a tier disagreement does, a borrowed resource included, since readiness reports it. A wrapper with readiness of its own, one that overrides the `Ready` it embeds, holds the handle in a named field, so that it is a resource of its own with an entry of its own.

### Ownership and registration options

Handing a value to DI and handing over its teardown are separate decisions. A component that is discovered or handed to `Manage` is owned by the App, which starts it and shuts it down. Registration options act on components, each checked at the call; [ADR-004](004-dependency-injection-and-infra.md) records their DI surface and the [container spec](../specs/container.md#registration-options) their rules. An option is named for what the App does with the registration, so a call site reads as a statement about the App and not about the value's methods:

- **`credo.Ingress()`** places a component in the ingress tier. On `Provide` and `ProvideValue` the binding must be a component at registration, since the tier is planned before the value exists.
- **`credo.Borrowed()`**, on `ProvideValue` only, keeps the binding and the value's readiness contribution and leaves starting and shutting it down to the caller, through every holder of the resource — for a pool that two Apps in one process share, or a fixture a test suite reuses across the Apps it builds. Identity cannot express that, because it is unique only inside one App. It stays for those production uses, not only for tests.
- **`credo.Override()`** replaces an earlier binding of the same type before `Finalize` and panics when there is none, so a stale override — one whose binding the wiring dropped — fails instead of adding a binding nothing resolves. It stays a registration option.

> **Current until v0.25.0 (W12):** the next two items name the options `credo.Closer()` and `credo.Named(name)`, which W12 renames; `credo.Named` applies to `Manage` only.

- **`credo.Closer()`** makes a binding whose type has a `Close` method a component the App closes after its consumers, abandoned at the deadline like any `Shutdown`. `Close` is never discovered on its own: the method is too common to mean that the App owns the value.
- **`credo.Named(name)`** names a component handed to `Manage` in reports and readiness.

> **Pending v0.25.0 (W12):** not implemented yet; until then the two options carry the names in the item list above.

- **`credo.CloseOnShutdown()`** makes a binding whose type has a `Close` method a component the App closes after its consumers, abandoned at the deadline like any `Shutdown`. It is named for what the App does with it, as `credo.Borrowed()`, `credo.Override()` and `credo.OptionalReadiness()` are, where `Closer` read as `io.Closer`, an interface the value would implement. It stays opt-in and `Close` is never discovered: the method is too common to mean that the App owns the value, and `bun.NewDB` returns a handle that embeds the `*sql.DB` it closes without sharing its identity, so discovery would close the pool while a sibling component still uses it.
- **`credo.ComponentName(name)`** labels a component in logs, `/ready` and a `LifecycleError`; it is not a DI key. `Named` suggested one: samber/do's `ProvideNamed` and uber/dig's `dig.Name` bind a second value of one type under a name, and ADR-004 rejects such named bindings. Read as a key, two `ProvideValue` calls of one type with two names look like two bindings, and the second panics as a duplicate; the duplicate-binding panic of a registration that carries the option says that the name is not a DI key.

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships, `credo.Named` applies to `Manage` only and readiness has no option.

- **`credo.ComponentName(name)`** applies to `Provide` and `ProvideValue` as well as `Manage`, and names the binding's component in reports and readiness; like `credo.Ingress()` it panics on a binding that is not a component at registration, since the name is planned before the value exists.
- **`credo.OptionalReadiness()`** reports a component in `/ready` without letting it gate readiness, on `Provide`, `ProvideValue` and `Manage` wherever the registration plans a readiness entry, and on `AddReadinessCheck`, whose one option it is ([ADR-016](016-health-checks.md)). It is the sibling of `credo.Borrowed()`: whether a dependency gates readiness is a fact only the application knows. `Start` and `Shutdown` are unchanged by it, so a component whose readiness is optional still fails the start when its `Start` fails; a database whose outage must not stop the start says so through its module's option, `sqldb.WithOptionalStart()`, since only the module knows that its pool is usable after a failed ping.

A teardown that is not a `Close` — a client's `Disconnect(ctx)`, a `Drain` that returns at once and must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself. Consumers depend on that type, so its edges are right, and its constructor's parameters give the teardown's own dependencies theirs; `sqldb.DB` wraps Bun this way. No new API is needed.

### Hooks

`OnStart` and `OnStop` are anonymous components of a tier; a hook is a leaf action, and anything with its own teardown is a component ([ADR-006](006-application-lifecycle.md#lifecycle-hooks)).

### Two tiers

> **Current until v0.25.0 (W7):** the next paragraph's last sentence: today the tier is declared only by the registration, so a WebSocket server takes `credo.Ingress()`. The paragraph after it replaces that sentence.

**Ingress is where work enters the process**: the HTTP listener, the WebSocket server, a scheduled worker that originates its runs, a consumer of an external queue. **Internal** is everything else — infrastructure and in-process consumers alike. The tier is declared per registration, with no marker interface and no third tier.

> **Pending v0.25.0 (W7):** not implemented yet; until then the sentence above holds.

The tier is declared by the registration, or by a type whose tier follows from its nature, and there is no third tier. Such a type implements the optional capability `credo.IngressComponent`, `interface { Component; IngressTier() }`, whose method is never called; `Component` gains no method. The check needs no value, so `Provide` with a constructor, `ProvideValue`, and `Manage` of a value or of a constructor all see it, and `Finalize` validates the tiers before anything is built. `websocket.Server` declares it, so `app.Manage(ws)` needs no option. `credo.Ingress()` stays for what has no type of its own or does not declare one — hooks, an application's own consumer type, a third-party type — and on a declaring type it is redundant and accepted. A type declares the tier only when no registration could rightly place it internal, so there is no way down and no `credo.Internal()`; a worker takes its tier from its configuration. A binding whose type hides the method, such as an interface view, plans as internal, and a value that has the method is refused there, as a hidden `Start` is ([Registration and discovery](#registration-and-discovery)).

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

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships, every component entry gates readiness and a store enters `/ready` through `store.Register` instead.

A resource has one entry, its holder of record's ([Resource identity](#resource-identity-one-resource-one-teardown)). A component registered with `credo.OptionalReadiness()` is still asked under the same deadline and shows its own status, `down` when it fails, but does not gate: while every gating entry is up and an optional one is not, `/ready` answers 200 with the status `degraded` ([ADR-016](016-health-checks.md)). A bound store is an ordinary component in this: its binding's type plans its ping in `Start`, its entry through `Ready` and its teardown, with no registration step of its own ([ADR-015](015-data-access.md)).

### Every way of serving starts the components

`Run`, `RunContext` and `ServeContext` run the start walk, and `App.Start(ctx)` runs it without a listener, for an App served through `ServeHTTP` by an external `http.Server` or in a test. An App that has anything to start refuses `ServeHTTP` until the start phase has succeeded, with the treatment a stored preparation error gets: a handler served against dependencies that were not started is developer misuse, not an availability outcome ([lifecycle spec](../specs/lifecycle.md#the-start-gate-on-servehttp)).

- Whoever serves the App through `ServeHTTP` owns that server's admission and drain and completes them before `App.Shutdown`. The rule carries the order: the internal tier stops after the HTTP drain only if that drain has happened.
- `*App` has `Shutdown(ctx) error` and `Start(ctx) error`, so it is a startable component, and a child App mounted with `Mount` is handed to `parent.Manage(child)`. The internal default is the right tier, since the parent's HTTP drain first finishes the requests that run the child's handlers; the parent's server is the owner whose drain the child relies on ([lifecycle spec](../specs/lifecycle.md#mounted-apps)).

### Scope

Components are start-once; restart remains a worker concern, and supervisor trees are out of scope. Reload stays outside the component model ([ADR-020](020-reload-and-partial-config-reload.md)). The word "component" names only this abstraction in Credo's documentation and godoc.

> **Current until v0.25.0 (W2):** the log attribute `component` names the logging subsystem in the `net/http` bridge (`component=net/http`) and the store registration (`component=store`), and the component in the lifecycle's own records.

> **Pending v0.25.0 (W2):** not implemented yet; until then the attribute is used as stated above.

The log attribute `component` names the application part a framework record comes from or is about — `component=<name>`, the component's name as the lifecycle's own records, its readiness entry and a `LifecycleError` show it — and `module` names the framework package that wrote a record, the `http.Server.ErrorLog` bridge's (`module=net/http`) included ([ADR-004](004-dependency-injection-and-infra.md)). Every call the App makes on a component receives a context carrying the component's scoped `Infra`, whose logger writes that key ([lifecycle spec](../specs/lifecycle.md#the-scoped-infra-carrier)).

## Rejected Alternatives

- **Requiring both `Start` and `Shutdown` for discovery.** It would keep a singleton with an unrelated `Start()` from being started by accident, but that holds anyway, since a type with only `Start` is not a component; and it would cost every resource without a start step — the class most resources fall in — its ordered teardown, or force a no-op `Start` on it.
- **Discovering every `Close`**, or discovering it only on values the App constructs. The method is too common to mean that the App owns the value, and `Close() error` takes no deadline; neither form is what ASP.NET Core or Spring does for an instance handed in, and the case that makes discovery unsafe — `bun.NewDB` returning a handle that embeds the `*sql.DB` it closes, under another identity — arises from a constructor too. `credo.Closer()` states the ownership explicitly.
- **Keeping the name `credo.Named` with a clearer duplicate-binding message.** It helps only after the mistake; the name `credo.ComponentName` prevents the reading that causes it.
- **A `Name() string` capability.** The name is the registration's, like the tier and the ownership options, and a method would need a value before one exists.
- **Moving `credo.Borrowed()` and `credo.Override()` into `testutil`.** Both act on production wiring — `Borrowed` on a pool two Apps in one process share, `Override` on a binding that code the composition root did not write registered first — and `testutil.WithOverride` already wraps `Override` for tests.
- **A teardown-function option**, `credo.ShutdownWith(func(ctx, T) error)`. Go cannot tie the option's type parameter to `Provide`'s at compile time, so a mismatch would surface only as a registration panic, and the dependencies the function uses would have no edges, so the graph could close them first; its non-generic form, `func(ctx) error`, would fit `ProvideValue` only.
- **Promising that a component is constructed only after its dependencies have started.** The start walk could keep that promise only for what nothing resolved before `Run`, and the documented bootstrap order resolves most of the graph to register routes.
- **A value with no edges that resolves its dependencies in `Start`.** The order it needs would become a registration-order convention; `Manage` accepts a constructor instead, which gives the component its edges.
- **A `Start` that blocks for the component's lifetime**, the `Run(ctx)` shape of a restartable `Service` abstraction with a restartable/start-once taxonomy. The start walk could not tell a started component from a running one, so neither the dependency order nor the rollback would have a signal to act on; a `Service`'s name is a registration option, and restart stays a worker concern — components are start-once.
- **A per-component lifetime context**, cancelled in order just before the component's `Shutdown`. It makes `go loop(ctx)` correct, but gives one resource two stop signals, the context and its `Shutdown`, and could not bound the start work, because a deadline cannot be removed from a context once set.
- **A start that no shutdown can interrupt.** An orchestrator kills the process after its grace period anyway, so the guarantee never held, and a cancelled start rolls back where a killed one cannot. A step that must finish says so with `context.WithoutCancel`, and migrations already require transactional or idempotent work.
- **Letting the tier win over a dependency that crosses it backwards**, so that the consumer starts before the ingress component it depends on and stops after it. Safe for one broadcast implementation, but as a framework rule it breaks every component whose `Start` needs its ingress dependency started or whose `Shutdown` still uses it, and it contradicts the dependency order stated for every component. **Moving the dependent into the ingress tier implicitly** was rejected too: the declared tier would then not say where the component stops.
- **An `After`/`Before` API and a third tier.** Constructor dependencies already order components, and the one edge DI cannot see — producer to consumer — is expressed as a constructor dependency. Two tiers capture the one distinction that matters — whether work enters the process there.

> **Current until v0.25.0 (W7):** the next item: today a marker interface is rejected outright and the tier belongs to each registration only.

- **A marker interface.** The tier belongs to each registration, not to a type: one worker type can be registered in either tier.

> **Pending v0.25.0 (W7):** not implemented yet; until then the item above holds.

- **An `IsIngress() bool` or `Tier() Tier` method.** It needs a value before one exists, while the tier is planned from the binding's type at registration; `IngressTier()` is never called.
- **Inferring the tier from the graph.** A queue consumer and a cache warmer have the same shape, and only one of them is where work enters the process.
- **A module registering itself.** The registration would be hidden in the module's constructor, while a component is registered where its ownership is decided ([ADR-004](004-dependency-injection-and-infra.md)); a type that declares its tier lets the application's one `Manage` or binding call place it.
- **`Name`, `Tier`, ordering, restart, liveness, metrics and tracing as methods of a component.** The name is `credo.Named`, the tier `credo.Ingress()`, ordering the constructor's dependencies; restart and pause are a worker concern; a component in the liveness check would turn a dependency's outage into a restart of every replica; metrics and tracing belong to the observability release. Each would also be a method `Component` could never add without silently dropping types from the graph.
- **The name `Boot` for `App.Start`.** In Laravel and Symfony it names the container's initialization phase, closer to `Finalize`, and it would break the pairing with `OnStart` and `Starter`. `Start` pairs with `App.Shutdown` and has the meaning fx's `App.Start` and .NET's `IHost.StartAsync` give it, with `Run` as start, wait and stop. Because `Start(addr)` listens in other frameworks, its godoc opens by saying that it runs the start phase without a listener.
- **The App counting in-flight `ServeHTTP` calls and waiting for them in `Shutdown`.** An atomic operation on every request for a guarantee it still could not give, since the owner's listener keeps admitting requests; `http.Server.Shutdown` already stops admission and waits.
- **Deciding a component's teardown from its binding's static type.** An interface that omits `Shutdown` would silently end the teardown of a value the App built — a producer with what it buffers. A warning, in debug mode or always, reports that loss without preventing it.
- **Calling `Shutdown` once per binding that holds a value.** It shuts one resource down several times, the first time while consumers of another holder may still run. Calling `Start` once per binding starts one resource several times, and starts a borrowed resource that a wrapper over it shows `Start` for.
- **Keeping every holder's dependencies on the shared resource.** A holder that reached the resource through a consumer would make the resource wait for that consumer while the consumer waits for the resource, and neither would be shut down. Dropping every non-first holder's dependencies instead would let what a wrapper's own fields use stop before the wrapper's consumers, and dropping every edge on the cycle would drop a real dependency too when two groups meet: the service group's dependency on the pool group would go with the pool view's route through the service. Only an edge that a holder which reached the resource through another holder contributes, and that closes a cycle, is a route rather than a dependency.
- **Releasing every value the registry refuses**, or none. A value refused for a holder conflict is the shared resource, so releasing it closes what its owner — possibly the caller of `credo.Borrowed()` — still holds; releasing none leaks a value only the App ever held.
- **Keying components by pointer alone, with "register pointers" as the rule.** It misses the value-shaped wrapper that several instances of one type require — `type PrimaryDB struct{ *sqldb.DB }` — whose interface view and raw handle would each be shut down again, silently closing the database twice.
- **Choosing among the teardowns that the holders of one resource declare by build order**, which a lazily built holder makes unpredictable and which may pick a holder without a teardown; or **by a fixed precedence of `credo.Closer()` over `Shutdown`**, which would skip a teardown the application wrote, a wrapper's flush, without a word.
> **Current until v0.25.0 (W11):** the next item, which W11 reverses: today a hidden `Start` or `Ready` is skipped.

- **Refusing a component whose value has a `Start` or `Ready` that its binding's type hides.** It guards no loss — unlike a hidden `Shutdown`, a hidden `Start` or `Ready` leaves nothing unreleased; it would break a value that an `OnStart` hook starts behind an interface; and its failure could first surface at a request for a lazily built value. **A warning in its place** would fire on that documented pattern.

> **Pending v0.25.0 (W11):** not implemented yet; until then the item above holds.

- **Skipping a capability the binding's type hides.** A component that ran unstarted, or a readiness entry that was never asked, was found only once the defect had shipped; the refusal names the binding and the remedy at the call or at the start. **A warning in its place** is read once the defect has shipped as well.
- **Calling a `Start` found on the built value.** It would make the plan depend on a value, while the start walk, the tiers and the `Finalize` checks are planned from types before anything is built.

## Consequences

The order a resource stops in follows from the graph rather than from which hook an integration chose: the last job an HTTP handler hands to an internal component is processed before the drain reaches the database; a failed start stops exactly the components that were built, except the one whose `Start` failed; and a resource is shut down once, after the consumers of every holder.

**Costs.**

- **One drain deadline, spent in order.** Both tiers share the deadline, and a correct order spends it sequentially, so a long HTTP drain leaves less for the internal tier. The cost is documented, not hidden.
- **The ingress tier is one more concept**, and choosing it wrongly has asymmetric effects ([ADR-023](023-worker-system.md#each-worker-is-a-component-the-supervisor-is-a-registry)).
- **The documented double start.** Where a binding's type shows both `Start` and `Shutdown`, an `OnStart` hook that also starts such a value makes it start twice; behind an interface that hides either method, the hook alone starts it.

> **Pending v0.25.0 (W11):** the next item replaces the previous one's last clause; until W11 ships, a value behind an interface that hides `Start` is started by the hook alone.

- **A value started by a hook behind an interface is bound differently.** A binding whose type hides its value's `Start` is refused, so such a value is bound as its concrete type with the interface as an `Alias`, and the start walk starts it in place of the hook, or bound with `credo.Borrowed()` when its caller both starts and stops it.
- **Readiness has a stated limit** ([Start](#start)): a component that has stopped for good leaves an alive, unready process that is never restarted. The deployment guide states this, and the liveness alternative with its cost.

**Deferred.** A constructor returning `(T, func(context.Context) error, error)` — the cleanup function of Wire's providers — waits for a type that is costly to wrap: a third-party API needed raw across many consumers. Adding it later breaks nothing, because the container rejects a three-value constructor; removing it after v1 would. A policy by which a terminal component failure stops the App is deferred, as are further capabilities — staged reload, degraded readiness, a signal for a failure after `Start`, introspection — each waiting for its owner.

# Bootstrap Simplification — Delivery Plan

**Status:** Decisions accepted 2026-10-13, W11's on 2026-11-08, and W12 and the additions to W1, W3 and W5 on 2026-11-09; W1 promotes them into the ADRs and specs; implementation pending. **Release:** v0.25.0. **Progress source:** [TODO.md](../../TODO.md#bootstrap-simplification). This plan defines scope, sequence and acceptance; progress checkboxes live only in TODO. When the last work item has shipped, fold what remains into the ADRs and specs, repoint every link to this file, and delete it.

v0.25.0 shortens the composition root to what only the application knows. A bound store is an ordinary component: the type of its binding plans the ping, the readiness entry and the teardown, and `store.Register` goes; every readiness entry reports its latency, and one registered with `credo.OptionalReadiness()` — a binding's, or a check's added with `AddReadinessCheck` — is reported without gating readiness: while it fails, `/ready` answers 200 with the status `degraded`. A resource has one readiness entry however many registrations hold it, and a database opened with `sqldb.WithOptionalStart()` does not stop the start while it is down. The first `Resolve` finalizes the container. A component type whose tier follows from its nature declares it, so a WebSocket server needs no registration option. A framework module that the App starts logs to the App's logger without taking `credo.Infra` as a parameter, and the framework's records name the application part they come from or are about with one key, `component`. Only a value that owns something to release or prepare is a component, so the rate limiter's object form goes, and rate limiting moves into a package of its own so that only its users compile go-limiter. Workers become a subsystem of the App — `app.Workers()`, in the root package — registered as a value, as a run function whose parameters the container supplies, or as a constructor the container builds, and the `worker` package and `internal/kernel` go. Framework components are not bound in the container automatically, and DI stays in the core, under a rule that says what lives in the root package. Two registration options are renamed for what they do: `credo.Named` becomes `credo.ComponentName`, a component's name and not a DI key, and `credo.Closer` becomes `credo.CloseOnShutdown`. `Provide` takes its binding's type from the constructor, a capability that type hides is refused instead of skipped, and an alias is a binding: an override of an aliased interface, which panics today, replaces it. Two defects of nested validation found while preparing the release are fixed.

```go
app.ProvideValue(db)
ws := websocket.New()
app.Manage(ws)
app.Workers().Continuous("outbox-relay", NewOutboxRelay)
app.Provide(NewUserController)
uc := app.MustResolve[*UserController]()
app.GET("/users", uc.List)
app.GET("/ws", ws.Handler(chatHandler))
```

## Contract map

| Work | Canonical decision | Detailed contract | Release |
| --- | --- | --- | --- |
| W1: promotion and the decisions that change no code | this plan; [ADR-002](../adr/002-code-acquisition-strategy.md), [ADR-004](../adr/004-dependency-injection-and-infra.md), [ADR-022](../adr/022-bootstrap-and-di-ownership.md) | — | v0.25.0 (documentation) |
| W2: the logger of a framework module | [ADR-004](../adr/004-dependency-injection-and-infra.md), [ADR-019](../adr/019-websocket-integration-and-drain.md) | [Lifecycle](../specs/lifecycle.md), [WebSocket](../specs/websocket.md) | v0.25.0 |
| W3: a store is an ordinary component | [ADR-015](../adr/015-data-access.md), [ADR-016](../adr/016-health-checks.md), [ADR-024](../adr/024-lifecycle-components.md) | [Store](../specs/store.md), [Health](../specs/health.md), [Lifecycle](../specs/lifecycle.md), [Container](../specs/container.md) | v0.25.0 |
| W4: workers in the root package | [ADR-023](../adr/023-worker-system.md) | [Worker](../specs/worker.md) | v0.25.0 |
| W5: worker constructors; built values claim nothing | [ADR-023](../adr/023-worker-system.md), [ADR-024](../adr/024-lifecycle-components.md) | [Worker](../specs/worker.md), [Lifecycle](../specs/lifecycle.md), [Container](../specs/container.md) | v0.25.0 |
| W6: the first `Resolve` finalizes | [ADR-022](../adr/022-bootstrap-and-di-ownership.md), [ADR-004](../adr/004-dependency-injection-and-infra.md) | [Container](../specs/container.md#sequential-bootstrap) | v0.25.0 |
| W7: a type declares its ingress tier | [ADR-024](../adr/024-lifecycle-components.md), [ADR-019](../adr/019-websocket-integration-and-drain.md) | [Lifecycle](../specs/lifecycle.md), [WebSocket](../specs/websocket.md) | v0.25.0 |
| W8: rate limiting: not a component, in its own package | [ADR-010](../adr/010-middleware-architecture.md), [ADR-024](../adr/024-lifecycle-components.md), [ADR-002](../adr/002-code-acquisition-strategy.md) | [Middleware](../specs/middleware.md) | v0.25.0 |
| W9: examples, guides and the release | — | [Pre-v1 migration guide](../guides/pre-v1-migration.md) | v0.25.0 |
| W10: nested validation fixes | [ADR-011](../adr/011-validation-strategy.md) | [Validation](../specs/validation.md) | v0.25.0 |
| W11: `Provide` binds what its constructor returns | [ADR-004](../adr/004-dependency-injection-and-infra.md), [ADR-024](../adr/024-lifecycle-components.md) | [Container](../specs/container.md), [Lifecycle](../specs/lifecycle.md) | v0.25.0 |
| W12: `ComponentName` and `CloseOnShutdown` | [ADR-024](../adr/024-lifecycle-components.md), [ADR-004](../adr/004-dependency-injection-and-infra.md) | [Container](../specs/container.md#registration-options), [Lifecycle](../specs/lifecycle.md) | v0.25.0 |

Breaking changes are allowed before v1, and one minor carries all of this work, so that each break reaches applications once: `store.Register` and its options, the `Infra` parameter of `websocket.New`, `NewRateLimiter` and the package of `RateLimit`, the provided-worker methods and the `worker` package, `store.Lifecycle`, `(*sqldb.DB).StoreRegistrationWarningCodes`, the type parameter of `Provide`, a direct binding after an alias of its type, the names `credo.Named` and `credo.Closer`, and the log keys `service` and `worker` and the `net/http` bridge's `component`. Each is named under its own CHANGELOG heading and in the migration guide's v0.25.0 section, and the release notes put first the changes that compile unchanged but behave differently. The implementation does not reopen the accepted decisions by adding options, modes or parallel APIs.

## Sequence

W1 lands before any code: it writes the decisions into the ADRs and specs, each promoted section marked pending until its work item ships, and adds the TODO section. The code follows W11 → W12 → W2 → W3 → W4 → W5 → W6:

- W11 comes first: it rewrites every `Provide` call in the repository, so the later items' code, tests and examples are written once in the new form, and W5's holders and W7's tier find the refusal of hidden capabilities in place.
- W12 follows, so that W2's carrier and W3's readiness options are written with the names the options keep, and W3 opens `credo.ComponentName` to `Provide` and `ProvideValue` under its final name.
- W2 comes before W3 because `sqldb`'s `Start` (W3) writes its warnings through the context carrier W2 adds.
- W3 removes `kernel.RegisterStore`, which leaves the worker as `internal/kernel`'s last client.
- W4 moves the worker into the root package and deletes `internal/kernel`.
- W5 is then written where the worker lives, not in a package about to move.
- W6 lands last, so that its message for a worker registration comes from the root's registration code, with no substring rewriting to keep alive.

W7 follows W2, since both change `websocket/server.go`. W8 and W10 touch separate code and may land at any point after W1. Once W5 has landed, the split of `internal/di` and the division of `App`'s implementation are judged (Out of scope). W9 closes the branch.

Each work item is one or more theme commits, each green on its own. The branch `release/v0.25.0` ends with the release preparation commit and becomes one pull request, rebase-merged after review and the required checks, and the `Release` workflow tags the preparation commit ([CONTRIBUTING](../../CONTRIBUTING.md#releasing)). The work items keep their numbers, so that references to them stay stable.

**Messages.** An error message is part of the API. Every error and panic this release adds or changes names what failed — the call, the binding or the component — and the remedy:

- a registration after the container was finalized, with what finalized it, where, and the calls that led there (W6);
- a worker registration given none of the forms — a worker, which is a `Worker` or a `func(context.Context) error`; a run function, which takes one `context.Context` and returns only an `error`; a constructor of a worker — naming the forms accepted, and a function that takes two contexts or takes a context and returns anything but a lone `error`, naming the run function's shape (W5);
- a constructor handed to `Manage` with `credo.ComponentName`, `credo.Ingress()` or `credo.OptionalReadiness()` whose value defers to its resource's holder of record, naming that holder and the remedies (W5);
- a worker's built value that depends on an ingress component, with the path and the remedy that fits: splitting the worker, or, when that component is the binding whose type the constructor returns, binding it without `credo.Ingress()` (W5);
- two registrations that hold one resource with different tiers, naming both, their tiers and the remedies (W5);
- two registrations that hold one resource and disagree on `credo.OptionalReadiness()` or on a `credo.ComponentName` name, naming both, the option and the remedies (W3);
- a failed `sqldb` ping, with the component's name and the deadline (W3);
- `credo.ComponentName` on a binding that is not a component (W3);
- `credo.OptionalReadiness()` on a registration that plans no readiness entry: a type without `Ready`, or one that is neither a component nor borrowed (W3);
- an option other than `credo.OptionalReadiness()` given to `AddReadinessCheck`, naming it and the one option accepted (W3);
- a WebSocket server that was never started (W7);
- a binding whose type hides `Start`, `Ready` or the ingress tier of its value, naming the binding, the methods it hides and the remedy of binding the concrete type with an alias (W11);
- a direct binding of a type bound through an alias, naming the alias and `credo.Override()` (W11);
- a `Resolve` of a type that `Finalize` dropped, naming the override that left it unreached (W11);
- an option other than `credo.Override()` given to `Alias` (W11);
- a duplicate binding when either registration carries `credo.ComponentName`, saying that the name labels the component in logs, `/ready` and lifecycle errors and is not a DI key, beside the remedy the message already gives, a wrapper type of its own for the second value (W12).

The messages speak the application's terms — its calls, types and options — not the lifecycle's: "holder of record" and "defers" name rules of ADR-024 and appear in no message. A `Manage` call with `credo.ComponentName` whose value defers, for example, reports: `*Relay shares the database of the ProvideValue binding of *sqldb.DB, whose readiness and shutdown follow that binding; give credo.ComponentName to that binding, or hold the database in a named field`.

Each test that triggers one asserts those parts of its message, so a message changes only on purpose.

**Verification.** Every commit runs `go vet`, the shadow analyzer, `gofmt`, the CI linter configuration and `go test -race` over every package except the ignored `tmp/` tree, plus `store/sqldb`'s tests in its own module from W3 on. Commits that change the lifecycle also run the root package's lifecycle tests with `-race -count=20`. Worker commits run the worker tests — the root's and `internal/worker`'s — with `-race -count=20`. Before the release commit, a `git grep` for each name W2–W8, W11 and W12 delete or rename, `Provide[`, `credo.Named` and `credo.Closer` among them, and for `credo.Ingress()` beside a WebSocket server, finds it only in the migration guide, published release notes and past CHANGELOG sections, which stay as they are. The sweep covers package READMEs and `doc.go` files as well as the ADRs, specs, guides, TODO and examples.

## W1: promotion and the decisions that change no code

**Promotion.** Each decision is written into its canonical ADR and its detailed spec (the contract map), with a pending marker naming v0.25.0 that the implementing work item removes:

- **ADR-002** records the root package's module allowlist, which only CONTRIBUTING and `architecture_test.go` record today, as a decision, together with its two rejected alternatives, and beside it the rule that places a package. A subsystem the App holds one of and reaches through `app.` lives in the root: routing, DI, the lifecycle, health, i18n, the `Use*` features and the workers, and later the observers and the application modules that the pre-v1 reshaping adds. A value the application builds and hands to the App lives in a package: `sqldb`'s database, the WebSocket server, a middleware such as CORS or a rate limit, a JWT authenticator, and later the pubsub adapters. A package the App never touches stays a package: `config`, `validation`, `fault`. The allowlist is the rule's constraint: a subsystem that needs a third-party module lives in a package however closely the App holds it. ADR-004 (DI stays in the core) and ADR-023 (workers in the root package) cite both.
  - A root that is stdlib-only transitively, with configuration loading made opt-in: `credo.New()` loads YAML automatically so that a setup works with minimal configuration (ADR-001), which puts a YAML parser in the graph.
  - YAML as the only entry, with `x/text` behind a `Translator` interface and mapstructure replaced by a native decoder: mapstructure stays until it is replaced, and `x/text` enters through `internal/i18n`, which the error pipeline and `UseI18n` import, so an interface alone does not remove it. `x/text` is maintained by the Go team and carries CLDR data, so allowlisting it is the honest choice.
- **ADR-015 and ADR-016: W3.** Every readiness entry carries its latency. ADR-016's deferred optional/critical decision is taken for readiness: `credo.OptionalReadiness()` reports an entry without letting it gate, a registration's or that of a check added with `AddReadinessCheck`, whose one option it is; `AddLivenessCheck` takes none. A failing optional entry shows `down`, and while every gating entry is up the top-level status is `degraded`, answered with 200. A database whose outage must not stop the start is opened with `sqldb.WithOptionalStart()`, an option of the module, which alone knows that its pool is usable after a failed ping; a component in general whose failed `Start` does not fail the start stays deferred, since it would need a reconnect contract. A resource has one readiness entry, its holder of record's, and the registrations that hold it agree on `credo.OptionalReadiness()` and on a `credo.ComponentName` name. Gating stays the default, and ADR-016 records its cost: a database's ping waits in its pool's queue while every connection of a bounded pool is busy; a readiness ping on a connection outside the pool stays deferred. A component that reports `degraded` itself, through a status capability beside `Ready`, stays deferred too: no component produces such a status — `sqldb` derives no DEGRADED, which would need an SLO, windowed deltas and hysteresis — and the capability can be added later without a break. `store.Lifecycle` goes with `store.Register`, its only consumer, and `store.Health` stays as what `sqldb`'s `Health` returns. Rejected alternatives:
  - `store.Register` kept as optional sugar beside the plan from the binding's type: two ways to one ping and one readiness entry, with ordering and duplicate-name rules between them.
  - The health engine recognizing `store.Lifecycle` on any component: the root cannot import `store`, and an internal seam for one interface rebuilds the store registry under another name.
  - Readiness without a start ping: it drops the guarantee that an unreachable database fails the start.
  - Moving `store.Health` into the root to keep the typed status in `/ready`: it inverts the dependency direction.
  - Dropping latency with the store branch: the database's entry is the one that shows it today, and a slow dependency is what a probe dashboard looks for first.
  - A database that gates readiness only when the application asks, through `AddReadinessCheck` or an option: Spring Boot's default, for the shared outage that `credo.OptionalReadiness()` answers per binding; as the default it gives a database's `Ready` a rule no other component's follows, brings back the registration step this release removes, and keeps in rotation a process that has lost its database while the others have not.
  - An entry per binding of one resource: no documented wrapper overrides the `Ready` it embeds, and two entries ping one database twice on every probe and leave `credo.OptionalReadiness()` on one of them without effect.
  - A start forgiven for every registration whose readiness is optional: the App cannot know whether a component is usable after its failed `Start`, which is why the option is the module's.
  - A wrapper that holds a database in a named field and does not forward `Ready`, to keep it from gating readiness: the field hides `Start`, `Shutdown` and `ResourceIdentity` too, and a forgotten forward is silent.
  - Leaving a non-gating entry out of `/ready`: an operator could no longer see it fail.
  - The name `credo.NonCritical()`: it names criticality in general, so it promises that a failing dependency does not stop the start either.
  - A failing optional entry that reports `degraded` instead of `down`: the entry would hide that its dependency is down.
  - An optional component whose dependants are refused at `Finalize` and whose failed `Start` lets the App continue: unusable for a database that repositories use, and the database stays closed after its failed `Start`.
  - Keeping `store.Lifecycle` as the adapter contract: once `store.Register` goes, no code checks it.
  - A module option that makes `Ready` succeed: an entry that reports up while its database is down.
- **ADR-004** carries five decisions:
  - **W2's logger rule**, with its rejected alternatives: a required logger option per module, repeated for every module; `credo.Infra` as the first parameter of every module constructor, the same repetition at every construction; a logger registration option on the DI methods, a logging concern on the container surface; a setter capability the App calls after build, hidden field population racing a value in use; and a warnings capability the start walk logs, which solves one log line and not the next module's logging. The principle that infrastructure travels through the visible `credo.Infra` carrier gains the context carrier as the path for module authors: only framework infrastructure travels in the context, never application services. One key names an application part in the framework's records: `component=<name>`, the key and value the lifecycle's own records use, written by the carrier's logger, by the logger a constructor receives, with the name of the binding it builds, and by `app.NewInfra(name)`; `service` is retired. `module` names the framework package that wrote a record, the `http.Server.ErrorLog` bridge's included. Rejected: `service` as the one key, since OpenTelemetry's `service.name` names the application and the lifecycle's records already use `component`; a key per role, `service` for the code that wrote a record and `component` for the part a framework record is about, since the carrier's logger serves a module's code and the application's alike; and keeping the constructor's `service`, whose value drops the package and the pointer, until a later revision, which leaves a component logging under two keys and breaks the keys twice.
  - **No automatic bindings.** Framework components are never bound in the container automatically. The one framework-made binding stays `RawConfig`. A hidden binding collides with `Override` and with a second instance of one type, a binding is created where ownership is decided, and infrastructure that many constructors need is never a binding: if the observability release gives constructors metrics or tracing, they arrive as `Infra` fields.
  - **DI stays in the core.** The one-line rejection of a separate public package is replaced by its reasons:
    - `app.Resolve[T]` exists because the API is a method of the App. A package would offer either the free-function form that Go 1.27's generic methods removed, or a container value that travels, which is the service-locator door.
    - Start order, drain order, resource identity and tier validation all run on the DI graph.
    - A public package would have to export the hook that produces framework parameters.
    - DI is already optional, and a CLI or batch job builds an App, resolves and shuts it down with no listener.
    - The root package links only its allowlist of modules.

    The cost accepted is the size of the root package, answered by grouping its package documentation by subject (W9); the symbol index of `go doc` and pkg.go.dev stays alphabetical.
  - **W6's implicit `Finalize`**, together with ADR-022.
  - **W11's binding type and aliases.** `Provide` binds its constructor's result type; an alias is a binding for the duplicate and override rules, `Alias` takes `credo.Override()` as its one option, and `Finalize` drops the type an overridden alias left unreached. Rejected: writing the type arguments and keeping `Provide[T]`, whose type argument the compiler cannot check against a constructor typed `any`; both forms side by side; dropping the alias's target at the `Override` call, before the registrations that may still need it; and keeping it built while the interface resolves the override.
- **ADR-022: W6.** The registration window ends at the first resolving call or at `Finalize`, whichever comes first. Rejected alternatives: keeping the explicit call, a ceremony that guards nothing an implicit freeze does not; `app.Build()` returning a separate resolver, the same step with a new type; and finalizing on `Has`, which registration uses to decide what to register. `MustResolve` and `MustResolveAll` stay: after the implicit `Finalize` their panic carries the `Finalize` report or a constructor's failure at the composition root, the startup misuse the panic rule covers.
- **ADR-024** carries five changes:
  - **W7's `IngressComponent`.** "Two tiers" and the rejected marker interface are reworded: the tier is declared by the registration, or by a type whose tier follows from its nature. Rejected: an `IsIngress() bool` or `Tier() Tier` method, which needs a value before one exists; inferring the tier from the graph, since a queue consumer and a cache warmer have the same shape; and a module registering itself.
  - **Two options renamed, the others kept, and one added**, each with its reason:
    - `credo.Closer()` becomes `credo.CloseOnShutdown()` (W12), named for what the App does with it, as `credo.Borrowed()`, `credo.Override()` and `credo.OptionalReadiness()` are, where `Closer` reads as `io.Closer`. It stays opt-in and `Close` is never discovered. `bun.NewDB` returns a handle that embeds the `*sql.DB` it closes, and the two do not share an identity, so discovery would close the pool while a sibling component still uses it.
    - `credo.Named(name)` becomes `credo.ComponentName(name)` (W12). The name labels a component in logs, `/ready` and lifecycle errors and is not a DI key, which `Named` suggests: samber/do's `ProvideNamed` and uber/dig's `dig.Name` bind a second value of one type under a name, and ADR-004 rejects such named bindings. Read as a key, two `ProvideValue` calls of one type with two names look like two bindings, and the second panics. Rejected: keeping `Named` with a clearer duplicate-binding message, which helps only after the mistake.
    - `credo.Borrowed()` stays, with its production uses.
    - `credo.Override()` stays.
    - `credo.OptionalReadiness()` is added (W3), the sibling of `credo.Borrowed()`: whether a dependency gates readiness is a fact only the application knows. Rejected names: `credo.WithoutReadiness()`, since `With` and `Without` name the options of `credo.New`; and `credo.NonCritical()`, which names criticality in general while the option changes readiness only.

    Recorded as rejected: discovering every `Close`, and discovering it only on values the App constructs — neither is what ASP.NET Core or Spring does for an instance handed in, and the bun case arises from a constructor too; a `Name() string` capability; moving `Borrowed` and `Override` into `testutil`.
  - **W8's component rule.** A value is a component when it owns something the App must release or prepare — a goroutine, a connection it opened, a buffer to flush, state it loads at start. Being middleware is a reason neither way.
  - **W5's holders that claim nothing, and the holder of record:** a resource's readiness entry and teardown belong to a binding or a value handed to `Manage` that holds it, and a value a constructor builds that holds it too defers to that registration.
  - **W11's hidden capabilities.** A component whose binding's type hides its value's `Start`, `Ready` or ingress tier is refused — a value handed in at the call, a built one at the start — where a `Start` only the built value has was never called; `credo.Borrowed()` is exempt, and a hidden `Shutdown` still makes the value a component. Rejected: a warning, read once the defect has shipped; and calling a `Start` found on the built value, which would make the plan depend on a value.
- **ADR-010: W8.** `ratelimit.Middleware(cfg...)`, in `middleware/ratelimit`, is the one constructor. Rejected: a no-op `Component` on every middleware; "middleware is never a component"; keeping `NewRateLimiter` without its lifecycle for store sharing.
- **ADR-019:** W2's `websocket.New(cfg...)` and `Config.Logger`, and W7's declared tier and unstarted-server message.
- **ADR-023, rewritten for W4 and W5.** Its package placement, its section on DI-provided workers, and its rejection of a "one supervisor per App" slot in the kernel are rewritten. In the root package the registry is a field of the App, and that rejection's reason — duplicate names are caught across registries — never addressed a snapshot that covers every worker. `worker.Provide[T](app, constructor)`'s rejection is restated: it belonged to a worker that is a binding, and a constructor that creates no binding duplicates nothing. The placeholder value (`worker.Provided[T]()`) stays rejected. The run function is recorded as a third form, with the method expression of a binding's `Run` as the form of a worker that other code uses. Recorded as rejected:
  - keeping the `worker` package, whose registries are separate by construction;
  - methods flat on the App;
  - one `Add` method with the kind as an argument, and its variants that infer or default the kind;
  - the registry in the root with the types in `worker`, an import cycle through `credo.Tier`;
  - root aliases of internal types;
  - the provided form kept beside the constructor form;
  - a typed constructor method;
  - refusing a bare `func(context.Context) error` with the remedy of wrapping it, a conversion the registry can make itself;
  - a `WorkerFunc` type beside the bare function, a second spelling of one worker;
  - registering the constructor as a binding inside the registration;
  - a handle that names a binding as the worker, such as `credo.Bound[T]()`: with `w any` it could be a descriptor the registration recognizes rather than a `Worker` that cannot run, but it is the provided form under another spelling, a second path to one result and a root name, and it removes no rule, since a constructor that returns a binding, whether a worker's or one handed to `Manage`, needs W5's holder of record anyway, while the method expression of the binding's `Run`, `(*OutboxRelay).Run`, already names the binding, in a form the compiler checks;
  - a run function whose `context.Context` must come first: a method expression puts its receiver first, so the bound worker's form would not register;
  - the worker's component calling the built value's `Shutdown` directly;
  - never tearing the built value down.
- **Specs:** store, health, lifecycle, container, WebSocket, worker and middleware.

**TODO.**

- A section "Bootstrap Simplification" above the v0.24.0 section, with one box per work item and one for the checkpoint that judges the split of `internal/di` and the division of `App`'s implementation.
- The v0.24.0 section closes: its last box, examples, guides and the release, is checked, and "only the release remains" becomes "shipped in v0.24.0".
- Architecture Governance, first line: "**Kernel + modules model**: the kernel is the root package with its subsystems — routing, DI and components, health, i18n and workers — the packages it builds on (`config`, `validation`, `fault`, `internal/`) and the first-party `middleware`, and must be stable first; modules are the packages that build on the kernel and that it never imports: adapters over a third-party engine or protocol (`store/sqldb`, `websocket`, later `pubsub` and `grpc`) and standalone utilities (`httpclient`)."
- Architecture Governance, second line: "Modules (`store/sqldb`, `websocket`, later `openapi`, `pubsub` and `grpc`) mature independently through their own maturity labels; a kernel subsystem matures with the root package." Health and i18n already contradicted the old line, which listed them as optional modules.
- v1 Gate, the batch of deferred breaking changes: "applied in one batch at v1.0.0, each announced one minor ahead in CHANGELOG" becomes "each shipped in a minor before the stability window opens, under its own CHANGELOG heading". A batch applied at the tag would break after the two minors without breaking changes that the gate's stability evidence requires, and the release cadence announces no break in advance. The `RequireContentType` items (4.7) and the `WithStrictDecoding` item drop "announce one minor ahead"; whether either default flips is not decided here.
- v1 Gate, the observability item: it first decides whether `credo.Infra` gains metrics and tracing fields at all — instrumentation may instead reach requests through an observer of the final response — and only then their types.

## W2: the logger of a framework module

**Change.**

- **The carrier.** Every call the App makes on a component — `Start`, `Ready`, `Shutdown`, and the `OnStart` and `OnStop` hooks — receives a context carrying the component's scoped `Infra`, read with `credo.InfraFrom(ctx) credo.Infra`. The scoped `Infra` carries the App's logger with `component=<name>`, the component's unit name: the name its readiness entry, a `LifecycleError` and the lifecycle's own records show, and the one `credo.ComponentName` sets. `app.NewInfra(name)` builds a different scope, for code outside such a call, with the same key (below). Outside such a call `InfraFrom` returns the zero `Infra`, whose logger falls back to the framework default. `context.WithoutCancel` keeps values, so work derived from the `Start` context sees the same `Infra`. `InfraFrom`'s godoc names the other two paths, since a call in a handler compiles and logs to the framework default: request code logs through `c.Logger()`, the request's logger or else the App's, and a constructor takes `credo.Infra`. The root installs the carrier around each call `internal/di` makes, through an internal hook, and around each readiness check its aggregation runs.
- **One key names an application part.** The logger a constructor receives through an `Infra` parameter writes `component=<name>` too, with the name of the binding it builds — its type as the lifecycle prints it (`*users.Service`), or its `credo.ComponentName` name — and `app.NewInfra(name)` writes `component=<name>`. `service` is retired: its value dropped the package and the pointer (`deriveServiceName`), so `users.Service` and `orders.Service` both wrote `service=Service`, and a component logged under `service` from its constructor and under `component` in the lifecycle's records. The `http.Server.ErrorLog` bridge writes `module=net/http` in place of `component=net/http`, and the worker's records take the key in W4. A stable event identity for framework records stays with the observability release: it adds a key and renames none.
- **The rule.** A framework module whose value is built outside the App and started by it takes an optional logger in its own configuration and otherwise uses the App's. Precedence: the logger given to the module, unscoped; then `InfraFrom(ctx).Logger`; then the framework default. Every log a module writes carries `module=<name>`, added with `Logger.With`, whichever logger it goes to. The App's own subsystems — the `Use*` features and the worker registry — already log to the App's logger and are outside the rule. `httpclient` stays credo-free.
- **`websocket.New(cfg ...Config)`** replaces `websocket.New(infra, cfg...)`, and `Config.Logger` is the module's own logger.
  - The server settles its logger in `Start` and refuses upgrades until then, so no request reads an unset logger. `slog.Default()` as a second default goes.
  - Request-time logs follow the access-log rule. Without `Config.Logger`, the connection logs — opened, closed, failed, close failed, handshake transport failure — stay on the request's logger with `connection_id`, and gain `module=websocket`.
  - With `Config.Logger`, every log goes to it. Each connection log carries `connection_id`, and `request_id` from `ctx.RequestID()` when the request ID feature is active. The server-level "shutdown incomplete" record carries neither.
- `sqldb.WithLogger(l)` lands with W3, where `sqldb` first logs.

**Tests.**

- `InfraFrom` inside `Start`, `Ready`, `Shutdown`, `OnStart` and `OnStop` returns the component's scoped `Infra`: a record written through its logger carries `component=<name>`, the key and value of the lifecycle's records about that component, and a component named with `credo.ComponentName` carries its given name. The same holds inside a goroutine whose context derives from `Start`'s through `context.WithoutCancel`. Outside a call, `InfraFrom` returns the zero `Infra`.
- A constructor's `Infra` logger writes `component=<name>` with its binding's name: `*users.Service` and `*orders.Service` stay apart, and a binding named with `credo.ComponentName` writes its name. `app.NewInfra(name)` writes `component=name`. No framework record carries `service`, and the `ErrorLog` bridge writes `module=net/http`.
- WebSocket without `Config.Logger`: a connection log carries `connection_id`, `module=websocket`, `request_id` under `UseRequestID`, and the application's `AddLogAttrs` enrichment.
- WebSocket with `Config.Logger`: every record reaches it. A connection log carries `connection_id` and the explicit `request_id`, "shutdown incomplete" carries neither, and the request's enrichment is absent.
- A routing `slog.Handler` that keeps the attributes it receives through `WithAttrs` sends the `module=websocket` records to a separate sink. This is the recipe the guides print.

**Documentation.** ADR-004, ADR-019, the lifecycle spec's carrier and the WebSocket spec flip. ADR-004 and the container spec state the keys — `component` for the application part a record comes from or is about, `module` for the framework package that wrote it, beside `request_id` and `connection_id` — and the container spec's scoped-logger examples, the lifecycle spec's `ErrorLog` paragraph, the deployment guide's filter and TODO's per-service scoping line move to them. Migration rows: `websocket.New(app.NewInfra("websocket"), cfg)` becomes `websocket.New(cfg)`; a logger of the module's own goes in `Config.Logger`; the connection logs gain `module=websocket`; `service=Service` becomes `component=*users.Service`, the binding's type as the lifecycle prints it, or its `credo.ComponentName` name, and `app.NewInfra(name)` writes `component=name`; the `ErrorLog` bridge's `component=net/http` becomes `module=net/http`.

## W3: a store is an ordinary component

**Change.**

- **`Start`.** `*sqldb.DB` gains `Start(ctx)`. It pings under `Config.PingTimeout`, a new field: zero means `sqldb.DefaultPingTimeout` (5 s, moved from `store`), and a negative value fails `Open`. Unlike `ConnectTimeout`, zero is not "no deadline", because an unbounded ping would hang the start; the field's godoc says so.
  - On success, `Start` logs each pool warning once at Warn, to its logger: `sqldb.WithLogger(l)`, else `InfraFrom(ctx).Logger`. The code — today only `sqldb.pool.max_open_unlimited` — is read from the effective pool when the ping succeeds, so a `SetMaxOpenConns` on its client before the start counts.
  - The record keeps its message, `credo: store configuration warning`, and its `code`, and carries `module=sqldb` in place of `component=store` and `store=<name>`; on the App's logger the scope adds `component=<name>`.
  - A failed ping closes the database before `Start` returns the error. The pool was opened by `Open`, not by `Start`, and ADR-024's failed-start rule does not shut down a component whose `Start` failed, so it assumes that `Start` released what it holds. A borrowed database is never started, so it never takes this path.
  - **`sqldb.WithOptionalStart()`**, a new `Open` option, makes a failed ping non-fatal: `Start` logs it at Warn, with the component's name, the deadline and the error, logs the pool warnings as after a successful ping, and returns nil, leaving the pool open, since `database/sql` opens connections on demand once the server is back. `Ready` keeps pinging, so the database gates readiness until it answers unless it is bound with `credo.OptionalReadiness()`. The option is the module's because only the module knows that its value is usable after a failed ping; ADR-024's failed-start rule is unchanged.
- **`Ready`.** `*sqldb.DB` gains `Ready(ctx)`, which returns an error naming the status when `Health` is not UP.
- **One binding is enough.** The start walk plans both methods from the binding's type, as for every component. `app.ProvideValue(db)` alone pings the database before its dependants start, reports it in `/ready`, and shuts it down after its consumers.
  - **One resource, one entry.** The start-once-per-resource rule (`Container.ClaimStart`) gives a raw handle and a wrapper bound together one ping, and `componentReadiness` gives them one readiness entry, its holder of record's (W5), whose `Ready` it asks: it builds one check per resource instead of one per unit that answers `Ready`. A wrapper with readiness of its own, one that overrides the `Ready` it embeds, holds the handle in a named field. The two take one tier (W5) and agree on the options that shape the entry: `credo.OptionalReadiness()` is on every registration that plans an entry for the resource or on none, and the names given with `credo.ComponentName` are one name, which the entry and the teardown carry, the holder of record's type name without one. A disagreement fails as a tier disagreement does (W5) — a panic at registration for a value handed in, a failed start for one a binding's constructor builds — naming both registrations, the option and the remedies: the same options on both, or a named field. A value a constructor builds that holds the handle adds no entry of its own: it defers to the binding (W5).
  - A borrowed database is not pinged, since its owner starts it; readiness still reports it.
  - Every bound database enters `/ready`. One that must not gate readiness is bound with `credo.OptionalReadiness()` (below).
- **`credo.ComponentName(name)`** (W12) opens to `Provide` and `ProvideValue`. It names the binding's component in reports and readiness, and a duplicate name panics, except on registrations of one resource, which give it one name (above). Like `credo.Ingress()`, it panics on a binding that is not a component at registration, since the name is planned before the value exists. The default name is the binding's type, such as `*sqldb.DB`.
- **`credo.OptionalReadiness()`**, a new registration option, reports a component in `/ready` without letting it gate readiness. `Ready` is still asked under the same deadline, the entry shows its own status — `down` when its check fails — and latency, and a failure is logged like any failing check. The top-level status comes from the gating entries as before, with one value added: `degraded`, answered with 200, when every gating entry is up and an optional one is not. `shutting_down` and a down gating entry still answer 503. `Start` and `Shutdown` are unchanged, so an unreachable database whose readiness is optional still fails the start unless it is opened with `sqldb.WithOptionalStart()` (above). The option applies to `Provide`, `ProvideValue` and `Manage` wherever the registration plans a readiness entry — its type shows `Ready`, and it is a component or borrowed — and panics elsewhere at registration, as `credo.ComponentName` does; a constructor handed to `Manage` with it whose value turns out to defer to another holder fails the start (W5). `planUnit` records it on the unit, `componentReadiness` carries it on the resource's readiness check, and the aggregation computes the top-level status from the gating checks, then reports `degraded` in place of `up` when an optional check fails.
- **`AddReadinessCheck(name, checker, opts ...credo.RegistrationOption)`** accepts `credo.OptionalReadiness()`, its one option, and panics on any other, naming it and the option accepted: a check of a cache or of a third-party API is then reported without gating, as an optional binding's entry is. The variadic parameter keeps every existing call compiling. `AddLivenessCheck` takes no option: liveness decides a restart, and a check that cannot fail it decides nothing.
- **Every readiness entry carries `latency`.** The runner measures each check from the start of its run, which `Probe` already records, to its result, and every entry, a named check's included, renders it with `Duration.String()`, as a store entry does today. The failure log carries it too. A database's `Ready` is its ping, so its entry keeps the latency it shows today.
- **Deleted:**
  - `store.Register`, `RegisterOption`, `WithName`, `WithPingTimeout`, `DefaultPingTimeout`, and the private `registrationWarningProvider` seam;
  - `(*sqldb.DB).StoreRegistrationWarningCodes`, which only that seam read, and its unit test, whose cases move to `Start`'s logging test;
  - the root's `stores.go`, with `kernel.RegisterStore` and `kernel.Store`;
  - the DI start-step mechanism (`internal/di/step.go`, `AddStartStep`), whose only user was stores;
  - the store branch of the readiness aggregation: `storeChecks`, `logFailedStores`, `internal/health`'s `StoreCheck`, `StoreFunc` and `StoreResult`, and the engine's store branch;
  - `store.Lifecycle` (`store/lifecycle.go`), the constraint of `Register`'s type parameter and its only consumer.

  `store.Health` stays, as what `sqldb`'s `Health` returns and its `Ready` reads. Without `Register` nothing would check `store.Lifecycle` — the root cannot import `store`, and the health engine recognizing it is rejected (W1) — so it would be a contract frozen at v1 that no code enforces. A store adapter other than `sqldb` implements `Start`, `Ready` and `Shutdown` itself, as every component does.
- **`architecture_test.go`:** the `store` row drops the imports registration needed.

**Tests.**

- A database bound with `ProvideValue` is pinged before its dependants start, reported by `/ready`, and shut down after its consumers.
- Ping failures:
  - a failing ping fails the start, rolls back, and leaves the database closed;
  - a ping that outlives `PingTimeout` fails with the deadline in the error;
  - a negative `PingTimeout` fails `Open`.
- The pool warning is logged once after a successful ping, with its message, its `code` and `module=sqldb` and without `store`: to the App's logger scoped to the component, or to `WithLogger`'s logger when one is given. It follows the effective pool when the ping succeeds — logged for a bounded pool reset with `SetMaxOpenConns(0)`, not for an unlimited one given a finite maximum — and a failed ping logs none.
- A raw handle and a wrapper that embeds it give one ping, one readiness entry and one teardown, after the consumers of both, in either registration order, and the entry carries a name given on either. `credo.OptionalReadiness()` on one of them alone, or two different `credo.ComponentName` names, panics at the second `ProvideValue`, and fails the start when a `Provide` constructor builds the second value, naming both registrations, the option and the remedies.
- A borrowed database is never pinged or shut down, and is reported by `/ready`.
- `credo.ComponentName` on `ProvideValue` names the readiness entry and the component in a `LifecycleError`. A duplicate name panics, and so does a name on a binding that is not a component.
- `/ready` names a database's entry `*sqldb.DB` or its given name, and renders it `down` when its ping fails; the failure is logged in the check shape.
- Every readiness entry carries `latency`: a component, a named check and a database. A check that outlives its deadline reports the time until the deadline, and the failure log carries the latency.
- `credo.OptionalReadiness()`: a failing optional entry is shown `down` and logged while `/ready` answers 200 with the top-level status `degraded`, and `up` returns once it recovers; a failing gating entry beside it answers 503 `down`; during the drain `/ready` answers 503 `shutting_down` whatever the optional entries show; the option panics on a registration that plans no readiness entry — a type without `Ready`, or one that is neither a component nor borrowed — and applies to a borrowed value whose type shows `Ready` and no `Shutdown`; with `credo.Borrowed()`, a database is reported, does not gate, and is never started.
- `AddReadinessCheck` with `credo.OptionalReadiness()`: a failing check is shown `down` and logged while `/ready` answers 200 `degraded`, and the same check without the option answers 503; another option panics, naming it.
- `sqldb.WithOptionalStart()`, through a connector that refuses connections: the start succeeds, the failed ping is logged at Warn with the component's name and the error, and the pool warnings are logged; `/ready` reports the database `down`, answering 503 without `credo.OptionalReadiness()` and 200 `degraded` with it; once the connector accepts, `Ready` passes and a query runs on the same pool. Without the option, the same start fails and closes the database.
- `/ready` resolves nothing per request (kept).
- The `store.Register` tests are deleted with it, not ported. `health_store_integration_test.go` and `store/sqldb/integration_test.go` move to the component path.

**Documentation.** ADR-015, ADR-016, ADR-024's registration options, and the store, health, lifecycle and container specs flip, ADR-016 and the health spec with the option on `AddReadinessCheck`, which CHANGELOG lists under Added; the store spec, ADR-015 and ADR-024 drop `store.Lifecycle`, and NOTICES drops `lifecycle.go` from its GoFr entry. The data-access guide loses `store.Register` from its quick start and its multiple-databases section, and its section on what `store.Register` does becomes one on what a bound database does — the ping in `Start`, `Ready`, the teardown after its consumers and the resource identity of its holders — with its adapter subsection naming `Start`, `Ready` and `Shutdown` in place of the interface. The pool warning moves with the code: ADR-015's pool paragraph and its registration-warnings item, the store spec's `sqldb` type and configuration sections and its test requirements, the pool paragraphs of the configuration and data-access guides, and `store/sqldb/doc.go`'s connection-pool section name `Start` as what logs it, and none names `StoreRegistrationWarningCodes`. That doc's health paragraph, which reasons that every store is critical, reasons from a database that gates readiness unless it is bound with `credo.OptionalReadiness()`, and the doc documents `WithOptionalStart()` beside the ping. The deployment guide's readiness section names the body changes and shows `credo.OptionalReadiness()` for a database that must not gate readiness and for a check added with `AddReadinessCheck`, the `degraded` status, and `sqldb.WithOptionalStart()` beside it for a database whose outage must not stop the start. It also explains why gating is the default and what it costs: a database's `Ready` pings through its pool, so with `MaxOpenConns` set, as the pool warning asks, a pool whose connections are all busy holds the ping until the check's deadline, and a process that is merely busy leaves rotation and moves its load to the others; the pool's size and `credo.OptionalReadiness()` are the answers it names. Migration rows:

- `store.Register` is deleted: the binding alone does what it did;
- a database bound without `store.Register`, which `/ready` did not report, now gates readiness; one that must not is bound with `credo.OptionalReadiness()`;
- `WithName` becomes `credo.ComponentName` on the binding;
- `WithPingTimeout` becomes `sqldb.Config.PingTimeout`, and `store.DefaultPingTimeout` becomes `sqldb.DefaultPingTimeout`;
- a borrowed database is pinged by its owner;
- `(*sqldb.DB).StoreRegistrationWarningCodes` is deleted: `Start` logs the code, and a program that runs the database without an App calls `Start` or reads `Stats().MaxOpenConnections`;
- the pool warning's `component=store` and `store=<name>` become `module=sqldb`, and `component=<name>` on the App's logger;
- a store adapter of the application's own implements `Start`, `Ready` and `Shutdown`; `store.Lifecycle`, which it implemented for `store.Register`, is deleted.

## W4: workers in the root package

**Change.**

- **`app.Workers() *credo.Workers`** returns the App's one registry, created on first use and the same value on every call.
  - It registers nothing, so an App that calls it and registers no worker has no start work.
  - It is not bound in DI; a module that registers workers takes the `*credo.App` it already receives.
  - Its methods are `Continuous`, `Scheduled`, `Snapshot` and `Lookup`. `Snapshot` and `Lookup` now cover every worker of the App, and a duplicate name panics at the second registration whoever made the first.

The public types are defined in the root package, not aliased from an internal one, so `go doc` shows them where users look. The root namespace forces these renames:

| `worker` today | Root package |
| --- | --- |
| `Worker` | `Worker` |
| `Func` | none: a `func(context.Context) error` registers as it is (W5) |
| `Info`, `Kind`, `Status` and their constants | `WorkerInfo`, `WorkerKind`, `WorkerStatus` (`WorkerKindContinuous`, `WorkerStatusPending`, …) |
| `Restart` | `RestartPolicy` |
| `RunInfo`, `CurrentRun(ctx)` | `WorkerRun`, `CurrentWorkerRun(ctx)` |
| `ErrRunTimeout` | `ErrWorkerRunTimeout` |
| `Supervisor` | `Workers` |
| `ContinuousConfig`, `ScheduledConfig`, `Schedule`, `ParseSchedule`, `DefaultMinRestartDelay`, `DefaultMaxRestartDelay` | names kept |

- **The engine moves to `internal/worker`**, which does not import the root package: the runner, the restart backoff, the schedule loop and the cron parser. The root converts at the boundary.
- **The registry calls `internal/di` directly.** The worker's `Finalize` remedy, its `Tier` field, travels as a parameter, so `kernel.Manage` and `manageMisuse` go. A registration after the freeze panics with the container's reason under the worker's own call (`credo: Workers.Continuous("outbox-relay"): …`).
- **The provided forms move with the package and go in W5.** Until then, the per-App record of provided types is a field of the registry rather than a weak-keyed global map. `Func` does not move: until W5 accepts a bare function, the tests that register one convert it through a test helper.
- **Deleted:** the `worker` package, with no aliases left behind, and `internal/kernel` with its installation in `component.go`.
- **Housekeeping:**
  - NOTICES: robfig/cron's "Adapted in: worker/" becomes `internal/worker/`, and the adapted files keep their headers.
  - `maturity_test.go` and README's maturity table drop `worker`.
  - `architecture_test.go` drops the `worker` and `internal/kernel` rows and adds `internal/worker`, with no Credo imports. `TestImportBoundary_RootPackageGraph`'s module allowlist does not change.
  - TODO's 4.2 heading names the root package.
- **The worker's records take W2's key:** `component=worker:<name>`, its unit name, in place of `worker=<name>`, so a worker's records and the lifecycle's records about it share one key and value; `module=worker`, `kind`, `status` and `reason` stay.
- **Unchanged:** component names (`worker:<name>`), tiers and their defaults, readiness conditions, the snapshot's JSON, and the restart and schedule semantics.

**Tests.**

- The engine tests follow their code into `internal/worker`, and the registry and registration tests move to the root with the renamed types.
- New tests:
  - `app.Workers()` returns one value;
  - an App that only calls it serves through `ServeHTTP` without `App.Start`;
  - workers registered through two calls of `app.Workers()` appear in one `Snapshot`, and a duplicate name across them panics at the second registration;
  - `WorkerInfo`'s golden JSON is unchanged byte for byte;
  - `internal/worker` imports no root package;
  - a worker's start and failure records carry `component=worker:<name>` and `module=worker`, and none carries `worker`.

**Documentation.** ADR-023's placement and the worker spec flip, and the worker guide moves to the root names; the spec's name paragraph and the guide's log sample print `component=worker:<name>`. Migration rows: the import goes; `worker.Use(app)` becomes `app.Workers()`; the naming table; `Snapshot` and `Lookup` cover every worker of the App; the records' `worker=<name>` becomes `component=worker:<name>`.

## W5: worker constructors; built values claim nothing

**Change.**

- **`w any`.** `Continuous(name, w, cfg...)` and `Scheduled(name, expr, w, cfg...)` take `w any`. A worker is a value whose type implements `Worker`, or a `func(context.Context) error`, named function types of that signature included. A function that takes exactly one `context.Context`, in any position, and returns only an `error` is a run function, whose other parameters come from the container; `func(context.Context) error` is its case without them. Any other function is a constructor over DI parameters, `func(deps...) W` or `func(deps...) (W, error)`, whose `W` is a worker in either form; anything else panics at registration, naming the forms accepted, and so do a function that takes two contexts and one that takes a context and returns anything but a lone `error`. The forms cannot be mistaken for one another: `Finalize` refuses a constructor that takes a `context.Context`, and a constructor returns `W` or `(W, error)`. A constructor that returns a method value such as `relay.Run` hands over the function alone, which has no teardown; a worker with a teardown of its own is returned as itself. `ContinuousProvided` and `ScheduledProvided` go, with the per-App record of provided types and its "one type under two names" panic.
- **A run function adds one unit, the loop**, `worker:<name>`, whose edges are the function's parameters: the start walk resolves them once, after their components have started, every run calls the function with that run's context and those values, and the drain stops the worker before them. It builds no value, so it adds no `worker-value:<name>` unit, no holder and nothing to defer. A missing parameter fails `Finalize` with its path, and a parameter bound in the ingress tier fails it under an internal loop, with the message and the two remedies of every internal component that depends on an ingress one. A method expression is a run function whose first parameter is the receiver — `(*OutboxRelay).Run` is a `func(*OutboxRelay, context.Context) error` — and a scheduled job over a database needs no type of its own: `func(ctx context.Context, db *sqldb.DB) error`. The function is called through `reflect` on each run, a cold path, as a constructor is.
- **A constructor never becomes a binding.** It is handed to the component registry as `Manage` hands one over:
  - its parameters are resolved from the container and give the worker its edges;
  - the start walk builds it after those dependencies have started, and the drain stops the worker before them;
  - a missing parameter fails `Finalize` with its path;
  - a constructor error, a panic or a nil worker fails the start.
- **Two units per constructor registration.** Each such registration adds two units that are not bindings — the built value and the loop — with an edge from the loop to the value. The loop's component is `worker:<name>` and the value's is `worker-value:<name>`. Worker names admit any printable character, so a suffix could collide with another worker's name; a distinct prefix cannot.
- **The built value is a holder like any constructor result**, under the rules a `Provide` result follows:
  - `Start` and `Ready` are planned from the constructor's result type: a type that shows `Shutdown` is started before its loop if it also shows `Start`, and asked for readiness if it shows `Ready`. A `Start` or `Ready` only the built value has fails the start, as W11 decides for every constructor.
  - Its teardown follows the built value, including a `Shutdown` the result type hides — a constructor declared to return `credo.Worker` that builds a `*Relay` with `Shutdown`.
  - It is shut down after its loop has stopped, through the component registry, never by a direct call from the worker, so the identity, kind and owner rules apply.
- **The built value claims no ownership.** A worker that embeds a bound `*sqldb.DB`, or two constructors that return one singleton, share that resource's one teardown. That teardown is run by the resource's holder of record (below), after the consumers of all of them, and a borrowed resource is torn down by none of its holders.
- **A registration is the holder of record.** A binding or a value handed to `Manage` that holds a resource is its holder of record, the first registered of them when several do. A value a constructor builds — a worker's, or one from a constructor handed to `Manage` — that holds the resource too defers to it: it adds no readiness entry of its own and never runs the teardown, so `/ready` shows the registration's entry, with its name and its `credo.OptionalReadiness()`, and the teardown carries the registration's name and tier, even when the constructor was registered first. Without such a registration the first registered holder is the holder of record, so two worker constructors that return one value they did not create give one entry and one teardown. Two bindings of one resource give it one entry too, and agree on its options (W3). A worker value that embeds a shared handle and overrides its `Ready` therefore has no entry of its own; a worker with readiness of its own holds the handle in a named field.
- **Registrations of one resource agree on its tier.** Today the first registered holder's tier decides the teardown's without a word: a raw handle bound internal and a wrapper that embeds it bound with `credo.Ingress()` give the database the tier of whichever was registered first, and with the wrapper first and no internal component among its consumers, it is torn down beside the HTTP drain while handlers that use the raw handle still run. Registrations that hold one resource — bindings, and a value handed to `Manage` — therefore agree on its tier. Beside the owner and kind checks of `internal/di/resource.go`, the admission of whichever value becomes known second fails, naming both registrations, their tiers and the remedies: give them one tier, or, when one of them has a teardown of its own to run in its tier, hold the handle in a named field, so that it is a resource of its own. That is a panic at registration when the value is handed in (`ProvideValue`, `Manage`) and a failed start when a binding's constructor builds it. A borrowed resource is exempt, since the App neither starts nor tears it down. A value a constructor builds for a worker or for `Manage` takes no part: one that defers carries no options (below), and one that is the holder of record holds a resource no registration holds. The same admission checks the options that shape the resource's one readiness entry (W3), a borrowed resource included, since readiness reports it.
- **`Manage` with a constructor follows the same rule.** Only a value handed to `Manage` claims, and it is still refused beside a `ProvideValue` binding or another `Manage` of one resource. As to ownership, this only admits what is refused today: a constructor handed to `Manage` whose value embeds a handle bound with `ProvideValue`, with or without `credo.Borrowed()`, now starts. Today that fails the start with a remedy, dropping the `Manage` call, that would remove the component the application asked for.
- **A value that defers takes no registration options.** A `Manage` call's options apply to the value it builds — `credo.ComponentName` to its name, `credo.Ingress()` to its tier, `credo.OptionalReadiness()` to its readiness entry — and a value that defers to its resource's holder of record has no entry and no teardown of its own for them to shape. A constructor handed to `Manage` with any of the three whose value defers therefore fails the start once the value is built, naming the holder it defers to and the remedies: move the option to the holder's registration, whose entry and teardown are the resource's, or, when the value has a readiness or a teardown of its own, hold the handle in a named field, so that it is a resource of its own. Without options such a call starts, as above.
- **Inside `internal/di`**, the managed flag splits into "not a binding" and "claims what it holds". `manage.go` stops claiming for a constructor, and a new operation adds an edge between two units that are not bindings. The teardown snapshot (`shutdown.go`) takes a resource's holder of record from those flags instead of its first registered holder, and the root's `componentReadiness` builds one check per resource, the holder of record's (W3). Admission (`resource.go`) adds the tier and entry-option checks beside the owner and kind checks.
- **Tiers.** The built value is internal, as a constructor result bound without `credo.Ingress()` is, and the loop keeps its worker's tier. A shared resource takes the tier of its holder of record, the first registered holder when no registration holds it, so a value that took its worker's tier would move that resource's one teardown into the ingress tier for a scheduled worker. A built value with its own teardown that depends on an ingress component fails `Finalize`, or fails its build when the teardown is found only on the value, with the path and the remedy of splitting it. When that ingress component is the binding whose type the constructor returns — `func(r *OutboxRelay) *OutboxRelay` over `app.Provide(NewOutboxRelay, credo.Ingress())`, a constructor that returns the binding — there is no part to split, and the message gives the remedy that fits: bind it without `credo.Ingress()`. The loop's tier, which its worker's configuration sets, decides when the worker stops taking work, and the value is then torn down in the internal tier, after the HTTP drain, which the handlers that use it need: bound ingress, it would be torn down beside the drain while they still call it.
- **Separate calls, shared values.** Each registration calls its constructor separately, so two registrations get two values unless the constructor returns one it did not create. The two loops then share that value, and the registry tears it down once. A value handed in stays the caller's.
- **A worker that other code uses is bound.** The value a constructor builds is not a binding, so nothing resolves it. A worker that handlers also use — an outbox relay whose `Enqueue` they call — is bound with `Provide` and registered through the method expression of its `Run`, a run function whose first parameter is the receiver:

  ```go
  app.Provide(NewOutboxRelay)
  app.Workers().Continuous("outbox-relay", (*OutboxRelay).Run)
  ```

  Handlers and the loop share one singleton, which starts once, before the loop, and is torn down once, after it (one resource, one teardown), and `testutil.WithOverride` replaces it for both. The registration builds no value, so the binding is the singleton's only holder: `/ready` shows its one entry, with its options, and the teardown carries its name, whichever of the two calls comes first. The compiler checks that `Run` exists; its shape is checked at registration. A constructor that returns the binding, `func(r *OutboxRelay) *OutboxRelay { return r }`, still registers and defers to the binding, its holder of record (above), as one handed to `Manage` does.

**Tests.**

- **Registration:**
  - a value, a bare `func(context.Context) error`, a named function type of that signature, a run function with its context first, last and between its parameters, a method expression among them, and a constructor of each shape — its result a `Worker` or such a function — register;
  - a function of another shape, or a constructor whose result is neither form, panics naming the forms accepted, and so do a function that takes two contexts and one that takes a context and returns a value beside its `error`;
  - a constructor that returns a method value: the function runs as the worker, and the value the method belongs to is not torn down;
  - a missing constructor parameter fails `Finalize` with its path;
  - a constructor error, a panic and a nil worker each fail the start.
- **Run functions:**
  - the parameters are resolved once, after their components have started, and each run receives them with its own context; the drain stops the loop before them;
  - a missing parameter fails `Finalize` with its path, and a parameter bound with `credo.Ingress()` fails it under a continuous worker's internal loop, naming both and the two remedies;
  - a run function adds no `worker-value:<name>` unit;
  - `(*OutboxRelay).Run` over a binding made with `Provide`: a controller resolving the binding and the loop share one value, started once before the loop and torn down once after it; `testutil.WithOverride` on the bound type replaces it for both; `/ready` shows the binding's one entry, which `credo.OptionalReadiness()` on the binding keeps from gating, in either registration order.
- **Shared resources and teardown:**
  - A worker that embeds a `*sqldb.DB` bound with `ProvideValue` has one teardown, after the worker's loop and every other consumer of the database, and `/ready` shows the database's entry alone, which does not gate when the database is bound with `credo.OptionalReadiness()`. With the database bound with `credo.Borrowed()`, it has no teardown and no ownership error.
  - Two worker constructors that return one singleton: both loops run, a value whose type shows `Ready` has one readiness entry, and it is torn down once.
  - A constructor that returns a binding made with `Provide`: a controller resolving the binding and the loop share one value, which starts once and is torn down once, after the loop; `testutil.WithOverride` on the bound type replaces it for both. With a type that shows `Ready`, `/ready` shows one entry, the binding's, and `credo.OptionalReadiness()` on the binding keeps it from gating; with the worker registered before the binding, the entry and the teardown's name are still the binding's.
  - The same constructor over a value bound with `ProvideValue` and `credo.Borrowed()`: the loop runs, the App neither starts nor shuts down the value, and `/ready` reports it once.
  - A constructor declared to return `credo.Worker` that builds a value with `Shutdown`: the value is torn down after its loop.
  - A result type with `Start` and `Shutdown` is started before its loop. A `Start` only the built value has fails the start, naming the constructor and the remedy of returning the concrete type (W11).
  - A raw handle bound internal and a wrapper that embeds it bound with `credo.Ingress()` are refused in either registration order, naming both registrations, their tiers and the remedies: with both bound by `ProvideValue` the second call panics, and with the wrapper built by `Provide` the start fails when it is built. `app.Manage(v, credo.Ingress())` beside an internal `Provide` binding whose constructor returns `v` fails the start likewise. Bound in one tier, the pair has one start and one teardown, in that tier; a borrowed handle beside a wrapper built by `Provide` with `credo.Ingress()` starts, and neither is torn down.
- **Values and tiers:**
  - two registrations of one constructor get two values, `worker-value:a` and `worker-value:b`, without a collision;
  - the built value of a scheduled worker is internal while its loop is ingress;
  - a built value whose teardown depends on an ingress component fails `Finalize` with the remedy of splitting the worker;
  - a constructor that returns a binding made with `credo.Ingress()` fails `Finalize` with the remedy of binding it without; bound without it, a scheduled worker's loop stays ingress, and the value is torn down in the internal tier, after the HTTP drain.
- **`Manage`:**
  - `app.Manage` of a value a `ProvideValue` binding holds is still refused, and so are two `Manage` calls of one value;
  - `app.Manage` of a constructor whose value embeds a handle bound with `ProvideValue` starts, with one teardown after every consumer;
  - with the handle bound with `credo.Borrowed()`, it starts with no teardown;
  - `app.Manage` of a constructor that returns a binding's own value: one readiness entry and one teardown, the binding's;
  - `app.Manage` of a wrapper's constructor whose value defers to the handle's binding, with `credo.ComponentName`, `credo.Ingress()` or `credo.OptionalReadiness()`: the start fails, naming the binding and the remedies; with the option on the handle's binding instead, it starts, and the handle's one entry carries the name and does not gate; a wrapper that holds the handle in a named field and has `Ready` and `Shutdown` of its own starts with an entry of its own under its name.

**Documentation.** ADR-023's run function and constructor forms, ADR-024's holders that claim nothing, its holder of record, the options a value that defers refuses, and the tier and entry options the registrations of one resource agree on, and the worker, lifecycle and container specs flip. The dependency-injection guide's section on wrappers and resource identity states the tier and entry rules, and that a wrapper with readiness of its own holds the handle in a named field. The worker guide covers:

- the three forms, and when each fits: a value, a run function over its dependencies — a scheduled job over a database needs no type of its own — and a constructor;
- resources a worker uses, which come in as constructor parameters;
- a worker type's own `Shutdown`, which is for state that outlives a single `Run`, since a restart calls `Run` again on the same value;
- a worker that handlers also use, bound with `Provide` and registered through the method expression of its `Run`: the binding keeps its readiness entry, its options and its teardown, and takes no `credo.Ingress()`, so that the teardown runs after the HTTP drain, which the handlers that use it need;
- reading a worker's status through `app.Workers().Lookup`.

Migration rows:

- `app.Provide[T](NewT)` plus `ContinuousProvided[T](name, cfg...)` becomes `app.Workers().Continuous(name, NewT, cfg...)`, and likewise for `ScheduledProvided`, when nothing else resolves `T`: the `Provide` call goes, since kept beside the constructor it builds a second value. When other code uses the worker, the binding stays, as `app.Provide(NewT)` (W11), and the registration takes its `Run` as a method expression, `(*Relay).Run` for a `*Relay`; the binding keeps its options, and the loop's tier comes from the worker's configuration.
- `worker.Func(fn)` becomes `fn`: a `func(context.Context) error` is a worker as it is.
- A worker its constructor builds is not injectable: a worker that other code uses is bound, as above, or the code that hands it work holds a handle both share.
- `testutil.WithOverride` on a bound worker's type replaces it as before; for a worker its constructor builds, it becomes an override of its dependencies.
- A worker whose built value owns state and uses an ingress component is split.
- A constructor handed to `Manage` whose value holds a bound resource takes no `credo.ComponentName`, `credo.Ingress()` or `credo.OptionalReadiness()`: the option moves to the binding, or the value holds the handle in a named field.
- Registrations of one resource that disagree on its tier are refused, where the first registered decided its teardown's tier: a raw handle and a wrapper that embeds it take one tier, or the wrapper holds the handle in a named field.

## W6: the first `Resolve` finalizes

**Change.**

- **The trigger.** The first `Resolve`, `ResolveAll`, `MustResolve` or `MustResolveAll` finalizes the container if it is not finalized yet, then resolves. `app.Finalize()` stays public as an optional early check.
  - `Has`, `GetConfig`, `MustGetConfig` and `ConfigExists` never finalize.
  - Finalizing stays DI-only: routes registered after the first `Resolve`, such as a controller's handlers, are accepted until the App is prepared, as after an explicit `Finalize`.
  - `Resolve` before `Finalize` no longer panics, and its "called before Finalize" message goes.
- **Failures.** A `Finalize` failure is the triggering call's error, and `MustResolve` panics with the whole joined report. Every later `Resolve` returns that error, as after a failed explicit `Finalize`. Concurrent first resolvers finalize once, under the container lock.
- **The trigger's record.** The container records what finalized it and where: `Finalize`, the first resolving call with its type, or the App's preparation with its entry point.
  - The site is the first frame outside Credo's packages, as route registration records it (`sourceloc.go`).
  - `testutil` frames are skipped too, so a test's trigger points at the test.
  - The first `ServeHTTP` call has no site, since its caller is `net/http`.
  - Up to three callers of the site are recorded with it, skipping Credo's packages, `testutil` and the standard library: when a helper library makes the first `Resolve`, the site names the helper's line, and its callers name the code that called it.
- **The message.** A registration after the freeze names the call, the trigger with its site and callers, and the remedy. This applies to `Provide`, `ProvideValue`, `Alias`, `BindMany`, `credo.Override()` on either, `Manage`, `Workers().Continuous`, `Workers().Scheduled` and `testutil.WithOverride`. One template serves every trigger:

  ```text
  credo: App.Provide[*Mailer]: called after the container was finalized by Resolve[*UserController] at main.go:42; register before that call
  credo: Workers.Continuous("outbox-relay"): called after the container was finalized by Finalize at main.go:40; register before that call
  credo: App.Manage[*Cache]: called after the container was finalized by the first ServeHTTP call; register before the App serves
  credo: App.Provide[*Mailer]: called after the container was finalized by Resolve[*Hub] at wiring/wiring.go:17 (called from main.go:31, main.go:12); register before that call
  ```

  The message for a registration after a shutdown began is unchanged.

**Tests.**

- `Resolve`, `ResolveAll`, `MustResolve` and `MustResolveAll` resolve without `Finalize`. `Has`, `GetConfig` and `ConfigExists` leave the container open, so a `Provide` after them succeeds.
- Every registration listed above panics after an implicit freeze, naming its call, the trigger with its type, the triggering line in the test file, and the remedy. The same holds after an explicit `Finalize`, with that call's site; after `testutil.Start`, with the test's line; and after the first `ServeHTTP` call, without a site.
- When a helper function makes the first `Resolve`, the message names the helper's line as the site and the test function's line as its caller; no frame of the standard library is listed.
- A failed implicit `Finalize`: `Resolve` returns the joined report, `MustResolve` panics with it, and a later `Resolve` returns the same error.
- Routes registered after the first `Resolve` serve.
- Concurrent first `Resolve` calls finalize once under `-race`.

**Documentation.** ADR-022, ADR-004, the container spec (its phases, its sequential-bootstrap section, and "must call it explicitly" goes) flip. The getting-started and dependency-injection guides drop `Finalize` from the common path; the explicit calls in tests and examples stay valid. Migration rows: `Finalize` is optional; `Resolve` before it works; a late registration's panic names what finalized the container, where, and the calls that led there.

## W7: a type declares its ingress tier

**Change.**

- **The interface.** `credo.IngressComponent` is an optional capability, `interface { Component; IngressTier() }`, whose method is never called; `Component` gains no method. `planUnit` places a unit in the ingress tier when `credo.Ingress()` is given or the binding's type implements it. The check needs no value, so `Provide` with a constructor, `ProvideValue`, and `Manage` of a value or of a constructor all see it, and `Finalize` validates the tiers before anything is built. A binding whose type hides the method, such as an interface view, plans as internal, and a value that has the method is refused, as W11 refuses a hidden `Start`.
- **`credo.Ingress()` stays** for what has no type of its own or does not declare one: hooks, an application's own consumer type, a third-party type. On a declaring type it is redundant and accepted.
- **No way down.** A type declares the tier only when no registration could rightly place it internal, and no `credo.Internal()` is added. The worker takes its tier from its configuration, as before.
- **`websocket.Server` implements it.** The message for a server that was never started becomes: `the server was never started; hand it to the App with app.Manage(server), or bind it`.

**Tests.**

- `app.Manage(ws)` and `app.ProvideValue(ws)` place the server in the ingress tier: it drains beside the HTTP drain and before the internal components its handlers use. With `credo.Ingress()` added, the call is accepted and the result is the same.
- A `Provide` constructor whose result type declares the tier is ingress, and an internal component that depends on it fails `Finalize` before any constructor runs.
- A binding under an interface that hides `IngressTier` is refused, as W11's hidden capabilities are: `ProvideValue` panics at the call, and a constructor's value fails the start.
- A server never handed to the App refuses an upgrade with the new message.

**Documentation.** ADR-024's "Two tiers" and its rejected marker interface, ADR-019, and the lifecycle and WebSocket specs flip. The getting-started guide and README's API table show the path without options. The options move to the dependency-injection guide's part on writing one's own component. Migration and release notes: a server registered without `credo.Ingress()` moves into the ingress tier, and an internal component that depends on it now fails `Finalize`, with the existing message and its two remedies.

## W8: rate limiting: not a component, in its own package

**Change.**

- **The object form goes.** `NewRateLimiter` goes, together with the `RateLimiter` type, its `Middleware`, `Close` and `Shutdown` methods, and its `credo.Component` assertion. The function form is the one constructor, and the middleware it returns shares its store across every route it is attached to. The in-memory store keeps `Close`, which `limiter.Store` requires. A store passed in through the configuration's `Store` stays its owner's component.
- **Rate limiting moves into `middleware/ratelimit`**, so that importing `middleware` — for `CORS`, say — no longer links go-limiter; today a build that imports `middleware` needs go-limiter's `go.sum` entry. The package is `ratelimit`:

  | `middleware` today | `middleware/ratelimit` |
  | --- | --- |
  | `RateLimit(cfg...)` | `Middleware(cfg...)` |
  | `RateLimitConfig` | `Config` |
  | `DefaultRateLimitConfig()` | `DefaultConfig()` |

  `Middleware(cfg ...Config)` follows the configuration-struct convention and `auth.Middleware`, and `Config.Skipper` keeps the type `middleware.Skipper`. `ratelimit.go` and `ratelimit_store.go` move with their tests. The package stays in the root module: a module of its own waits for a consumer that cannot resolve a version conflict or must keep go-limiter out of its module graph.
- **Housekeeping:**
  - `architecture_test.go`: go-limiter moves from the `middleware` row to a new `middleware/ratelimit` row.
  - `maturity_test.go` and README's maturity table add `middleware/ratelimit` as `beta`, and the package's `doc.go` closes with its maturity line.
  - NOTICES: go-limiter's "Wrapped in" names `middleware/ratelimit/`.

**Tests.**

- One `ratelimit.Middleware` value attached to two routes spends one budget. `ExampleNewRateLimiter` becomes the new package's example of that shared budget, and the benchmarks and internal tests move with the code.
- The dependency graph of `middleware` contains no go-limiter package, so that the split cannot regress.

**Documentation.** ADR-010 and ADR-002's wrap list; the middleware package doc's "RateLimit Lifecycle" section, which becomes the new package's `doc.go`; the middleware spec's lifecycle paragraph and file table; the middleware guide. Migration rows: `NewRateLimiter(cfg).Middleware()` and `middleware.RateLimit(cfg)` become `ratelimit.Middleware(cfg)`; `middleware.RateLimitConfig` becomes `ratelimit.Config`, and `middleware.DefaultRateLimitConfig()` becomes `ratelimit.DefaultConfig()`; the `app.Manage(limiter)` call goes with the variable it named.

## W9: examples, guides and the release

**Change.** Every example migrates and runs (`go run .`, not only build and vet), and `examples/saas` shows the composition root above. The root package's documentation is grouped into sections — DI, components, workers — so that its size reads by subject. The sections order the package overview only: the symbol index stays alphabetical, which no doc comment changes, so `ContinuousConfig`, `RestartPolicy`, `Schedule` and `ScheduledConfig` stand apart from `Worker`, `WorkerInfo` and `Workers`.

The guides are brought up to date and lead with the path that needs no option: getting-started, dependency-injection, deployment, data-access, WebSocket, worker, middleware, configuration and error-handling. So are README's API summary and maturity table and the examples README. The dependency-injection guide shows in one table what each registration form claims — `ProvideValue`, `Provide`, `Manage` with a value and with a constructor, a worker value and a worker constructor, and `credo.Borrowed()` — since a call site does not show it: `Manage(v)` claims the teardown of what `v` holds, while `Manage(func() T { return v })` claims nothing a registration already holds. CLAUDE.md and AGENTS.md follow: Philosophy 3 names the `InfraFrom` path for module authors (W2); Philosophy 5 states the boundary it keeps, no reflection on a hot path, while `Provide`, `Manage` and the worker registrations, which take `any`, decide their argument's form when they are called, and its `app.Provide[T](constructor)` becomes `app.Provide(constructor)`, as the DI section's method list and the root re-export list do (W11); the DI section's registration options take their new names (W12); the package organization states ADR-002's rule for what lives in the root (W1); and the API verb convention gains `Add`, for one of several registrations of a kind (`AddLivenessCheck`, `AddReadinessCheck`), and the noun accessor, for the App's one registry of a subsystem that takes many registrations (`app.Workers()`).

**Documentation drift.** The drift a review found in code comments and examples is corrected, each item verified again when it is fixed:

- `examples/saas/config.yaml:23` says `server` is the only section the framework reads; the framework also reads `i18n` when `UseI18n` is installed.
- `New`'s godoc promises an error for a typed-nil option (`credo.go:218-221`), but `parseOptions` cannot fail; the typed-nil `Leveler` check lives in `UseAccessLog`, and it panics.
- `route.go:26` says route middleware is applied lazily at request time; route chains are precompiled.
- `store/sqldb/doc.go:338` says the module is versioned independently from the root module. It is released in lockstep with the root, and its `beta` label is what keeps it outside the v1 compatibility promise.

The migration guide's v0.25.0 section opens with the changes that compile unchanged, then a table of what v0.25.0 deletes or renames, with each row's replacement. The table gathers the rows of W2–W8, W11 and W12, with the worker naming table in full. It closes with the short list of rules an application keeps:

- a database is bound, not registered;
- a WebSocket server is handed over with `app.Manage(ws)`;
- workers are registered through `app.Workers()`, as a value, a run function or a constructor;
- `Provide` takes the constructor, and an interface is bound with `Alias`;
- `Finalize` is optional.

**Release.**

- **CHANGELOG** under the release heading.
- **`docs/releases/v0.25.0.md`**, putting first what compiles unchanged but behaves differently:
  - every bound `*sqldb.DB` is pinged at start and enters `/ready`, whether or not it was registered before, and a borrowed one is no longer pinged; one that must not gate readiness takes `credo.OptionalReadiness()`, and while it fails `/ready` answers 200 with the status `degraded`;
  - the `/ready` body: every entry carries `latency`; a degraded store reports `down`; a database's entry is named `*sqldb.DB` or by `credo.ComponentName`; a resource held by two registrations, such as a raw handle and a wrapper that embeds it, reports one entry where two `store.Register` calls reported two; and a failure is logged in the check shape, with its latency;
  - a WebSocket server registered without `credo.Ingress()` moves into the ingress tier, and an internal component that depends on it fails `Finalize`;
  - the log keys: a constructor's `Infra` logger and `app.NewInfra(name)` write `component` in place of `service`, with the binding's full name (`*users.Service`); a worker's records write `component=worker:<name>` in place of `worker=<name>`; the `http.Server.ErrorLog` bridge writes `module=net/http` in place of `component=net/http`; WebSocket connection logs gain `module=websocket`, and `sqldb`'s pool warning carries `module=sqldb` in place of `component=store` and `store=<name>`, with `component=<name>` when it goes to the App's logger rather than to one given with `sqldb.WithLogger`;
  - `Snapshot` and `Lookup` cover every worker of the App;
  - `Resolve` before `Finalize` works, and a late registration's panic names what finalized the container, where, and the calls that led there;
  - `Manage` of a constructor over a handle bound with `ProvideValue` or `credo.Borrowed()` starts;
  - `Manage` of a constructor whose value holds a binding's resource — the binding's own value, or a wrapper that embeds it — no longer adds a readiness entry of its own, and the binding's options apply to the one it keeps; such a call that carries `credo.ComponentName`, `credo.Ingress()` or `credo.OptionalReadiness()` fails the start, naming the binding;
  - registrations that hold one resource and disagree on its tier — a handle bound internal and a wrapper that embeds it bound with `credo.Ingress()` — are refused, where the first registered decided its teardown's tier without a word;
  - two registrations of one worker constructor no longer panic;
  - a binding under an interface that hides its value's `Start`, `Ready` or ingress tier is refused — `ProvideValue` at the call, a constructor at the start — where the capability was skipped without a word; a direct binding after an alias of its type panics; and `testutil.WithOverride` of an aliased interface replaces it, where it panicked, and the type the alias pointed at is no longer built when nothing else uses it (W11);
  - a nested `Validate` with a pointer receiver runs on the field itself, so what it normalizes is kept, and a nil nested pointer is valid where it panicked (W10).
- **`store/sqldb/go.mod`** requires `credo v0.25.0`, since `sqldb` now imports the root package for `InfraFrom`.
- **`releasegate tidy|prepared|candidate`**, then the pull request, the required checks and the `Release` dispatch.

## W10: nested validation fixes

Found while preparing this release, and fixed in it.

**Change.**

- **A nil nested pointer is valid.** `validation.Field(&r.Address)` with no rules validates a `Validatable` field by calling its `Validate`. With `Address *Address` nil — a request that omits an optional object — the engine calls `Validate` on a nil receiver, and a method that reads its fields panics, which the request pipeline answers with a 500 (verified on v0.24.1). A nil pointer or nil interface is valid and its `Validate` is not called, as in ozzo-validation, the engine's source, which returns before calling it; presence is what `NotNil` and `Required` check.
- **A pointer-receiver `Validate` runs on the field.** For a struct field whose `Validate` has a pointer receiver, `validateNested` (`validation/rule.go`) calls it on a pointer to a copy, so what it normalizes is lost, while a bound target's own `Validate` runs on the target. The engine calls it through the field's own pointer, which also drops the copy's allocation.
- **Unchanged:** a field with rules runs its rules only. Whether it also runs its `Validate` once the rules pass, as ozzo-validation does, is decided separately: it changes which violations a request reports, so it is not a fix.

**Tests.**

- An optional nested pointer left nil validates without calling `Validate`, through `ValidateStruct` and through `BindBody`; a non-nil one is validated, and its violations carry the field's name as their prefix.
- A nested struct field whose pointer-receiver `Validate` normalizes a value keeps the normalized value, through `ValidateStruct` and through `BindBody`.
- A nested value-receiver `Validate` and a nested pointer field that is set keep today's results.

**Documentation.** ADR-011 and the validation spec state the nil rule, and the spec's nested example adds an optional nested pointer. CHANGELOG Fixed, and the release notes list both behaviors among the changes that compile unchanged (W9).

## W11: `Provide` binds what its constructor returns

Found while preparing this release, and fixed in it: the dependency-injection guide's override of an aliased interface panics today, and an interface binding skips its value's `Start` without a word.

**Change.**

- **`app.Provide(ctor, opts...)`.** The type parameter goes: the binding's type is the constructor's first result type, which `internal/di` already reads to check it against the type argument (`provide.go:174`). A constructor declared to return a concrete type binds that type, and the interfaces it serves are bound with `Alias`; one declared to return an interface binds the interface. `ProvideValue[T]`, `Resolve[T]`, `Alias[I, T]` and `BindMany[I, T]` keep their type parameters. A message that names a binding keeps the form `App.Provide[*Mailer]`, the call with its binding's type, since a constructor written as a function literal has no useful name.
- **A hidden capability is refused.** A component whose binding's type hides its value's `Start` or `Ready` — and, from W7, `IngressTier` — is refused instead of being run without it: `ProvideValue` panics at the call, and a value a constructor builds fails the start once it is built, where the container finds a `Shutdown` the type hides today. The message names the binding, the methods its type hides and the remedy: bind the concrete type, and bind the interface with `Alias`. `credo.Borrowed()` is exempt, since the App neither starts nor stops the value, and a hidden `Shutdown` still makes the value a component whose teardown follows it. The values a worker constructor and a constructor handed to `Manage` build follow the same rule (W5).
- **An alias is a binding.** The duplicate check (`bindLocked`, `provide.go:71-84`) and the override check (`checkOverrideLocked`, `:56-66`) consult the aliases (`bind.go:43`) as well as the registrations, so a direct binding of an aliased type without `credo.Override()` panics, as `Alias` over a bound type does, and with it replaces the alias. `Alias` accepts `credo.Override()`, its one option, to point an interface at another type, and panics on any other.
- **`Finalize` drops what an override leaves unreached.** The type an overridden alias pointed at stays registered until `Finalize`, which drops it before it validates the graph when no other alias, `BindMany` entry or constructor parameter — of a binding, a worker or a constructor handed to `Manage` — reaches it, and removes its holder records (`forgetHolderLocked`, `resource.go:178`). A dropped type is never built or started, and `Resolve` of it reports the override that dropped it.
- **The repository migrates once.** Every `Provide` call in the code, the tests, the examples, the guides and the specs drops its type argument; a call whose type argument named an interface binds the concrete type with an alias.

**Tests.**

- `app.Provide` binds the result type of `func(...) T` and of `func(...) (T, error)`; a constructor declared to return an interface binds the interface; a function of another shape panics, naming the forms accepted.
- A constructor declared to return an interface whose value has `Start` and `Shutdown` fails the start, naming the binding, `Start` and the remedy, and likewise for `Ready`; `ProvideValue[I](v)` whose value hides `Start` panics at the call, and with `credo.Borrowed()` it is accepted and neither started nor stopped; a value whose type hides only `Shutdown` is torn down as today; a worker constructor and a `Manage` constructor declared to return an interface follow the same rule.
- `ProvideValue[I](fake)` after `Alias[I, T]()` panics as a duplicate, naming the alias, as `Alias[I, T]()` after a binding of `I` does; with `credo.Override()` it replaces the alias.
- Over the guide's pattern, `testutil.WithOverride[I](fake)` resolves the fake for every consumer of `I`, and `T`, a component with `Start`, is neither built nor started; with a constructor that also takes `T`, or another alias or a `BindMany` entry naming it, `T` is kept, built and started; `Resolve[T]` of a dropped `T` reports the override.
- `Alias[I, T2](credo.Override())` points `I` at `T2`; `Alias` with another option panics; `credo.Override()` with neither a binding nor an alias of the type still panics.

**Documentation.** ADR-004 states the binding's type, the alias rules and the drop, with their rejected alternatives; ADR-024 states that a capability the binding's type hides is refused, where "a `Start` only the built value has is never called" stood; the container and lifecycle specs flip. The dependency-injection guide binds an interface as the concrete type plus `Alias` and overrides the interface in its testing section; the getting-started guide, README and every example drop the type argument; CLAUDE.md and AGENTS.md follow (W9). Migration rows:

- `app.Provide[T](ctor)` becomes `app.Provide(ctor)` when `T` is the constructor's result type.
- `app.Provide[I](NewConcrete)`, whose `I` is an interface the result implements, becomes `app.Provide(NewConcrete)` with `app.Alias[I, *Concrete]()`, or a constructor declared to return `I`; the concrete binding plans the `Start` and `Ready` the interface hid, so a component that ran unstarted starts.
- A `ProvideValue` under an interface that hides its value's `Start`, `Ready` or ingress tier binds the concrete type with an alias, or takes `credo.Borrowed()` when the caller starts and stops the value.
- A direct binding after an alias of its type takes `credo.Override()`, or the alias goes.

Release notes, first: a binding under an interface that hides its value's `Start` or `Ready` fails, where the method was skipped; a direct binding after an alias of its type panics; `testutil.WithOverride` of an aliased interface replaces it, and the real implementation is no longer built when nothing else uses it.

## W12: `ComponentName` and `CloseOnShutdown`

Found while preparing this release, and done in it: v0.25.0 opens `credo.Named` to `Provide` and `ProvideValue` (W3), so renaming it now changes each call once.

**Change.**

- **`credo.Named(name)` becomes `credo.ComponentName(name)`.** The name labels a component in logs, `/ready` and a `LifecycleError`; it is not a DI key. `Named` reads as one to a reader who knows DI containers: samber/do's `ProvideNamed` and uber/dig's `dig.Name` bind a second value of one type under a name, and ADR-004 rejects such named bindings, spelling the qualifier it rejects `credo.Named[*sqldb.DB, Analytics]`. Read that way, `app.ProvideValue(primary, credo.Named("primary"))` beside `app.ProvideValue(replica, credo.Named("replica"))` looks like two bindings, and the second call panics as a duplicate.
- **The duplicate-binding panic explains the name.** When either registration of a duplicate binding carries `credo.ComponentName`, the message says that the name is not a DI key, beside the remedy it already gives (`duplicateError`, `internal/di/provide.go`):

  ```text
  credo: App.ProvideValue[*sqldb.DB]: *sqldb.DB is already registered; credo.ComponentName("replica") names a component in logs, /ready and lifecycle errors and is not a DI key; bind a second instance under a wrapper type of its own
  ```

- **`credo.Closer()` becomes `credo.CloseOnShutdown()`**, named for what the App does with it, as `credo.Borrowed()`, `credo.Override()` and `credo.OptionalReadiness()` are; `Closer` reads as `io.Closer`, an interface the value would implement. It stays opt-in, and `Close` is never discovered.
- **No alias is kept.** The old names go in this release, and every use in the code, the tests, the examples, the guides and the specs moves to the new ones.

**Tests.**

- The existing tests of both options run under the new names.
- A duplicate binding with `credo.ComponentName` on either registration panics with the message above, and one without it keeps today's message.

**Documentation.** ADR-024's options with their reasons (W1), ADR-004's registration options, the container spec's registration options, the lifecycle spec, the dependency-injection guide's part on writing one's own component, and every document, godoc and example that names either option. Migration rows: `credo.Named(name)` becomes `credo.ComponentName(name)`, and `credo.Closer()` becomes `credo.CloseOnShutdown()`. CHANGELOG names both under the release's breaking changes.

## Out of scope

- Splitting `internal/di` into DI and lifecycle. It is judged once W3 and W5 have landed, with `step.go` gone and worker constructors in place, by how narrow the seam between the two sides has become; the outcome is recorded in ADR-004 and TODO, and a split ships in a later release. The same checkpoint judges dividing `App`'s implementation into internal parts — its field count and the lifecycle manager's pointer back to it — which changes no API either.
- A `WithDebug` hint naming a binding that shows a `Close` shape and is not a component.
- The rest of the optional/critical policy: a component in general whose failed `Start` does not fail the start, which needs a reconnect contract (`sqldb`'s is `WithOptionalStart`, W3); a component that reports `degraded` itself, which no component produces yet; and a readiness ping on a connection outside the pool.
- A per-module tracer or meter override, should the observability release add such fields to `Infra`.
- `connection_id` in an access record written to a dedicated access logger, which waits for an `Attrs` option of the access log; `request_id` links the access record and the connection logs meanwhile.
- A stable event identity for framework records, with the observability release: it adds a key and renames none, since W2 settles the keys.
- Moving JWT out of `auth`, into `auth/jwtauth`: the pre-v1 reshaping plan's R11.
- ADR-023's deferred items: a worker kind that runs once at start, a terminal failure that stops the App, a lock hook for scheduled runs, and a per-run logger.
- The decisions left outside v0.24.0 in TODO's Architecture Governance.

The alternatives the ADRs record as rejected stay rejected.

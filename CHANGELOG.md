# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Credo is pre-1.0: minor (`0.x`) releases may contain breaking changes; when they do, the break is called out explicitly under **Changed** or **Removed**.

The `store/sqldb` submodule is versioned in lockstep with the root module (path-prefixed `store/sqldb/vX.Y.Z` tags — see [CONTRIBUTING.md#releasing](CONTRIBUTING.md#releasing)); its changes are recorded here.

## [Unreleased]

### Added

- **Semantic faults** — the new stdlib-only `fault` leaf package defines transport-neutral `Kind` values and a `Provider` seam. Credo's root error pipeline applies its default HTTP policy from the semantic kind before consulting the legacy `HTTPStatus()` interface, while an outer `*HTTPError` still overrides that default. Error logging consumes the already-classified response status instead of independently re-deriving policy.
- **Data access (`store`)** — structured semantic errors: `store.Error` carries `Kind`, optional operation/resource/constraint/driver-code metadata, `Transient`, and the original cause. New exact sentinels distinguish `ErrAlreadyExists`, `ErrConstraint`, `ErrSerialization`, `ErrDeadlock`, `ErrContention`, and `ErrUnavailable`; `KindOf` and `IsTransient` expose classification without transport coupling. `ErrDuplicate` remains an alias of `ErrAlreadyExists`, and specific constraint/concurrency errors continue to match the deprecated `ErrConflict` umbrella.
- **Data access lifecycle ownership** — `store.WithCallerOwnedLifecycle()` is
  the explicit shutdown opt-out for a value that uses a separate
  `WithLifecycle(lc)` health handle. Direct `Lifecycle` values remain
  framework-owned after successful registration. The optional
  `store.LifecycleIdentityProvider` extension lets a semantic wrapper return
  the underlying resource pointer (or another stable identity token); an
  embedded `*sqldb.DB` promotes the adapter's implementation automatically.
  `App.CanProvideValue[T]()` provides the store registration path (and other
  composition helpers) a non-mutating frozen/direct-duplicate preflight; it is
  point-in-time only and does not reserve T or guarantee that a later
  normal/protected publication succeeds.
- **Protected DI values** — `App.ProvideProtectedValue[T]` publishes a
  pre-built singleton that `App.Replace[T]` cannot overwrite;
  `App.ProtectBinding[T](expected ...T)` adds the same protection to an existing
  direct binding before Finalize. With no expected value it is idempotent and
  does not resolve T. With one expected value it performs an atomic CAS-style
  compare-and-protect against `Replace`: the singleton must already be resolved,
  comparable, and unchanged, otherwise no protection is added. These are
  low-level integration primitives for values coupled to external lifecycle or
  health state; ordinary application bindings should remain
  override-friendly with `ProvideValue`.
- **Access logging** — `WithAccessLogSkipper(func(*credo.Context) bool)` installs a pre-dispatch predicate that excludes matching requests from the built-in access log without disabling it. The new `credo.MetaAccessLog` route meta (`route.SetMeta(credo.MetaAccessLog, false)`) silences a single route or, by `LookupMeta` inheritance, a whole group; a route-level value overrides its group's, and `middleware.AccessLog` honours the same meta. See [ADR-010](docs/adr/010-middleware-architecture.md).
- **Health checks** — `HealthConfig.LogRequests` (default `false`) keeps `/health` and `/ready` probe requests out of the access log; set it to `true` to log them. Because the meta is applied per route, `true` re-enables logging even under a group that silenced access logging. See [ADR-016](docs/adr/016-health-checks.md).
- **TLS** — `WithTLSFiles(certFile, keyFile)` and `WithTLSConfig(*tls.Config)` configure HTTPS, as do the `server.tls.cert_file` / `server.tls.key_file` config keys. Sources resolve by precedence (`WithTLSConfig` > `WithTLSFiles` > `server.tls.*` > plaintext; whole-source override, never a conflict error). `Run` and `RunContext` serve HTTPS automatically when TLS is configured; the certificate is validated once at preflight, so a bad cert — or an explicitly-set-but-empty/nil source (`WithTLSConfig(nil)`, `WithTLSFiles` with an empty path) — fails fast and rolls the state back to `building` rather than silently downgrading to a lower-precedence source or plaintext. `ServeContext` is TLS-exempt — wrap the listener with `tls.NewListener` for HTTPS. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **HTTP→HTTPS redirect** — `WithHTTPRedirect(addr)` runs a second, plaintext listener that permanently redirects every request to its HTTPS equivalent (301 for GET/HEAD, 308 for other methods). It requires TLS (preflight fails fast otherwise) and binds, serves, and drains with the main server — closing before the main server on drain, and tearing the app down if the redirect listener fails at runtime, so a requested redirect never silently dies. `ServeContext` ignores it. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Configuration** — typed-snapshot getters over `Unmarshal`: `config.(*Config).Get[T](key) (T, error)` plus `MustGet[T]`, and `(*credo.App).GetConfig[T](key) (T, error)` plus `MustGetConfig[T]`. Each decodes a config section into a value of `T` in one call (the `Must` forms panic, matching the `MustProvide`/`MustResolve` family); there is deliberately no `MustLoad`. They are composition-root sugar — a handler has no `App` accessor, so typed config still flows to services via DI. See [ADR-005](docs/adr/005-configuration-architecture.md).
- **Data access (`store/sqldb`)** — typed terminal methods on `*SelectQuery` (Go 1.27 concrete-type generic methods): `One[T](ctx) (T, error)` and `All[T](ctx) ([]T, error)` return the queried type directly instead of scanning into a caller-supplied destination. `T` drives both the table and the destination, so the query is built model-less (`db.Select().Where(...).One[User](ctx)`). `One` applies `LIMIT 1` (first row; multiple matches are not an error — add `OrderExpr` for determinism) and maps no-row to `store.ErrNotFound`; `All` returns a non-nil empty slice with a nil error when nothing matches. Both use internal execution snapshots and inject the ambient transaction exactly like `Scan`. See [ADR-015](docs/adr/015-data-access.md).
- **Data access (`store/sqldb`)** — `(*SelectQuery).Page[T](ctx, req) (*pagination.Page[T], error)`, a typed pagination terminal completing the `One`/`All`/`Page` family. It runs COUNT + a LIMIT/OFFSET SELECT and returns a ready `*pagination.Page[T]`; `T` drives the table, so the query is built model-less (`db.Select().Where(...).OrderExpr(...).Page[User](ctx, req)`). `BindQuery` still applies `PageRequest.Validate`'s forgiving defaults/clamp policy, while the terminal copies the request and independently enforces strict execution invariants before COUNT. Nil, non-positive, native-offset-overflow, and Bun v1.2.18 signed-int32 LIMIT/OFFSET violations wrap `pagination.ErrInvalidPageRequest`; the caller's request is never mutated and valid custom `PerPage` values above 50 remain honoured. On zero rows SELECT is skipped and the page preserves the snapshot's page/per-page with a non-nil empty slice. COUNT and SELECT use internal execution snapshots and both join the ambient transaction, but a normal Read Committed transaction does not make the two statements share a snapshot. See [ADR-015](docs/adr/015-data-access.md).
- **Data access pagination count contract** — `Page.Total` is the cardinality of
  the complete logical projection before ordering and the page window: an
  ungrouped aggregate normally contributes one result row, `Distinct` counts
  selected projection tuples, `Group` counts groups, and `Group` + `Having`
  counts the groups left after `Having`. Credo now counts a universal outer
  `_credo_count_source` derived table after stripping root ORDER/LIMIT/OFFSET/FOR;
  its behavior is pinned by conformance tests. `Count` and `Page` reject
  standalone `Having` and direct `UNION`/`INTERSECT`/`EXCEPT` shapes with
  `sqldb.ErrUnsupportedCountQuery` before database I/O; restructure those
  shapes behind an outer derived table/CTE and compose an explicit count query,
  data query, and `pagination.NewPage`. The logical projection is evaluated by
  the count source; expensive, volatile, or set-returning projections should
  use that explicit custom composition. A first-class custom-count strategy is
  deferred until two real consumers need the same abstraction. `Page` retains
  exact-total metadata; total-free offset windows remain a separate future
  `Slice`/cursor response rather than overloading `Page` with unknown totals.
  Logical COUNT also runs the model SELECT hook lifecycle on its private source
  (`BeforeSelect`, `BeforeAppendModel`, and successful-query `AfterSelect`), so
  hook-added filters/projections contribute to `Total`; a `Page` that reaches
  its data SELECT runs the lifecycle once for COUNT and once for SELECT. Count
  does not scan or mutate a bound model, and nondeterministic hooks/volatile SQL
  can still differ across the two executions regardless of isolation. MySQL
  projections are preflighted for its unique derived-column-name rule;
  duplicate names, wildcards, implicit aliases, and unprovable raw-expression
  names fail before I/O, while unique portable `AS` aliases pass. The proof
  checks both normal MySQL escaping and `NO_BACKSLASH_ESCAPES`, so session mode
  cannot hide a projection separator or executable comment. The outer count
  query preserves `QueryEvent.Model` for observability while soft-delete policy
  remains solely on the inner source.
- **Pagination snapshot guidance** — `Page` does not start an implicit
  transaction. PostgreSQL Read Committed can observe different statement
  snapshots; an outer read-only Repeatable Read transaction is the documented
  PostgreSQL/MySQL path when one snapshot is required. MySQL's guarantee is
  limited to InnoDB ordinary nonlocking reads and configured isolation. SQLite
  uses the first-read snapshot of an explicit transaction; WAL permits a
  concurrent writer while rollback-journal mode may serialize it. The pinned
  modernc SQLite driver does not reliably enforce `sql.TxOptions.Isolation` or
  `ReadOnly`, so SQLite callers use plain `InTx`; driver-capability validation is
  deferred as a fail-loud follow-up.
- **Data access (`store`)** — typed transaction scopes: `NewTxScope[T]()` fixes the transaction contract once, and `scope.WithTx`, `GetTx`, `RequireTx`, and `Conn` all use the same `T`. This prevents a concrete transaction from being stored and then silently missed through an interface-typed read. `WithTx` rejects nil and typed-nil transaction handles; `RequireTx` and its free-function counterpart return the new `store.ErrTxMissing` instead of falling back. Distinct scopes isolate multiple logical connections with the same transaction type. See [ADR-015](docs/adr/015-data-access.md).
- **Data access (`store/sqldb`)** — `DB.Conn(ctx) bun.IDB` exposes the active per-DB transaction, or the base DB outside a transaction, for advanced native Bun operations; `DB.RequireTx(ctx)` returns `store.ErrTxMissing` rather than falling back. Native executions participate in the selected transaction but still bypass Credo error mapping. See [ADR-015](docs/adr/015-data-access.md).
- **Data access (`store/sqldb`)** — `WithTxCleanupTimeout(d)` configures how long Credo waits for each nested savepoint creation/release/rollback and fail-safe ambient abort (default five seconds, positive values only). Callback duration is unaffected. Uncertain nested operations synchronously mark shared state rollback-only; an outer callback that swallows the inner error returns the new `ErrTxRollbackOnly` rather than committing.
- **Pagination** — `(*pagination.Page[T]).Map[U](fn func(T) U) *Page[U]` (a Go 1.27 generic method) returns a new page with each record transformed by `fn`, carrying the pagination metadata (`Total`, `Page`, `PerPage`, `TotalPages`) over unchanged. It is the canonical model→DTO step: build `Page[Model]` from `SelectQuery.Page` and `Map` it to `Page[DTO]`, so an intermediate `Page[Model]` no longer means hand-copying metadata. `fn` must be pure — a nil `fn` panics, even for an empty page; for a fallible conversion, map in the service and build with `NewPage`. See the [pagination spec](docs/specs/pagination.md).

### Changed

- **BREAKING — `SelectQuery.Count` now counts complete logical projection rows.**
  It wraps the projection as `_credo_count_source` after removing root
  ORDER/LIMIT/OFFSET/FOR, so ungrouped aggregate, distinct, and grouped queries
  return their result-row cardinality instead of Bun's replacement-projection
  count. The projection can now be evaluated during COUNT. Standalone `Having`
  and direct compound roots, which previously could error or return a misleading
  count, fail before I/O with `ErrUnsupportedCountQuery`; use an explicit
  derived-table count/data composition for those or for expensive/volatile
  projections. MySQL also fails loud before I/O when the derived-source output
  names are duplicate or cannot be proved under either MySQL backslash-escape
  mode; raw expressions use unique `AS` aliases.
- **BREAKING — `pagination.PageRequest.Offset` now returns `(int, error)`.** Migrate `offset := req.Offset()` to `offset, err := req.Offset()` and handle `pagination.ErrInvalidPageRequest` with `errors.Is`. Unlike `Normalize`/`Validate`, `Offset` is strict and non-mutating: it rejects non-positive values and native `int` multiplication overflow instead of silently producing an unsafe offset. `SelectQuery.Page` adds the narrower Bun v1.2.18 signed-int32 LIMIT/OFFSET check and rejects every invalid request before COUNT; it does not clamp valid custom page sizes.
- **Data access (`store/sqldb`)** — curated `SelectQuery.Limit` and `Offset` now reject values outside Bun v1.2.18's signed-int32 storage range with `sqldb.ErrInvalidLimitOffset`; the builder records the error and its terminal returns before database execution instead of allowing an `int`→`int32` narrowing. Values inside the range, including zero and negative values, retain Bun semantics. Raw Bun reached through `Apply` or `Unwrap` remains an explicit escape hatch and is not covered by the curated-method guard.
- **BREAKING — store registration ownership and publication are now explicit.**
  `WithLifecycle(lc)` no longer succeeds with a warning when the registered
  value cannot be shut down by DI; pair it with
  `WithCallerOwnedLifecycle()` and arrange shutdown yourself, or make the
  registered value implement the complete `store.Lifecycle` contract. A value
  that already implements `Lifecycle` may no longer supply a second lifecycle
  or caller-owned opt-out, and a Shutdowner-only value cannot split Shutdown
  from Ping/Health. Ownership transfers only when `Register` succeeds; every
  failure remains caller-owned. For a framework-owned value DI is the sole
  framework shutdown owner: a teardown makes at most one attempt if its live
  deadline reaches the registration, and may make none if the deadline expires
  first. Migrate embedded wrappers by removing their redundant `WithLifecycle`
  option. Migrate named-field wrappers by delegating all Lifecycle methods
  (framework-owned) or adding the explicit caller-owned option and an
  `OnShutdown` hook.
- **BREAKING — store names, types, and declared resource identities are unique.**
  Explicit empty, padded, control-character, and reserved `credo.` names are
  rejected rather than normalized; omitted names use the pointer-unwrapped,
  package-qualified named type and unnamed types require `WithName`. Register
  privately reserves name, DI type, and resource identity, keeps pending
  entries invisible, and commits Registry visibility only after Ping and
  authoritative DI publication both succeed. Identity defaults to the
  top-level Lifecycle value itself; pointer-backed implementations are
  recommended. A semantic/named wrapper must implement
  `ResourceIdentity() any` explicitly and return its underlying pointer; Credo
  does not scan struct fields. Tokens must be non-nil, comparable, reflexively
  equal, and stable; non-comparable values and NaN-like tokens fail before Ping.
  Within the `store.Register` ledger, a repeated identity under another type or
  ownership mode is rejected. Migrate a second interface view to
  `app.Alias[I, T]()` instead of re-registering it. Raw `app.Provide`,
  `app.ProvideFactory`, `app.ProvideValue`, `app.ProvideProtectedValue`, or
  `app.Replace` calls are outside this ledger and must not publish the same
  lifecycle again; a caller-owned handle must not also be registered in DI as a
  Shutdowner.
  Concurrent external DI mutation can still win after the point-in-time
  preflight; final publication remains authoritative.
- **BREAKING — successful store and Registry DI bindings are protected.**
  `store.Register[R]` publishes `R` with `ProvideProtectedValue`, and the
  Registry binding is created protected or, when supplied by the composition
  root, resolved and validated before its pointer is atomically
  compare-and-protected against `Replace`. A raced replacement, invalid nil, or
  failing Registry binding remains unprotected and replaceable before Finalize
  so composition can repair it and retry. After adoption, later
  `app.Replace[R]` and `app.Replace[*store.Registry]` calls return an error
  instead of detaching DI from lifecycle/readiness state. Install
  test/application substitutes before registration or register the intended
  lifecycle value on a fresh App.
- **Data access health compatibility** — `store.Health` adds a JSON-excluded typed `Cause error` field, and `store/sqldb.Health` now places ping failures there instead of copying their text into `Details["error"]`. Keyed struct literals and `Lifecycle.Health(ctx) Health` implementations remain source-compatible; positional `store.Health{...}` literals must add the new field or migrate to keyed literals.
- **Data access (`store/sqldb`)** — driver error normalization is now context- and driver-family-aware. PostgreSQL SQLSTATE, strict MySQL error envelopes, and SQLite numeric codes preserve the original cause and driver code in `*store.Error`; constraint, serialization, deadlock, contention, timeout, unavailable, and read-only conditions are distinct. PostgreSQL `57014` consults cancellation/deadline state and maps an active request only when statement timeout is verified. MySQL 1205 and SQLite busy/locked map to contention rather than HTTP timeout; MySQL 1290 is no longer assumed read-only. Loose message matching was removed so domain/hook text such as “duplicate key validation” passes through unchanged.
- **BREAKING — `config.Load` and `config.LoadBytes` now return `*config.Config` instead of `credo.RawConfig`.** The concrete `*config.Config` still satisfies `RawConfig`, so passing the result to `credo.WithRawConfig` or storing it in a `RawConfig`/`credo.RawConfig` variable keeps compiling unchanged; only code that depends on the exact interface return type (for example assigning `config.Load` to a `func(...) (credo.RawConfig, error)` value) needs adjusting. The concrete return type is what carries the new `Get[T]`/`MustGet[T]` methods. See [ADR-005](docs/adr/005-configuration-architecture.md).
- **Lifecycle** — a failed startup (an `OnStart` hook returning an error) or a non-graceful `Serve` failure after the server reached `running` now runs the full teardown chain (DI container shutdown + `OnShutdown` hooks) and ends in the terminal `stopped` state, instead of rolling back to `building`. This releases resources an earlier `OnStart` hook started (workers, locks, connections) instead of leaking them. `OnShutdown` hooks consequently run on every teardown, including a failed startup, so they must be idempotent and must not assume any particular `OnStart` hook completed. Pre-session failures (TLS preflight, listener bind) still roll back to `building` and remain retryable. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Access logging** — the built-in access logger and `middleware.AccessLog` now share a single emit core (`internal/observe.EmitAccessLog`), keeping their attribute set, `"request completed"` message, and status→level mapping identical. No behavior change for existing callers.
- **TLS** — `Run` and `RunContext` now serve HTTPS when TLS is configured (previously plaintext-only); see **Added** and **Removed**.
- **BREAKING — typed Select terminals now require a model-less query.** `One[T]`, `All[T]`, and `Page[T]` no longer silently replace a model bound through `Select`, `Model`, or `Apply`; they return `sqldb.ErrTypedTerminalModel` before executing. Use `Model(&dest).Relation(...).Scan(ctx)` for bound models and relation loading, and use explicit-destination `Scan` for projections.
- **Data access (`store/sqldb`)** — `DB.Select`, `Insert`, `Update`, and `Delete` now accept at most one optional model argument. Supplying more causes the builder to record `sqldb: <Op> accepts at most one model, got N`; the terminal returns that error without executing instead of silently ignoring extra arguments.
- **BREAKING — `store.TxScope` is now `store.TxScope[T]`.** Construct it with `store.NewTxScope[T]()`; its methods no longer take their own type argument (`scope.WithTx[T](ctx, tx)` becomes `scope.WithTx(ctx, tx)`). The matching `*InScope` free functions now accept `*TxScope[T]`. The old unscoped `store.WithTx` / `GetTx` / `Conn` remain temporarily available but are deprecated because they cannot prevent cross-call concrete/interface mismatch or isolate same-type logical connections.
- **Data access (`store/sqldb`)** — transaction callbacks now have explicit failure semantics. After a successful rollback the exact callback error is returned without SQL mapping; callback and rollback failures retain both causes while only the rollback side is normalized. Nil callbacks return `ErrNilTxCallback` before BEGIN. Nested transactions accept nil/zero options but return `ErrNestedTxOptions` for non-default isolation/read-only options that Bun savepoints cannot apply. Savepoint creation is cancellation-aware; release/rollback remains usable after callback-context cancellation; a nil result after cancellation rolls back. Uncertain nested operations poison the ambient transaction before its bounded abort, preventing scheduler-dependent outer commits. Commit errors remain mapped but are documented as outcome-ambiguous, not unconditional retry signals.

### Removed

- **BREAKING — `(*store.Registry).Add` is removed.** Registry is now a
  read-only health view; only `store.Register` can create entries. This prevents
  health-only entries from bypassing startup Ping, DI publication, and shutdown
  ownership. Migrate direct `Registry.Add(name, lc)` calls to a typed
  `store.Register[R](app, value, ...)` registration.
- **BREAKING — `App.RunTLS` and `App.RunTLSContext` are removed.** TLS is now server configuration rather than a serve-method variant. Migrate by configuring TLS at construction and calling the plain entry points: `app.RunTLS(cert, key)` → `credo.New(credo.WithTLSFiles(cert, key))` then `app.Run()`; `app.RunTLSContext(ctx, cert, key)` → `WithTLSFiles` then `app.RunContext(ctx)`. For full `crypto/tls` control use `WithTLSConfig`. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **BREAKING — `auth.SetUser` / `auth.GetUser` / `auth.RequireUser` are removed.** The authenticated principal is now reached through generic `*credo.Context` methods instead of `context.Context` helpers: `ctx.SetUser(user)` (T inferred), `ctx.GetUser[T]()`, and `ctx.RequireUser[T]()` (returns `credo.ErrUnauthorized` wrapping the new `credo.ErrUserMissing`). `auth.Middleware` is unchanged at the call site — it now stores the user via `ctx.SetUser`. Migrate handler reads: `auth.GetUser[T](ctx.Context())` → `ctx.GetUser[T]()`. See [ADR-012](docs/adr/012-authentication-and-authorization.md).
- **BREAKING — `sqldb.Paginate` and `sqldb.PaginateRequest` are removed.** The typed `(*SelectQuery).Page[T]` terminal replaces `PaginateRequest` one-for-one: `sqldb.PaginateRequest[User](ctx, q, req)` → `q.Page[User](ctx, req)`. For a fallible model→DTO conversion, fetch a model-less `Page[Model]`, map `modelPage.Records` with ordinary error handling, and construct `pagination.NewPage(dtos, modelPage.Total, modelPage.Page, modelPage.PerPage)`. The ORM-agnostic pagination data shapes remain; the `PageRequest.Offset` signature change is described above. See [ADR-015](docs/adr/015-data-access.md).

### Fixed

- **Pagination** — `pagination.NewPage` now calculates ceiling division as quotient plus remainder, avoiding the `total + perPage - 1` overflow that could produce an invalid `TotalPages` near `math.MaxInt64`.
- **Health/readiness** — named and store checks now share one bounded parallel runner. Per-check deadlines are enforced even when callbacks ignore cancellation; overlapping requests join a stable in-flight probe so a hung dependency cannot accumulate one goroutine per Kubernetes poll. Store checks are independently timeout- and panic-isolated, typed `store.Health.Cause` values are logged and masked by default, arbitrary adapter statuses fail closed, and a custom/store name collision produces an explicit 503 instead of silently overwriting a response entry. All stores remain critical; `DEGRADED` is explicitly readiness-blocking. Optional/critical policy and bounded low-cardinality tags remain separate follow-up decisions. See [ADR-016](docs/adr/016-health-checks.md).
- **Data access (`store/sqldb`)** — Select terminal snapshots and public `SelectQuery.Clone` now preserve the execution fields Bun v1.2.18 omits: explicit connection, builder error, `WherePK`, soft-delete flags, and CTE materialization flags. Credo retains Bun's clone behavior for the rest. Public `Clone` is a top-level builder fork, not a recursive object-graph copy: a bound destination and nested CTE/relation query values may remain shared, so source and clone must not mutate or scan shared values concurrently.
- **Routing** — `App.Mount` is now atomic: a mount that conflicts with an existing explicit route registers nothing. `Mount` makes fourteen radix registrations (every forwarded method on both the exact prefix and the catch-all), and the radix tree has no delete, so a conflict discovered partway through previously stranded the earlier registrations as orphan routes — reachable by dispatch yet hidden from introspection. `Mount` now preflights every method/pattern pair and panics before mutating the tree if any explicit route already occupies one, leaving the router exactly as it was. See [ADR-007](docs/adr/007-router-and-routing.md).
- **Docs** — `WithLogger`'s godoc no longer claims a "nop logger" is used when it is left unset; the framework default logger (a text handler on stderr) is, so access and request logging are on by default with no configuration.

## [0.1.0] - 2026-06-10

Initial public release.

### Added

- **Routing** — radix-tree router (adapted from Chi) with `{param}`, regex constraints, and `{path...}` catch-all; named routes with `BuildURI`/`BuildURL`; route Meta with parent-chain lookup; host routing; app-level 404/405/5xx status handlers; trailing-slash redirect; automatic HEAD handling.
- **Context** — pooled `credo.Context` with a `Request`/`Response` split, one-step bind-and-validate (`BindBody`/`BindQuery`), internal rewrites (`Rewrite`/`OriginalPath`), and `Context()` for context-taking APIs.
- **Error handling** — `func(*credo.Context) error` handler signature, RFC 7807 Problem Details responses, pluggable `ErrorRenderer`, built-in panic recovery.
- **Middleware** — four-tier execution (built-in → global → group → route); built-ins on by default: Recover, RequestID, AccessLog (opt-out via `Without*`). Suite: CORS, CSRF (stdlib `CrossOriginProtection`), Secure, Compress, Timeout, RateLimit, Rewrite, ContractGuard.
- **Static files** — `os.Root`-sandboxed `app.Static`/`app.File`.
- **Dependency injection** — generics-based container (`Provide[T]`/`Resolve[T]`/`ProvideFactory`/`Alias`/`Replace`), validated graph freeze (`Finalize`), reverse-order shutdown, and the `credo.Infra` carrier (per-service logger; tracer/metrics with noop fallbacks).
- **Configuration** — koanf-adapted loader (`config.Load`), env-specific file derivation, `.env` support, typed config snapshots via `RawConfig.Unmarshal`.
- **Validation** — programmatic generic rules (`Rule[T]`, ozzo-style field refs), file-upload rules, RFC 7807 error output; no struct tags.
- **Authentication** — generic `auth.SetUser[T]`/`GetUser[T]`, JWT / Basic / API-key authenticators, middleware factory, RBAC-via-route-Meta pattern.
- **Internationalization** — JSON locale files, `ctx.T()`/`ctx.Locale()`, key-based error translation with three-level fallback.
- **Data access** — `store` contracts (universal errors, context-based transaction scope) and the `store/sqldb` Bun wrapper submodule: curated query-builder proxies, `db.InTx`/`RunInTx`, and migrations via a thin `bun/migrate` wrapper (`app.OnStart(db.Migrate)`, mark-applied-on-success default).
- **Background workers** — continuous and cron-scheduled workers (`worker.Register`, parser adapted from robfig/cron v3), panic recovery, graceful shutdown.
- **Health checks** — `/health` + `/ready` (Kubernetes-compatible), store-registry auto-integration, readiness errors masked by default.
- **Outbound HTTP** — `httpclient` package: `*http.Client` factory with a fixed RoundTripper chain (timeout → retry → logging → trace), safe-by-default retry, per-attempt structured logging, manual W3C trace context propagation.
- **Pagination** — query-param parsing and normalization, with `store`-integrated `Paginate` in `store/sqldb`.
- **Observability baseline** — structured logging (slog) wired by default with request IDs and access logs; OpenTelemetry tracing and Prometheus metrics are experimental with noop fallbacks.
- **testutil** — hermetic `testutil.NewApp` with DI overrides (`WithOverride`/`WithConfig`/`WithWiring`) and `LogBuffer` log assertions.

Adapted open-source code is attributed in [NOTICES](NOTICES); the per-component acquisition strategy is documented in [docs/adr/002-code-acquisition-strategy.md](docs/adr/002-code-acquisition-strategy.md).

[Unreleased]: https://github.com/credo-go/credo/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/credo-go/credo/releases/tag/v0.1.0

# TODO — Credo Framework Task Tracker

> This file tracks current progress across sessions.
> Tasks are marked `[ ]` → `[x]` upon completion.
> These tasks will be converted to GitHub issues when ready.

**Current phase**: Phase 3 — Enterprise Features
**Project status**: Beta -- Core APIs are stable enough for real application development.
Breaking changes may still happen before v1.

---

## Phase 1 — Foundation

### 1.1 Project Skeleton
- [x] Directory structure (28 packages)
- [x] go.mod (`github.com/credo-go/credo`, Go 1.26)
- [x] CLAUDE.md, README.md, LICENSE, CONTRIBUTING.md, SECURITY.md
- [x] .gitignore, .golangci.yml, Makefile
- [x] .github/ templates (PR, issues, CI workflow)
- [x] NOTICES file (third-party attribution)
- [x] Code Adaptation Strategy documented

### 1.2 Radix Tree Router (`internal/radix/` + root package)
> Note: `router/` package was merged into root package on 2026-02-28. Files now live in root: `mux.go`, `routectx.go`, `walk.go`, `route.go`, `group.go`.
**Source**: Chi `tree.go` (MIT, primary) + httprouter (BSD-3, reference) + Goyave (MIT)
**Spec**: [`docs/specs/router.md`](docs/specs/router.md)
**ADRs**: [`docs/adr/007-router-and-routing.md`](docs/adr/007-router-and-routing.md)
- [x] Adapt Chi `tree.go` → `internal/radix/` (split into method.go, context.go, pattern.go, sort.go, tree.go)
- [x] Add copyright headers (Chi primary, httprouter reference), update package name to `radix`
- [x] Adapt: `{param}` syntax, `{id:[0-9]+}` regex, `{path...}` catch-all
- [x] Copy Chi `mux.go`, `chi.go` → root package (originally `router/`, merged 2026-02-28)
- [x] Adapt: route groups, sub-router mounting
- [x] Implement `http.Handler` interface on `App`
- [x] **Fluent Route API** (Goyave): HTTP method registrations return `*Route` for chaining
- [x] **Route Meta system** (Goyave): `SetMeta(key, val)` / `LookupMeta(key)` with parent chain inheritance
- [x] **Named Routes** (Goyave): `route.Name("x")` + strict `BuildURI(params...)` / `BuildURL(params...)`
- [x] **StatusHandler** (Goyave): App-level 404/405/5xx handlers
- [x] **3-tier middleware** (Goyave): Global / Group / Route levels
- [x] **HEAD auto-handling**: GET routes automatically respond to HEAD (body discarded)
- [x] **Trailing slash redirect**: Auto 301/308 redirect when trailing-slash variant matches
- [x] Write `doc.go` for root package
- [x] Update NOTICES with exact files adapted
- [x] Tests pass, `make lint` clean

### 1.3 Root Package Types
**Source**: Echo `context.go` (MIT) + Goyave (MIT)
**Spec**: [`docs/specs/context.md`](docs/specs/context.md)
**ADRs**: [`docs/adr/011-validation-strategy.md`](docs/adr/011-validation-strategy.md), [`docs/adr/008-context-design.md`](docs/adr/008-context-design.md)
- [x] Define `Handler` type: `type Handler func(*Context) error`
- [x] Define `Validatable` interface: `Validate() error`
- [x] Define `Context` struct with core methods ([ADR-008](docs/adr/008-context-design.md))
  - [x] Response helpers: `JSON()`, `XML()`, `HTML()`, `Text()`, `Blob()`, `Stream()` (on `Response`)
  - [x] `BindBody()` — JSON decoder + auto-validate ("parse, don't validate")
  - [x] `BindQuery()` — stub returning 501 (see Phase 2.5)
  - [x] `RouteParams()`, `QueryParam()` (no `FormValue()` — see [ADR-008](docs/adr/008-context-design.md)); `RouteParam(name)` single-value shortcut avoids retaining the framework-owned params map
  - [x] `Request()`, `Response()`, `Set()`, `Get()`
  - [x] `NoContent()`, `Redirect()`
  - [x] `Route()` — access matched `*Route` (for Meta, Name, BuildURI)
- [x] Implement `context.go` as struct (inspired by Echo, [ADR-008](docs/adr/008-context-design.md))
- [x] Define `App` struct with `New()` constructor
  - [x] HTTP method registrations return `*Route` (fluent API)
  - [x] `GlobalMiddleware()` for global middleware (runs on 404/405 too)
  - [x] `Group()` for route groups, `group.Middleware()` for group-level middleware
  - [x] `Run()` for lifecycle (`Shutdown()` is placeholder — see Phase 2.5)
- [x] Define `Middleware` type: `func(Handler) Handler`
- [x] Define `Route` struct with fluent methods: `Name()`, `SetMeta()`, `Middleware()`
- [x] ~~Define `Component` struct~~ — **Removed**: replaced by explicit Infra injection ([ADR-004](docs/adr/004-dependency-injection-and-infra.md))
- [x] `ErrorRenderer` function type for pluggable error response formatting
- [x] Context pool via `sync.Pool` (root package `pool.go`)
- [x] `dispatch()` executes handler chain directly (no stdlib adapter needed)
- [x] Write `doc.go` for root package
- [x] Tests pass, `go vet` clean

> **Middleware Unification** (2026-02-28): `app.Use()` and `group.Use()` removed.
> Replaced by `app.GlobalMiddleware()`, `group.Middleware()`, `route.Middleware()`.
> `chain.go` deleted, `mux_test.go` merged into `credo_test.go`.
> `dispatch()` replaces `mux.ServeHTTP`/`routeHTTP` — runs middleware chain natively.

### 1.4 Basic Middleware
**Source**: Chi `middleware/` (MIT, core logic) + Echo `middleware/` (MIT, config struct + Skipper pattern)
> Note: Middleware converted to return `credo.Middleware` on 2026-02-28. `wrapResponseWriter` deleted (uses `credo.Response` directly).
- [x] `middleware/doc.go` — Package documentation
- [x] ~~`middleware/wrap_writer.go`~~ — Deleted: middleware now uses `credo.Response` directly
- [x] `middleware/requestid.go` — X-Request-Id injection + `GetReqID()` helper
  - [x] `RequestIDConfig`: Header, Generator, Limit
  - [x] crypto/rand + deterministic fallback (timestamp+counter)
- [x] Built-in panic recovery (`recover.go`) — outermost layer in `compile()`, `WithoutRecover()` opt-out
- [x] `middleware/recover.go` — Optional per-group/route recovery with `Recover(cfg ...RecoverConfig)`
  - [x] `RecoverConfig`: Logger, DisableStackTrace, StackSize
  - [x] Re-panic `http.ErrAbortHandler`, case-insensitive WebSocket upgrade check
- [x] `middleware/accesslog.go` — Structured request logging (slog)
  - [x] `AccessLogConfig`: Logger, Skipper
  - [x] Log level by status: 2xx/3xx=Info, 4xx=Warn, 5xx=Error
- [x] Add copyright headers (Chi + Echo attribution)
- [x] Tests: 32 tests (requestid + recover + logger), -race clean
- [x] Update NOTICES (Chi + Echo entries)

### 1.5 Configuration (`config/`)
**Source**: koanf (MIT)
**Spec**: [`docs/specs/config.md`](docs/specs/config.md)
- [x] Copy koanf core: `koanf.go`, `maps/` utils → `store.go`, `maps.go`
- [x] Adapt provider interface for Credo → `interfaces.go` (ByteProvider, MapProvider, Parser)
- [x] Implement providers:
  - [x] `env` — Environment variables (`provider_env.go`)
  - [x] `dotenv` — `.env` file parser (`provider_dotenv.go`, original inline parser)
  - [x] `file` — JSON/YAML file reader (`provider_file.go`)
- [x] RawConfig interface: `Unmarshal(key, dst) error` + `Exists(key) bool`
  - [x] Unmarshal supports both structs and primitives
  - [x] ~~`config.Get[T]` / typed getters~~ removed — RawConfig 2-method design
- [x] Priority order: env vars > .env > config files
- [x] Parsers: `parser_json.go` (encoding/json), `parser_yaml.go` (gopkg.in/yaml.v3)
- [x] Orchestration: `config.go` (Config struct, New, Load, Options, RawConfig compliance)
- [x] `config/doc.go`
- [x] Tests: 89 tests (unit + integration with temp files), -race clean
- [x] Update NOTICES
- [x] External deps added: `gopkg.in/yaml.v3`, `github.com/go-viper/mapstructure/v2`

---

## Phase 2 — Core Services

### 2.1 DI Container (`internal/di/`) + `credo.Infra`
**Source**: samber/do (MIT)
**Spec**: [`docs/specs/container.md`](docs/specs/container.md)
**ADRs**: [`docs/adr/004-dependency-injection-and-infra.md`](docs/adr/004-dependency-injection-and-infra.md)
- [x] Adapt samber/do core: container, lifecycle types
  - [x] **Key divergence**: typed constructor params (not `func(Injector)` signature)
  - [x] `credo.Provide[T](app, constructor)` — register with typed constructor
  - [x] `credo.ProvideFunc[T](app, fn)` — compiler-checked constructor closure; fn resolves its own deps, opaque to Finalize graph validation
  - [x] `credo.ProvideValue[T](app, value)` — register pre-built value
- [x] `credo.Resolve[T](app)` — retrieve instance
- [x] `credo.MustResolve[T](app)` — panics if not found
- [x] Lifecycle support: `Singleton` (only — RequestScoped removed)
- [x] `Alias[I, T]()` — interface-to-concrete type alias
- [x] `BindMany[I, T]()` / `ResolveAll[I]()` — ordered interface collections
- [x] `[]I` constructor injection via `BindMany` (empty slice when unbound)
- [x] `Finalize()` — freeze container + validate dependency graph
- [x] Shutdown: `Shutdowner` interface (root package), reverse-order `Shutdown()`
- [x] Validation via `Finalize()` — missing deps, cycle detection (DFS)
- [x] `internal/di/doc.go` with samber/do attribution
- [x] Tests: container tests (provide, resolve, lifecycle, concurrent singleton)
- [x] Update NOTICES (samber/do attribution for internal/di/)

### 2.2 credo.Infra — Infrastructure Carrier
**Source**: Original
**Spec**: [`docs/specs/container.md`](docs/specs/container.md)
**ADR**: [`docs/adr/004-dependency-injection-and-infra.md`](docs/adr/004-dependency-injection-and-infra.md)
> ⚠️ **2026-06-11 — Infra slimmed (pre-v1 breaking change).** The speculative
> `Metrics`/`Tracer` carriers — `MeterProvider`/`TracerProvider`/`Counter`/
> `Histogram`/`Span` interfaces and `WithMetrics`/`WithTracer` options — were
> removed; `Infra` now carries `Logger` only. Phase 3.5 redesigns the
> observability surface against real OTel/Prometheus adapters (see ADR-004
> amendment).
- [x] Implement `credo.Infra` struct in root package (`infra.go`)
  - [x] `Logger` → `*slog.Logger` (scoped with service name, fallback: framework stderr logger)
  - [x] ~~`Metrics` → `MeterProvider`~~ (removed 2026-06-11 — see note above)
  - [x] ~~`Tracer` → `TracerProvider`~~ (removed 2026-06-11 — see note above)
- [x] Define root package interfaces: `RawConfig`, `Shutdowner` (`interfaces.go`) ~~+ `MeterProvider`/`TracerProvider`~~
- [x] Container Infra detection: type switch on constructor param (Model 1)
- [x] Per-service scoping: Logger tagged with `"service"="TypeName"`
- [x] Default-logger fallback: Logger defaults to the framework stderr logger when not configured
- [x] Tests: Infra production, scoping, noop fallback, direct construction in tests
- [x] `app.NewInfra(name)` — scoped Infra outside DI (middleware, startup code)
- [x] `config.Load()` returns `credo.RawConfig` (compile-time verified)
- [x] ~346 total tests across all packages, all passing with `-race`

### 2.3 Validation Engine (`validation/`)
**Source**: ozzo-validation (MIT, API design) + Goyave (MIT, organization) + govy (architecture inspiration only)
**Spec**: [`docs/specs/validation.md`](docs/specs/validation.md)
**ADR**: [`docs/adr/011-validation-strategy.md`](docs/adr/011-validation-strategy.md)
- [x] Implement generic `Rule[T]` interface, `ValidateStruct()`, `Field[T]()`
- [x] Pointer-based field refs with cached reflection for field name extraction
- [x] `Validatable` interface in root package (auto-called by `BindBody`/`BindQuery`)
- [x] Dev-mode warning when bind target lacks Validatable (`WithDebug()` / `server.debug`)
- [x] **PATCH support**: `NilSafe[T]` wrapper for pointer fields (skip validation when nil)
- [x] Implement rules (topic-based grouping):
  - [x] `string_rules.go` — Required, Email, URL, UUID, Regex, Length
  - [x] `numeric_rules.go` — Min, Max, Between
  - [x] `collection_rules.go` — Each, When, NilSafe
  - [x] `date_rules.go` — DateBefore, DateAfter
  - [x] `common_rules.go` — In, NotNil, By (inline custom)
- [x] `ValidationError` struct with `Code` + `Params` fields (i18n-ready)
- [x] RFC 7807 Problem Details error format (`Errors` type)
- [x] `validation/doc.go`
- [x] Tests per rule group + integration tests (86 tests, -race clean)
- [x] Update NOTICES
- [x] **File validation rules** — 🚩 v0.1.0 LAUNCH SCOPE — `Rule[T]` implementations for `*multipart.FileHeader` (`validation/file_rules.go`):
  - [x] `MaxFileSize(bytes)` — reject files exceeding size limit
  - [x] `AllowedMimeTypes(types...)` — restrict to specific MIME types (case-insensitive, ignores media-type params)
  - [x] `AllowedExtensions(exts...)` — restrict to specific file extensions (case-insensitive, optional leading dot)

### 2.4 Error Handling
- [x] RFC 7807 `ProblemDetails` struct
- [x] Default `ErrorRenderer` on `App` (internal `handleError` method)
- [x] HTTP error types: `NewHTTPError(code, message)`
- [x] Validation error → Problem Details conversion
- [x] Tests

### 2.5 Binding & Lifecycle Completion
> These items were deferred from Phase 1.3 as stubs. They must be completed before Phase 3.

**BindQuery implementation** — [`docs/specs/context.md`](docs/specs/context.md) lines 156-180
- [x] Decode query params into struct via `query:"name"` struct tags (reflection)
- [x] Auto-validate via `Validatable` interface after decoding
- [x] Tests

**BindBody decoder expansion** — [`docs/specs/context.md`](docs/specs/context.md) lines 128-131
- [x] Content-Type detection (sniff `application/json`, `application/xml`, `application/x-www-form-urlencoded`, `multipart/form-data`)
- [x] XML decoder (`encoding/xml`)
- [x] Form URL-encoded decoder
- [x] Multipart form decoder (including `*multipart.FileHeader` and `[]*multipart.FileHeader` binding)
- [x] Tests

**App Lifecycle State Machine** — [`docs/specs/lifecycle.md`](docs/specs/lifecycle.md)
- [x] `appState` state machine: `building` → `running` → `stopping` → `stopped`
- [x] Store `*http.Server` reference in `App`
- [x] `Shutdown(ctx)` — graceful drain + LIFO shutdown hooks + `errors.Join`
- [x] `OnShutdown(fn)` — register shutdown hooks
- [x] `State()` / `IsRunning()` — public accessors
- [x] `OnStart(fn)` — register FIFO startup hooks
- [x] `Addr() net.Addr` — actual bound address accessor
- [x] Registration guards: `checkFrozen()` on addRoute, Mount, StatusHandler, SetMeta, OnStart, OnShutdown
- [x] `frozen bool` → `atomic.Bool` (thread-safe)
- [x] Tests (17 tests: state transitions, graceful drain, hooks, frozen guards)

**App Config Bootstrap** — [ADR-005](docs/adr/005-configuration-architecture.md), [ADR-006](docs/adr/006-application-lifecycle.md)
- [x] `credo.New(credo.WithRawConfig(store))` — functional options, RawConfig auto-registered in DI
- [x] Server config (host, port, timeouts) framework-internal — no user-facing CoreConfig
- [x] `Run()` / `Shutdown()` / `RunTLS()` lifecycle
- [x] `config.Load(opts...)` returns `credo.RawConfig`

**Import Boundary Fitness Test**
- [x] `architecture_test.go` — verifies root package doesn't import feature packages (go/parser)

---

## Phase 3 — Enterprise Features

### 3.1 i18n (`internal/i18n/`)
**Source**: go-i18n (MIT)
**ADR**: [`docs/adr/013-internationalization.md`](docs/adr/013-internationalization.md)
- [x] Adapt go-i18n core to `internal/i18n/`: Bundle, Localizer, Message types
- [x] CLDR plural rule support (`internal/i18n/internal/plural/` — 200+ languages)
- [x] String-based public APIs: `TranslateForLang()`, `FieldNameForLang()`, `HasMessages()`
- [x] `TranslationKeyer` interface in root package (replaces `ErrorTranslator`)
- [x] Default error renderer i18n-aware (nil bundle check, zero cost when unused)
- [x] `app.UseI18n(...)` — root package setup (bundle + locale detection middleware)
- [x] `ctx.Locale()` / `ctx.T(key, data...)` — request-scoped locale access
- [x] `I18nConfig` with zero-config defaults, RawConfig auto-read
- [x] JSON-only locale file loader (directory-per-locale: `{lang}/messages.json` + `fields.json`)
- [x] Two-mode field name translation (default: field-agnostic, opt-in: `fields.json`)
- [x] Built-in locale files: English + Turkish (`internal/i18n/locales/`)
- [x] Tests: internal engine + root integration + white-box translate tests
- [x] Update NOTICES (go-i18n MIT attribution)

### 3.2 Auth & Security (`auth/` + `middleware/`)
**Spec**: [`docs/specs/auth.md`](docs/specs/auth.md)
**ADR**: [`docs/adr/012-authentication-and-authorization.md`](docs/adr/012-authentication-and-authorization.md)
**Source**: Original (user accessors), Echo `middleware/` (MIT, implementations)

**Auth User Accessors** (`auth/`):
- [x] `auth.SetUser[T](ctx, user)` — store user via `context.WithValue` (unexported key)
- [x] `auth.GetUser[T](ctx)` — retrieve user with generic type safety
- [x] `auth.RequireUser[T](ctx)` — retrieve or return `ErrUserMissing`
- [x] `Authenticator[T]` interface: `Authenticate(r *http.Request) (T, error)`
- [x] `ErrorFunc` type for custom auth failure responses
- [x] `auth.Middleware[T](authenticator, onError)` — middleware factory
- [x] `auth/doc.go`
- [x] Tests (14 tests: set/get, type mismatch, nil, middleware factory, integration)

**Auth Implementations** (`auth/`):
- [x] `auth/jwt.go` — JWT validation (Authenticator[T] implementation)
- [x] `auth/apikey.go` — API key (header/query)
- [x] `auth/basic.go` — HTTP Basic

**Security Middleware** (`middleware/`):
- [x] `middleware/secure.go` — Security headers (HSTS, CSP, X-Frame)
- [x] `middleware/cors.go` — CORS with config struct
- [x] `middleware/ratelimit.go` — from go-limiter (Apache-2.0)
- [x] `middleware/compress.go` — gzip/deflate response compression (Chi source)
- [x] `middleware/timeout.go` — request timeout (Echo source)
- [x] `middleware/csrf.go` — CSRF protection via stdlib `net/http.CrossOriginProtection` (`CSRF(cfg ...CSRFConfig)`: TrustedOrigins, InsecureBypassPatterns, ErrorHandler; rejections → 403 RFC 7807 via error pipeline)
- [x] Tests per middleware
- [x] Update NOTICES

### 3.3 Store (`store/` + `store/sqldb/`)
**Source**: GoFr (Apache-2.0, health/interface design), Goyave (MIT, connection patterns), uptrace/bun (BSD-2-Clause, wrapped)
**Spec**: [`docs/specs/store.md`](docs/specs/store.md)
**ADR**: [`docs/adr/015-data-access.md`](docs/adr/015-data-access.md)

**Phase 3.3a — Core Package** (`store/`):
- [x] `ErrNotFound`, `ErrDuplicate`, `ErrConflict`, `ErrTimeout`, `ErrReadOnly` sentinels
- [x] `Lifecycle` interface (Ping, Shutdown, Health)
- [x] `Health` / `HealthStatus` types
- [x] `Registry` — LIFO shutdown, HealthAll, duplicate name rejection
- [x] `Register[R]()` — ping + DI + Registry registration
- [x] `RegisterOption`: `WithName`, `WithPingTimeout`, `WithLifecycle` (`WithCritical`, `WithTags` deferred to health package)
- [x] TX context helpers: `WithTx[T]`, `GetTx[T]`, `Conn[T]`
- [x] Tests (errors, registry, register, tx context)
- [x] `store/doc.go`

**Phase 3.3b — Bun Wrapper** (`store/sqldb/`, submodule: `github.com/credo-go/credo/store/sqldb`):
- [x] `DB` type wrapping `*bun.DB` with lifecycle methods
- [x] `Config` struct (Driver, Host, Port, Name, User, Password, DSN, ConnectTimeout, MaxOpen, MaxIdle, MaxLifetime, SSLMode, Options)
- [x] `Open(cfg, opts...)` — factory with functional options
- [x] Error mapping (sql.ErrNoRows→ErrNotFound, unique→ErrDuplicate, FK→ErrConflict, timeout→ErrTimeout)
- [x] `RunInTx` / `RunInTxWith` — TX management with context propagation and savepoints
- [x] Query builder proxies: `SelectQuery`, `InsertQuery`, `UpdateQuery`, `DeleteQuery`
- [x] 5 guardrails: TX inject, Apply varargs+nil, Unwrap builder-only, QueryHook trace, raw terminals
- [x] `Client() *bun.DB` escape hatch
- [x] `Paginate[T]` helper for pagination package
- [x] Tests (db, config, error mapping, tx, query proxies, integration)

**Phase 3.3c** (deferred):
- [ ] Redis store contracts (depends on `pubsub/` design; also feeds the cache / rate-limit store / pubsub-backend stories)
- [ ] Observability QueryHook for automatic trace spans (depends on Phase 3.5)

**Phase 3.3d**:
- [x] Update NOTICES with GoFr (Apache-2.0) + Goyave + uptrace/bun attribution

**Phase 3.3e — Migrations & TX ergonomics**:
- [x] `db.InTx(ctx, fn)` — method-form TX sugar over `RunInTx` (handler-side ergonomics; called with `ctx.Context()`) — plus `db.InTxWith` for `sql.TxOptions` symmetry
- [x] Migration wrapper over `bun/migrate` (replaces the goose plan — see Phase 5.2):
  - [x] `db.RegisterMigrations(...)` — accept `*migrate.Migrations` (+ pass-through `migrate.MigratorOption`s; mark-applied-on-success by default so failed migrations are retried on next start)
  - [x] `OnStart` lifecycle integration (opt-in auto-run on app start) — `db.Migrate` matches the `App.OnStart` hook signature: `app.OnStart(db.Migrate)`
  - [x] `embed.FS` migration bundling support (Bun's `Discover` works on any `fs.FS`; covered by tests)
  - [x] Seeding: documented as plain migration files (no separate mechanism)
- [x] Tests (`migrate_test.go` + `InTx` cases in `integration_test.go`)

### 3.4 Health Checks (root package)
**Source**: Original (written from scratch)
> **2026-06-11 — engine folded into root.** The engine now lives unexported in
> the root package (`health_engine.go`); `internal/health/` holds only the
> module-internal DI seam (`StoreFunc`) through which `store.Register`
> contributes store health. `SetHealthStoreFunc`/`HealthStoreResult` were
> removed from the public API (see ADR-016 amendment). User-facing behavior
> is unchanged.
- [x] Engine with concurrent check execution (root, unexported; was `internal/health/`)
- [x] `HealthConfig` with `*bool` toggles, custom paths, check timeout
- [x] `/health` (liveness) + `/ready` (readiness) handlers via `app.UseHealth()`
- [x] `AddLivenessCheck` / `AddReadinessCheck` with `HealthChecker` interface
- [x] Store health integration ~~via `SetHealthStoreFunc` callback~~ via `internal/health.StoreFunc` DI seam (2026-06-11)
- [x] K8s probe compatible JSON responses (200/503)
- [x] Engine tests (`health_engine_test.go`)
- [x] Root package tests (`health_test.go`)
- [x] ADR-016 written
- [x] `HealthConfig.Group` — register health routes on a specific group (prefix + middleware)

### 3.5 Observability (`observability/`)
**Source**: GoFr (Apache-2.0) + slog-multi (MIT, study only)
> ⚠️ **v0.1.0 reframe.** Logging (slog) is real and featured; tracing (OTel) ships as *experimental*; a stable Prometheus metrics adapter is optional. Do **not** rush the full OTel wrapper before v1.
> **2026-06-11:** the speculative root-package `MeterProvider`/`TracerProvider` interfaces and `Infra.Metrics`/`Infra.Tracer` fields were removed (see §2.2 note). This phase starts from a clean slate: design the metrics/tracing carriers from real OTel/Prometheus adapters, aligned with the v1 / Go 1.27 window.
- [ ] Structured logging setup (slog handlers)
- [ ] OpenTelemetry trace provider wiring
- [ ] Prometheus metrics registry
- [ ] `middleware/metrics.go` — request latency histograms
- [ ] `middleware/tracer.go` — trace ID injection/propagation
- [ ] Auto-wired on `app.New()` with zero-config defaults
- [ ] No-op defaults, sampling config, and cost guardrails from day one
- [ ] Tests
- [ ] Update NOTICES

### 3.6 Pagination (`pagination/`)
**Source**: Original (no external source)
- [x] `Page[T]` generic response type
- [x] Offset/limit pagination
- [ ] Cursor-based pagination (future — separate `CursorPage[T]` type)
- [x] Auto-read `?page=`, `?limit=`, `?cursor=` from request
- [x] `Meta` struct (total, current_page, per_page, last_page)
- [x] Tests

---

## Phase 4 — Extended Features

### 4.1 Pub/Sub & In-Process Events (`pubsub/`)
**Source**: watermill (MIT)
> A typed in-process event API is pubsub's channel backend plus generics sugar,
> not a second eventing system.
- [ ] Copy Publisher/Subscriber interfaces, Message type
- [ ] Go channel in-process implementation
- [ ] `app.Subscribe("topic", handler)` registration
- [ ] Typed in-process events — generics sugar over the channel backend (absorbs the old `app.Emit()`/`app.On()` plan)
- [ ] Outbox pattern for DB transaction safety (later — `store` + `pubsub` integration)
- [ ] Backend implementations (demand-driven, one at a time): `pubsub/redis/` first (shares the Redis story with store contracts), then NATS / Kafka
- [ ] Tests
- [ ] Update NOTICES

### 4.2 Worker System (`worker/`)
**Source**: robfig/cron v3 parser (MIT, expression parser only)
- [x] Adapt cron expression parser from robfig/cron v3
- [x] `worker.Register(app, w, opts...)` API
- [x] Continuous + scheduled worker execution modes
- [x] Graceful shutdown (wait for active workers)
- [x] Integration with app lifecycle
- [x] Tests
- [x] Update NOTICES
- [ ] Observability hooks (metrics/tracing)

### 4.3 gRPC (`grpc/`)
**Source**: GoFr `grpc.go` (Apache-2.0)
> Deliberately **thin** — the value is shared
> lifecycle + DI + `Infra` interceptors (logging/recovery/tracing), not wrapping
> gRPC itself. No codegen tooling, no gateway/transcoding. Late Phase 4, after
> observability (interceptors need it).
- [ ] Dual-protocol from same `App` struct (shared lifecycle: `Run`/`Shutdown`)
- [ ] Shared DI container + `Infra` interceptors (logging, recovery, tracing)
- [ ] Tests
- [ ] Update NOTICES

### 4.4 WebSocket & SSE (`websocket/`)
**Source**: coder/websocket (ISC)
> SSE is dependency-free (stdlib flusher) and may
> ship first as a quick win — increasingly relevant for LLM/streaming responses.
> WebSocket follows via coder/websocket adaptation.
- [ ] `ctx.SSE()` for Server-Sent Events (stdlib-only — can land before WebSocket)
- [ ] Copy core upgrade + connection handling
- [ ] `ctx.Upgrade()` API for WebSocket
- [ ] Room/broadcast support
- [ ] Tests
- [ ] Update NOTICES

### 4.5 OpenAPI (`openapi/`)
**Source**: kin-openapi (MIT)
> Hardest retrofit on the roadmap — the handler
> signature (`func(*Context) error`) carries no request/response types, so spec
> generation needs a meta/registration-based design. Write `docs/specs/openapi.md`
> **before** implementation; do not rush this one.
- [ ] Design spec first: how routes declare request/response types (Route Meta vs explicit registration)
- [ ] Copy OpenAPI 3.x Go type definitions
- [ ] Auto-generate spec from Credo router registrations
- [ ] Request/response validation middleware
- [ ] Embedded Swagger UI handler
- [ ] Tests
- [ ] Update NOTICES

### 4.7 Contract Guards (`middleware/`) — 🚩 v0.1.0 LAUNCH SCOPE
**Source**: Original (builds on existing Route Meta system) · `middleware/contractguard.go`
> **Registration note:** ContractGuard reads matched-route Meta, so it is a
> **group/route** middleware (`group.Middleware(...)`), not `app.GlobalMiddleware` —
> app-global middleware runs *before* the route is matched. Applied globally it
> is a safe no-op. (Spec originally said "global"; corrected to match the
> Built-in → Global → Group → Route execution order.)
- [x] Define standard meta key constants in `middleware/` (`MetaAccept`, `MetaMaxBody`, `MetaRequireHeaders`, `MetaRequireQuery`, `MetaScope`, `MetaAPIVersion`)
- [x] `middleware.ContractGuard()` — single meta-driven middleware that reads meta and enforces:
  - [x] `Accept` → Content-Type check (wildcards, param-insensitive) → 415 Unsupported Media Type
  - [x] `MaxBody` → eager Content-Length + `http.MaxBytesReader` wrap → 413 Payload Too Large
  - [x] `RequireHeaders` → header existence check → 400 Bad Request
  - [x] `RequireQuery` → query param existence check → 400 Bad Request
  - [x] `Scope` → pluggable `ScopeChecker` (auth is generic); fail-closed when unset → 403 Forbidden
  - [x] `APIVersion` → version header (`X-API-Version`) or `version` path param check → 400 Bad Request
- [x] Group-level Meta inheritance for contract propagation (via `Route.LookupMeta` parent chain)
- [x] `ContractConfig.CustomChecks` extension point for user-defined checks
- [x] Tests per contract type (`contractguard_test.go`)
- [x] Document Meta-driven contract pattern in `docs/guides/middleware.md`

### 4.8 HTTP Client (`httpclient/`)
**Source**: Original (stdlib `net/http` wrapper)
> Outbound HTTP with retry/timeout/logging/trace propagation is universal in
> enterprise services. Built as a composable `http.RoundTripper` chain — works
> with existing stdlib tooling. **The lean core ships independently of Phase
> 3.5**; tracing/metrics hooks land when observability does. No `app.HTTPClient()`
> sugar — plain DI (explicit-first).
- [x] `httpclient.New(opts...)` — `*http.Client` factory with canonical RoundTripper chain: Client.Timeout → retry → logging → trace → base; composable `NewRetryTransport`/`NewLoggingTransport`/`NewTraceTransport` exports; spec: [`docs/specs/httpclient.md`](docs/specs/httpclient.md)
- [x] `WithTimeout`, `WithRetry(cfg ...RetryConfig)` — full-jitter backoff; `DefaultRetryIf` never retries POST/429/context cancellation; GetBody-only body replay; exhaustion returns the last response unchanged
- [x] Structured request/response logging via `WithLogging(*slog.Logger)` — package is stdlib-only; one line per attempt, query string + userinfo stripped, 5xx→Error / 4xx→Warn / else Info
- [x] W3C `traceparent` propagation via `WithTracePropagation()` + `TraceContextFromRequest`/`SetTraceContext`/`GetTraceContext`; child span ID per attempt, invalid inbound → new root
- [ ] Request/response metrics — duration histogram, status counter (depends on Phase 3.5)
- [ ] Circuit breaker — deferred (keep the core lean; revisit on demand)
- [x] Tests *(33 tests + 32 subtests across 4 files: retry/backoff/replay, level mapping + redaction, W3C parse/derive table, chain-order integration)*

### 4.9 Admin Server & Debug Endpoints (root package)
**Source**: Original (inspired by Yokai's core/app server split)
> Optional second HTTP server on an ops port — K8s-friendly separation of
> operational endpoints from public traffic. Internally stdlib `http.ServeMux`
> (few routes; no second router needed). **After Phase 3.5** (`/metrics` needs
> it; interim: `HealthConfig.Group` + IP restrict). **JSON only — no HTML
> dashboard**.
- [ ] `credo.WithAdminServer(addr)` option — starts/stops with app lifecycle
- [ ] Health endpoint relocation when admin is active (`/health`, `/ready` move; behavior unchanged when absent)
- [ ] `/metrics` — Prometheus (depends on Phase 3.5)
- [ ] `/debug/pprof/*` — Go pprof
- [ ] `/debug/routes` — registered route list (JSON)
- [ ] `/debug/config` — resolved config dump (sensitive-key masking)
- [ ] `/debug/di` — registered DI services (JSON)
- [ ] Minimal fixed middleware: recover + access log (no user stack)
- [ ] Tests

### 4.10 Controller Registration (root package)
**Source**: Goyave pattern
> Small convention API completing the Clean Architecture story: controllers
> implement an interface and register their own routes.
- [ ] Registration interface (e.g. `RegisterRoutes(g *credo.Group)`)
- [ ] `app.Register(controllers...)` / group-level equivalent
- [ ] Align CLI `credo make:controller` output with it (Phase 5.1)
- [ ] Tests

---

## Phase 5 — CLI and Tooling

### 5.1 CLI Tool (`cmd/credo/`)
> `credo new` is the priority — it delivers the
> "Clean Architecture as default via CLI scaffolding" promise (philosophy #2).
> `make:*` generators are secondary.
- [ ] `credo new <project>` — scaffold Clean Architecture project (priority)
- [ ] `credo make:controller <name>` (secondary; aligns with Phase 4.10)
- [ ] `credo make:usecase <name>` (secondary)
- [ ] `credo make:repository <name>` (secondary)
- [ ] `credo migrate:up`, `migrate:down`, `migrate:create` — optional sugar over the `store/sqldb` migration wrapper (Phase 3.3e), not a separate engine
- [ ] Tests

### 5.3 Examples
- [x] `examples/hello/` — Minimal hello-world (10 lines)
- [x] `examples/saas/` — Full SaaS scaffold (auth, validation, DI, middleware)

---

## Cross-Cutting Concerns

### Architecture Governance
- [ ] **Kernel + Modules model**: Kernel = root + router + middleware + internal (must be stable first)
- [ ] Optional modules (i18n, health, openapi, pubsub) mature independently via capability interfaces
- [ ] **Maturity labels** on each package `doc.go`: `experimental`, `beta`, `stable`
- [ ] **Capability interfaces** + contract test suites for each module boundary
- [ ] Keep root package re-export surface minimal — avoid premature aliases
- [ ] **Registration-time route validation** (`app.ValidateRoutes()` or auto-run before `app.Run()`):
  - [ ] Routes with `Scope` meta must have auth middleware registered
  - [ ] Routes with `Accept` meta must have ContractGuard middleware
  - [ ] Detect duplicate route patterns / conflicting registrations
  - [ ] Warn on routes without any middleware (optional strict mode)
  - [ ] **Param conflict detection**: same segment with different param names or regexes (e.g., `/{id:[0-9]+}` vs `/{name:[a-z]+}`) — warn or error at registration time
  - [ ] **Duplicate route warning**: `setEndpoint` currently overwrites silently — strict mode should error, lenient mode should log warning

### Performance Budgets
- [ ] Define threshold benchmarks for hot paths: router match, context pool, DI resolve
- [ ] CI benchmark regression tests (fail on >10% regression)
- [ ] Middleware overhead budget: max added latency per middleware layer

### Router Improvements
- [ ] **Mount introspection**: `Mount()` routes don't appear in `Routes()` — at minimum record mount point pattern in routeStore
- [x] **Document middleware ordering**: group middleware is collected at compile time from the group parent chain — registration order affects execution order only, never membership (semantics changed 2026-06-11 from registration-time capture; documented in `doc.go`, the middleware spec, and the guide)

### Deferred Features (from specs/ADRs)
> These are explicitly deferred — tracked here for visibility, not scheduled.
- [x] ~~**RequestScoped middleware**~~: Removed — RequestScoped lifecycle rejected in favor of context+middleware
- [x] Controller registration (Goyave pattern) — tracked in **Phase 4.10**
- [x] Static file serving — [ADR-017](docs/adr/017-static-file-serving.md), [Spec](docs/specs/static.md)
- [ ] BuildProxyURL — [ADR-007](docs/adr/007-router-and-routing.md), deferred to Phase 3+
- [ ] Optional `.Validate()` on Route — [Router Spec](docs/specs/router.md), deferred (add if demand warrants)
- [ ] `embed.FS` config provider — [Config Spec](docs/specs/config.md), deferred beyond Phase 1.5
- [x] Interface alias `Alias[I,T]()` for DI — implemented
- [ ] `app.Container()` ergonomic sugar — [Container Spec](docs/specs/container.md), deferred
- [ ] `Has[T]()` probe API — registration check without singleton construction, cleaner alternative to Resolve-if-missing-Provide pattern
- [ ] `cache/` contracts (in-memory + Redis) — consider together with Redis store contracts (Phase 3.3c); demand-driven, no commitment yet
- [ ] Fluent validation builder — v1 scope: requires Go 1.27 generic methods ([golang/go#77273](https://github.com/golang/go/issues/77273), release ~Aug 2026); v1 raises the minimum Go version to 1.27+; the programmatic `Rule[T]` API remains the substrate

### Documentation
- [ ] `doc.go` for every package (include maturity label)
- [ ] `example_test.go` for core packages (root, middleware, config)
- [x] ADRs tracked (18 total):
  - [x] [`001-framework-identity-and-goals.md`](docs/adr/001-framework-identity-and-goals.md) — Framework identity and goals
  - [x] [`002-code-acquisition-strategy.md`](docs/adr/002-code-acquisition-strategy.md) — Code acquisition strategy
  - [x] [`003-application-architecture.md`](docs/adr/003-application-architecture.md) — Application architecture
  - [x] [`004-dependency-injection-and-infra.md`](docs/adr/004-dependency-injection-and-infra.md) — DI container and credo.Infra
  - [x] [`005-configuration-architecture.md`](docs/adr/005-configuration-architecture.md) — Configuration architecture (RawConfig, typed config via DI)
  - [x] [`006-application-lifecycle.md`](docs/adr/006-application-lifecycle.md) — Application lifecycle
  - [x] [`007-router-and-routing.md`](docs/adr/007-router-and-routing.md) — Router and routing
  - [x] [`008-context-design.md`](docs/adr/008-context-design.md) — Context design
  - [x] [`009-handler-and-error-handling.md`](docs/adr/009-handler-and-error-handling.md) — Handler and error handling
  - [x] [`010-middleware-architecture.md`](docs/adr/010-middleware-architecture.md) — Middleware architecture
  - [x] [`011-validation-strategy.md`](docs/adr/011-validation-strategy.md) — Validation strategy
  - [x] [`012-authentication-and-authorization.md`](docs/adr/012-authentication-and-authorization.md) — Authentication and authorization
  - [x] [`013-internationalization.md`](docs/adr/013-internationalization.md) — Internationalization
  - [x] [`014-observability.md`](docs/adr/014-observability.md) — Observability (Draft; logging baseline accepted, tracing/metrics pending)
  - [x] [`015-data-access.md`](docs/adr/015-data-access.md) — Data access
  - [x] [`016-health-checks.md`](docs/adr/016-health-checks.md) — Health checks
  - [x] [`017-static-file-serving.md`](docs/adr/017-static-file-serving.md) — Static file serving
  - [x] [`018-host-routing-and-rewrite.md`](docs/adr/018-host-routing-and-rewrite.md) — Host routing and rewrite
- [ ] `docs/guides/quick-start.md`
- [ ] Write guide: Infra injection model (Model 1)

### CI/CD
- [ ] GitHub Actions: test matrix (Go 1.26, latest)
- [ ] Codecov or Coveralls integration
- [ ] Automated golangci-lint on PRs
- [ ] Release workflow with goreleaser

### Quality Gates
- [ ] 80%+ coverage for Phase 1-2 packages
- [ ] Zero lint warnings on `make lint`
- [ ] Benchmark suite for hot paths (router match, context pool, DI resolve)

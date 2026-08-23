# TODO — Credo Framework Task Tracker

> This file tracks current progress across sessions. Tasks are marked `[ ]` → `[x]` upon completion. These tasks will be converted to GitHub issues when ready.

**Current phase**: Phase 3 — Enterprise Features **Project status**: Beta -- Core APIs are stable enough for real application development. Breaking changes may still happen before v1.

---

## Phase 1 — Foundation

### 1.1 Project Skeleton

- [x] Directory structure (28 packages)
- [x] go.mod (`github.com/credo-go/credo`, Go 1.27)
- [x] CLAUDE.md, README.md, LICENSE, CONTRIBUTING.md, SECURITY.md
- [x] .gitignore, .golangci.yml, Makefile
- [x] .github/ templates (PR, issues, CI workflow)
- [x] NOTICES file (third-party attribution)
- [x] Code Adaptation Strategy documented

### 1.2 Radix Tree Router (`internal/radix/` + root package)

> Note: `router/` package was merged into root package on 2026-02-28. Files now live in root: `mux.go`, `routectx.go`, `walk.go`, `route.go`, `group.go`. **Source**: Chi `tree.go` (MIT, primary) + httprouter (BSD-3, reference) + Goyave (MIT) **Spec**: [`docs/specs/router.md`](docs/specs/router.md) **ADRs**: [`docs/adr/007-router-and-routing.md`](docs/adr/007-router-and-routing.md)

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

**Source**: Echo `context.go` (MIT) + Goyave (MIT) **Spec**: [`docs/specs/context.md`](docs/specs/context.md) **ADRs**: [`docs/adr/011-validation-strategy.md`](docs/adr/011-validation-strategy.md), [`docs/adr/008-context-design.md`](docs/adr/008-context-design.md)

- [x] Define `Handler` type: `type Handler func(*Context) error`
- [x] Define `Validatable` interface: `Validate() error`
- [x] Define `Context` struct with core methods ([ADR-008](docs/adr/008-context-design.md))
  - [x] Response helpers: `JSON()`, `XML()`, `HTML()`, `Text()`, `Blob()`, `Stream()` (on `Response`)
  - [x] `BindBody()` — JSON decoder + auto-validate ("parse, don't validate")
  - [x] Strict JSON bodies + typed decode errors: `encoding/json/v2` decoding with exactly one JSON value per body (trailing data rejected), duplicate object members rejected (incl. case-variant repeats; member matching stays v1 case-insensitive), decode failures return `*BindError` with machine-readable reasons (`syntax`, `type_mismatch`, `invalid_value`, `empty_body`, `trailing_data`, `duplicate_field`) rendered as RFC 7807 `errors[]` (code = reason, i18n via `bind.<reason>`); unknown-field rejection considered and rejected (must-ignore API evolution norm; strictness stays about ambiguity, not extensibility)
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

> **Middleware Unification** (2026-02-28): `app.Use()` and `group.Use()` removed. Replaced by `app.GlobalMiddleware()`, `group.Middleware()`, `route.Middleware()`. `chain.go` deleted, `mux_test.go` merged into `credo_test.go`. `dispatch()` replaces `mux.ServeHTTP`/`routeHTTP` — runs middleware chain natively.

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
  - [x] Re-panic `http.ErrAbortHandler`; suppress HTTP fallback only after ground-truth `Response.Hijacked()`
- [x] `middleware/accesslog.go` — Structured request logging (slog)
  - [x] `AccessLogConfig`: Logger, MinLevel, Skipper, ResultFilter
  - [x] Built-in controls: dedicated Logger, MinLevel, Skipper, ResultFilter
  - [x] Log level by status: 1xx/2xx/3xx=Info, 4xx=Warn, 5xx+=Error
- [x] Add copyright headers (Chi + Echo attribution)
- [x] Tests: 32 tests (requestid + recover + logger), -race clean
- [x] Update NOTICES (Chi + Echo entries)

### 1.5 Configuration (`config/`)

**Source**: koanf (MIT) **Spec**: [`docs/specs/config.md`](docs/specs/config.md)

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

**Source**: samber/do (MIT) **Spec**: [`docs/specs/container.md`](docs/specs/container.md) **ADRs**: [`docs/adr/004-dependency-injection-and-infra.md`](docs/adr/004-dependency-injection-and-infra.md)

- [x] Adapt samber/do core: container, lifecycle types
  - [x] **Key divergence**: typed constructor params (not `func(Injector)` signature)
  - [x] `app.Provide[T](constructor)` — register with typed constructor
  - [x] `app.ProvideFactory[T](fn)` — compiler-checked factory closure; fn resolves its own deps, opaque to Finalize graph validation
  - [x] `app.ProvideValue[T](value)` — register pre-built value
  - [x] `app.CanProvideValue[T]()` — read-only point-in-time frozen/direct-duplicate preflight for integrations that must validate before I/O; final normal/protected value publication remains authoritative
  - [x] `app.ProvideProtectedValue[T]()` / `app.ProtectBinding[T](expected ...T)` — low-level Replace protection for DI values coupled to external lifecycle/health state; the optional expected value atomically compares the resolved singleton before protection, and ordinary bindings remain replaceable
- [x] `app.Resolve[T]()` — retrieve instance
- [x] `app.MustResolve[T]()` — panics if not found
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

**Source**: Original **Spec**: [`docs/specs/container.md`](docs/specs/container.md) **ADR**: [`docs/adr/004-dependency-injection-and-infra.md`](docs/adr/004-dependency-injection-and-infra.md)

> ⚠️ **2026-06-11 — Infra slimmed (pre-v1 breaking change).** The speculative `Metrics`/`Tracer` carriers — `MeterProvider`/`TracerProvider`/`Counter`/ `Histogram`/`Span` interfaces and `WithMetrics`/`WithTracer` options — were removed; `Infra` now carries `Logger` only. Phase 3.5 redesigns the observability surface against real OTel/Prometheus adapters (see ADR-004 amendment).

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
- [x] `config.Load()` returns `*config.Config` (satisfies `credo.RawConfig`; compile-time verified)
- [x] ~346 total tests across all packages, all passing with `-race`

### 2.3 Validation Engine (`validation/`)

**Source**: ozzo-validation (MIT, API design) + Goyave (MIT, organization) + govy (architecture inspiration only) **Spec**: [`docs/specs/validation.md`](docs/specs/validation.md) **ADR**: [`docs/adr/011-validation-strategy.md`](docs/adr/011-validation-strategy.md)

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
- [x] Offset `Page` tests (input, logical COUNT, snapshot, metadata, mapping)
- [ ] Cursor conformance tests (tamper/scope/rotation, stable mixed order, insert/delete versus offset drift, and real PostgreSQL/MySQL/SQLite round trips for large int/time/string plus consumer UUID/decimal keys)

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

- [x] `appState` state machine: `building` → `starting` → `running` → `stopping` → `stopped`
- [x] `lifecycleManager` owns the server session, listeners, lifecycle context, and bound address
- [x] `Shutdown(ctx)` — readiness withdrawal → pre-drain → cancellation → HTTP/subsystem drain → DI teardown → LIFO hooks, with `errors.Join`
- [x] `OnShutdown(fn)` — register shutdown hooks
- [x] `State()` / `IsRunning()` — public accessors
- [x] `OnStart(fn)` — register FIFO startup hooks
- [x] `OnPreDrain(fn)` — register unordered pre-cancellation hooks for the narrow live-worker/live-DI drain phase
- [x] `OnDrain(fn)` — register unordered pre-DI hooks concurrent with HTTP drain
- [x] `Addr() net.Addr` — actual bound address accessor
- [x] Registration guards: `checkFrozen()` on addRoute, Mount, StatusHandler, SetMeta, OnStart, OnPreDrain, OnDrain, OnShutdown
- [x] `frozen bool` → `atomic.Bool` (thread-safe)
- [x] Tests: state transitions, graceful drain, hook ordering/failure/deadline behavior, frozen guards

**App Config Bootstrap** — [ADR-005](docs/adr/005-configuration-architecture.md), [ADR-006](docs/adr/006-application-lifecycle.md)

- [x] `credo.New(credo.WithRawConfig(rawCfg))` — functional options, RawConfig auto-registered in DI
- [x] Server config (host, port, timeouts) framework-internal — no user-facing CoreConfig
- [x] `Run()` / `RunContext()` / `ServeContext()` / `Shutdown()` lifecycle
- [x] TLS as server config — `WithTLSFiles` / `WithTLSConfig` / `server.tls.*` (precedence; `Run`/`RunContext` serve HTTPS, no separate `RunTLS`)
- [x] `config.Load(opts...)` returns `*config.Config` (satisfies `credo.RawConfig`); typed getters `Config.Get[T]` / `App.GetConfig[T]`

**Import Boundary Fitness Test**

- [x] `architecture_test.go` — verifies root package doesn't import feature packages (go/parser)

---

## Phase 3 — Enterprise Features

### 3.1 i18n (`internal/i18n/`)

**Source**: go-i18n (MIT) **ADR**: [`docs/adr/013-internationalization.md`](docs/adr/013-internationalization.md)

- [x] Adapt go-i18n core to `internal/i18n/`: Bundle, Localizer, Message types
- [x] CLDR plural rule support (`internal/i18n/internal/plural/` — 200+ languages)
- [x] String-based public APIs: `TranslateForLang()`, `FieldNameForLang()`, `HasMessages()`
- [x] Key-based HTTP translation through `HTTPError.MessageKey`, semantic `fault.Provider` root policy, and legacy `HTTPStatus()`→MsgKey mapping (no `TranslationKeyer` interface)
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

**Spec**: [`docs/specs/auth.md`](docs/specs/auth.md) **ADR**: [`docs/adr/012-authentication-and-authorization.md`](docs/adr/012-authentication-and-authorization.md) **Source**: Original (user accessors), Echo `middleware/` (MIT, implementations)

**Auth User Accessors** (root `*credo.Context`):

- [x] `(*credo.Context).SetUser[T](user)` — store user on the request (root-private generic key); `ctx.SetUser(user)` blessed inference form
- [x] `(*credo.Context).GetUser[T]()` — retrieve user with generic type safety
- [x] `(*credo.Context).RequireUser[T]()` — retrieve or return `credo.ErrUnauthorized` wrapping `credo.ErrUserMissing`
- [x] `Authenticator[T]` interface: `Authenticate(r *http.Request) (T, error)`
- [x] `ErrorFunc` type for custom auth failure responses
- [x] `auth.Middleware[T](authenticator, onError)` — middleware factory, calls `ctx.SetUser`
- [x] `auth/doc.go`
- [x] Tests: accessor unit tests (root `user_test.go`) + middleware factory + integration

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

**Source**: GoFr (Apache-2.0, health/interface design), Goyave (MIT, connection patterns), uptrace/bun (BSD-2-Clause, wrapped) **Spec**: [`docs/specs/store.md`](docs/specs/store.md) **ADR**: [`docs/adr/015-data-access.md`](docs/adr/015-data-access.md)

**Phase 3.3a — Core Package** (`store/`):

- [x] Semantic store error model: transport-neutral `fault.Kind`/`store.Kind`, structured `store.Error`, exact sentinels (`AlreadyExists`, `Constraint`, `Serialization`, `Deadlock`, `Contention`, `Timeout`, `Unavailable`, `ReadOnly`), deprecated `ErrDuplicate` alias, and deprecated `ErrConflict` umbrella
- [x] `Lifecycle` interface (Ping, Shutdown, Health) + optional explicit `LifecycleIdentityProvider` (`ResourceIdentity() any`; pointer-backed default recommended, token comparable/reflexive/stable)
- [x] `Health` / `HealthStatus` types
- [x] `Registry` — read-only `HealthAll` facade; private name+type+declared-resource-identity reservation prevents incomplete/bypass/duplicate entries inside the `store.Register` ledger; no public mutation/shutdown path
- [x] `Register[R]()` — local preflight + private reservation + deadline-scoped ping + protected DI publication/health commit; R and validated/adopted Registry bindings reject Replace, while invalid Registry bindings remain repairable before Finalize
- [x] `RegisterOption`: `WithName`, `WithPingTimeout`, `WithLifecycle`, explicit `WithCallerOwnedLifecycle` opt-out (`WithCritical`, `WithTags` deferred to health package)
- [x] Typed TX scope: `TxScope[T]` + `WithTx` / `GetTx` / `RequireTx` / `Conn`; per-scope type binding, nil/typed-nil rejection, and same-type multi-connection isolation (`WithTx[T]` / `GetTx[T]` / `Conn[T]` retained only as deprecated compatibility helpers)
- [x] Tests (errors, registry, register, tx context)
- [x] `store/doc.go`

**Phase 3.3b — Bun Wrapper** (`store/sqldb/`, submodule: `github.com/credo-go/credo/store/sqldb`):

- [x] `DB` type wrapping `*bun.DB` with lifecycle methods
- [x] `Config` struct (Driver, Host, Port, Name, User, Password, DSN,
  ConnectTimeout, MaxOpen, pointer-valued MaxIdle, MaxLifetime, MaxIdleTime,
  SSLMode, Options) + fail-loud pool validation, `DB.Stats`, cumulative health
  diagnostics, successful-registration unlimited-pool warning, exact driver
  aliases, IPv6-safe generated DSNs, non-zero network ports, rounded-up
  PostgreSQL sub-second timeouts, secret-safe option-conflict rejection, and
  explicit-nil plus known-family-mismatch dialect/connector rejection
- [x] `Open(cfg, opts...)` — factory with functional options
- [x] Context/driver-family-aware error mapping: structured PostgreSQL SQLSTATE, strict MySQL number envelopes, SQLite numeric codes, cancellation-vs-timeout separation, unavailable classification, cause/code preservation, and no loose domain-message fallback
- [x] `RunInTx` / `RunInTxWith` — per-DB typed context propagation, exact callback-error preservation, mapped begin/commit/rollback failures, panic rollback/re-panic, nil-callback guard, and cancellation-safe savepoints with fail-loud nested options + ambient abort on cleanup failure
- [x] Query builder proxies: `SelectQuery`, `InsertQuery`, `UpdateQuery`, `DeleteQuery`
- [x] 8 query guardrails: TX execution snapshot/injection, public Select Clone contract, Apply varargs+nil, Unwrap builder-only, raw terminals, ApplyQueryBuilder, curated Select Limit/Offset int32 narrowing rejection, and fail-loud unsupported Count/Page shapes
- [x] Select execution snapshots preserve explicit/raw `Conn`, builder errors, `WherePK`, soft-delete flags, model hooks and relation state across `Scan`/`Count`/`Exists`/`Page`
- [x] Typed `One`/`All`/`Page` are model-less terminals: pre-bound `Select`/`Model`/`Apply` state returns `ErrTypedTerminalModel` before DB execution; optional query model args reject arity >1
- [x] Escape hatches: transaction-aware `Conn(ctx) bun.IDB` / `RequireTx(ctx)` plus base-pool `Client() *bun.DB`
- [x] Finite nested-savepoint creation/cleanup/ambient-abort wait budget: 5s default + `WithTxCleanupTimeout`; callback duration excluded; shared rollback-only state closes async-abort commit races
- [x] `Page[T]` typed pagination terminal on `SelectQuery` (replaced the `Paginate`/`PaginateRequest` free functions); immutable request snapshot and strict pre-COUNT `ErrInvalidPageRequest` guard, including Bun v1.2.18's int32 LIMIT/OFFSET range
- [x] Universal `_credo_count_source` logical count conformance: plain projections, ungrouped aggregates, `Distinct`, `Group`, and `Group+Having`; root ORDER/LIMIT/OFFSET/FOR is excluded; model SELECT hooks run on the private source, `QueryEvent.Model` remains visible, and soft-delete policy is not duplicated on the outer count
- [x] Standalone `Having` and direct compound Count/Page reject pre-I/O with `ErrUnsupportedCountQuery`; MySQL server-oracle conformance under normal and `NO_BACKSLASH_ESCAPES` wraps logical-count 1060 after I/O with the driver cause, accepts server-valid wildcard/implicit expressions, and leaves non-count 1060 unchanged; advanced callers restructure unsafe shapes behind an outer derived table/CTE
- [x] Count source renders exactly once and revalidates relation-callback mutations; predicates/projections survive, while model replacement, root ORDER/LIMIT/OFFSET/FOR, standalone Having, and compound roots fail pre-I/O
- [x] Tests (db, config, error mapping, tx, query proxies, integration)

**Phase 3.3c** (deferred):

- [ ] Redis store contracts (depends on `pubsub/` design; also feeds the cache / rate-limit store / pubsub-backend stories)
- [ ] Observability QueryHook for automatic trace spans (depends on Phase 3.5)
- [ ] General resource-identity/lifecycle registry across store/pubsub/gRPC/worker only after a second concrete subsystem needs it; raw DI duplication remains outside the store ledger and unsupported
- [ ] Fail-loud driver capability validation for requested `sql.TxOptions` isolation/read-only semantics; pinned modernc SQLite currently ignores isolation and does not enforce read-only, so SQLite snapshot guidance uses plain `InTx`

**Phase 3.3d**:

- [x] Update NOTICES with GoFr (Apache-2.0) + Goyave + uptrace/bun attribution

**Phase 3.3e — Migrations & TX ergonomics**:

- [x] `db.InTx(ctx, fn)` — method-form TX sugar over `RunInTx` (handler-side ergonomics; called with `ctx.Context()`) — plus `db.InTxWith` for `sql.TxOptions` symmetry
- [x] Migration wrapper over `bun/migrate` (replaces the goose plan; the optional `credo migrate:*` CLI sugar lives in Phase 5.1):
  - [x] `db.RegisterMigrations(...)` — accept `*migrate.Migrations` (+ pass-through `migrate.MigratorOption`s; mark-applied-on-success gives at-least-once re-attempt for Up errors surfaced by Bun, not automatic retry safety)
  - [x] `OnStart` lifecycle integration (dev/single-replica opt-in) — `db.Migrate` matches the `App.OnStart` hook signature: `app.OnStart(db.Migrate)`
  - [x] `embed.FS` migration bundling support (Bun's `Discover` works on any `fs.FS`; covered by tests)
  - [x] Seeding: documented as plain migration files (no separate mechanism)
  - [x] Cancellation-detached migration unlock with a fixed five-second caller bound; timeout is an uncertain outcome and is not automatically retried
  - [x] Multi-replica production contract: one-shot pre-deploy job first, `OnStart` for dev/single-replica convenience, expand-contract and replay-safe/idempotent retry guidance
  - [ ] Track [Bun #1389](https://github.com/uptrace/bun/issues/1389) and add a conformance test before promising `.tx.up.sql` commit-error → unapplied-marker correctness
- [x] Tests (`migrate_test.go` + `InTx` cases in `integration_test.go`)

### 3.4 Health Checks (root package)

**Source**: Original (written from scratch)

> **2026-06-11 / 2026-07-10 — engine folded into root, bounded execution added.** The engine lives unexported in the root package (`health_engine.go`); `internal/health/` holds the stable bounded `Probe` primitive plus the module-internal per-store DI seam (`StoreFunc`). `SetHealthStoreFunc`/`HealthStoreResult` remain removed from the public API (see ADR-016).

- [x] Engine with concurrent check execution (root, unexported; was `internal/health/`)
- [x] Bounded common runner for named + store checks: immutable results,
  enforced per-check deadlines, panic isolation, and parallel execution
- [x] Stable per-check singleflight: overlapping probes reuse one execution;
  non-cooperative callbacks cannot accumulate one goroutine per request
- [x] Typed store health cause (`Health.Cause`, JSON-excluded), operator logging,
  default response masking, and explicit `ExposeErrors` opt-in
- [x] Store status allowlist and fail-closed custom/store name-collision handling
- [x] Store registration/Registry typed-nil hardening
- [ ] Optional/critical stores and bounded low-cardinality tags (separate API decision)
- [x] Lifecycle ownership and Registry/seam atomicity hardening: direct `Lifecycle` is framework-owned; separate handle requires explicit caller-owned opt-out; default identity is the top-level Lifecycle and semantic wrappers explicitly forward `ResourceIdentity`; equal tokens cannot repeat inside the Register ledger; raw DI duplication is unsupported; failure leaves no visible entry/value; interface access uses `Alias`
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

> ⚠️ **v0.1.0 reframe.** Logging (slog) is real and featured; tracing (OTel) ships as _experimental_; a stable Prometheus metrics adapter is optional. Do **not** rush the full OTel wrapper before v1. **2026-06-11:** the speculative root-package `MeterProvider`/`TracerProvider` interfaces and `Infra.Metrics`/`Infra.Tracer` fields were removed (see §2.2 note). This phase starts from a clean slate: design the metrics/tracing carriers from real OTel/Prometheus adapters, aligned with the v1 / Go 1.27 window.

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
- [x] Offset/limit pagination; strict non-mutating `Offset() (int, error)` rejects non-positive/native-overflow values while Normalize/Validate retain forgiving defaults and clamp policy
- [x] Cursor/keyset design gate: forward-only `CursorPage[T]` is distinct from the working name `Slice[T]` for future total-free offset pagination; terminal-owned stable order, non-null immutable keys, explicit unique tie-breaker, `per_page+1`, no COUNT, and signed scope-bound token policy are fixed
- [ ] Cursor-based pagination implementation — gate requires a concrete consumer, a fail-loud Bun post-hook ordering/window boundary, invalid-argument transport mapping, canonical wire-format golden vectors, and real PostgreSQL/MySQL/SQLite conformance; backward/nullable/expression-key/encrypted cursor variants remain deferred
- [x] Auto-read `?page=` and `?per_page=` from request (reserved cursor input `?after=` remains deferred)
- [x] `Meta` struct (`total_count`, `page`, `per_page`, `total_pages`, `has_next`, `has_prev`)
- [x] `NewPage` computes ceiling division without overflowing `TotalPages` at `math.MaxInt64`
- [x] `Map[U]` method on `Page[T]` — item projection (model → DTO) preserving pagination meta
- [x] COUNT+SELECT isolation matrix and deterministic SQLite WAL drift/snapshot conformance; `Page` never starts an implicit transaction
- [ ] First-class custom count source/strategy (defer until two real consumers repeat explicit `Count` + data query + `NewPage` composition)
- [ ] Total-free offset response — `Slice[T]` is the working name and gets its own design gate; do not encode an unknown total in `Page`/`Meta`
- [x] Tests

### 3.8 Reload & Partial Config Reload (root + `config/`) — [ADR-020](docs/adr/020-reload-and-partial-config-reload.md)

> Accepted 2026-08-23. Target: v0.5.0.

- [x] ADR-020 + spec updates (`lifecycle.md`, `config.md`), ADR-005/006 cross-references
- [x] `config`: `Reloader` interface, `Changes` (sorted leaf-key symmetric difference), `(*Config).Reload()` re-running the captured load pipeline with an atomic snapshot swap; fixed `CREDO_ENV`
- [ ] root: `Reload(ctx)` (running-only, serialized, validate-before-publish, no rollback, `errors.Join`), `OnReload` (FIFO), `OnConfigChange[T](key, fn)` (generic method; non-`Reloader` store panics at registration), `WithReloadTimeout` + `server.reload_timeout`, restart-required Warn for unsubscribed changed keys
- [ ] Signal path: SIGHUP under `Run()` (Unix build tag; coalescing; never terminates); `RunContext`/`ServeContext` stay signal-free
- [ ] TLS: file-based sources served via `GetCertificate` + atomic pointer; internal reload participant re-reads the pair on every reload (failure keeps the old pair); `WithTLSConfig` untouched
- [ ] Tests: config diff/atomicity, reload state/serialization/abort/partial-failure, SIGHUP (unix), certificate rotation via real handshakes
- [ ] Docs: configuration guide ("Reloading Configuration", reloadable column, rotation), getting-started, new `docs/guides/deployment.md` (systemd `ExecReload`, containers, admin-endpoint reload), DI/worker/middleware cross-notes, README, `doc.go`, `examples/saas`, release notes

### 3.7 Test Utilities (`testutil/`)

**Source**: Original (inspired by Yokai's test toolkit)

> Shipped 2026-06-09; recorded here retroactively (the design discussion lived in the local open-questions worklist). `NewApp` returns a **real** `*credo.App`, hermetic by default: no config files are read, the logger is silent, shutdown registers via `tb.Cleanup`, and the container is left unfinalized so tests can keep providing and resolving.

- [x] `testutil.NewApp(tb, opts...) *credo.App` — hermetic test app factory (empty `RawConfig`, `slog.DiscardHandler` logger, best-effort cleanup shutdown)
- [x] `WithWiring(fns...)` — dependency setup; runs before overrides
- [x] `WithOverride[T](v)` — DI override built on `app.Replace[T]` / `app.MustReplace[T]` (the public replace primitives were added as its enabling API)
- [x] `WithConfig(key, val)` — dotted-key config injection through the real loader (nested map → JSON → `config.LoadBytes`)
- [x] `LogBuffer` — injectable slog capture: `Handler()`, `Entries()`, `Reset()`, `AssertHas(tb, LogEntry)` (string levels, subset attribute matching, JSON-normalized numbers)
- [x] Tests + testable examples (`app_test.go`, `internal_test.go`, `example_test.go`; 95.5% coverage)
- [ ] `NewTraceExporter` — trace span capture and assertion (depends on Phase 3.5)

---

## Phase 4 — Extended Features

### 4.1 Pub/Sub & In-Process Events (`pubsub/`)

**Source**: watermill (MIT)

> A typed in-process event API is pubsub's channel backend plus generics sugar, not a second eventing system.

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
- [x] `worker.Register(app, w, opts...) error` + `worker.MustRegister(app, w, opts...)` API
- [x] Continuous + scheduled worker execution modes
- [x] Graceful shutdown (wait for active workers)
- [x] Integration with app lifecycle
- [x] Tests
- [x] Update NOTICES
- [ ] Observability hooks (metrics/tracing)

### 4.3 gRPC (`grpc/`)

**Source**: GoFr `grpc.go` (Apache-2.0)

> Deliberately **thin** — the value is shared lifecycle + DI + `Infra` interceptors (logging/recovery/tracing), not wrapping gRPC itself. No codegen tooling, no gateway/transcoding. Late Phase 4, after observability (interceptors need it).

- [ ] Dual-protocol from same `App` struct (shared lifecycle: `Run`/`Shutdown`)
- [ ] Shared DI container + `Infra` interceptors (logging, recovery, tracing)
- [ ] Tests
- [ ] Update NOTICES

### 4.4 WebSocket (`websocket/`) and SSE

**Source**: coder/websocket v1.8.15 (ISC), wrapped and exact-pinned

WebSocket server support is implemented as an adapter rather than copied
protocol code. The canonical API stays on the existing router:
`ws := websocket.Use(app, cfg)` and
`app.GET(path, ws.Handler(handler))`. Hub/room, outbound client, reconnect,
heartbeat scheduler, quota, distributed fan-out, and RFC 8441 remain
demand-gated follow-ups rather than MVP promises.

- [x] Credo-owned message/close/config/connection façade over coder/websocket
- [x] Secure same-origin default, subprotocol policy, 32 KiB read limit, compression off
- [x] RFC 7807 pre-upgrade errors and fail-loud non-Hijacker behavior
- [x] App-managed `OnDrain` integration plus explicit external-server shutdown
- [x] Real TCP/WSS/HTTP2-negative, race/conformance, fuzz, and observability coverage
- [x] ADR/spec/guide/example and NOTICES attribution

SSE is a separate deferred transport; it is not folded into the WebSocket
package or lifecycle. Before shipping, `Response` and every supported wrapper
chain must provide fail-loud `http.Flusher` capability/error semantics—silent
buffering or a claimed-but-nonfunctional Flush is unacceptable. Only then
should an SSE response API and disconnect/drain contract be designed.

- [ ] Design and prove the Flush capability boundary across middleware
- [ ] Specify SSE framing, heartbeat, disconnect, and drain semantics
- [ ] Add an SSE API only after those gates pass

### 4.5 OpenAPI (`openapi/`)

**Source**: kin-openapi (MIT)

> Hardest retrofit on the roadmap — the handler signature (`func(*Context) error`) carries no request/response types, so spec generation needs a meta/registration-based design. Write `docs/specs/openapi.md` > **before** implementation; do not rush this one.

- [ ] Design spec first: how routes declare request/response types (Route Meta vs explicit registration)
- [ ] Copy OpenAPI 3.x Go type definitions
- [ ] Auto-generate spec from Credo router registrations
- [ ] Request/response validation middleware
- [ ] Embedded Swagger UI handler
- [ ] Tests
- [ ] Update NOTICES

### 4.7 Contract Guards (`middleware/`) — 🚩 v0.1.0 LAUNCH SCOPE

**Source**: Original (builds on existing Route Meta system) · `middleware/contractguard.go`

> **Registration note:** ContractGuard reads matched-route Meta, so it is a **group/route** middleware (`group.Middleware(...)`), not `app.GlobalMiddleware` — app-global middleware runs _before_ the route is matched. Applied globally it is a safe no-op. (Spec originally said "global"; corrected to match the Built-in → Global → Group → Route execution order.)

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

> Outbound HTTP with retry/timeout/logging/trace propagation is universal in enterprise services. Built as a composable `http.RoundTripper` chain — works with existing stdlib tooling. **The lean core ships independently of Phase 3.5**; tracing/metrics hooks land when observability does. No `app.HTTPClient()` sugar — plain DI at application boundaries.

- [x] `httpclient.New(opts...)` — `*http.Client` factory with canonical RoundTripper chain: Client.Timeout → retry → logging → trace → base; composable `NewRetryTransport`/`NewLoggingTransport`/`NewTraceTransport` exports; spec: [`docs/specs/httpclient.md`](docs/specs/httpclient.md)
- [x] `WithTimeout`, `WithRetry(cfg ...RetryConfig)` — full-jitter backoff; `DefaultRetryIf` never retries POST/429/context cancellation; GetBody-only body replay; exhaustion returns the last response unchanged
- [x] Structured request/response logging via `WithLogging(*slog.Logger)` — package is stdlib-only; one line per attempt, query string + userinfo stripped, 5xx→Error / 4xx→Warn / else Info
- [x] W3C `traceparent` propagation via `WithTracePropagation()` + `TraceContextFromRequest`/`SetTraceContext`/`GetTraceContext`; child span ID per attempt, invalid inbound → new root
- [ ] Request/response metrics — duration histogram, status counter (depends on Phase 3.5)
- [ ] Circuit breaker — deferred (keep the core lean; revisit on demand)
- [x] Tests _(33 tests + 32 subtests across 4 files: retry/backoff/replay, level mapping + redaction, W3C parse/derive table, chain-order integration)_

### 4.9 Admin Server & Debug Endpoints (root package)

**Source**: Original (inspired by Yokai's core/app server split)

> Optional second HTTP server on an ops port — K8s-friendly separation of operational endpoints from public traffic. Internally stdlib `http.ServeMux` (few routes; no second router needed). **After Phase 3.5** (`/metrics` needs it; interim: `HealthConfig.Group` + IP restrict). **JSON only — no HTML dashboard** (an HTML UI was cut: bitrot and a security surface without differentiation — enterprises already run Grafana/Backstage).

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

> Small convention API completing the Clean Architecture story: controllers implement an interface and register their own routes.

- [ ] Registration interface (e.g. `RegisterRoutes(g *credo.Group)`)
- [ ] `app.Register(controllers...)` / group-level equivalent
- [ ] Align CLI `credo make:controller` output with it (Phase 5.1)
- [ ] Tests

---

## Phase 5 — CLI and Tooling

### 5.1 CLI Tool (`cmd/credo/`)

> `credo new` is the priority — it delivers the "Clean Architecture as default via CLI scaffolding" promise (philosophy #2). `make:*` generators are secondary.

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
  - [ ] **Duplicate route diagnostics** (decision closed — strict stays): radix returns `DuplicateRouteError`; mux keeps strict fail-fast panic. A lenient/debug warning mode is rejected — it would legitimize silent route shadowing, which breaks named-route, route-meta, and middleware resolution. Only diagnostic _quality_ may still improve (clearer message, both conflicting locations, source position); the fail-loud behavior never changes.

### Performance Budgets

- [ ] Define threshold benchmarks for hot paths: router match, context pool, DI resolve
- [ ] CI benchmark regression tests (fail on >10% regression)
- [ ] Middleware overhead budget: max added latency per middleware layer

### Router Improvements

- [x] **Mount introspection**: mounts now appear as a single clean `RouteKindMount` entry in `Routes()`/`WalkRoutes` (cleaned prefix + sorted forwarded method set); internal catch-all/fan-out hidden. Shipped with route introspection v2 (`RouteInfo` gained `Name`/`Meta`/`Methods`/`Kind`/`AutoHead`, deterministic total-order output)
- [x] **Document middleware ordering**: group middleware is collected at compile time from the group parent chain — registration order affects execution order only, never membership (semantics changed 2026-06-11 from registration-time capture; documented in `doc.go`, the middleware spec, and the guide)
- [x] **Mount registration atomicity**: `Mount` now preflights its exact + catch-all method fan-out (read-only `radix.Node.FindEndpoint` + `mux.wouldConflict`) and panics before mutating the tree when an explicit route already conflicts, so a recovered duplicate panic leaves no partial radix/store entries (the radix tree has no delete — guarantee is check-before-insert, not rollback). Regression test covers a pre-existing exact route + overlapping `Mount`; structural conflicts need no preflight because the catch-all registers first and shares the exact prefix, so they always fire on the first insert. See [ADR-007](docs/adr/007-router-and-routing.md).

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
- [ ] Fluent validation builder — the language prerequisite is met (generic methods, [golang/go#77273](https://github.com/golang/go/issues/77273), landed with the repo's Go 1.27 floor, 2026-06); still deferred by choice; the programmatic `Rule[T]` API remains the substrate

### Documentation

- [ ] `doc.go` for every package (include maturity label)
- [ ] `example_test.go` for core packages (root, middleware, config) — middleware and testutil ship examples; root and config still missing
- [x] ADRs tracked (20 total):
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
  - [x] [`019-websocket-integration-and-drain.md`](docs/adr/019-websocket-integration-and-drain.md) — WebSocket integration and drain
  - [x] [`020-reload-and-partial-config-reload.md`](docs/adr/020-reload-and-partial-config-reload.md) — Reload signal, partial config reload, TLS rotation (Accepted; implementation in 3.8)
- [x] `docs/guides/quick-start.md` — superseded by [`getting-started.md`](docs/guides/getting-started.md), which opens with a minimal "Hello, Credo" quickstart before the full walkthrough
- [x] Write guide: Infra injection model (Model 1) — covered by [`docs/guides/dependency-injection.md`](docs/guides/dependency-injection.md)

### CI/CD

- [x] GitHub Actions CI (`ci.yml`) — build, vet, `go mod tidy` check, tests, and an Examples gate; Go version resolved from `go-version-file: go.mod` (the floor equals the latest release today, so the "1.27 + latest" matrix is a single version)
- [x] Automated golangci-lint on PRs — split into a blocking safe-set job and a non-blocking full canary job until golangci-lint's staticcheck/unused/gosec engines support Go 1.27 (tracked by the canary, independent of the 1.27 GA release)
- [x] CodeQL security analysis (`codeql.yml`)
- [x] Upstream advisory watch (`upstream-watch.yml`) — monthly govulncheck plus an adapted-upstream review reminder (`SECURITY-UPSTREAMS.md`); adapted code is invisible to Dependabot
- [x] Lockstep library release gate — CI builds an external `store/sqldb` consumer from synthetic root+nested tags without local `replace`; the manual `Release` workflow validates the prepared root requirement and atomically publishes both module tags
- [ ] Codecov or Coveralls integration
- [ ] Release workflow with goreleaser — becomes relevant with `cmd/credo` (Phase 5.1); the library itself releases via git tags

### Quality Gates

- [x] 80%+ coverage for Phase 1-2 packages — every root-module package measures 83–100% (root 94.0%, config 94.3%, validation 96.5%, middleware 94.0%, internal/di 94.3%, internal/radix 92.1%; snapshot 2026-07-03)
- [ ] Zero lint warnings on `make lint` — blocked on golangci-lint gaining full Go 1.27 support; CI runs a safe linter subset as blocking in the meantime
- [x] Benchmark suite for hot paths (router match, context pool, DI resolve) — 34 benchmarks across 8 files: root `benchmark_test.go` (ServeHTTP static/param/JSON/middleware/parallel, exercising the router and the context pool), `internal/di` (Resolve variants), `internal/radix`, `middleware`, `validation`, i18n

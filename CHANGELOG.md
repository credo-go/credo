# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Credo is pre-1.0: minor (`0.x`) releases may contain breaking changes; when they do, the break is called out explicitly under **Changed** or **Removed**.

The `store/sqldb` submodule is versioned in lockstep with the root module (path-prefixed `store/sqldb/vX.Y.Z` tags — see [CONTRIBUTING.md#releasing](CONTRIBUTING.md#releasing)); its changes are recorded here.

## [Unreleased]

### Added

- **Access logging** — `WithAccessLogSkipper(func(*credo.Context) bool)` installs a pre-dispatch predicate that excludes matching requests from the built-in access log without disabling it. The new `credo.MetaAccessLog` route meta (`route.SetMeta(credo.MetaAccessLog, false)`) silences a single route or, by `LookupMeta` inheritance, a whole group; a route-level value overrides its group's, and `middleware.AccessLog` honours the same meta. See [ADR-010](docs/adr/010-middleware-architecture.md).
- **Health checks** — `HealthConfig.LogRequests` (default `false`) keeps `/health` and `/ready` probe requests out of the access log; set it to `true` to log them. Because the meta is applied per route, `true` re-enables logging even under a group that silenced access logging. See [ADR-016](docs/adr/016-health-checks.md).
- **TLS** — `WithTLSFiles(certFile, keyFile)` and `WithTLSConfig(*tls.Config)` configure HTTPS, as do the `server.tls.cert_file` / `server.tls.key_file` config keys. Sources resolve by precedence (`WithTLSConfig` > `WithTLSFiles` > `server.tls.*` > plaintext; whole-source override, never a conflict error). `Run` and `RunContext` serve HTTPS automatically when TLS is configured; the certificate is validated once at preflight, so a bad cert — or an explicitly-set-but-empty/nil source (`WithTLSConfig(nil)`, `WithTLSFiles` with an empty path) — fails fast and rolls the state back to `building` rather than silently downgrading to a lower-precedence source or plaintext. `ServeContext` is TLS-exempt — wrap the listener with `tls.NewListener` for HTTPS. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **HTTP→HTTPS redirect** — `WithHTTPRedirect(addr)` runs a second, plaintext listener that permanently redirects every request to its HTTPS equivalent (301 for GET/HEAD, 308 for other methods). It requires TLS (preflight fails fast otherwise) and binds, serves, and drains with the main server — closing before the main server on drain, and tearing the app down if the redirect listener fails at runtime, so a requested redirect never silently dies. `ServeContext` ignores it. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Configuration** — typed-snapshot getters over `Unmarshal`: `config.(*Config).Get[T](key) (T, error)` plus `MustGet[T]`, and `(*credo.App).GetConfig[T](key) (T, error)` plus `MustGetConfig[T]`. Each decodes a config section into a value of `T` in one call (the `Must` forms panic, matching the `MustProvide`/`MustResolve` family); there is deliberately no `MustLoad`. They are composition-root sugar — a handler has no `App` accessor, so typed config still flows to services via DI. See [ADR-005](docs/adr/005-configuration-architecture.md).

### Changed

- **BREAKING — `config.Load` and `config.LoadBytes` now return `*config.Config` instead of `credo.RawConfig`.** The concrete `*config.Config` still satisfies `RawConfig`, so passing the result to `credo.WithRawConfig` or storing it in a `RawConfig`/`credo.RawConfig` variable keeps compiling unchanged; only code that depends on the exact interface return type (for example assigning `config.Load` to a `func(...) (credo.RawConfig, error)` value) needs adjusting. The concrete return type is what carries the new `Get[T]`/`MustGet[T]` methods. See [ADR-005](docs/adr/005-configuration-architecture.md).
- **Lifecycle** — a failed startup (an `OnStart` hook returning an error) or a non-graceful `Serve` failure after the server reached `running` now runs the full teardown chain (DI container shutdown + `OnShutdown` hooks) and ends in the terminal `stopped` state, instead of rolling back to `building`. This releases resources an earlier `OnStart` hook started (workers, locks, connections) instead of leaking them. `OnShutdown` hooks consequently run on every teardown, including a failed startup, so they must be idempotent and must not assume any particular `OnStart` hook completed. Pre-session failures (TLS preflight, listener bind) still roll back to `building` and remain retryable. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Access logging** — the built-in access logger and `middleware.AccessLog` now share a single emit core (`internal/observe.EmitAccessLog`), keeping their attribute set, `"request completed"` message, and status→level mapping identical. No behavior change for existing callers.
- **TLS** — `Run` and `RunContext` now serve HTTPS when TLS is configured (previously plaintext-only); see **Added** and **Removed**.

### Removed

- **BREAKING — `App.RunTLS` and `App.RunTLSContext` are removed.** TLS is now server configuration rather than a serve-method variant. Migrate by configuring TLS at construction and calling the plain entry points: `app.RunTLS(cert, key)` → `credo.New(credo.WithTLSFiles(cert, key))` then `app.Run()`; `app.RunTLSContext(ctx, cert, key)` → `WithTLSFiles` then `app.RunContext(ctx)`. For full `crypto/tls` control use `WithTLSConfig`. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **BREAKING — `auth.SetUser` / `auth.GetUser` / `auth.RequireUser` are removed.** The authenticated principal is now reached through generic `*credo.Context` methods instead of `context.Context` helpers: `ctx.SetUser(user)` (T inferred), `ctx.GetUser[T]()`, and `ctx.RequireUser[T]()` (returns `credo.ErrUnauthorized` wrapping the new `credo.ErrUserMissing`). `auth.Middleware` is unchanged at the call site — it now stores the user via `ctx.SetUser`. Migrate handler reads: `auth.GetUser[T](ctx.Context())` → `ctx.GetUser[T]()`. See [ADR-012](docs/adr/012-authentication-and-authorization.md).

### Fixed

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

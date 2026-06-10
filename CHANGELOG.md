# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Credo is pre-1.0: minor (`0.x`) releases may contain breaking changes; when they
do, the break is called out explicitly under **Changed** or **Removed**.

The `store/sqldb` submodule is versioned in lockstep with the root module
(path-prefixed `store/sqldb/vX.Y.Z` tags — see
[CONTRIBUTING.md#releasing](CONTRIBUTING.md#releasing)); its changes are
recorded here.

## [0.1.0] - 2026-06-10

Initial public release.

### Added

- **Routing** — radix-tree router (adapted from Chi) with `{param}`, regex
  constraints, and `{path...}` catch-all; named routes with
  `BuildURI`/`BuildURL`; route Meta with parent-chain lookup; host routing;
  app-level 404/405/5xx status handlers; trailing-slash redirect; automatic
  HEAD handling.
- **Context** — pooled `credo.Context` with a `Request`/`Response` split,
  one-step bind-and-validate (`BindBody`/`BindQuery`), internal rewrites
  (`Rewrite`/`OriginalPath`), and `Context()` for context-taking APIs.
- **Error handling** — `func(*credo.Context) error` handler signature,
  RFC 7807 Problem Details responses, pluggable `ErrorRenderer`, built-in
  panic recovery.
- **Middleware** — four-tier execution (built-in → global → group → route);
  built-ins on by default: Recover, RequestID, AccessLog (opt-out via
  `Without*`). Suite: CORS, CSRF (stdlib `CrossOriginProtection`), Secure,
  Compress, Timeout, RateLimit, Rewrite, ContractGuard.
- **Static files** — `os.Root`-sandboxed `app.Static`/`app.File`.
- **Dependency injection** — generics-based container
  (`Provide[T]`/`Resolve[T]`/`ProvideFunc`/`Alias`/`Replace`), validated
  graph freeze (`Finalize`), reverse-order shutdown, and the `credo.Infra`
  carrier (per-service logger; tracer/metrics with noop fallbacks).
- **Configuration** — koanf-adapted loader (`config.Load`), env-specific file
  derivation, `.env` support, typed config snapshots via
  `RawConfig.Unmarshal`.
- **Validation** — programmatic generic rules (`Rule[T]`, ozzo-style field
  refs), file-upload rules, RFC 7807 error output; no struct tags.
- **Authentication** — generic `auth.SetUser[T]`/`GetUser[T]`, JWT / Basic /
  API-key authenticators, middleware factory, RBAC-via-route-Meta pattern.
- **Internationalization** — JSON locale files, `ctx.T()`/`ctx.Locale()`,
  key-based error translation with three-level fallback.
- **Data access** — `store` contracts (universal errors, context-based
  transaction scope) and the `store/sqldb` Bun wrapper submodule: curated
  query-builder proxies, `db.InTx`/`RunInTx`, and migrations via a thin
  `bun/migrate` wrapper (`app.OnStart(db.Migrate)`,
  mark-applied-on-success default).
- **Background workers** — continuous and cron-scheduled workers
  (`worker.Register`, parser adapted from robfig/cron v3), panic recovery,
  graceful shutdown.
- **Health checks** — `/health` + `/ready` (Kubernetes-compatible),
  store-registry auto-integration, readiness errors masked by default.
- **Outbound HTTP** — `httpclient` package: `*http.Client` factory with a
  fixed RoundTripper chain (timeout → retry → logging → trace),
  safe-by-default retry, per-attempt structured logging, manual W3C trace
  context propagation.
- **Pagination** — query-param parsing and normalization, with
  `store`-integrated `Paginate` in `store/sqldb`.
- **Observability baseline** — structured logging (slog) wired by default
  with request IDs and access logs; OpenTelemetry tracing and Prometheus
  metrics are experimental with noop fallbacks.
- **testutil** — hermetic `testutil.NewApp` with DI overrides
  (`WithOverride`/`WithConfig`/`WithWiring`) and `LogBuffer` log assertions.

Adapted open-source code is attributed in [NOTICES](NOTICES); the
per-component acquisition strategy is documented in
[docs/adr/002-code-acquisition-strategy.md](docs/adr/002-code-acquisition-strategy.md).

[0.1.0]: https://github.com/credo-go/credo/releases/tag/v0.1.0

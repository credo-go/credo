# SaaS Composition Example

Run `go run .` from this directory to use its bundled configuration. The example demonstrates typed config, DI-managed services, JWT authentication, group authorization, localization, a data store, a scheduled worker, a WebSocket endpoint and health. Business operations are in-memory stubs; this is a composition example. It has its own module and an in-tree Credo replacement. CI builds, vets, checks tidy, serves `/health` and verifies graceful SIGTERM exit.

## Bootstrap order

`run()` in main.go follows the [documented bootstrap order](../../docs/specs/container.md#documented-order), one numbered comment per step:

1. **Configuration** — `config.Load`, the typed `app` and `databases.default` sections, the live `log` section and the logger whose level it sets, `credo.New(credo.WithRawConfig(rawCfg), …)`.
2. **Bindings** — the store, opened from the `databases.default` section as `sqldb.Open` opens a database and bound with `app.ProvideValue(tenantStore, credo.ComponentName("tenants-db"))`, the typed `app` section, and the constructor of `TenantService`, which takes `credo.Infra` first.
3. **Feature mounts and satellite registrations** — `UseRequestID`, `UseAccessLog`, `UseCompress`, `UseI18n` with a programmatic catalog, `UseHealth`, a scheduled worker registered through its constructor, `NewUsageReporter`, which also takes `credo.Infra` first, the WebSocket server handed to `app.Manage(ws)`, and the `OnStart` and `OnStop` hooks.
4. **`Finalize`** — an optional early check: the graph is validated and its error handled before anything is built; without it, the first `Resolve` would finalize the container.
5. **`Resolve`, middleware and routes** — `TenantService` is resolved and its handlers bound; Secure and CORS are global middleware, authentication and the admin role check stay on their groups, and a `StatusHandler` answers the router's 404.
6. **`Run`** — the start phase pings the store, starts the worker and the WebSocket server and runs the start hooks before the App accepts requests; SIGINT/SIGTERM drains the listener, the WebSocket server and the worker first, then `TenantService`, then the store.

## What each part shows

- **Store.** `TenantStore` stands in for a `*sqldb.DB`: like it, it is opened before the App binds it and is a component whose type shows `Start`, `Ready` and `Shutdown`, and one call, `app.ProvideValue(tenantStore, credo.ComponentName("tenants-db"))`, is all it takes. The start phase calls its `Start` before the services that use it start, `/ready` reports it as `tenants-db`, and the App shuts it down after those services. Its `Shutdown` logs through `credo.InfraFrom(ctx)`, the scoped `Infra` the App's call carries, so the record carries `component=tenants-db`. An application with `store/sqldb` binds the opened handle the same way.
- **Worker.** `UsageReporter` is registered through its constructor, `app.Workers().Scheduled("usage-report", "@every 1h", NewUsageReporter)`, which the start phase calls with the container's values; one run per hour, and it stops with the HTTP drain.
- **WebSocket.** `websocket.New()` is an ingress component by its type, so `app.Manage(ws)` takes no option; `GET /ws/echo` echoes every message.
- **Localization.** `UseI18n` takes a programmatic English catalog: error codes, validation codes and field display names are exact keys, and `ctx.T` translates the admin greeting.
- **Validation and errors.** `CreateTenantRequest.Validate` uses programmatic rules and one custom rule that returns `validation.NewError`; failures render in the default error envelope with field-level `violations`.
- **Reload.** The `log` section is live: its `OnChange` sets the `slog.LevelVar`, so changing `log.level` and sending SIGHUP switches the level in place. `app` stays static, so a reload that changes `app.name` reports it as needing a restart.

## Pre-v1 migration

**DI minor, HTTP minor (2026-09-05) and v0.24.0 (2026-10-08) applied.** main.go uses the shipped APIs.

1. DI minor (done): value/constructor registration finishes before an error-checked Finalize, then TenantService is resolved. Routes and DI-backed extension/hook registration stay before HTTP preparation. Building-state Shutdown is available to clean up registered resources after a later bootstrap failure. Hooks capture resolved dependencies instead of resolving during drain.
2. HTTP minor (done): explicit `UseRequestID` and `UseAccessLog` preserve this example's request correlation and records, and compression is installed with `UseCompress` instead of global middleware. Secure and CORS remain global middleware; authentication/authorization keep their group scope.
3. v0.24.0 (done): the bootstrap follows the six documented steps, with every feature mount and satellite registration before `Finalize`. A store took two calls, the binding and a store registration; the WebSocket server is one `Manage` registration with `Ingress`; workers are registered before `Finalize`; `UseI18n` returns nothing and panics on misuse, and locale files are read in the start phase; hooks are `OnStart` and `OnStop`. A custom validation rule's client-visible failure is `validation.NewError`; `StatusHandler` accepts only 404 and 405.
4. v0.25.0 (done): `TenantStore` implements `Start`, `Ready` and `Shutdown` and is bound with `app.ProvideValue` and `credo.ComponentName("tenants-db")` alone; there is no store registration. `UsageReporter` is registered through its constructor on `app.Workers()`, the App's one worker registry, and its records carry `component=worker:usage-report`. The WebSocket server takes no `credo.Ingress()`, `Provide` takes the constructor without a type argument, and `Finalize` is an optional early check.

The [migration guide](../../docs/guides/pre-v1-migration.md) maps removed APIs and links the ADRs that define the shipped contracts.

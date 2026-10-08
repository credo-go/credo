# SaaS Composition Example

Run `go run .` from this directory to use its bundled configuration. The example demonstrates typed config, DI-managed services, JWT authentication, group authorization, localization, a data store, a scheduled worker, a WebSocket endpoint and health. Business operations are in-memory stubs; this is a composition example. It has its own module and an in-tree Credo replacement. CI builds, vets, checks tidy, serves `/health` and verifies graceful SIGTERM exit.

## Bootstrap order

`run()` in main.go follows the [documented bootstrap order](../../docs/specs/bootstrap-and-di-lifecycle.md#documented-order), one numbered comment per step:

1. **Configuration** — `config.Load`, the typed `app` and `databases.default` sections, the logger, `credo.New(credo.WithRawConfig(rawCfg), …)`.
2. **`Provide`** — the typed config values and the constructors of `TenantStore`, `TenantService` and `UsageReporter`, each taking `credo.Infra` first.
3. **Feature mounts and satellite registrations** — `UseRequestID`, `UseAccessLog`, `UseCompress`, `UseI18n` with a programmatic catalog, `UseHealth`, `store.Register[*TenantStore]`, a provided scheduled worker, the WebSocket server handed to `app.Manage(ws, credo.Ingress())`, and the `OnConfigChange`, `OnStart` and `OnStop` hooks.
4. **`Finalize`** — the graph is validated and its error handled before anything is built.
5. **`Resolve`, middleware and routes** — `TenantService` is resolved and its handlers bound; Secure and CORS are global middleware, authentication and the admin role check stay on their groups, and a `StatusHandler` answers the router's 404.
6. **`Run`** — the start phase pings the store, starts the worker and the WebSocket server and runs the start hooks before the App accepts requests; SIGINT/SIGTERM drains the listener, the WebSocket server and the worker first, then `TenantService`, then the store.

## What each part shows

- **Store.** `TenantStore` stands in for a `*sqldb.DB`: it implements `store.Lifecycle`, is bound with `app.Provide` and named `tenants-db` with `store.Register`. `/ready` reports its health; the App shuts it down after the services that use it. An application with `store/sqldb` binds the opened handle the same way.
- **Worker.** `UsageReporter` is built from the container and registered with `worker.Use(app).ScheduledProvided`, one run per hour; it stops with the HTTP drain.
- **WebSocket.** `websocket.New(app.NewInfra("websocket"))` is an ingress component; `GET /ws/echo` echoes every message.
- **Localization.** `UseI18n` takes a programmatic English catalog: error codes, validation codes and field display names are exact keys, and `ctx.T` translates the admin greeting.
- **Validation and errors.** `CreateTenantRequest.Validate` uses programmatic rules and one custom rule that returns `validation.NewError`; failures render in the default error envelope with field-level `violations`.
- **Reload.** `OnConfigChange("app", …)` switches the log level when `app.debug` changes and the process receives SIGHUP.

## Pre-v1 migration

**DI minor, HTTP minor (2026-09-05) and v0.24.0 (2026-10-08) applied.** main.go uses the shipped APIs.

1. DI minor (done): value/constructor registration finishes before an error-checked Finalize, then TenantService is resolved. Routes and DI-backed extension/hook registration stay before HTTP preparation. Building-state Shutdown is available to clean up registered resources after a later bootstrap failure. Hooks capture resolved dependencies instead of resolving during drain.
2. HTTP minor (done): explicit `UseRequestID` and `UseAccessLog` preserve this example's request correlation and records, and compression is installed with `UseCompress` instead of global middleware. Secure and CORS remain global middleware; authentication/authorization keep their group scope.
3. v0.24.0 (done): the bootstrap follows the six documented steps, with every feature mount and satellite registration before `Finalize`. A store is two calls — the binding and `store.Register`; the WebSocket server is one `Manage` registration with `Ingress`; workers are registered through `worker.Use(app)`; `UseI18n` returns nothing and panics on misuse, and locale files are read in the start phase; hooks are `OnStart` and `OnStop`. A custom validation rule's client-visible failure is `validation.NewError`; `StatusHandler` accepts only 404 and 405.

The [migration guide](../../docs/guides/pre-v1-migration.md) maps removed APIs and links the ADRs that define the shipped contracts.

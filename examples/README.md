# Credo Examples

Each runnable example is a separate Go module that replaces Credo with the repository root. Run it from its own directory so configuration discovery uses the bundled files.

| Directory | Purpose | Current verification |
| --- | --- | --- |
| [hello](hello/README.md) | Minimal routing, binding and QUERY | Build, vet, tidy, `/` smoke and graceful signal exit in CI |
| [saas](saas/README.md) | The documented bootstrap order with every satellite: typed config, DI, auth, middleware, i18n, a store, a scheduled worker, a WebSocket server and health | Build, vet, tidy, `/health` smoke and graceful signal exit in CI |
| [references](references/README.md) | Copyable config and locale catalogs | Reference data; not an application |

## Accepted pre-v1 migration

**DI, router, HTTP and wire minors applied 2026-09-05; v0.24.0 applied 2026-10-08.** The runnable source uses the shipped APIs. The [migration guide](../docs/guides/pre-v1-migration.md) records the shipped contracts; the URL round-trip change did not alter the examples. In v0.24.0 the SaaS example follows the [documented bootstrap order](../docs/specs/bootstrap-and-di-lifecycle.md#documented-order) as six numbered steps with every satellite in it, and the hello example needed no change: `credo.New`, routes and `Run` stay the minimal shape.

| Delivery | Hello | SaaS |
| --- | --- | --- |
| DI minor (done) | DI-independent route setup kept; Run prepares implicitly | DI writes finish before an error-checked Finalize; TenantService is resolved afterwards and its routes bound |
| HTTP minor (done) | Minimal default profile kept: recovery enabled; request features omitted | Calls UseRequestID, UseAccessLog and UseCompress explicitly |
| v0.24.0 bootstrap (done) | No bindings, so no explicit Finalize: `New`, routes, `Run` | Configuration, `Provide`, feature mounts and satellite registrations, an error-checked `Finalize`, `Resolve` with middleware and routes, `Run` — each a numbered step in `run()` |
| v0.24.0 components (done) | None required by the example | The store stand-in is bound with `Provide` and named with `store.Register`; the WebSocket server is handed to `Manage` with `Ingress`; the worker is a provided scheduled worker; `TenantService` and the store are components the App shuts down after their consumers; hooks are `OnStart`/`OnStop` |
| v0.24.0 errors (done) | None required by the example | `UseI18n` with a programmatic catalog returns nothing; the custom validation rule returns `validation.NewError`; `StatusHandler` registers the router's 404 only |
| User middleware | None required by the example | Keep Secure/CORS global and authentication/authorization on their existing groups |
| Cleanup | Keep graceful Run exit handling | Capture dependencies in hooks; bootstrap `Shutdown` is available for cleanup on setup errors |

Custom renderers are one successful `Use` registration, after any required DI resolution and before HTTP preparation. Feature configs have one public path. YAML/JSON may provide application-owned parameters, but cannot silently activate a feature.

Smoke coverage checks that each example starts, answers its smoke URL and exits 0 on SIGTERM. Build alone does not prove the sample starts or shuts down correctly: the SaaS start phase pings its store and starts its worker and WebSocket server before `/health` answers. Root `go test ./...` does not cover these nested modules. Keep source comments, dependency versions and docs aligned with each delivered API.

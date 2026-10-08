# SaaS Composition Example

Run `go run .` from this directory to use its bundled configuration. The example demonstrates typed config, a DI-managed TenantService, JWT authentication, group authorization and health. Business operations are stubs; this is a composition example. It has its own module and an in-tree Credo replacement. CI builds, vets, checks tidy, serves `/health` and verifies graceful SIGTERM exit.

## Pre-v1 migration

**DI minor and HTTP minor applied (2026-09-05).** main.go uses the shipped APIs.

1. DI minor (done): value/constructor registration finishes before an error-checked Finalize, then TenantService is resolved. Routes and DI-backed extension/hook registration stay before HTTP preparation. Building-state Shutdown is available to clean up registered resources after a later bootstrap failure. Hooks capture resolved dependencies instead of resolving during drain.
2. HTTP minor (done): explicit `UseRequestID` and `UseAccessLog` preserve this example's request correlation and records, and compression is installed with `UseCompress` instead of global middleware. Secure and CORS remain global middleware; authentication/authorization keep their group scope.
3. The example does not mount i18n. An application that adds it calls `app.UseI18n(cfg)`, which returns nothing, panics on misuse and reads its catalogs in the start phase. A custom detector would take `Detect(*Context)`, resolve on first use and handle absent auth data; call `Locale` after auth when the authenticated language must survive later request restoration (earlier reads still win). Every call consumes the registration, so it is never followed by another `UseI18n` call.
4. Source comments describe the explicit request features. Successful and error responses are compressed, health probes stay silent, and auth, startup and graceful shutdown are covered by the CI smoke run.

The [migration guide](../../docs/guides/pre-v1-migration.md) maps removed APIs and links the ADRs that define the shipped contracts.

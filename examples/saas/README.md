# SaaS Composition Example

Run `go run .` from this directory to use its bundled configuration. The example demonstrates typed config, a DI-managed TenantService, JWT authentication, group authorization, i18n and health. Business operations are stubs; this is a composition example. It has its own module and an in-tree Credo replacement. CI builds, vets, checks tidy, serves `/health` and verifies graceful SIGTERM exit.

## Pre-v1 migration

**Pending implementation.** Apply these changes with the framework minor that supplies them; the current main.go continues to use callable APIs.

1. DI minor: finish value/constructor registration before an error-checked Finalize, then resolve TenantService. Keep routes and DI-backed extension/hook registration before HTTP preparation. Use building-state Shutdown to clean up registered resources after later bootstrap failure once that path is implemented. Hooks capture resolved dependencies instead of resolving during drain.
2. HTTP minor: add explicit UseRequestID and UseAccessLog to preserve this example's request correlation and records. Move middleware.Compress out of GlobalMiddleware into UseCompress. Secure and CORS remain global middleware; authentication/authorization keep their group scope.
3. Keep UseI18n and the current catalogs. Custom detectors migrate to Detect(*Context), resolve on first use and handle absent auth data. Call Locale after auth when the authenticated language must survive later request restoration; earlier reads still win. Successful inactive discovery consumes registration, so it is not followed by another UseI18n call.
4. Update source comments about automatic request features. Check successful and error responses for compression, health-probe logging policy, auth behavior, startup and graceful shutdown.

The [migration preview](../../docs/guides/pre-v1-migration.md) maps removed APIs, and the [implementation plan](../../docs/plans/pre-v1-implementation.md) defines dependencies and gates.

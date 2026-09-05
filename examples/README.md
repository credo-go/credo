# Credo Examples

Each runnable example is a separate Go module that replaces Credo with the repository root. Run it from its own directory so configuration discovery uses the bundled files.

| Directory | Purpose | Current verification |
| --- | --- | --- |
| [hello](hello/README.md) | Minimal routing, binding and QUERY | Build, vet, tidy, `/` smoke and graceful signal exit in CI |
| [saas](saas/README.md) | Typed config, DI, auth, middleware and health | Build, vet, tidy, `/health` smoke and graceful signal exit in CI |
| [references](references/README.md) | Copyable config and locale catalogs | Reference data; not an application |

## Accepted pre-v1 migration

**DI minor and HTTP minor applied 2026-09-05.** The runnable source uses the shipped APIs. The [migration guide](../docs/guides/pre-v1-migration.md) and [implementation plan](../docs/plans/pre-v1-implementation.md) define the remaining URL round-trip work, which does not change the examples.

| Delivery | Hello | SaaS |
| --- | --- | --- |
| DI minor, P1–P3 (done) | DI-independent route setup kept; Run prepares implicitly | DI writes finish before an error-checked Finalize; TenantService is resolved afterwards and its routes bound |
| HTTP minor, P8 (done) | Minimal default profile kept: recovery enabled; request features omitted | Calls UseRequestID, UseAccessLog and UseCompress explicitly; retains UseI18n |
| User middleware | None required by the example | Keep Secure/CORS global and authentication/authorization on their existing groups |
| Cleanup | Keep graceful Run exit handling | Capture dependencies in hooks; bootstrap `Shutdown` is available for cleanup on setup errors |

Custom renderers are one successful `Use` registration, after any required DI resolution and before HTTP preparation. Feature configs have one public path. YAML/JSON may provide application-owned parameters, but cannot silently activate a feature.

Smoke coverage checks default versus explicit request-ID/access-log behavior, SaaS health and auth, successful and error-response compression, and graceful exit. Build alone does not prove the sample starts or shuts down correctly. Root `go test ./...` does not cover these nested modules. Keep source comments, dependency versions and docs aligned with each delivered API.

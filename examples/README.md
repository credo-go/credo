# Credo Examples

Each runnable example is a separate Go module that replaces Credo with the repository root. Run it from its own directory so configuration discovery uses the bundled files.

| Directory | Purpose | Current verification |
| --- | --- | --- |
| [hello](hello/README.md) | Minimal routing, binding and QUERY | Build, vet, tidy, `/` smoke and graceful signal exit in CI |
| [saas](saas/README.md) | Typed config, DI, auth, middleware and health | Build, vet, tidy, `/health` smoke and graceful signal exit in CI |
| [references](references/README.md) | Copyable config and locale catalogs | Reference data; not an application |

## Accepted pre-v1 migration

**DI minor applied 2026-09-05; HTTP minor pending.** The runnable source uses callable APIs only. The [migration guide](../docs/guides/pre-v1-migration.md) and [implementation plan](../docs/plans/pre-v1-implementation.md) define when the remaining rows migrate. New Use APIs in those documents are target spellings, not imports that can be added before the framework implementation exists.

| Delivery | Hello | SaaS |
| --- | --- | --- |
| DI minor, P1–P3 (done) | DI-independent route setup kept; Run prepares implicitly | DI writes finish before an error-checked Finalize; TenantService is resolved afterwards and its routes bound |
| HTTP minor, P8 | Preserve the minimal default profile: recovery enabled; request features omitted | Explicitly call UseRequestID and UseAccessLog; replace middleware.Compress with UseCompress; retain UseI18n |
| User middleware | None required by the example | Keep Secure/CORS global and authentication/authorization on their existing groups |
| Cleanup | Keep graceful Run exit handling | Capture dependencies in hooks; bootstrap `Shutdown` is available for cleanup on setup errors |

Custom-renderer samples in guides migrate from Set to one successful Use registration in the HTTP minor, after any required DI resolution and before HTTP preparation. Feature configs have one public path. YAML/JSON may provide application-owned parameters, but cannot silently activate a feature.

After migration, smoke coverage must check default versus explicit request-ID/access-log behavior, SaaS health and auth, successful and error-response compression, and graceful exit. Build alone does not prove the sample starts or shuts down correctly. Root `go test ./...` does not cover these nested modules. Keep source comments, dependency versions and docs aligned with each delivered API.

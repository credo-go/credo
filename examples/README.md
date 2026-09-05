# Credo Examples

Each runnable example is a separate Go module that replaces Credo with the repository root. Run it from its own directory so configuration discovery uses the bundled files.

| Directory | Purpose | Current verification |
| --- | --- | --- |
| [hello](hello/README.md) | Minimal routing, binding and QUERY | Build, vet, tidy, `/` smoke and graceful signal exit in CI |
| [saas](saas/README.md) | Typed config, DI, auth, middleware and health | Build, vet, tidy, `/health` smoke and graceful signal exit in CI |
| [references](references/README.md) | Copyable config and locale catalogs | Reference data; not an application |

## Accepted pre-v1 migration

**Pending implementation, 2026-09-05.** The runnable source still uses today's API. The [migration preview](../docs/guides/pre-v1-migration.md) and [implementation plan](../docs/plans/pre-v1-implementation.md) define when to migrate it. New Use APIs in those documents are target spellings, not imports that can be added before the framework implementation exists. G1–G4 design decisions are accepted; implementation and verification remain outstanding.

| Delivery | Hello | SaaS |
| --- | --- | --- |
| DI minor, P1–P3 | Keep DI-independent route setup; Run prepares implicitly | Finish DI writes, explicitly Finalize with error handling, then resolve TenantService and bind routes |
| HTTP minor, P8 | Preserve the minimal default profile: recovery enabled; request features omitted | Explicitly call UseRequestID and UseAccessLog; replace middleware.Compress with UseCompress; retain UseI18n |
| User middleware | None required by the example | Keep Secure/CORS global and authentication/authorization on their existing groups |
| Cleanup | Keep graceful Run exit handling | Capture dependencies in hooks; demonstrate bootstrap cleanup on setup errors once supported |

Custom-renderer samples in guides migrate from Set to one successful Use registration in the HTTP minor, after any required DI resolution and before HTTP preparation. Feature configs have one public path. YAML/JSON may provide application-owned parameters, but cannot silently activate a feature.

After migration, smoke coverage must check default versus explicit request-ID/access-log behavior, SaaS health and auth, successful and error-response compression, and graceful exit. Build alone does not prove the sample starts or shuts down correctly. Root `go test ./...` does not cover these nested modules. Keep source comments, dependency versions and docs aligned with each delivered API.

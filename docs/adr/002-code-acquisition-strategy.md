# ADR-002: Code Acquisition Strategy

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001 **Related:** ADR-014

## Context

An all-in-one framework (ADR-001) requires numerous components: router, DI, config, validation, observability, data access, etc. For each component, three paths are possible:

1. **Write from scratch**: Full control but high cost, unproven code.
2. **Adapt (fork & own)**: Copy proven open-source code in a license-compliant manner, adapt to Credo's architecture, and take ownership. Inter-component incompatibilities and unnecessary features are resolved at the framework level.
3. **Wrap (import & pin)**: Import the library directly, wrap it behind a thin adapter layer exposing the Credo API, and pin the version.

Each approach has different trade-offs. Adapt gives full control over behavior but requires manually tracking upstream changes. Wrap receives upstream security patches and compatibility updates through reviewed version bumps that scanners track, but control over the API surface is limited.

## Decision

A hybrid strategy is adopted; the criterion that determines which path to choose follows. [NOTICES](../../NOTICES) is the per-file inventory of adapted and wrapped code with each source's license, and [SECURITY-UPSTREAMS.md](../../SECURITY-UPSTREAMS.md) records how each source is monitored.

### Adapt (fork & own) — Components whose behavior we want to own

Components that shape the framework's core experience and directly affect the API surface are adapted — for example the router and radix tree (from Chi), the request context and binder (from Echo) and the DI container (from samber/do).

For these components, Credo evolves the API independently from upstream, removes unnecessary features, and ensures inter-component consistency.

### Wrap + pin — Components with fast evolution and high security pressure

Components that implement specs/protocols, have high CVE pressure, and evolve rapidly are wrapped — for example the SQL ORM and migration engine (uptrace/bun, in `store/sqldb`), JWT parsing (golang-jwt/jwt/v5), the WebSocket protocol engine (coder/websocket) and the rate limiter's store contract (sethvargo/go-limiter, in `middleware/ratelimit`, a package of its own so that only its users link it).

For these components, Credo provides a thin adapter layer and pins the version. Upgrades arrive as reviewed version bumps (Dependabot pull requests) that pass the [wrapped-dependency upgrade gate](../../CONTRIBUTING.md#wrapped-dependency-upgrades). A planned package chooses its path when it is designed; tracing and metrics are to be wrapped ([ADR-014](014-observability.md)).

### Direct import — Small, stable utilities

Low-level, small tools with stable APIs are imported directly: `golang.org/x/text` (BCP 47 language tags and CLDR plural data for i18n), `go.yaml.in/yaml/v3` and `github.com/go-viper/mapstructure/v2` (config decoding). These are the only modules the root package links besides its own, an allowlist decided [below](#what-lives-in-the-root-package).

### What lives in the root package

**The root package's module allowlist.** Every application pays for what the root package links, so the modules it links are a decision, not a snapshot: the standard library, Credo's own module, and exactly `go.yaml.in/yaml/v3`, `github.com/go-viper/mapstructure/v2` and `golang.org/x/text`. `credo.New()` loads configuration automatically so that a setup works with minimal configuration ([ADR-001](001-framework-identity-and-goals.md)), which puts a YAML parser and the decoder of typed sections in the graph; `x/text` enters through `internal/i18n`, which the error pipeline and `UseI18n` import, and is maintained by the Go team and carries the CLDR data that BCP 47 matching and plural rules need. `TestImportBoundary_RootPackageGraph` in `architecture_test.go` enforces the list: a module that enters through `config`, `validation` or an internal package the root imports is added to it in the same change, and one that leaves the graph is removed. The [contributing guide](../../CONTRIBUTING.md#coding-standards) states the procedure; the rejected alternatives are [below](#rejected-alternatives).

**The rule that places a package.** Where a piece of Credo lives follows from how the App holds it:

> **Pending v0.25.0 (W4):** the workers named in the next paragraph still live in the `worker` package (`worker.Use(app)` returns a `*worker.Supervisor`); W4 moves them into the root package as `app.Workers()`.

- A subsystem the App holds one of and reaches through `app.` lives in the root package: routing, DI, the lifecycle, health, i18n, the `Use*` features and the workers, and later the observers and the application modules planned before v1.
- A value the application builds and hands to the App lives in a package of its own: `sqldb`'s database, the WebSocket server, a middleware such as CORS or a rate limit, a JWT authenticator, and later the pubsub adapters.
- A package the App never touches stays a package: `config`, `validation`, `fault`.

The allowlist is the rule's constraint: a subsystem that needs a third-party module lives in a package however closely the App holds it. [ADR-004](004-dependency-injection-and-infra.md) (DI stays in the core) and [ADR-023](023-worker-system.md) (workers in the root package) apply this rule.

### Adaptation Process

Every adapted file follows this sequence:

```
1. LICENSE CHECK  → Verify that the source license permits adaptation (MIT, BSD-3-Clause, ISC or Apache-2.0)
2. COPY           → Bring files into Credo's directory structure
3. ATTRIBUTE      → Retain original copyright header at top of file
4. ADAPT          → Align package name, imports, naming with Credo
5. SIMPLIFY       → Remove code unnecessary for Credo's goals
6. TEST           → Copy/adapt original tests, add Credo-specific tests
7. DOCUMENT       → Update NOTICES file and package doc.go
```

### Attribution Rules

- Every adapted file retains the original copyright header at the top.
- The NOTICES file (project root) lists each source project.
- When code has been >80% rewritten, an "Originally derived from [project]" note suffices, but the NOTICES entry must remain.

## Rejected Alternatives

- **A root package that is stdlib-only transitively, with configuration loading made opt-in.** `credo.New()` loads YAML automatically so that a setup works with minimal configuration (ADR-001); making that opt-in moves a step into every application to remove a parser from the graph.
- **YAML as the only entry, with `x/text` behind a `Translator` interface and mapstructure replaced by a native decoder.** mapstructure stays until it is replaced, and `x/text` enters through `internal/i18n`, which the error pipeline and `UseI18n` import, so an interface alone does not remove it. `x/text` is maintained by the Go team and carries the CLDR data, so allowlisting it is the honest choice.

## Consequences

**Positive:**

- Full control and consistency over core components
- Infrastructure components take upstream security and compatibility fixes through reviewed version bumps
- Core components start from a proven codebase instead of from scratch
- License compliance is checked per source (adaptation step 1) and recorded in NOTICES

**Negative:**

- Upstream tracking burden for adapted core components: Dependabot and `govulncheck` do not see adapted code, so SECURITY-UPSTREAMS.md tracks it through a monthly manual review
- Two different acquisition models = requires clarity within the team (which component belongs to which category; NOTICES records each adapted or wrapped source)
- Limited API surface control for wrapped components

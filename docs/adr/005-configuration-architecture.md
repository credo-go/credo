# ADR-005: Configuration Architecture

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-004 **Related:** ADR-014, ADR-020 **Specification:** [Config spec](../specs/config.md) **Guide:** [Configuration guide](../guides/configuration.md)

## Context

Credo's config system must address two fundamental needs:

1. **Framework bootstrap**: The framework's own needs such as server port, timeout, and observability settings.
2. **Application config**: Application-specific settings such as database connection, API keys, and business logic parameters.

A naive approach (string key-based API like `GetString("db.host")`, `GetInt("server.port")`) creates the following problems:

- **Type-unsafe**: `Get("key") any` → caller is forced to type-assert
- **Error-swallowing**: `GetString("key")` returns zero value if not found, error silently lost — conflicts with the "errors are values" principle
- **Two parallel APIs**: Offering struct decoding beside untyped key getters (`Get(key) any`, `GetString`) increases cognitive load, developers get directed to the wrong path
- **Runtime key lookup**: Config is loaded at startup. There is almost no need for dynamic key lookup at runtime (plugin/extension system and feature flags are not in current scope)

Loading itself has failure modes of its own: ambient sources (a stray `.env` in the working directory, a `CREDO_*` variable in the process environment) silently change configuration and leak into tests, and a misspelled key silently becomes a default value.

## Decision

### Config = Typed Snapshot via DI

Config is positioned as a "snapshot produced at startup," not as a "globally accessed service." `config.Load()` produces a `RawConfig`, modules call `Unmarshal` once at the boundary to create typed structs, then register them in DI. Business code receives typed config via constructors — string keys never appear beyond the module boundary.

The snapshot is immutable for everything injected through DI. A section may opt into trigger-driven reload through a typed `app.OnConfigChange[T]` subscription; every other section is restart-only and the framework reports it as such ([ADR-020](020-reload-and-partial-config-reload.md)).

Use of string keys in business code is documented as an **anti-pattern**.

### RawConfig: 2-Method Interface

`RawConfig` is limited to `Unmarshal(key, &dst) error` + `Exists(key) bool`: no scalar getters, no `Get(key) any`. `Unmarshal` decodes primitives as well as structs, following the `encoding/json.Unmarshal` precedent, so a single value needs no wrapper struct. Two methods keep custom implementations (a `WithRawConfig` store) trivial; the ergonomic typed getter lives on the concrete types instead (below).

### config.Load() Returns Error, Not Panic

`config.Load()` performs I/O and returns `(*config.Config, error)` — the concrete type, which satisfies `RawConfig`. Each call produces an independent `Config`; there is no package-global instance. Load returns I/O, parse and option errors, and a section's `Validate()` error returns from `Unmarshal`/`Get`; neither panics, because config errors are expected operational outcomes and returning them is Go-idiomatic and assertable in tests.

### credo.New() Auto-Loads and Registers RawConfig

`credo.New()` loads configuration through `config.Load()` when no `RawConfig` is given, so the zero-config path works out of the box; `credo.WithRawConfig(rc)` supplies a pre-loaded store and bypasses the auto-load. The `RawConfig` is always registered in the DI container. File selection belongs to the `config.Load` options (`config.WithFiles`, …): the root package deliberately does not duplicate them, so a missing `RawConfig` always means "use Credo's default discovery".

### Typed Snapshot Getter: Get[T] / GetConfig[T]

Go 1.27 concrete-type generic methods allow an ergonomic typed getter over `Unmarshal`: a generic `Get[T]` on `*config.Config` and `GetConfig[T]` on `*App`, each with a `Must` variant ([signatures](../specs/config.md#typed-snapshot-getter--gett--getconfigt)). It collapses the `var x T; rc.Unmarshal(key, &x)` two-step into one call that still returns the decode error.

These are bootstrap/composition-root sugar, not a runtime service locator: `*credo.Context` has no `App()` accessor, so handlers and services cannot reach `app.GetConfig` through the request, and typed structs still flow to business code via DI. The `Must` variants panic, matching `MustResolve` and the registration calls for fail-fast startup wiring. The getter lives on the concrete types rather than on `RawConfig` because a generic method cannot live on an interface.

### RawConfig Defined in config/ Package

The `RawConfig` interface is defined in the `config/` package to avoid circular imports (config/ needs to return it, root needs to accept it). The root package re-exports it as `credo.RawConfig` via a type alias.

### Cascade Merge of Config Files

Config file loading uses cascade merge: all found base files are merged (later files override earlier for overlapping keys), and when `CREDO_ENV` is set the env-specific files merge on top. An environment overrides only the keys it changes, without code changes and without repeating the base keys. File names and order: [config spec](../specs/config.md#source-precedence--discovery).

### Dotenv Path Resolution

The `.env` path can be named in code (`WithDotenvPath`), through the `CREDO_ENV_FILE` variable, or left at the default `.env` in the working directory. A binary-relative deployment cannot rely on the working directory, and a variable an operator must know about is hidden knowledge, so the path is also settable in code. A path someone named explicitly is required — naming it states intent, so a missing file fails the load — while `WithDotenvOptional()` downgrades that to a warning so callers need not pre-check the file, and the default `.env` stays optional for zero-config local development. Resolution order and the bootstrap keys: [config spec](../specs/config.md#env-resolution-policy).

### `.env` Entries Are Not Prefix-Filtered

Process environment variables are prefix-filtered (default `CREDO_`); `.env` entries are not. A `.env` file is project-scoped — it lives with the project and is not shared with other processes — so a namespace prefix only adds verbosity, while the process environment shares one global namespace with the OS and other tools, where prefix filtering is essential. This matches the convention of most frameworks (Laravel, Django, Express, GoFr).

### Source Opt-Outs (Hermetic Loading)

`WithoutProcessEnv()` and `WithoutDotenv()` each remove a configuration source entirely. A deployment that mandates one authoritative config file (reproducible images, security-sensitive services) and a test suite that must not inherit the developer's shell both need to shut out ambient sources. The opt-out kills the whole source, bootstrap keys (`CREDO_ENV`, `CREDO_ENV_FILE`) included, rather than only its merge layer, so "the process environment does not exist for config purposes" holds without exceptions; reload replays the opt-outs, so a disabled source cannot leak in later. Combined with explicit files (or an embedded document), loading is fully hermetic. Mechanics: [config spec](../specs/config.md#source-opt-outs--hermetic-loading).

### Strict Decoding

`WithStrictDecoding()` makes typed decodes reject unknown keys and drops weak string-to-number/bool coercion. Unknown-key typos are the dominant operational config hazard — a misspelled key silently becomes a default value — and weak coercion masks type errors in file-based configs, whose values are already typed. The policy rides on the `Config` instance, so one option covers every decode path, the framework's own `server` decode and reload validation included. Strict mode is opt-in because weak coercion is what lets string env and `.env` overrides reach typed fields; it targets typed sources and pairs with the source opt-outs. The error guarantee is scoped rather than absolute: unknown-key errors carry key paths only, while type-mismatch errors may quote the value. Whether strict becomes the default at v1 is an open breaking decision in the [v1 Gate](../../TODO.md#v1-gate). Mechanics: [config spec](../specs/config.md#strict-decoding).

### Config Is Not in Infra

Config is not included in the `credo.Infra` struct (ADR-004). Rationale:

- Config may require different sections for each service (`*OrderConfig` vs `*DatabaseConfig`), which one carrier type cannot express
- Config is a startup snapshot (partial reload is an explicit per-section subscription, ADR-020); `Infra` carries runtime infrastructure (today the logger; metrics and tracing are planned, [ADR-014](014-observability.md))
- Passing typed config explicitly via DI is more type-safe

## Rejected Alternatives

| Alternative | Reason for rejection |
| --- | --- |
| Scalar getters (GetString/GetInt/GetBool) | Error-swallowing API, returns zero value, facilitates abuse. Distinct from the generic `Get[T]`, which returns `(T, error)` and decodes whole sections, not loose scalars |
| `Get(key) any` | Type-unsafe, `.(int)` assertion risk, the generic `Get[T]`/Unmarshal is sufficient |
| Whole-config `app.Config()` accessor | Returns the entire config object, encouraging runtime key-based access. The keyed `app.GetConfig[T]` stays composition-root-only, so typed config via DI remains the path into business code |
| `MustLoad` (panic on load) | `config.Load` performs I/O; its error must be handled. `MustGet`/`MustGetConfig` panic only when decoding an already-loaded config |
| User-facing CoreConfig type | Server config is a framework-internal concern — user should not be forced to embed it |
| Root `WithConfigFiles` option | Duplicates `config.Load(config.WithFiles(...))` and expands root API surface |
| `WithoutAutoConfig` option | Weak practical need; `WithRawConfig` covers explicit control without adding another mode |
| ASP.NET `IOptions<T>` pattern | Extra abstraction layer, simple DI is sufficient in Go |
| Global config instance | Makes multi-instance tests harder, independent Apps conflict |
| First-found-wins file discovery | An env-specific file would have to repeat every base key; cascade merge lets each layer carry only what it changes |
| Repurposing `WithPrefix("")` as the environment opt-out | An empty prefix removes the filter and matches every environment variable; reading it as "disable" would be a surprising reinterpretation. Disabling a source is a separate, explicit intent |
| Separate unknown-key and coercion strictness options | Two half-strict modes to explain, with no demonstrated demand |
| Sanitizing every decode error to value-free text | A brittle wrapper around mapstructure that destroys debuggability; the guarantee is scoped to unknown-key errors instead |
| Whole-config hot reload | Rejected in [ADR-020](020-reload-and-partial-config-reload.md): typed structs already injected through DI cannot observe the change |

## Consequences

**Positive:**

- Config access is end-to-end type-safe (struct → DI → constructor)
- String key appears only at the module boundary, in a single place
- Config errors are values — assertable in tests
- A deployment can pin its configuration to exactly the files it ships (source opt-outs + strict decoding)

**Negative:**

- DI registration is required for each config section (minimal boilerplate)
- RawConfig still exists — may appear as two APIs (mitigate: position as "bootstrap only," document accordingly)
- Module authors must know the Unmarshal + DI registration pattern
- Decoding relies on mapstructure, so its coercion rules and error texts (a type-mismatch error may quote the value) are part of the contract
- Under strict decoding, string env/`.env` overrides on typed fields do not decode, and every typed view must cover its whole section

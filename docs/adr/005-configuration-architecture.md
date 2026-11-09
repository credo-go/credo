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

> **Current until v0.26.0 (R15):** the next paragraph names the `app.OnConfigChange[T]` subscription, which live sections replace.

The snapshot is immutable for everything injected through DI. A section may opt into trigger-driven reload through a typed `app.OnConfigChange[T]` subscription; every other section is restart-only and the framework reports it as such ([ADR-020](020-reload-and-partial-config-reload.md)).

> **Pending v0.26.0 (R15):** the next paragraph replaces the paragraph above when R15 ships; until then a section follows a reload only through an `app.OnConfigChange[T]` subscription.

A section is a snapshot unless it is declared live. A section bound through its descriptor's `Decode` is decoded once, and a change to it takes a restart; a section bound through `Live` is a `*config.Live[T]` whose value a reload replaces whole once every live section the reload changes has validated. A section is static or live within one configuration, never both, and every key no live section covers is reported as needing a restart ([ADR-020](020-reload-and-partial-config-reload.md#a-section-is-a-snapshot-unless-it-is-declared-live)).

Use of string keys in business code is documented as an **anti-pattern**.

### RawConfig: 2-Method Interface

`RawConfig` is limited to `Unmarshal(key, &dst) error` + `Exists(key) bool`: no scalar getters, no `Get(key) any`. `Unmarshal` decodes primitives as well as structs, following the `encoding/json.Unmarshal` precedent, so a single value needs no wrapper struct. Two methods keep custom implementations (a `WithRawConfig` store) trivial; the ergonomic typed getter lives on the concrete types instead (below).

### config.Load() Returns Error, Not Panic

`config.Load()` performs I/O and returns `(*config.Config, error)` — the concrete type, which satisfies `RawConfig`. Each call produces an independent `Config`; there is no package-global instance. Load returns I/O, parse and option errors, and a section's `Validate()` error returns from `Unmarshal`/`Get`; neither panics, because config errors are expected operational outcomes and returning them is Go-idiomatic and assertable in tests.

### credo.New() Auto-Loads and Registers RawConfig

`credo.New()` loads configuration through `config.Load()` when no `RawConfig` is given, so the zero-config path works out of the box; `credo.WithRawConfig(rc)` supplies a pre-loaded store and bypasses the auto-load. The `RawConfig` is always registered in the DI container. File selection belongs to the `config.Load` options (`config.WithFiles`, …): the root package deliberately does not duplicate them, so a missing `RawConfig` always means "use Credo's default discovery".

### Typed Snapshot Getter: Get[T] / GetConfig[T]

> **Current until v0.26.0 (R5):** the next paragraph describes `Config.Get[T]` and `Config.MustGet[T]`, which go, and the keyed `App.GetConfig[T]`, which takes the section descriptor; the [section descriptor](#a-section-is-named-once-configsectiont) replaces it.

Go 1.27 concrete-type generic methods allow an ergonomic typed getter over `Unmarshal`: a generic `Get[T]` on `*config.Config` and `GetConfig[T]` on `*App`, each with a `Must` variant ([signatures](../specs/config.md#typed-snapshot-getter--gett--getconfigt)). It collapses the `var x T; rc.Unmarshal(key, &x)` two-step into one call that still returns the decode error.

These are bootstrap/composition-root sugar, not a runtime service locator: `*credo.Context` has no `App()` accessor, so handlers and services cannot reach `app.GetConfig` through the request, and typed structs still flow to business code via DI. The `Must` variants panic, matching `MustResolve` and the registration calls for fail-fast startup wiring. The getter lives on the concrete types rather than on `RawConfig` because a generic method cannot live on an interface.

### A Section Is Named Once: `config.Section[T]`

> **Pending v0.26.0 (R5):** not implemented yet; a section is read by its key at each reader — `Unmarshal`, `Config.Get[T]`, `App.GetConfig[T]` — and bound through a constructor of the application's own or `ProvideValue`.

A section's key and its type are named once, by a typed descriptor that every reader of the section takes:

```go
var AppSection = config.Section[AppConfig]("app")

app.Provide(AppSection.Decode)       // a snapshot of the section, bound in DI
cfg := app.MustGetConfig(AppSection) // the same descriptor in the composition root
```

`config.Section[T]` is a named string type, `type Section[T any] string`, so `config.Section[AppConfig]("app")` names a section and its type by conversion. `Section[T].Decode(src config.RawConfig) (T, error)` decodes the section through `Unmarshal`, which runs the target's `Validate`, and an empty key is an error. It lives in `config` and needs no root import, since `credo.RawConfig` is an alias of `config.RawConfig`. The descriptor is the receiver of the call that reads it, where a request key is an argument of its carrier, `c.Get(key)`: the descriptor's method value is the constructor `Provide` takes, while a request value has the Context as its one carrier.

`Decode`'s method value is an ordinary constructor over the framework's `RawConfig` binding, so binding a section needs no App method: a missing section or a failed validation is a constructor error at `Resolve` or at start that names the key, not a registration panic, and a second binding of one type panics with the container's message and its remedy. The bound value is the section's `T`, a snapshot. ADR-001's automatic loading and the rejection of `IOptions<T>` (below) stand: the descriptor binds the section's `T` unwrapped, and only a section declared live is wrapped ([ADR-020](020-reload-and-partial-config-reload.md#a-section-is-a-snapshot-unless-it-is-declared-live)).

A section that only one constructor reads is decoded in that constructor and not bound: two databases whose sections both decode into `sqldb.Config` each call their own section's `Decode` where they are opened, and the databases are what the container binds. A section that business code receives is bound once, or under a type of its own.

**One reader per holder.** `section.Decode(cfg)` reads a section from a `*config.Config`, so the configuration has no getter of its own. `App.GetConfig` and `App.MustGetConfig` stay for the composition root, which holds the App rather than its configuration, and take the descriptor: `app.GetConfig(AppSection)` infers `T`, and `app.GetConfig[AppConfig]("app")` keeps compiling, since the untyped constant converts. `testutil`'s empty configuration honours the missing-key contract: its `Unmarshal` returns the not-found error a real `*config.Config` returns, and `Exists` stays false.

Rejected: an App method that binds a section, which `Provide` with `Decode` already does; a descriptor type that refuses a literal key, which would break every `GetConfig` call written with one, for a check decoding already makes; reporting unknown top-level keys by recording the sections a run decodes, which would flag a section first decoded after the start, one only another deployment reads and, under `WithPrefix("")`, every process variable; and configuration as an `Infra` field, which could carry only the untyped `RawConfig` and would put string keys into business code, the service locator ADR-004 rejects.

**Problem.** A section's key and type are repeated at every read — `Unmarshal("app", &x)` in a constructor, `GetConfig[AppConfig]("app")` in the composition root, a subscription's key — so a misspelled key or a mismatched type at one read surfaces only when that read runs. Binding a section takes a hand-written constructor around `Unmarshal` or a `ProvideValue` of a value decoded in the composition root, and the configuration and the App each have a getter, two spellings of one read. `testutil`'s empty configuration accepts every key, so a test passes where the same wiring fails in production on a missing section.

**Removes.** `Config.Get[T]` and `Config.MustGet[T]` (`cfg.Get[T]("app")` becomes `config.Section[T]("app").Decode(cfg)`); `testutil`'s empty configuration accepting any key; `App.GetConfig[T]` and `App.MustGetConfig[T]` keep compiling with a literal key and take the descriptor.

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

> **Current until v0.26.0 (R3):** the next paragraph describes strict decoding as the `WithStrictDecoding()` opt-in; [strict decoding by default](#strict-decoding-by-default) replaces it.

`WithStrictDecoding()` makes typed decodes reject unknown keys and drops weak string-to-number/bool coercion. Unknown-key typos are the dominant operational config hazard — a misspelled key silently becomes a default value — and weak coercion masks type errors in file-based configs, whose values are already typed. The policy rides on the `Config` instance, so one option covers every decode path, the framework's own `server` decode and reload validation included. Strict mode is opt-in because weak coercion is what lets string env and `.env` overrides reach typed fields; it targets typed sources and pairs with the source opt-outs. The error guarantee is scoped rather than absolute: unknown-key errors carry key paths only, while type-mismatch errors may quote the value. Whether strict becomes the default at v1 is an open breaking decision in the [v1 Gate](../../TODO.md#v1-gate). Mechanics: [config spec](../specs/config.md#strict-decoding).

### Strict Decoding by Default

> **Pending v0.26.0 (R3):** not implemented yet; decoding is weak unless `WithStrictDecoding()` is given.

`config.Load` decodes strictly unless told otherwise: an unknown key is an error naming its key path, and a value is not coerced across kinds. The policy rides on the `Config` instance as before, so it covers `Unmarshal`, a section's `Decode`, the decoding of a reload's candidate and the framework's own `server` section in `credo.New`.

Strings parse into basic types. Process environment and `.env` values are strings, so a strict mode that refused every string for a typed field would refuse every override of one. The default therefore adds mapstructure's `StringToBasicTypeHookFunc` beside the duration and `TextUnmarshaler` hooks that stay on in both modes: `CREDO_SERVER__PORT=9090` decodes into an `int`. Conversions across kinds stay refused — a boolean into a number, a number into a string, a scalar into a slice. The hook does not know a value's source, so a quoted number in a file decodes too; parsing a string by its target type is not a conversion between kinds.

`config.WithWeakDecoding()` is the opt-in, and its contract is one sentence: mapstructure's weak coercion. A refused value's error names the key path, the conversion refused and `config.WithWeakDecoding()` as the remedy. Rejected: a normative coercion rule set with a conformance suite; the opt-in's contract stays that one sentence.

**Problem.** A misspelled key silently becomes a default value, and weak coercion masks type errors in file-based configuration, whose values are already typed; both are what an operator meets in production, and the safe behaviour is an option an application must know to ask for. The opt-in strict mode, in turn, refuses every string override of a typed field, so an application that wants it must also give up environment overrides.

**Removes.** `config.WithStrictDecoding()`, now the default; weak coercion and silently ignored unknown keys without `WithWeakDecoding()`.

### Config Is Not in Infra

Config is not included in the `credo.Infra` struct (ADR-004). Rationale:

> **Current until v0.26.0 (R15):** the second item's parenthesis names reload as "an explicit per-section subscription"; with R15 a reload replaces the value of a section declared live ([ADR-020](020-reload-and-partial-config-reload.md#a-section-is-a-snapshot-unless-it-is-declared-live)).

- Config may require different sections for each service (`*OrderConfig` vs `*DatabaseConfig`), which one carrier type cannot express
- Config is a startup snapshot (partial reload is an explicit per-section subscription, ADR-020); `Infra` carries runtime infrastructure (today the logger; metrics and tracing are planned, [ADR-014](014-observability.md))
- Passing typed config explicitly via DI is more type-safe

## Rejected Alternatives

> **Current until v0.26.0 (R5):** the first, second and fourth rows name `Config.Get[T]` and `Config.MustGet[T]`, which go; with R5 their reasons name the section descriptor's `Decode` and `App.MustGetConfig`. The section descriptor's own rejections are in [its section](#a-section-is-named-once-configsectiont).

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

> **Current until v0.26.0 (R3):** the last two items describe weak decoding as the default contract and strict decoding refusing string overrides; with R3 strict decoding is the default, strings parse into basic types, and only `WithWeakDecoding()` makes mapstructure's coercion part of the contract.

- DI registration is required for each config section (minimal boilerplate)
- RawConfig still exists — may appear as two APIs (mitigate: position as "bootstrap only," document accordingly)
- Module authors must know the Unmarshal + DI registration pattern
- Decoding relies on mapstructure, so its coercion rules and error texts (a type-mismatch error may quote the value) are part of the contract
- Under strict decoding, string env/`.env` overrides on typed fields do not decode, and every typed view must cover its whole section

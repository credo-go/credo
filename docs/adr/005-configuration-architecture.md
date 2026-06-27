# ADR-005: Configuration Architecture

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-004 **Spec:** [specs/config.md](../specs/config.md)

## Context

Credo's config system must address two fundamental needs:

1. **Framework bootstrap**: The framework's own needs such as server port, timeout, and observability settings.
2. **Application config**: Application-specific settings such as database connection, API keys, and business logic parameters.

A naive approach (string key-based API like `GetString("db.host")`, `GetInt("server.port")`) creates the following problems:

- **Type-unsafe**: `Get("key") any` → caller is forced to type-assert
- **Error-swallowing**: `GetString("key")` returns zero value if not found, error silently lost — conflicts with the "errors are values" principle
- **Two parallel APIs**: Offering struct decode (LoadInto) + key-based access (Get) together increases cognitive load, developers get directed to the wrong path
- **Runtime key lookup**: Config is immutable and loaded at startup. There is almost no need for dynamic key lookup at runtime (plugin/extension system and feature flags are not in current scope)

## Decision

### Config = Typed Snapshot via DI

Config is positioned as a "snapshot produced at startup," not as a "globally accessed service." `config.Load()` produces a `RawConfig`, modules call `Unmarshal` once at the boundary to create typed structs, then register them in DI. Business code receives typed config via constructors — string keys never appear beyond the module boundary.

Use of string keys in business code is documented as an **anti-pattern**.

### RawConfig: 2-Method Interface

`RawConfig` is limited to `Unmarshal(key, &dst) error` + `Exists(key) bool`. No scalar getters (`GetString`/`GetInt`/`GetBool`), no `Get(key) any`. `Unmarshal` supports both structs and primitives, following the `encoding/json.Unmarshal` precedent. The ergonomic typed-snapshot getter (`Get[T]`/`GetConfig[T]`, below) is a generic method on the concrete `*config.Config` and `*App` — not on the `RawConfig` interface, which stays minimal so custom implementations remain trivial (a generic method cannot live on an interface anyway).

### config.Load() Returns Error, Not Panic

`config.Load()` performs I/O and returns `(*config.Config, error)` — the concrete type, which satisfies `RawConfig`. No package-global instance — each call produces an independent `Config`. Invalid config (parse error, validation) returns error, never panics. There is no `MustLoad`: a load that touches the filesystem must surface its error, not panic.

### credo.New() Auto-Loads and Registers RawConfig

`credo.New()` automatically loads configuration via `config.Load()` when no explicit `RawConfig` is provided. Use `credo.WithRawConfig(rawCfg)` to pass a pre-loaded config; doing so bypasses auto-load. `RawConfig` is always registered in the DI container. There is no whole-config `app.Config()` accessor; for keyed reads at the composition root, `app.GetConfig[T](key)` decodes a section on demand (see below).

### Typed Snapshot Getter: Get[T] / GetConfig[T]

Go 1.27 concrete-type generic methods add an ergonomic typed-snapshot getter over `Unmarshal`: `(*config.Config).Get[T](key) (T, error)` plus `MustGet[T]`, and `(*App).GetConfig[T](key) (T, error)` plus `MustGetConfig[T]`. Each decodes a key into a value of `T` and returns it, collapsing the `var x T; rawCfg.Unmarshal(key, &x)` two-step into one call that still returns the decode error.

These are positioned as bootstrap/composition-root sugar, not a runtime service locator. There is no `App()` accessor on `*credo.Context` (ADR-008), so handlers and services cannot reach `app.GetConfig` through the request — config reads stay at the composition root, and typed structs still flow to business code via DI. `MustGet`/`MustGetConfig` panic on error, matching the `MustProvide`/`MustResolve` family for fail-fast startup wiring; `Get`/`GetConfig` return the error and the zero value of `T`. The getter lives on the concrete types only: `App.GetConfig` delegates through `RawConfig.Unmarshal`, so it works identically for the auto-loaded `*config.Config` and any custom `WithRawConfig` implementation.

Root-level file controls are deliberately not duplicated. Applications that need explicit file selection call `config.Load(config.WithFiles(...))` and pass the result with `WithRawConfig`. There is no `credo.WithConfigFiles` option. There is also no `WithoutAutoConfig`: a missing explicit `RawConfig` means "use Credo's default config discovery," which keeps the default all-in-one experience simple.

### RawConfig Defined in config/ Package

The `RawConfig` interface is defined in the `config/` package to avoid circular imports (config/ needs to return it, root needs to accept it). The root package re-exports it as `credo.RawConfig` via a type alias.

### Cascade Replaces First-Found-Wins

Config file loading uses cascade merge: all found base files are merged (later files override earlier for overlapping keys). When `CREDO_ENV` is set, env-specific files (`config.{env}.*`) are merged on top. This enables environment-based overrides without code changes.

### Config Is Not in Infra

Config is not included in the `credo.Infra` struct (ADR-004). Rationale:

- Config may require different sections for each service (`*OrderConfig` vs `*DatabaseConfig`)
- Config is an immutable startup-time snapshot; Logger/Metrics/Tracer are runtime infrastructure
- Passing typed config explicitly via DI is more type-safe

### Rejected Alternatives

| Alternative | Reason for rejection |
| --- | --- |
| Scalar getters (GetString/GetInt/GetBool) | Error-swallowing API, returns zero value, facilitates abuse. Distinct from the generic `Get[T]`, which returns `(T, error)` and decodes whole sections, not loose scalars |
| `Get(key) any` | Type-unsafe, `.(int)` assertion risk, the generic `Get[T]`/Unmarshal is sufficient |
| Whole-config `app.Config()` accessor | Returns the entire config object, encouraging runtime key-based access. The keyed `app.GetConfig[T]` stays composition-root-only (no `Context.App()`), so typed config via DI remains the path into business code |
| `MustLoad` (panic on load) | `config.Load` performs I/O; its error must be handled. `MustGet`/`MustGetConfig` panic only when decoding an already-loaded config |
| User-facing CoreConfig type | Server config is a framework-internal concern — user should not be forced to embed it |
| Root `WithConfigFiles` option | Duplicates `config.Load(config.WithFiles(...))` and expands root API surface |
| `WithoutAutoConfig` option | Weak practical need; `WithRawConfig` covers explicit control without adding another mode |
| Put Config in Infra | Each service requires different config sections, a single type is not enough |
| ASP.NET `IOptions<T>` pattern | Extra abstraction layer, simple DI is sufficient in Go |
| Global config instance | Makes multi-instance tests harder, independent Apps conflict |
| Invalid config → panic | Config errors are expected scenarios, returning error is Go-idiomatic |

## Consequences

**Positive:**

- Config access is end-to-end type-safe (struct → DI → constructor)
- String key appears only at the module boundary, in a single place
- RawConfig is minimal (2 methods) — abuse is impossible
- Unmarshal supports primitives — no need for wrapper structs
- Invalid config returns error — assertable in tests
- No global instance — multi-instance, independent Apps are safe
- Server config is internal — user doesn't know CoreConfig, clean DX
- `credo.New()` auto-loads config — zero-config works out of the box
- `credo.New(WithRawConfig(rawCfg))` for explicit control — no risk of forgetting

**Negative:**

- DI registration is required for each config section (minimal boilerplate)
- RawConfig still exists — may appear as two APIs (mitigate: position as "bootstrap only," document accordingly)
- Module authors must know the Unmarshal + DI registration pattern
- Unmarshal primitive support requires mapstructure + reflect switch (simple but requires care)

## Dotenv Path Resolution

**`WithDotenvPath(path)` option added**: programmatic override for the `.env` file path. Resolution order: `WithDotenvPath` > `CREDO_ENV_FILE` env var > default `".env"`. The single-pass loader reads the resolved file once, so `CREDO_ENV` is also picked up from the custom path.

**`WithDotenvOptional()` option added**: downgrades a missing explicit `.env` file (via `WithDotenvPath` or `CREDO_ENV_FILE`) from an error to a warning. The default implicit `".env"` is always optional regardless of this setting.

**Rationale**: binary-relative deployments couldn't rely on cwd to find `.env`. The `CREDO_ENV_FILE` env var worked but was hidden knowledge. `WithDotenvPath` makes discoverability explicit. `WithDotenvOptional` addresses the strict fail-on-missing behavior that required callers to pre-check file existence before loading config.

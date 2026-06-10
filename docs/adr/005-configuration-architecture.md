# ADR-005: Configuration Architecture

**Status:** Accepted
**Date:** 2026-03-01
**Depends on:** ADR-004
**Spec:** [specs/config.md](../specs/config.md)

## Context

Credo's config system must address two fundamental needs:

1. **Framework bootstrap**: The framework's own needs such as server port,
   timeout, and observability settings.
2. **Application config**: Application-specific settings such as database
   connection, API keys, and business logic parameters.

A naive approach (string key-based API like `GetString("db.host")`,
`GetInt("server.port")`) creates the following problems:

- **Type-unsafe**: `Get("key") any` → caller is forced to type-assert
- **Error-swallowing**: `GetString("key")` returns zero value if not found,
  error silently lost — conflicts with the "errors are values" principle
- **Two parallel APIs**: Offering struct decode (LoadInto) + key-based
  access (Get) together increases cognitive load, developers get directed
  to the wrong path
- **Runtime key lookup**: Config is immutable and loaded at startup. There
  is almost no need for dynamic key lookup at runtime (plugin/extension
  system and feature flags are not in current scope)

## Decision

### Config = Typed Snapshot via DI

Config is positioned as a "snapshot produced at startup," not as a
"globally accessed service." `config.Load()` produces a `RawConfig`,
modules call `Unmarshal` once at the boundary to create typed structs,
then register them in DI. Business code receives typed config via
constructors — string keys never appear beyond the module boundary.

Use of string keys in business code is documented as an **anti-pattern**.

### RawConfig: 2-Method Interface

`RawConfig` is limited to `Unmarshal(key, &dst) error` + `Exists(key) bool`.
No typed getters, no `Get(key) any`. `Unmarshal` supports both structs and
primitives, following the `encoding/json.Unmarshal` precedent.

### config.Load() Returns Error, Not Panic

`config.Load()` performs I/O and returns `(credo.RawConfig, error)`. No
package-global instance — each call produces an independent `RawConfig`.
Invalid config (parse error, validation) returns error, never panics.

### credo.New() Auto-Loads and Registers RawConfig

`credo.New()` automatically loads configuration via `config.Load()` when no
explicit `RawConfig` is provided. Use `credo.WithRawConfig(store)` to pass
a pre-loaded config. `RawConfig` is always registered in the DI container.
There is no `app.Config()` accessor.

### RawConfig Defined in config/ Package

The `RawConfig` interface is defined in the `config/` package to avoid
circular imports (config/ needs to return it, root needs to accept it).
The root package re-exports it as `credo.RawConfig` via a type alias.

### Cascade Replaces First-Found-Wins

Config file loading uses cascade merge: all found base files are merged
(later files override earlier for overlapping keys). When `CREDO_ENV` is set,
env-specific files (`config.{env}.*`) are merged on top. This enables
environment-based overrides without code changes.

### Config Is Not in Infra

Config is not included in the `credo.Infra` struct (ADR-004). Rationale:
- Config may require different sections for each service (`*OrderConfig`
  vs `*DatabaseConfig`)
- Config is an immutable startup-time snapshot; Logger/Metrics/Tracer are
  runtime infrastructure
- Passing typed config explicitly via DI is more type-safe

### Rejected Alternatives

| Alternative | Reason for rejection |
|-------------|---------------------|
| Typed getters (GetString/GetInt/GetBool) | Error-swallowing API, returns zero value, facilitates abuse |
| `Get(key) any` | Type-unsafe, `.(int)` assertion risk, Unmarshal is sufficient |
| `app.Config()` accessor | Encourages runtime key-based access, typed config via DI should be the only path |
| User-facing CoreConfig type | Server config is a framework-internal concern — user should not be forced to embed it |
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
- `credo.New(WithRawConfig(store))` for explicit control — no risk of forgetting

**Negative:**
- DI registration is required for each config section (minimal boilerplate)
- RawConfig still exists — may appear as two APIs (mitigate: position as
  "bootstrap only," document accordingly)
- Module authors must know the Unmarshal + DI registration pattern
- Unmarshal primitive support requires mapstructure + reflect switch (simple
  but requires care)

## Dotenv Path Resolution

**`WithDotenvPath(path)` option added**: programmatic override for the
`.env` file path. Resolution order: `WithDotenvPath` > `CREDO_ENV_FILE`
env var > default `".env"`. The single-pass loader reads the resolved
file once, so `CREDO_ENV` is also picked up from the custom path.

**`WithDotenvOptional()` option added**: downgrades a missing explicit
`.env` file (via `WithDotenvPath` or `CREDO_ENV_FILE`) from an error to
a warning. The default implicit `".env"` is always optional regardless
of this setting.

**Rationale**: binary-relative deployments couldn't rely on cwd to find
`.env`. The `CREDO_ENV_FILE` env var worked but was hidden knowledge.
`WithDotenvPath` makes discoverability explicit. `WithDotenvOptional`
addresses the strict fail-on-missing behavior that required callers to
pre-check file existence before loading config.

# Configuration Spec

**Status**: Approved **Package**: `config/` **Sources**: koanf (MIT) **Depends on**: — **ADRs**: [005-configuration-architecture](../adr/005-configuration-architecture.md) **Roadmap**: [`TODO.md` Phase 1.5](../../TODO.md)

---

## Canonical Source

Implementation-level details for Credo configuration are defined in this file. Other documents should keep only high-level references and link here.

---

## Overview

The `config/` package provides struct-centric configuration loading with a single-pass loader (map-merge utilities adapted from koanf). It exposes a high-level Credo API focused on type safety and developer ergonomics.

The primary pattern is:

1. **`credo.New()`** — auto-loads config via `config.Load()`, or use `credo.WithRawConfig(rc)` for explicit control
2. **`rc.Unmarshal("section", &typed)`** — typed config at module boundary
3. **`app.ProvideValue(&typed)`** — register typed config in DI

Business code accesses config as typed structs via DI. String keys in business code are an explicit anti-pattern.

---

## Goals

1. **Struct-Centric**: Encourage users to define their own config structs for compile-time safety. Typed config via DI is the primary access pattern.
2. **Deterministic precedence**: Later sources always override earlier ones.
3. **Zero-Config Local DX**: Automatic file discovery and silent `.env` ignore if missing.
4. **Explicit Production Intent**: Surface errors if a custom `CREDO_ENV_FILE` is specified but missing.
5. **Tagless by Default**: `MapFieldName` auto-converts PascalCase field names to snake_case, so config structs need no tags for standard names. The `credo` struct tag is an escape hatch for non-standard mappings — used only when the desired key differs from the field's snake_case name.
6. **Source-appropriate normalization**: `.env` files and process env vars share the same key normalization (lowercase + `__` → `.`), but `.env` files do not require a prefix — they are project-scoped and need no namespace isolation. Process env vars are prefix-filtered to avoid collisions with system variables.

---

## Source Precedence & Discovery

Configuration merges in this order (lowest to highest priority):

1. **Base config files**: All found among `config.json`, `config.yaml`, `config.yml` are loaded and merged in order (later files override earlier ones for overlapping keys). Users can override the candidate list via `WithFiles()`.
2. **Env-specific config files**: When `CREDO_ENV` is set (e.g., `production`), env-specific files are loaded and merged on top of base files. This applies in both discovery mode and explicit mode (`WithFiles()`).
   - **Discovery mode**: fixed pattern `config.{env}.json`, `config.{env}.yaml`, `config.{env}.yml`.
   - **Explicit mode**: derived from each specified file by inserting `.{env}` before the extension (e.g., `myapp.yaml` → `myapp.production.yaml`). Derived files are optional — missing files are silently skipped.
   - `CREDO_ENV` can be set via process environment variable or in the `.env` file. Process env takes precedence.
3. **.env file**: Resolved via `CREDO_ENV_FILE` or default `.env`. All entries are loaded (no prefix filtering). Keys are normalized using the same lowercase + `__` → `.` pipeline (see [Key Model & Env Normalization](#key-model--env-normalization)).
4. **Process environment variables**: Prefixed (default `CREDO_*`, overridable via `WithPrefix()`). Only variables matching the active prefix are loaded. Bootstrap environment variables (`CREDO_ENV_FILE`, `CREDO_ENV`) are always excluded from the merged configuration.

Later sources override earlier sources on key conflicts.

### Config File Discovery — Cascade Merge Semantics

The default discovery order is **`config.json`** → `config.yaml` → `config.yml`. All found files are loaded and merged (later files override earlier ones for overlapping keys; non-overlapping keys are preserved).

```text
config.json              ← base layer (loaded first)
config.yaml / config.yml ← merged on top (overrides overlapping keys)
config.{env}.json        ← env-specific (only when CREDO_ENV is set)
config.{env}.yaml/.yml   ← env-specific (merged on top)
```

When `CREDO_ENV` is set (e.g., `CREDO_ENV=production`), env-specific files are automatically derived and merged after base files. This allows environment-based overrides without code changes.

To load specific files (bypassing discovery), use `WithFiles("path/to/myconfig.json")`. Env-specific derivation still applies when `CREDO_ENV` is set — each listed file derives a `name.{env}.ext` overlay.

---

## .env Resolution Policy

- **Case 1: `CREDO_ENV_FILE` is set**: Credo attempts to load the file from the specified path. If the file is missing, `config.Load` returns an error (explicit intent).
- **Case 2: `CREDO_ENV_FILE` is NOT set**: Credo attempts to load `.env` from the current working directory. If missing, it is silently ignored (zero-config local DX).
- **Bootstrap key stability**: `CREDO_ENV_FILE` and `CREDO_ENV` are fixed bootstrap key names. They are intentionally not affected by `WithPrefix()` — bootstrap behavior is Credo's own concern, not the application's. They control loading behavior only and are never merged into the config tree.
- **Single-pass `.env` read**: The `.env` file is read and parsed exactly once per `Load`. `CREDO_ENV` is taken from the parsed pairs _before_ config files load (enabling `.env`-based environment selection), while the pairs themselves are merged into the config tree _after_ config files — so the precedence chain is unchanged. Process env var `CREDO_ENV` always takes precedence over the `.env` value. Because the read happens up front, `.env` errors (missing explicit file, parse failure) surface before config-file errors.

### .env Prefix Policy

`.env` files are **not prefix-filtered**. All entries are loaded and normalized (lowercase + `__` → `.`). This differs from process env vars, which require the configured prefix (default `CREDO_`).

```bash
# .env — all entries are loaded
SERVER__PORT=8080
DB__DSN=postgres://localhost/mydb
DEBUG=true
```

The equivalent process env vars require the prefix:

```bash
export CREDO_SERVER__PORT=8080
export CREDO_DB__DSN=postgres://localhost/mydb
export CREDO_DEBUG=true
```

> **Rationale**: `.env` files are project-scoped — they live in the project root and are not shared with other processes. Namespace isolation via prefix is unnecessary and adds verbosity. Process env vars, by contrast, share a global namespace with the OS and other tools, making prefix filtering essential. This matches the convention used by virtually all frameworks (Laravel, Django, Express, GoFr).

---

## API

### config.Load() — Primary Entry Point

```go
rawCfg, err := config.Load(opts...) // returns (credo.RawConfig, error)
```

- Loads all sources (files, `.env`, env vars) and merges them.
- Returns `credo.RawConfig` — the sole mechanism for accessing loaded config.
- Returns error on I/O failure, parse error, or invalid config. Never panics.
- No package-global instance — each call produces an independent `RawConfig`.
- **Options**: `WithFiles(paths...)`, `WithPrefix(prefix)`, `WithDotenvPath(path)`, `WithDotenvOptional()`. `.env` file path resolution: `WithDotenvPath` > `CREDO_ENV_FILE` env var > default `".env"`. A missing explicit path is an error unless `WithDotenvOptional()` is set (downgrades to a warning).

### config.RawConfig Interface

Defined in the `config/` package and re-exported from the root as `credo.RawConfig` (type alias). This is the **only** config access interface:

```go
// root package (credo)
type RawConfig interface {
    Unmarshal(key string, dst any) error
    Exists(key string) bool
}
```

Limited to 2 methods by design — no typed getters, no `Get(key) any`. For design rationale and rejected alternatives, see [ADR-005](../adr/005-configuration-architecture.md).

Behavior contract:

- `Unmarshal(key, &dst)` decodes a config sub-tree into `dst`; returns error if the key is missing or decoding fails.
- `Exists(key)` checks both leaf and intermediate keys.
- Empty key `""` represents the root of the config tree.

### credo.New() Config Integration

`credo.New()` automatically loads configuration via `config.Load()` when no explicit `RawConfig` is provided. Use `credo.WithRawConfig(rawCfg)` to pass a pre-loaded config (e.g., from `config.LoadBytes()` with embedded data). Passing `WithRawConfig` bypasses auto-load entirely; the provided `RawConfig` is registered in the DI container as-is. Server config is framework-internal (no user-facing `CoreConfig`). No `app.Config()` accessor — typed config via DI only. See [ADR-005](../adr/005-configuration-architecture.md#credonew-auto-loads-and-registers-rawconfig).

Root `credo.New` intentionally does not expose `WithConfigFiles` or `WithoutAutoConfig` options. File selection belongs to `config.Load` (`config.WithFiles`, `config.WithDotenvPath`, etc.). Explicit applications load config first, then pass it with `credo.WithRawConfig`.

Framework-read server keys include listen settings, debug mode, routing behavior, and `server.trusted_proxies` for reverse-proxy metadata trust. `credo.WithTrustedProxies(...)` is the explicit option form and overrides the config value when both are present.

### config.LoadBytes() — Embedded Config

```go
rc, err := config.LoadBytes(data, config.FormatJSON, opts...)
```

Creates a Config from raw bytes. After parsing, `.env` and env var layers are applied on top (same precedence as `Load`). Useful with `go:embed`.

### Typed Config via DI — Primary Pattern

String keys appear **once** at the module boundary. Beyond this point, everything is typed:

```
RawConfig ──Unmarshal──→ *DatabaseConfig ──DI──→ Service
                ↑                                   ↑
          string key (1x)                  typed (compile-time)
```

For code examples, see [ADR-005 — Config = Typed Snapshot via DI](../adr/005-configuration-architecture.md#config--typed-snapshot-via-di) and [Configuration Guide — Typed Config + DI](../guides/configuration.md#typed-config--di).

---

## Key Model & Env Normalization

- **`MapFieldName` (auto-conversion, the default)**: The decoder converts PascalCase struct field names to snake_case before looking up config keys, so **struct tags are optional**. Field names alone resolve to the right keys:

  | Field Name    | Auto Key       |
  | ------------- | -------------- |
  | `MaxOpen`     | `max_open`     |
  | `SSLMode`     | `ssl_mode`     |
  | `APIKey`      | `api_key`      |
  | `ReadTimeout` | `read_timeout` |
  | `Port`        | `port`         |

- **Struct Tags (escape hatch)**: Add a `credo` tag only when the desired key **differs** from the field's snake_case name — for example, remapping to a vendor or legacy key, or a nested path. Explicit tags always take precedence over `MapFieldName`.

  ```go
  type AppConfig struct {
      Port   int    // auto → "port"
      Debug  bool   // auto → "debug"
      APIKey string // auto → "api_key"

      Region string `credo:"aws_region"` // tag → key "aws_region", not "region"
  }
  ```

- **Env Prefix**: Defaults to `CREDO_`, but can be changed via `WithPrefix()` option. Applies to **process env vars only** — `.env` files are not prefix-filtered.
- **Env Normalization Rules**:
  - **Process env vars**: Strip prefix (e.g., `CREDO_`) → lowercase → `__` → `.`
  - **.env entries**: Lowercase → `__` → `.` (no prefix stripping)
  - **Delimiters**:
    - Double underscore (`__`) → Nested dot (`.`)
    - Single underscore (`_`) → Stays as `_` inside a segment (never treated as nesting)
  - **Process env example**: `CREDO_SERVER__READ_TIMEOUT` → `server.read_timeout`
  - **.env example**: `SERVER__READ_TIMEOUT` → `server.read_timeout`
  - Both sources map to the same config keys; only the prefix handling differs.
- **Dotted Keys**: Internal config tree uses dotted notation for nested lookups.
- **Map Key Constraint**: Map keys in `map[string]T` fields must **not** contain double underscores (`__`), as `__` is the nesting delimiter. A key like `my__db` would be misinterpreted as two nesting levels instead of a single map key. Use `_` or `-` instead (e.g., `my_db`, `read-replica`).

---

## Default Values

`config.Load()` merges configuration sources into the internal config tree. `Unmarshal` merges into the provided struct, preserving fields not present in any source. This enables a clean default values pattern:

```go
// 1. Define a factory that returns sensible defaults
func DefaultDatabaseConfig() DatabaseConfig {
    return DatabaseConfig{
        Host:        "localhost",
        Port:        5432,
        MaxOpenConns: 25,
    }
}

// 2. Pre-initialize, then unmarshal — sources override only the keys they provide
rc, err := config.Load()
if err != nil {
    log.Fatal(err)
}

dbCfg := DefaultDatabaseConfig()
rc.Unmarshal("databases.default", &dbCfg)
// dbCfg.Port is 5432 unless config file, .env, or env var overrides it
```

**How it works**: `mapstructure.Decode()` modifies the target struct in-place, setting only the fields that exist in the input map. Fields not in any source keep their pre-initialized values. An explicit zero value in a source (e.g., `port: 0` in YAML) **does** override the default — this is correct behavior (explicit intent).

**Recommendation**: Always define a `Default*Config()` factory for production applications. This makes defaults visible, testable, and independent of config file presence — critical for env-var-only deployments (containers, serverless).

---

## Validatable Integration

If the struct passed to `Unmarshal` implements the following interface, `Validate()` is called automatically after the struct is populated:

```go
interface {
    Validate() error
}
```

A non-nil error from `Validate()` is wrapped and returned by `Unmarshal`:

```go
type DatabaseConfig struct {
    Host string
    Port int
}

func (c *DatabaseConfig) Validate() error {
    if c.Host == "" {
        return errors.New("databases.default.host is required")
    }
    return nil
}

// Unmarshal automatically calls Validate()
var dbCfg DatabaseConfig
if err := rc.Unmarshal("databases.default", &dbCfg); err != nil {
    log.Fatal(err) // includes validation error
}
```

The `config` package does not import `credo/validation`. The interface is checked via a local inline type assertion, keeping the dependency graph clean.

---

## Map[string]T Support (Dynamic Keys)

Fields of type `map[string]T` are fully supported from all sources — config files, `.env`, and process env vars. The `__` delimiter creates nesting in the internal config tree, and map keys are resolved at the unmarshal step via mapstructure.

**How it works**: Env vars like `CREDO_DATABASES__DEFAULT__HOST` are normalized to dotted keys (`databases.default.host`), unflattened into a nested `map[string]any`, and finally unmarshaled into `map[string]T` by mapstructure.

> **Map key constraint**: Map keys must not contain `__`, as it is the nesting delimiter. Use `_` or `-` instead (e.g., `read_replica`, `read-replica`).

For practical examples (JSON config, env var overrides, Go struct), see [Configuration Guide — Multi-Database Config](../guides/configuration.md#multi-database-config).

---

## Single-Pass Loader Model

There are no provider/parser interfaces: sources are internal functions that each produce a `map[string]any`, merged in precedence order into one nested map held by `Config`.

- **Config files** — `os.ReadFile` + format dispatch by extension (`encoding/json` / `gopkg.in/yaml.v3`); the same parser backs `LoadBytes`.
- **`.env` file** — Credo's own line parser (`parseDotenv`), read once per `Load`; entries normalized (lowercase, `__` → `.`) and unflattened.
- **Process env vars** — prefix-filtered, normalized the same way.

Key lookup walks the nested map directly (`lookup`); there is no flattened key index. Dots in keys always act as path separators.

---

## Error Handling Contract

- **Config File Error**: I/O or syntax error in JSON/YAML (only if found).
- **Explicit .env Error**: `CREDO_ENV_FILE` points to a missing/unreadable file.
- **Type Conversion**: Failing to map a string env var to a struct's `int` field.
- **Validation Error**: `dst.Validate()` returns a non-nil error after unmarshalling.
- All errors are returned, never panicked. Invalid config from `config.Load()` returns error.

---

## Koanf Adaptation Scope

Credo forks koanf and trims aggressively. Only what Credo needs is kept; everything else is deleted at copy time. This keeps the dependency graph clean and the codebase small.

### What We Keep

| koanf source | Credo file | Notes |
| --- | --- | --- |
| `maps/maps.go` (partial) | `config/maps.go` | `unflatten`, `mergeMaps`, `copyMap`, `intfaceKeysToStrings`; `lookup` replaces flatten-based key index |

The provider/parser architecture, the byte/map provider interfaces, and the per-format parser wrappers were initially adapted and later removed when the loader became single-pass (no external use, ~400 lines). Env/dotenv reading is now a Credo-written internal function set in `load.go`/`dotenv.go`.

### What We Cut

| koanf source | Reason |
| --- | --- |
| `getters.go` (~15KB, ~40 functions) | Pre-generics API; `Unmarshal` with primitive support replaces all typed getters |
| `koanf.Conf.StrictMerge` | Credo always uses loose merge; complexity not needed |
| `koanf.NewWithConf()` | `New()` with options is sufficient; `Conf` becomes internal |
| `koanf.Sprint()`, `Print()` | Debug utilities, not part of Credo's public API |
| `koanf.KeyMap()` (public) | Internal use only |
| `koanf.All()` | `Raw()` is sufficient |
| `koanf.Delete()`, `koanf.Set()` | No hot-reload, no runtime mutation |
| `koanf.Slices()` | Edge case not in spec |
| `maps.MergeStrict()` | Cut with `StrictMerge` |
| `providers/consul`, `etcd`, `vault`, etc. | Remote/cloud config — out of scope |
| `providers/basicflag`, `posflag`, `cliflag*` | CLI flag parsing — not Credo's concern |
| `providers/confmap`, `structs`, `rawbytes` | Redundant given Credo's `config.Load()` API |
| `parsers/toml`, `hcl`, `hjson`, etc. | Only JSON and YAML are supported |
| `providers/fs/` | `embed.FS` replaced by `config.LoadBytes()` |

---

## File Layout

```text
config/
├── doc.go
├── config.go   ← RawConfig, Config, Option types, Unmarshal/Exists, decoder
├── load.go     ← Load()/LoadBytes(), file/.env/env source loading and merging
├── dotenv.go   ← .env line parser (parseDotenv)
├── maps.go     ← internal maps helpers (lookup, unflatten, merge, copy)
└── *_test.go
```

---

## Test Requirements

- Unit tests for each source merger (`mergeEnv`, `mergeDotenv`, `parseDotenv`)
- Parser tests for JSON and YAML inputs (`parseConfig`)
- `Unmarshal` tests for nested structures and primitives
- Integration tests with temp files and controlled env vars
- Precedence tests proving `env > .env > file`
- `.env` loads all entries (no prefix filtering)
- Cascade merge: multiple config files loaded and merged correctly
- Env-specific files loaded when CREDO_ENV is set
- `MapFieldName`: fields without `credo` tag use auto-converted snake_case name
- `Validatable` integration: `Validate()` is called and its error is propagated
- `config.Load()` returns independent `RawConfig` instances (no shared state)
- `Unmarshal` with empty key `""` returns full config tree
- `Unmarshal` with primitive types (`int`, `string`, `bool`) works correctly
- `Exists` returns correct values for leaf and intermediate keys

---

## Cross-Document Alignment

This spec defines configuration **mechanisms and rationale**. The [Configuration Guide](../guides/configuration.md) owns usage examples and quick-reference tables.

Related documents:

- [ADR-005](../adr/005-configuration-architecture.md) — config architecture decision
- [ADR-004](../adr/004-dependency-injection-and-infra.md) — config's relationship with DI
- `TODO.md` — task tracker

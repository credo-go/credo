# Configuration Spec

**Status:** Implemented (beta) **Package:** `config/` **ADR:** [ADR-005](../adr/005-configuration-architecture.md), [ADR-020](../adr/020-reload-and-partial-config-reload.md) **Guide:** [Configuration guide](../guides/configuration.md) **Sources:** koanf (MIT)

This spec owns the `config` package contract — sources and precedence, `.env` resolution, decoding, the typed getters, the store side of reload — and the root's config integration (`credo.New`, `WithRawConfig`, `GetConfig`). The App-level reload orchestration is in the [Lifecycle Spec](lifecycle.md#appreloadctx-contextcontext-error); usage examples and the config key reference are in the [Configuration Guide](../guides/configuration.md).

## Overview

The `config/` package provides struct-centric configuration loading with a single-pass loader (map-merge utilities adapted from koanf). It exposes a high-level Credo API focused on type safety and developer ergonomics.

The primary pattern is:

1. **`credo.New()`** — auto-loads config via `config.Load()`, or use `credo.WithRawConfig(rc)` for explicit control
2. **`rc.Unmarshal("section", &typed)`** — typed config at module boundary
3. **`app.ProvideValue(&typed)`** — register typed config in DI

String keys appear **once**, at the module boundary; business code accesses config as typed structs via DI, and string keys in business code are an explicit anti-pattern. Examples: [Configuration Guide — Typed Config + DI](../guides/configuration.md#typed-config--di).

```
RawConfig ──Unmarshal──→ *DatabaseConfig ──DI──→ Service
                ↑                                   ↑
          string key (1x)                  typed (compile-time)
```

## Goals

1. **Typed access**: applications define their own config structs and receive them through DI.
2. **Deterministic precedence**: Later sources always override earlier ones.
3. **Zero-config locally, fail-loud for explicit intent**: discovered files, env-specific overlays and the default `.env` are optional; an explicit `WithFiles` list none of whose files exists, or an explicitly named `.env` path that is missing (unless `WithDotenvOptional()`), is an error.

## Source Precedence & Discovery

Configuration merges in this order (lowest to highest priority):

1. **Base config files**: All found among `config.json`, `config.yaml`, `config.yml` are loaded and merged in order (later files override earlier ones for overlapping keys). `WithFiles(paths...)` replaces this candidate list with an explicit one: at least one listed file must exist or `Load` fails, and `WithFiles()` with no arguments disables file loading.
2. **Env-specific config files**: When `CREDO_ENV` is set (e.g., `production`), env-specific files are loaded and merged on top of base files. This applies in both discovery mode and explicit mode (`WithFiles()`).
   - **Discovery mode**: fixed pattern `config.{env}.json`, `config.{env}.yaml`, `config.{env}.yml`.
   - **Explicit mode**: derived from each specified file by inserting `.{env}` before the extension (e.g., `myapp.yaml` → `myapp.production.yaml`). Derived files are optional — missing files are silently skipped.
   - `CREDO_ENV` can be set via process environment variable or in the `.env` file. Process env takes precedence, unless `WithoutProcessEnv()` is set.
3. **.env file**: Resolved per the [.env Resolution Policy](#env-resolution-policy). All entries are loaded without prefix filtering ([.env Prefix Policy](#env-prefix-policy)); keys are normalized using the same lowercase + `__` → `.` pipeline (see [Key Model & Env Normalization](#key-model--env-normalization)). Disabled entirely by `WithoutDotenv()`.
4. **Process environment variables**: Prefixed (default `CREDO_*`, overridable via `WithPrefix()`). Only variables matching the active prefix are loaded. Bootstrap environment variables (`CREDO_ENV_FILE`, `CREDO_ENV`) are always excluded from the merged configuration. Disabled entirely by `WithoutProcessEnv()`.

Later sources override earlier sources on key conflicts.

### Source Opt-Outs — Hermetic Loading

`WithoutProcessEnv()` and `WithoutDotenv()` disable sources 4 and 3 respectively. Each opt-out kills its **whole source, bootstrap keys included** — not just the merge layer:

| Option | Merge layer | Bootstrap keys | Still works |
| --- | --- | --- | --- |
| `WithoutProcessEnv()` | No env vars merged (regardless of `WithPrefix`) | Process-env `CREDO_ENV` ignored; `CREDO_ENV_FILE` not consulted | `WithDotenvPath` (programmatic); a `.env`-sourced `CREDO_ENV` still drives env-specific file derivation |
| `WithoutDotenv()` | No `.env` entries merged | No `.env` file read at all, so no `.env`-sourced `CREDO_ENV` | Process-env `CREDO_ENV` still drives env-specific file derivation |

Combining both with `WithFiles(...)` (or `LoadBytes`) yields **fully hermetic loading**: only the listed files (or the embedded document) are read, and `Reload`/`Stage` replay the same opt-outs — a disabled source can never leak in at reload time. An empty `WithPrefix("")` is not an opt-out: it removes the filter and merges every process environment variable.

`WithoutDotenv()` combined with an explicit `WithDotenvPath(...)` is contradictory and fails loud: `Load`/`LoadBytes` return an error. `WithoutDotenv()` + `WithDotenvOptional()` is merely redundant and is allowed.

### Config File Discovery — Cascade Merge Semantics

The default discovery order is **`config.json`** → `config.yaml` → `config.yml`. All found files are loaded and merged (later files override earlier ones for overlapping keys; non-overlapping keys are preserved).

```text
config.json              ← base layer (loaded first)
config.yaml / config.yml ← merged on top (overrides overlapping keys)
config.{env}.json        ← env-specific (only when CREDO_ENV is set)
config.{env}.yaml/.yml   ← env-specific (merged on top)
```

`WithFiles(...)` bypasses discovery; explicit files and their env-specific derivation follow items 1–2 above.

## .env Resolution Policy

The `.env` path resolves in this order; the first rule that applies wins:

1. **`WithoutDotenv()`**: no `.env` file is read at all — `CREDO_ENV_FILE` is not consulted and the default `.env` is not probed.
2. **`WithDotenvPath(path)`**: the explicit path.
3. **`CREDO_ENV_FILE`** (process environment): the explicit path. Not consulted under `WithoutProcessEnv()`.
4. **Default**: `.env` in the current working directory. If missing, it is silently ignored (zero-config local DX).

- **Missing explicit file**: when rule 2 or 3 names a file that does not exist, `Load`/`LoadBytes` return an error (explicit intent), unless `WithDotenvOptional()` is set, which logs a Warn (through `WithLogger`) and continues without `.env`. An unreadable or unparsable `.env` is always an error.
- **Bootstrap key stability**: `CREDO_ENV_FILE` and `CREDO_ENV` are fixed bootstrap key names. They are intentionally not affected by `WithPrefix()` — bootstrap behavior is Credo's own concern, not the application's. They control loading behavior only and are never merged into the config tree.
- **Single-pass `.env` read**: The `.env` file is read and parsed exactly once per `Load`. `CREDO_ENV` is taken from the parsed pairs _before_ config files load (enabling `.env`-based environment selection), while the pairs themselves are merged into the config tree _after_ config files — so the precedence chain is unchanged. Because the read happens up front, `.env` errors (missing explicit file, parse failure) surface before config-file errors.

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

Rationale: [ADR-005](../adr/005-configuration-architecture.md#env-entries-are-not-prefix-filtered).

## API

### config.Load() — Primary Entry Point

```go
cfg, err := config.Load(opts...) // returns (*config.Config, error)
```

- Loads all sources (files, `.env`, env vars) and merges them.
- Returns `*config.Config` — the concrete type, which satisfies `credo.RawConfig`. Use `cfg.Get[T](key)` for typed snapshot access, `cfg.Unmarshal(key, &dst)` for the low-level form, or pass `cfg` straight to `credo.WithRawConfig`.
- Returns an error for an I/O failure, a parse error, conflicting options, an explicit file list none of whose files exists, a missing explicit `.env` file, or an unsupported file format ([Error Handling Contract](#error-handling-contract)). Never panics. A section's `Validate()` error is returned by `Unmarshal`/`Get`, not by `Load`. There is no `MustLoad`.
- No package-global instance — each call produces an independent `Config`.
- **Options**: `WithFiles(paths...)`, `WithPrefix(prefix)`, `WithDotenvPath(path)`, `WithDotenvOptional()`, `WithoutProcessEnv()`, `WithoutDotenv()`, `WithStrictDecoding()`, `WithLogger(*slog.Logger)` (load-time warnings such as a missing optional explicit `.env`; default `slog.Default()`). `.env` path rules: [.env Resolution Policy](#env-resolution-policy).

### config.RawConfig Interface

Defined in the `config/` package and re-exported from the root as `credo.RawConfig` (type alias). This is the **only** config access interface:

```go
// root package (credo)
type RawConfig interface {
    Unmarshal(key string, dst any) error
    Exists(key string) bool
}
```

Limited to 2 methods — no scalar getters (`GetString`/`GetInt`/`GetBool`), no `Get(key) any`. The typed-snapshot getter `Get[T]`/`GetConfig[T]` (below) is a generic method on the concrete `*config.Config` and `*App`, not on this interface. Rationale and rejected alternatives: [ADR-005](../adr/005-configuration-architecture.md).

Behavior contract:

- `Unmarshal(key, &dst)` decodes a config sub-tree into `dst` (a pointer to a struct, map, slice, or primitive); returns error if the key is missing or decoding fails. The empty key `""` decodes the whole tree and returns an error when the configuration is empty. A key present with a null value leaves `dst` unchanged and returns no error.
- `Exists(key)` checks both leaf and intermediate keys, and reports a key whose value is null as present. `Exists("")` is false.

### Reload — Partial Reload (ADR-020)

```go
type Reloader interface { Reload() (Changes, error) }
type Stager interface { Stage() (Staged, error) }
type Staged interface { RawConfig; Changes() Changes; Commit() }

func (c *Config) Reload() (Changes, error)   // Stage + Commit
func (c *Config) Stage() (Staged, error)
func (c Changes) Affects(prefix string) bool  // a changed key equals prefix or lies under "prefix."; "" matches any change
func (c Changes) Keys() []string              // sorted copy of the changed leaf key paths
func (c Changes) Empty() bool                 // nothing changed
```

`*Config` implements both `Reloader` and `Stager`. `Stage` re-runs the load pipeline with the options captured by the original `Load` — the same files (or the same `LoadBytes` document), prefix, `.env` path and source opt-outs, and the environment name resolved at first load (`CREDO_ENV` is not re-derived; switching environments is a restart) — into a candidate returned as a `Staged` handle: a `RawConfig` over the candidate, `Changes()` — the sorted symmetric difference of leaf key paths against the snapshot current at stage time (values are never retained) — and `Commit()`, which swaps the internal snapshot atomically so that concurrent `Unmarshal`/`Exists`/`Get` observe the old or the new tree, never a mix. An uncommitted candidate is discarded. `Reload` is `Stage` followed by `Commit`. Any load error leaves the current snapshot untouched and is returned. `RawConfig` stays a two-method interface; a custom store opts into reload by implementing `Reloader` (one-shot: atomic for readers, previous snapshot untouched on error) or, to get validate-before-publish, `Stager`. The App-level orchestration — validate-before-publish through typed `OnConfigChange[T]` subscribers, the restart-required warning for unsubscribed keys, file-based TLS rotation — is in the [Lifecycle Spec](lifecycle.md#appreloadctx-contextcontext-error). The process environment is re-read on every stage; what a supervisor hands the process stays fixed until restart ([Deployment Guide](../guides/deployment.md#systemd)).

### credo.New() Config Integration

`credo.New()` automatically loads configuration via `config.Load()` when no explicit `RawConfig` is provided. Use `credo.WithRawConfig(rawCfg)` to pass a pre-loaded config (e.g., from `config.LoadBytes()` with embedded data). Passing `WithRawConfig` bypasses auto-load entirely; the provided `RawConfig` is registered in the DI container as-is. Server config is framework-internal (no user-facing `CoreConfig`). There is no whole-config `app.Config()` accessor; the keyed `app.GetConfig[T](key)` (below) covers composition-root reads, and typed config still flows to business code via DI. See [ADR-005](../adr/005-configuration-architecture.md#credonew-auto-loads-and-registers-rawconfig).

Root `credo.New` intentionally does not expose `WithConfigFiles` or `WithoutAutoConfig` options. File selection belongs to `config.Load` (`config.WithFiles`, `config.WithDotenvPath`, etc.). Explicit applications load config first, then pass it with `credo.WithRawConfig`.

The framework reads its own settings from the `server` section when present (listen address, timeouts, header and body limits, trailing-slash redirect, debug mode, strict bodies, trusted proxies, TLS files); the key list with types, defaults and reloadability is in the [Configuration Guide](../guides/configuration.md#server--server). An explicit `With*` option replaces its key: `WithAddr` (host and port), `WithShutdownTimeout`, `WithReloadTimeout`, `WithMaxBodyBytes`, `WithRedirectTrailingSlash`, `WithTrustedProxies`, and `WithTLSFiles` (cert and key as one pair). `WithDebug` and `WithStrictBodies` are OR'd with `server.debug` and `server.strict_bodies`.

### config.LoadBytes() — Embedded Config

```go
rc, err := config.LoadBytes(data, config.FormatJSON, opts...)
```

Creates a Config from one raw document. Useful with `go:embed`.

- `format` is `config.FormatJSON` or `config.FormatYAML` (also `"yml"` or a file extension such as `".yaml"`, case-insensitive); any other format is an error.
- The document is parsed first, so a malformed document fails before any `.env` I/O. The `.env` and process-environment layers then merge on top with the same rules and options as `Load`.
- `CREDO_ENV` selects no env-specific overlay: there is no file to derive one from.
- The bytes are copied: `Reload`/`Stage` re-parse the same document and re-read only `.env` and the process environment.

### Typed Snapshot Getter — Get[T] / GetConfig[T]

A generic, one-call form of `Unmarshal` for reading a typed section at the composition root:

```go
cfg, err := config.Load(config.WithFiles("app.yaml"))
// ...
db, err := cfg.Get[DatabaseConfig]("database")   // (*config.Config).Get[T]
port := cfg.MustGet[int]("server.port")           // MustGet panics on error

// Or straight from the App, over its registered RawConfig:
db, err = app.GetConfig[DatabaseConfig]("database")
db = app.MustGetConfig[DatabaseConfig]("database")
```

| Method | Signature | On error |
| --- | --- | --- |
| `(*config.Config).Get[T]` | `(key string) (T, error)` | returns zero `T` + error |
| `(*config.Config).MustGet[T]` | `(key string) T` | panics |
| `(*App).GetConfig[T]` | `(key string) (T, error)` | returns zero `T` + error |
| `(*App).MustGetConfig[T]` | `(key string) T` | panics |
| `(*App).ConfigExists` | `(key string) bool` | — (mirrors `RawConfig.Exists`; a presence check during registration, before `Resolve` is available) |

- `T` may be a struct, map, slice, or primitive — the same decode rules (weak by default, strict under `WithStrictDecoding`) and `Validate()` hook as `Unmarshal`.
- `App.GetConfig` decodes through the registered `RawConfig.Unmarshal`, so it behaves identically for the auto-loaded `*config.Config` and any custom `WithRawConfig` implementation.
- These are **bootstrap/composition-root** helpers: `*credo.Context` has no `App()` accessor, so handlers and services cannot reach them through the request ([ADR-005](../adr/005-configuration-architecture.md#typed-snapshot-getter-gett--getconfigt)).
- `MustGet`/`MustGetConfig` panic with the error `Get`/`GetConfig` would return.

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

- **Env Prefix**: Defaults to `CREDO_`, but can be changed via `WithPrefix()` option. Applies to **process env vars only** ([.env Prefix Policy](#env-prefix-policy)).
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

## Strict Decoding

`WithStrictDecoding()` switches every typed decode from the Config to strict mode. Weak mode remains the default.

| Aspect | Default (weak) | Strict |
| --- | --- | --- |
| Unknown keys (config keys with no target field, nested included) | Silently ignored | Error naming the key path |
| `"123"` → int, `"true"` → bool, general weak coercion | Allowed | Error |
| Duration strings (`"5s"` → `time.Duration`) | Allowed | Allowed (retained hook) |
| `encoding.TextUnmarshaler` fields | Allowed | Allowed (retained hook) |
| Numeric kind conversions (JSON `float64` → int field) | Allowed | Allowed (native decoding, not weak coercion) |

The policy rides on the Config instance, so one option covers every decode path: `Unmarshal`, `Get`/`MustGet`, the `Staged` candidate from `Stage` (and therefore `OnConfigChange` reload subscriber validation), and the framework's own decode of the `server` section in `credo.New` — an unknown `server.*` key fails app construction under a strict store.

Consequences to design for:

- **Typed sources only**: env vars and `.env` entries are always strings, so string overrides on typed fields fail to decode under strict mode. Pair with `WithoutProcessEnv()`/`WithoutDotenv()`, or stay on weak decoding when env overrides are needed.
- **Complete typed views**: every struct that decodes a section — including narrow `OnConfigChange[T]` subscribers and full-tree decodes — must cover all keys of that section. A full-tree decode of a config containing `server.*` needs a `Server` field on the target.
- **Error hygiene scope**: unknown-key errors report key paths and never values; type-mismatch errors may quote the offending value (mapstructure behavior — accepted limitation).

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

## Validatable Integration

If the struct passed to `Unmarshal` implements the following interface, `Validate()` is called automatically after the struct is populated:

```go
interface {
    Validate() error
}
```

A non-nil error from `Validate()` is returned by `Unmarshal`, wrapped as `config: validation: <err>`:

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

## Map[string]T Support (Dynamic Keys)

Fields of type `map[string]T` are fully supported from all sources — config files, `.env`, and process env vars. The `__` delimiter creates nesting in the internal config tree, and map keys are resolved at the unmarshal step via mapstructure.

**How it works**: Env vars like `CREDO_DATABASES__DEFAULT__HOST` are normalized to dotted keys (`databases.default.host`), unflattened into a nested `map[string]any`, and finally unmarshaled into `map[string]T` by mapstructure.

> **Map key constraint**: Map keys must not contain `__`, as it is the nesting delimiter — a key like `my__db` would be read as two nesting levels. Use `_` or `-` instead (e.g., `read_replica`, `read-replica`).

For practical examples (JSON config, env var overrides, Go struct), see [Configuration Guide — Multi-Database Config](../guides/configuration.md#multi-database-config).

## Single-Pass Loader Model

There are no provider/parser interfaces: sources are internal functions that each produce a `map[string]any`, merged in precedence order into one nested map held by `Config`.

- **Config files** — `os.ReadFile` + format dispatch by extension (`encoding/json/v2` / `go.yaml.in/yaml/v3`); the same parser backs `LoadBytes`. A file holds one value: JSON rejects trailing data and duplicate members; YAML rejects duplicate keys and a second document — a `---` after the first document, an empty one included — while a leading `---` and a closing `...` belong to the one document and an empty or comment-only file is an empty config.
- **`.env` file** — Credo's own line parser (`parseDotenv`), read once per `Load`; entries normalized (lowercase, `__` → `.`) and unflattened.
- **Process env vars** — prefix-filtered, normalized the same way.

Key lookup walks the nested map directly (`lookup`); there is no flattened key index. Dots in keys always act as path separators.

## Error Handling Contract

- **Config File Error**: I/O or syntax error in JSON/YAML (only if found), including a YAML file with more than one document (`yaml: a config file holds one document; found another after the first`).
- **Missing Explicit Config Files**: `WithFiles` lists files of which none exists (`none of the specified config files were found: …`).
- **Unsupported Format**: a config file whose extension, or a `LoadBytes` format, is neither JSON nor YAML (`unsupported format: …`).
- **Conflicting Options**: `WithoutDotenv` + `WithDotenvPath`, rejected before any I/O.
- **Explicit .env Error**: an explicit `.env` path (`WithDotenvPath` or `CREDO_ENV_FILE`) is missing (a Warn instead under `WithDotenvOptional()`) or unreadable, or a `.env` file fails to parse.
- **Key Not Found**: `Unmarshal`/`Get` on a key that does not exist (`config: key "…" not found`).
- **Empty Configuration**: a full-tree `Unmarshal("", &dst)` of an empty configuration (`config: configuration is empty`).
- **Type Conversion**: Failing to map a string env var to a struct's `int` field; under `WithStrictDecoding`, also an unknown key or a weak coercion.
- **Validation Error**: `dst.Validate()` returns a non-nil error after unmarshalling (`config: validation: …`).
- All errors are returned, never panicked; only `MustGet`/`MustGetConfig` panic.
- **Reload Error**: any load error during `Reload()`/`Stage()` is returned and the previous snapshot stays current.

## Redacted Formatting

`*Config` implements `String()`, `GoString()`, and `slog.LogValuer` with a metadata-only description (`config.Config(N keys, values redacted)`): formatting a Config with `%v`/`%s`/`%+v`/`%#v` or logging it as an slog attribute can never leak values or key names. This mirrors the `Changes` doctrine (key paths only, never values, safe to log). The state lives behind one private pointer and the formatting methods are declared on the value type, so a dereferenced copy (`*cfg`) is redacted exactly like the pointer; a nil `*Config` formats as fmt's usual `<nil>`.

## Koanf Adaptation Scope

`config/maps.go` derives from koanf's `maps/maps.go` (`unflatten`, `mergeMaps`, `copyMap`, `intfaceKeysToStrings`; attribution in [NOTICES](../../NOTICES)). Nothing else is adapted:

- **Provider/parser architecture** (`providers/*`, `parsers/*`): the loader is single-pass, and file, `.env` and environment reading is Credo's own (`load.go`, `dotenv.go`). A single embedded document goes through `LoadBytes`; an `fs.FS` source with discovery and env-specific overlays is deferred. Remote (consul, etcd, vault) and CLI-flag providers are out of scope, and only JSON and YAML are supported.
- **Scalar getters, `All()`, `KeyMap()`**: `Unmarshal` and `Get[T]` decode typed values, `Unmarshal("", &dst)` decodes the whole tree, and lookup walks the nested map (no flattened key index).
- **`Set()`/`Delete()`**: no in-place mutation; reload re-loads the whole snapshot ([Reload](#reload--partial-reload-adr-020)).
- **`StrictMerge`/`MergeStrict`**: merges are always loose.

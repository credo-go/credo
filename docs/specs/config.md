# Configuration Spec

**Status:** Implemented (beta) **Package:** `config/` **ADR:** [ADR-005](../adr/005-configuration-architecture.md), [ADR-020](../adr/020-reload-and-partial-config-reload.md) **Guide:** [Configuration guide](../guides/configuration.md) **Sources:** koanf (MIT)

> **Contract.** Every section of this spec is normative — what application code can observe: the sources and their precedence, the decode rules, the descriptors, the reload and its result, and the errors — except [Current Behaviour](#current-behaviour), which describes the loader's mechanisms as they are now: not guaranteed, they may change in any release. The examples run as `Example*` tests in [`config/example_test.go`](../../config/example_test.go), so a drift fails the build.

This spec owns the `config` package contract — sources and precedence, `.env` resolution, decoding, section descriptors, the store side of reload — and the root's config integration (`credo.New`, `WithRawConfig`, `GetConfig`). The App-level reload orchestration is in the [Lifecycle Spec](lifecycle.md#appreloadctx-contextcontext-configreloadresult-error); usage examples and the config key reference are in the [Configuration Guide](../guides/configuration.md).

## Overview

The `config/` package provides struct-centric configuration loading with a single-pass loader (map-merge utilities adapted from koanf). It exposes a high-level Credo API focused on type safety and developer ergonomics.

The primary pattern is:

1. **`credo.New()`** — auto-loads config via `config.Load()`, or use `credo.WithRawConfig(rc)` for explicit control
2. **`var AppSection = config.Section[AppConfig]("app")`** — the section's key and type, named once
3. **`app.Provide(AppSection.Decode)`** — the section bound in DI as a snapshot; `app.Provide(AppSection.Live)` binds a [live section](#live-sections)

The key appears **once**, in the descriptor; business code receives the section's typed value through its constructor, and string keys in business code are an explicit anti-pattern. [Section Descriptors](#section-descriptors--sectiont) states the contract; examples: [Configuration Guide — Typed Config + DI](../guides/configuration.md#typed-config--di).

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

Combining both with `WithFiles(...)` (or `LoadBytes`) yields **fully hermetic loading**: only the listed files (or the embedded document) are read, and `Reload(ctx)` replays the same opt-outs — a disabled source can never leak in at reload time. An empty `WithPrefix("")` is not an opt-out: it removes the filter and merges every process environment variable.

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

Every `.env` entry therefore reaches the tree, so a strict whole-tree decode sees each one ([Decoding](#decoding)).

## API

### config.Load() — Primary Entry Point

```go
cfg, err := config.Load(opts...) // returns (*config.Config, error)
```

- Loads all sources (files, `.env`, env vars) and merges them.
- Returns `*config.Config` — the concrete type, which satisfies `credo.RawConfig`. Read a section with its descriptor, `section.Decode(cfg)`, use `cfg.Unmarshal(key, &dst)` for the low-level form, or pass `cfg` straight to `credo.WithRawConfig`.
- Returns an error for an I/O failure, a parse error, conflicting options, an explicit file list none of whose files exists, a missing explicit `.env` file, or an unsupported file format ([Error Handling Contract](#error-handling-contract)). Never panics. A section's `Validate()` error is returned by `Unmarshal` (and so by a section's `Decode`), not by `Load`. There is no `MustLoad`.
- No package-global instance — each call produces an independent `Config`.
- **Options**: `WithFiles(paths...)`, `WithPrefix(prefix)`, `WithDotenvPath(path)`, `WithDotenvOptional()`, `WithoutProcessEnv()`, `WithoutDotenv()`, `WithWeakDecoding()`, `WithLogger(*slog.Logger)` (load-time warnings such as a missing optional explicit `.env`; default `slog.Default()`). `.env` path rules: [.env Resolution Policy](#env-resolution-policy).

### config.RawConfig Interface

Defined in the `config/` package and re-exported from the root as `credo.RawConfig` (type alias). This is the **only** config access interface:

```go
// root package (credo)
type RawConfig interface {
    Unmarshal(key string, dst any) error
    Exists(key string) bool
}
```

Limited to 2 methods — no scalar getters (`GetString`/`GetInt`/`GetBool`), no `Get(key) any`. The typed reader is the [section descriptor](#section-descriptors--sectiont), whose `Decode` reads any `RawConfig`. Rationale and rejected alternatives: [ADR-005](../adr/005-configuration-architecture.md).

Behavior contract:

- `Unmarshal(key, &dst)` decodes a config sub-tree into `dst` (a pointer to a struct, map, slice, or primitive); returns error if the key is missing or decoding fails. The empty key `""` decodes the whole tree and returns an error when the configuration is empty. A key present with a null value leaves `dst` unchanged and returns no error.
- `Exists(key)` checks both leaf and intermediate keys, and reports a key whose value is null as present. `Exists("")` is false.

### Live Sections

```go
var Limits = config.Section[LimitsConfig]("limits")

app.Provide(Limits.Live) // decoded and validated when built, replaced whole by each reload that changes it

func NewOrderService(limits *config.Live[LimitsConfig]) *OrderService // the signature says the value is live

l := s.limits.Load() // once per operation: a value no reload changes
```

`ExampleSection_Live` runs a live section through a reload, with its result; `ExampleFixed` a fixed handle.

| API | Contract |
| --- | --- |
| `Section[T].Live(src config.RawConfig) (*config.Live[T], error)` | Decodes and validates the section as `Decode` does — a missing section, an invalid one and an empty key fail alike — and registers the handle with the `*config.Config` it read. Over a configuration that cannot reload (`testutil`'s, a custom `RawConfig`, a `Config` not built by `Load` or `LoadBytes`) it fails, naming the configuration's type and `config.Load` |
| `(*config.Live[T]).Load() T` | The current value. A consumer calls it once per operation |
| `(*config.Live[T]).OnChange(fn func(ctx context.Context, prev, next T) error)` | Registers a callback for state derived from the section — a limiter, a compiled pattern, a `slog.LevelVar` — run after a reload that changed the section has published its new value. A nil `fn` panics |
| `config.Fixed(v T) *config.Live[T]` | A handle whose value is `v` and never changes, and which runs no callback; for a consumer's unit test |

- **Static by default, live by declaration.** A section bound through `Decode` is a snapshot: a reload does not change it, and a change to it is reported as needing a restart. Each reload that changes a key under a live section replaces the handle's value whole. No other binding changes at a reload, and no singleton is rebuilt.
- **One nature per section.** A section is static or live within one configuration. Every read through its descriptor records the section's nature in the `*config.Config` it reads — `Decode`, `App.GetConfig` and `App.MustGetConfig` as static, `Live` as live — and the framework's read of `server` counts as static. A read of the other nature fails, naming the section and both reads: a constructor at its construction, the composition root at its call. A section nested in another, or one that contains it, counts as the same section. Two `Decode` reads of one section, and two `Live` reads, do not fail. A raw `Unmarshal` with a string key is outside the check.
- **What the type does not enforce.** `Load` in each operation rather than once at construction; derived state in `OnChange` rather than computed from a value read once; and a value's slices and maps read, never written, since every `Load` of one reload shares them.

### Reload and Its Result

`(*Config).Reload(ctx context.Context)` re-reads the sources with the options captured by the original `Load` — the same files (or the same `LoadBytes` document), prefix, `.env` path and source opt-outs, and the environment name resolved at first load (`CREDO_ENV` is not re-derived; switching environments is a restart). The process environment is re-read on every reload; what a supervisor hands the process stays fixed until restart ([Deployment Guide](../guides/deployment.md#systemd)). A load error returns a result that says nothing was published and the error `config: reload: …; nothing was published`, with the current snapshot untouched. Otherwise, in order:

1. Decodes and validates every live section the reload changes against the candidate. One failure aborts the reload with every old value current, and its error names the section's key and the error and says that nothing was published (`config: reload: live section "limits": …; nothing was published, and every live section keeps its value`); every failing section is named.
2. Publishes the snapshot and the changed sections as one generation. Concurrent `Unmarshal` and `Exists` calls observe the old or the new tree, never a mix, and once a `Load` returns a reload's value for one section, every later `Load` of that configuration returns that reload's values or newer ones. Two `Load` calls can still straddle a reload, so values that must agree within one operation belong to one section.
3. Runs the `OnChange` callbacks in registration order, for the sections the reload changed, with the reload's context. An error or a recovered panic is joined into the error `Reload` returns beside its result (`config: reload: OnChange callback of live section "limits": …`, a panic as `panic: …`), the later callbacks still run, and nothing rolls back. No callback is skipped when the context has ended, since the next reload's diff would not show its change again.
4. Returns its result: what changed, the sections it replaced, the keys that need a restart and the keys a later source keeps. A live section covers every key under it; every other key whose value differs from the one the start read, a key of a section bound through `Decode` and a `server.tls.*` path included, needs a restart. A key whose file value changed while `.env` or the process environment keeps it is reported with that source. The keys that need a restart are computed against the snapshot the start read, so a reload that changes nothing reports them again and one that reverts a key to its start value clears it. An aborted reload returns a result that says nothing was applied and why, beside its error. Key paths are reported, never values.

```go
func (c *Config) Reload(ctx context.Context) (ReloadResult, error)

type ReloadResult struct {
    Published       bool            `json:"published"`         // the snapshot was published
    Aborted         string          `json:"aborted,omitempty"` // why nothing was published; empty when Published
    Changed         []string        `json:"changed"`           // changed leaf keys, sorted, against the snapshot before the reload
    Replaced        []string        `json:"replaced"`          // live sections replaced, in the order they were declared live
    RestartRequired []string        `json:"restart_required"`  // keys that differ from the start outside every live section, sorted
    Overridden      []OverriddenKey `json:"overridden"`        // file keys whose change a later source hides
}

type OverriddenKey struct {
    Key    string `json:"key"`    // the dotted key path whose file value changed
    Source string `json:"source"` // the later source: the .env entry or the environment variable
}
```

The result is the same value `App.Reload` returns; its JSON form is what an admin endpoint answers with. A nil `ctx` is an error, and so is a `Config` not built by `Load` or `LoadBytes`. Reloads of one `Config` are serialized. `App.Reload` keeps the trigger, the slot and the drain's step, logs this result and returns it beside its error ([Lifecycle Spec](lifecycle.md#appreloadctx-contextcontext-configreloadresult-error)). `RawConfig` stays a two-method interface, and a custom `RawConfig` does not reload: it is loaded with `config.Load`.

### credo.New() Config Integration

`credo.New()` automatically loads configuration via `config.Load()` when no explicit `RawConfig` is provided. Use `credo.WithRawConfig(rawCfg)` to pass a pre-loaded config (e.g., from `config.LoadBytes()` with embedded data). Passing `WithRawConfig` bypasses auto-load entirely; the provided `RawConfig` is registered in the DI container as-is. Server config is framework-internal (no user-facing `CoreConfig`). There is no whole-config `app.Config()` accessor; `app.GetConfig(section)` ([below](#section-descriptors--sectiont)) covers composition-root reads, and typed config still flows to business code via DI. See [ADR-005](../adr/005-configuration-architecture.md#credonew-auto-loads-and-registers-rawconfig).

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
- The bytes are copied: `Reload(ctx)` re-parses the same document and re-reads only `.env` and the process environment.

### Section Descriptors — Section[T]

```go
var AppSection = config.Section[AppConfig]("app")

app.Provide(AppSection.Decode)       // a snapshot of the section, bound in DI
cfg := app.MustGetConfig(AppSection) // the same descriptor in the composition root
```

Runnable, over `config.LoadBytes`, as `ExampleSection_Decode`.

| API | Signature | On error |
| --- | --- | --- |
| `config.Section[T]` | `type Section[T any] string` | — (a section's key and type, named by conversion) |
| `Section[T].Decode` | `(src config.RawConfig) (T, error)` | returns zero `T` + error |
| `(*App).GetConfig` | `(section config.Section[T]) (T, error)` | returns zero `T` + error |
| `(*App).MustGetConfig` | `(section config.Section[T]) T` | panics, naming the key |
| `(*App).ConfigExists` | `(key string) bool` | — (mirrors `RawConfig.Exists`; a presence check during registration, before `Resolve` is available) |

- **`Decode`** decodes the section through `src.Unmarshal`, so the [decode rules](#decoding) and the [`Validate()` hook](#validatable-integration) apply; `T` may be a struct, map, slice or primitive, and an empty key is an error. A failure is wrapped as `config: section "<key>": …`. It lives in `config` and needs no root import, since `credo.RawConfig` is an alias of `config.RawConfig`.
- **Binding.** `Decode`'s method value is an ordinary constructor over the framework's `RawConfig` binding: `app.Provide(AppSection.Decode)` binds the section's `T`. A missing section or a failed validation is a constructor error at `Resolve` or at start that names the key, not a registration panic, and a second binding of one type panics with the container's message and its remedy ([container spec](container.md)). The bound value is a snapshot: a reload does not change it, and a section that must follow a reload is declared [live](#live-sections).
- **Several sections of one type.** A section that only one constructor reads is decoded in that constructor and not bound: two databases whose sections both decode into `sqldb.Config` each call their own section's `Decode` where they are opened, and the databases are what the container binds. A section that business code receives is bound once, or under a type of its own.
- **One reader per holder.** A `*config.Config` is read with `section.Decode(cfg)`; it has no getter of its own. `App.GetConfig` and `App.MustGetConfig` are for the composition root, which holds the App rather than its configuration: `app.GetConfig(AppSection)` infers `T`, and `app.GetConfig[AppConfig]("app")` compiles too, since the untyped constant converts to the descriptor. They decode through the registered `RawConfig`, so they behave alike for the auto-loaded `*config.Config` and a `WithRawConfig` implementation. `*credo.Context` has no `App()` accessor, so handlers and services cannot reach them through the request ([ADR-005](../adr/005-configuration-architecture.md#a-section-is-named-once-configsectiont)). `MustGetConfig` panics with the error `GetConfig` would return.
- **`testutil`'s empty configuration** honours the missing-key contract: its `Unmarshal` returns the not-found error a real `*config.Config` returns, and `Exists` stays false.

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

## Decoding

Every typed decode from a Config is strict unless the Config was loaded with `config.WithWeakDecoding()`:

| Aspect | Default (strict) | `WithWeakDecoding()` |
| --- | --- | --- |
| Unknown keys (config keys with no target field, nested included) | Error naming the key path | Silently ignored |
| A string into a basic type (`"9090"` → `int`, `"true"` → `bool`, `"1.5"` → `float64`) | Parsed by the target type (`StringToBasicTypeHookFunc`) | Allowed |
| Duration strings (`"5s"` → `time.Duration`) | Allowed (retained hook) | Allowed |
| `encoding.TextUnmarshaler` fields | Allowed (retained hook) | Allowed |
| Numeric kind conversions (JSON `float64` → int field) | Allowed (native decoding, not weak coercion) | Allowed |
| Conversions across kinds — a boolean into a number, a number into a string, a scalar into a slice | Error | Allowed |

`WithWeakDecoding()`'s contract is one sentence: mapstructure's weak coercion. Strict decoding is the default and has no option of its own. Runnable as `ExampleWithWeakDecoding`.

The policy rides on the Config instance, so it covers every decode path: `Unmarshal`, a section's `Decode` and `Live`, the decoding of a reload's candidate, and the framework's own decode of the `server` section in `credo.New` — an unknown `server.*` key fails app construction.

- **Environment overrides decode.** Process environment and `.env` values are strings, and the string hook parses each by its target type, so `CREDO_SERVER__PORT=9090` decodes into an `int`. The hook does not know a value's source, so a quoted number in a file decodes too; parsing a string by its target type is not a conversion between kinds.
- **Complete typed views**: every struct that decodes a section — a live section's `T` and full-tree decodes included — must cover all keys of that section. A full-tree decode of a config containing `server.*` needs a `Server` field on the target.
- **A full-tree decode sees every source.** `Unmarshal("", &dst)` decodes every `.env` entry (read with no prefix), every prefixed environment variable and the framework's `server` keys, and fails on each its target does not cover. The remedies: decode sections (`config.Section[T]`), give the target fields for those keys, or load with `config.WithoutDotenv()`. An unknown key that a `.env` entry or an environment variable set names that source, from the record behind `ReloadResult.Overridden`: `config: unknown key "x_y" (from .env entry X_Y): …`.
- **The error names the remedy.** A value strict decoding refuses fails with the key path, the conversion refused, and `config.WithWeakDecoding()` as the opt-in; a string that does not parse as its target type names the key path without the remedy, since weak coercion would not parse it either. Every offending key of one decode is reported. Unknown-key errors report key paths and never values; type-mismatch errors may quote the offending value (mapstructure behavior — accepted limitation).

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

Fields not in any source keep their pre-initialized values. An explicit zero value in a source (e.g., `port: 0` in YAML) **does** override the default — this is correct behavior (explicit intent). With the `Validate()` hook below, the pattern runs as `ExampleConfig_Unmarshal`.

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

The `config` package does not import `credo/validation`.

## Map[string]T Support (Dynamic Keys)

Fields of type `map[string]T` are fully supported from all sources — config files, `.env`, and process env vars. The `__` delimiter creates nesting, so `CREDO_DATABASES__DEFAULT__HOST` sets the `host` of the map entry `default` under `databases`.

> **Map key constraint**: Map keys must not contain `__`, as it is the nesting delimiter — a key like `my__db` would be read as two nesting levels. Use `_` or `-` instead (e.g., `read_replica`, `read-replica`).

For practical examples (JSON config, env var overrides, Go struct), see [Configuration Guide — Multi-Database Config](../guides/configuration.md#multi-database-config).

## One Value per File

A config file, and a `LoadBytes` document, holds one value: JSON rejects trailing data and duplicate members; YAML rejects duplicate keys and a second document — a `---` after the first document, an empty one included — while a leading `---` and a closing `...` belong to the one document and an empty or comment-only file is an empty config. Dots in keys always act as path separators.

## Error Handling Contract

- **Config File Error**: I/O or syntax error in JSON/YAML (only if found), including a YAML file with more than one document (`yaml: a config file holds one document; found another after the first`).
- **Missing Explicit Config Files**: `WithFiles` lists files of which none exists (`none of the specified config files were found: …`).
- **Unsupported Format**: a config file whose extension, or a `LoadBytes` format, is neither JSON nor YAML (`unsupported format: …`).
- **Conflicting Options**: `WithoutDotenv` + `WithDotenvPath`, rejected before any I/O.
- **Explicit .env Error**: an explicit `.env` path (`WithDotenvPath` or `CREDO_ENV_FILE`) is missing (a Warn instead under `WithDotenvOptional()`) or unreadable, or a `.env` file fails to parse.
- **Key Not Found**: `Unmarshal`, or a section's `Decode`, on a key that does not exist (`config: key "…" not found`; through `Decode`, `config: section "…": config: key "…" not found`).
- **Empty Section Key**: `Section[T].Decode` or `Live` with an empty key (`config: Section.Decode: empty section key; …`, `config: Section.Live: …`).
- **Empty Configuration**: a full-tree `Unmarshal("", &dst)` of an empty configuration (`config: configuration is empty`).
- **Strict Decoding Refusal**: an unknown key (`config: unknown key "app.porrt": no field of the target decodes it; fix the configuration, or opt in to weak coercion with config.WithWeakDecoding()`) or a conversion across kinds (`config: key "app.port": …; strict decoding converts no value across kinds: …`) names the key path, the conversion refused, and `config.WithWeakDecoding()`.
- **Type Conversion**: a string that does not parse as its target type (`config: key "server.port": …`), in either mode.
- **Validation Error**: `dst.Validate()` returns a non-nil error after unmarshalling (`config: validation: …`).
- **Reload Error**: a load error during `Reload(ctx)` is returned beside a result that says nothing was published, and the previous snapshot stays current.
- **Live Section at Reload**: a live section that fails to decode or validate at a reload names its key and the error and says that nothing was published; the old values stay current.
- **Callback Error**: an `OnChange` callback's error or recovered panic is joined into the error `Reload` returns, naming its section; the reload stays published.
- **Configuration That Cannot Reload**: `Section.Live` over a configuration that cannot reload, and `App.Reload` with one, each name the configuration's type and `config.Load`.
- **One Nature per Section**: a static read of a live section, or a live read of a static one, names the section and both reads (`config: section "limits" is read both as static, by …, and as live, by …; …`).
- All errors are returned, never panicked; only `MustGetConfig` panics, with the error `GetConfig` would return, naming the key, and `Live.OnChange` with a nil callback.

## Redacted Formatting

`*Config` implements `String()`, `GoString()`, and `slog.LogValuer` with a metadata-only description (`config.Config(N keys, values redacted)`): formatting a Config with `%v`/`%s`/`%+v`/`%#v` or logging it as an slog attribute can never leak values or key names. This mirrors the reload result's doctrine (key paths only, never values, safe to log). A dereferenced copy (`*cfg`) is redacted exactly like the pointer; a nil `*Config` formats as fmt's usual `<nil>`. Runnable as `ExampleConfig_String`.

## Current Behaviour

Not guaranteed: the loader's mechanisms, as they are now. A release may change them without notice, provided the contract holds.

### Single-Pass Loader Model

There are no provider/parser interfaces: sources are internal functions that each produce a `map[string]any`, merged in precedence order into one nested map held by `Config`.

- **Config files** — `os.ReadFile` + format dispatch by extension (`encoding/json/v2` / `go.yaml.in/yaml/v3`); the same parser backs `LoadBytes`.
- **`.env` file** — Credo's own line parser (`parseDotenv`), read once per `Load`; entries normalized (lowercase, `__` → `.`) and unflattened.
- **Process env vars** — prefix-filtered, normalized the same way.

Key lookup walks the nested map directly (`lookup`); there is no flattened key index. Env vars like `CREDO_DATABASES__DEFAULT__HOST` are normalized to dotted keys (`databases.default.host`), unflattened into a nested `map[string]any`, and unmarshaled into a `map[string]T` by mapstructure.

### Decoding

`mapstructure.Decode()` modifies the target in place, setting only the fields that exist in the input map, which is how a pre-initialized value keeps its defaults. The `Validate()` hook is checked with a local inline type assertion. A live section's `Load` is one atomic load.

### Redaction

A `Config`'s state lives behind one private pointer, and its formatting methods are declared on the value type, which is how a dereferenced copy stays redacted.

### Koanf Adaptation Scope

`config/maps.go` derives from koanf's `maps/maps.go` (`unflatten`, `mergeMaps`, `copyMap`, `intfaceKeysToStrings`; attribution in [NOTICES](../../NOTICES)). Nothing else is adapted:

- **Provider/parser architecture** (`providers/*`, `parsers/*`): the loader is single-pass, and file, `.env` and environment reading is Credo's own (`load.go`, `dotenv.go`). A single embedded document goes through `LoadBytes`; an `fs.FS` source with discovery and env-specific overlays is deferred. Remote (consul, etcd, vault) and CLI-flag providers are out of scope, and only JSON and YAML are supported.
- **Scalar getters, `All()`, `KeyMap()`**: `Unmarshal` and a section's `Decode` decode typed values, `Unmarshal("", &dst)` decodes the whole tree, and lookup walks the nested map (no flattened key index).
- **`Set()`/`Delete()`**: no in-place mutation; reload re-loads the whole snapshot ([Reload](#reload-and-its-result)).
- **`StrictMerge`/`MergeStrict`**: merges are always loose.

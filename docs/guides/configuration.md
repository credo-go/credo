# Configuration Guide

This guide covers how to configure a Credo application. For internal design rationale, see [Configuration Spec](../specs/config.md).

All config examples in this guide use JSON for consistency. Credo also supports YAML/YML with the same structure.

---

## Quick Start

### Zero-Config

`credo.New()` automatically loads configuration from files, `.env`, and environment variables:

```go
app, err := credo.New()
if err != nil {
    log.Fatal(err)
}
```

This discovers `config.json`, `config.yaml`, or `config.yml` in the working directory, loads `.env` if present (all entries), and applies `CREDO_*` environment variables.

### Explicit File

Credo does not expose root-level file options such as `credo.WithConfigFiles`. When you want explicit file control, load config yourself and pass the result to `credo.New`:

```go
rawCfg, err := config.Load(config.WithFiles("myconfig.json"))
if err != nil {
    log.Fatal(err)
}
app, err := credo.New(credo.WithRawConfig(rawCfg))
```

Passing `WithRawConfig` bypasses `credo.New()`'s auto-load path. The provided `RawConfig` is registered in DI as-is, while framework server settings are still read from its `"server"` section when present.

### go:embed

```go
import "github.com/credo-go/credo/config"

//go:embed config.json
var configData []byte

func main() {
    rawCfg, err := config.LoadBytes(configData, config.FormatJSON)
    if err != nil {
        log.Fatal(err)
    }
    app, err := credo.New(credo.WithRawConfig(rawCfg))
    // ...
}
```

Environment variables still override embedded values because `config.LoadBytes` applies the `.env` and process environment layers before the `RawConfig` is passed to `credo.New`.

---

## Source Precedence

Configuration sources are merged in this order (later overrides earlier):

```
1. Base config files     ← config.json, config.yaml, config.yml (all found)
2. Env-specific files    ← config.{CREDO_ENV}.* (when CREDO_ENV is set)
3. .env file             ← WithDotenvPath > CREDO_ENV_FILE > default ".env" (no prefix filtering)
4. Environment variables ← CREDO_* prefix (prefix-filtered)
```

For overlapping keys, higher-numbered sources win. Non-overlapping keys from all sources are preserved.

Sources 3 and 4 can be disabled entirely with `config.WithoutDotenv()` and `config.WithoutProcessEnv()` — see [Hermetic Config](#hermetic-config).

---

## Hermetic Config

For deployments where configuration must come from a known file and nothing else — reproducible container images, security-sensitive services, tests that must not be affected by the developer's shell — disable the ambient sources explicitly:

```go
rawCfg, err := config.Load(
    config.WithFiles("config.json"),   // required: Load fails if missing
    config.WithoutProcessEnv(),        // no env vars, no env-sourced CREDO_ENV / CREDO_ENV_FILE
    config.WithoutDotenv(),            // no .env file, from any path
)
```

Each opt-out removes its source entirely, bootstrap keys included: `WithoutProcessEnv()` also stops `CREDO_ENV` and `CREDO_ENV_FILE` from being read out of the process environment, and `WithoutDotenv()` never reads a `.env` file at all. `Reload` replays the same opt-outs, so a disabled source cannot leak in later. The same options work with `config.LoadBytes` for fully hermetic embedded configuration.

Note that `config.WithPrefix("")` is **not** a way to disable environment variables — an empty prefix removes the filter and merges every process environment variable into the tree.

### Strict Decoding

Hermetic setups usually also want typo detection: with `config.WithStrictDecoding()`, a config key that does not map to a field of the target struct is an error (nested sections included), and weak string coercion (`"8080"` → int, `"true"` → bool) is off. Duration strings (`"5s"`), `encoding.TextUnmarshaler` fields, and JSON numbers into int fields keep decoding:

```go
rawCfg, err := config.Load(
    config.WithFiles("config.json"),
    config.WithoutProcessEnv(),
    config.WithoutDotenv(),
    config.WithStrictDecoding(),
)
```

Strict mode applies to every decode from that store: `Unmarshal`/`Get`, reload validation, and the framework's own `server` section read in `credo.New` — a typo under `server.*` then fails startup instead of silently using a default. Two things to design for: string env/.env overrides on typed fields no longer decode (strict mode is meant for typed, file-only sources like the setup above), and every struct that decodes a section must cover all of that section's keys — including narrow `OnConfigChange` subscribers and full-tree decodes (a config with a `server` section needs a `Server` field on a full-tree target).

### Logging the Config

`*config.Config` and its dereferenced copies format as metadata only (`config.Config(N keys, values redacted)`) through `%v`, `%+v`, `%#v`, and as an slog attribute, so passing the store to a log line, a panic message, or a debug dump can never leak a secret or a key name. To inspect values, decode the section into a typed struct and log the fields you mean to expose.

---

## Environment-Based Config

Complete, equivalent YAML and JSON starter files plus an `.env.example` are
available under [`examples/references/config`](../../examples/references/config/).
They are versioned copyable references; choose one base format unless layered
file merging is intentional.

Set `CREDO_ENV` to load environment-specific overrides automatically:

```bash
CREDO_ENV=production ./myapp
```

This loads the base files first, then merges any found files matching `config.production.json`, `config.production.yaml`, or `config.production.yml`.

**Example directory layout:**

```text
config.json                  ← shared defaults
config.production.json       ← production overrides (ports, timeouts, etc.)
config.staging.json          ← staging overrides
```

`config.json`:

```json
{
  "server": {
    "port": 3000,
    "read_timeout": "30s"
  },
  "debug": true
}
```

`config.production.json`:

```json
{
  "server": {
    "port": 8080,
    "read_timeout": "60s"
  },
  "debug": false
}
```

With `CREDO_ENV=production`, the effective config is port=8080, read_timeout=60s, debug=false.

Env-specific file derivation works in both discovery and explicit mode. In explicit mode, the env-specific filename is derived by inserting `.{env}` before the file extension:

```go
// With CREDO_ENV=production (from process env or .env):
rawCfg, err := config.Load(config.WithFiles("myapp.yaml"))
// Loads: myapp.yaml (required) + myapp.production.yaml (optional overlay)
```

`CREDO_ENV` can also be set in the `.env` file. Process env takes precedence.

### Custom .env Path

By default, Credo looks for `.env` in the working directory. For deployments where the binary runs from a different directory, use `WithDotenvPath`:

```go
rawCfg, err := config.Load(
    config.WithDotenvPath("/etc/myapp/.env"),
)
```

`WithDotenvPath` takes precedence over the `CREDO_ENV_FILE` environment variable. A missing file at the specified path is an error. To downgrade the missing-file error to a warning, combine with `WithDotenvOptional()`:

```go
rawCfg, err := config.Load(
    config.WithDotenvPath("/etc/myapp/.env"),
    config.WithDotenvOptional(),
)
```

This also works with `CREDO_ENV_FILE`:

```bash
CREDO_ENV_FILE=/etc/myapp/.env ./myapp
```

`WithDotenvOptional()` applies to both `WithDotenvPath` and `CREDO_ENV_FILE`.

---

## Typed Config + DI

The primary pattern is to unmarshal config once at the module boundary, then inject the typed struct via DI. Field names map to snake_case config keys automatically (e.g. `MaxOpen` → `max_open`), so struct tags are optional — add a `credo:"..."` tag only when the key differs from the field's snake_case name:

```go
type DatabaseConfig struct {
    Host    string
    Port    int
    MaxOpen int
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    // Resolve the auto-loaded RawConfig from DI.
    rc := app.MustResolve[credo.RawConfig]()

    // Unmarshal and register typed config.
    var dbCfg DatabaseConfig
    if err := rc.Unmarshal("databases.default", &dbCfg); err != nil {
        log.Fatal(err)
    }
    app.MustProvideValue(&dbCfg)

    // Services receive *DatabaseConfig via constructor injection.
    app.MustProvide[*MyService](NewMyService)
}

func NewMyService(infra credo.Infra, cfg *DatabaseConfig) *MyService {
    // cfg is fully typed — no string keys
    return &MyService{cfg: cfg}
}
```

String keys appear **once** at the module boundary. Beyond that, everything is typed and compile-time safe.

### Typed Getter Shorthand

`app.GetConfig[T](key)` collapses the resolve-then-unmarshal step into a single call. Its sibling `config.(*Config).Get[T]` does the same when you hold a `*config.Config` directly (for example the value returned by `config.Load`):

```go
app, err := credo.New()
if err != nil {
    log.Fatal(err)
}

// One call replaces MustResolve[RawConfig] + var + Unmarshal.
dbCfg, err := app.GetConfig[DatabaseConfig]("databases.default")
if err != nil {
    log.Fatal(err)
}
app.MustProvideValue(&dbCfg)
```

Use `MustGetConfig[T]` (or `cfg.MustGet[T]`) to panic on a missing or invalid required section — fail-fast startup wiring, mirroring `MustProvide`/`MustResolve`. These getters are composition-root sugar: a handler has no `App` accessor, so config reading stays out of business code, and services still receive typed structs via DI.

---

## Reloading Configuration

The typed snapshot is loaded once at startup — but a long-running service can pick up changes to its config files without restarting. `app.Reload(ctx)` (or `SIGHUP` under `app.Run()` on Unix; see the [Deployment Guide](deployment.md)) re-reads every source the snapshot was built from, computes which leaf keys changed, and notifies the typed subscribers registered for those sections:

```go
app, err := credo.New()
if err != nil {
    log.Fatal(err)
}

// A reloadable section: decoded and validated on every change, then applied.
type Limits struct {
    RPS   int
    Burst int
}

func (l Limits) Validate() error {
    if l.RPS <= 0 || l.Burst < l.RPS {
        return fmt.Errorf("limits: rps must be > 0 and burst >= rps")
    }
    return nil
}

var limits atomic.Pointer[Limits] // the live value; handlers read limits.Load()

app.OnConfigChange("limits", func(ctx context.Context, next Limits) error {
    limits.Store(&next)
    return nil
})

// Log level without a restart: slog.LevelVar is already atomic.
var level slog.LevelVar

app.OnConfigChange("logging", func(ctx context.Context, next struct{ Level slog.Level }) error {
    level.Set(next.Level)
    return nil
})
```

The rules that make this safe:

- **Only affected subscribers run.** A subscriber for `limits` fires when any leaf under `limits` (or `limits` itself) changes; a change to `logging.level` does not touch it. Nested keys are independent subscriptions — `databases` and `databases.primary` both fire when `databases.primary.dsn` changes.
- **Validate before publish.** Every affected `T` is decoded from the _candidate_ snapshot first; if `T` has a `Validate() error` method it runs too. Any failure aborts the reload before anything is published — the old snapshot stays current, no subscriber runs, and the error is returned (and logged as `reload aborted before publish`). A bad YAML edit can therefore never half-apply.
- **Apply is the subscriber's job, and it is atomic in your domain.** The framework never rebuilds DI singletons. Hold reloadable values behind `atomic.Pointer[T]`, a `slog.LevelVar`, or a swappable limiter inside the service that owns them, and have the subscriber swap them (see [DI: Config Changes and Singletons](dependency-injection.md#config-changes-and-singletons)). Subscriber errors are collected, later subscribers still run, and the joined error is returned — there is no rollback.
- **Unsubscribed changes mean a restart.** Every changed key that no subscriber (or framework participant) covers is logged once at `WARN` with `restart required` and the key paths — never the values. That log line is how you discover which sections still need a subscriber.
- **`OnReload` for everything else.** `app.OnReload(func(ctx) error)` hooks run FIFO after the subscribers on every reload, whether or not config changed — re-open a rotated log file, refresh an allowlist, drive your own certificate rotation.

Registration happens before `Run`; `OnConfigChange` panics at registration when the app's `RawConfig` cannot reload (a subscription that could never fire is a startup bug). `*config.Config` — the store `credo.New()` loads and the one `config.Load` returns — always can.

What a reload does **not** do:

- It never switches environments: `CREDO_ENV` is fixed at first load, so `config.dev.yaml` stays the overlay even if the variable changes.
- It never applies server settings. `server.host`, ports, timeouts, body limits, and proxies are read by `credo.New()` once; changing them is a restart. The one exception is `server.tls.*`: file-based certificates are re-read on every reload (see [TLS Server Config](#tls-server-config)).
- It cannot see environment changes the process never received. The process environment is re-read, but a supervisor's `EnvironmentFile=` is handed over at process start, so values sourced from env still require a restart.

A custom `credo.WithRawConfig` store opts into all of this by implementing `config.Stager` (two-phase: stage a candidate, then commit — this is what enables validate-before-publish) or the one-shot `config.Reloader` (published first, validated after). The [Configuration Spec](../specs/config.md#reload--partial-reload-adr-020) has the contracts.

---

## Multi-Database Config

For multiple databases, keep each config section separate and unmarshal them independently at the module boundary:

```go
func setupDatabases(app *credo.App) error {
    rc := app.MustResolve[credo.RawConfig]()

    var primaryCfg sqldb.Config
    if err := rc.Unmarshal("databases.primary", &primaryCfg); err != nil {
        return err
    }

    var analyticsCfg sqldb.Config
    if err := rc.Unmarshal("databases.analytics", &analyticsCfg); err != nil {
        return err
    }

    // open/register each connection separately
    return nil
}
```

Example config structure:

```json
{
  "databases": {
    "primary": {
      "driver": "pgx",
      "host": "localhost",
      "port": 5432,
      "name": "app"
    },
    "analytics": {
      "driver": "pgx",
      "host": "localhost",
      "port": 5432,
      "name": "analytics"
    }
  }
}
```

Use one section per logical connection. The [Data Access Guide](data-access.md) shows how these configs map to DI wrapper types such as `PrimaryDB` and `AnalyticsDB`.

---

## Validation

If your config struct implements `Validate() error`, it is called automatically by `Unmarshal`:

```go
type DatabaseConfig struct {
    Host string
    Port int
}

func (c *DatabaseConfig) Validate() error {
    if c.Host == "" {
        return errors.New("database host is required")
    }
    if c.Port <= 0 {
        return errors.New("database port must be positive")
    }
    return nil
}

// Unmarshal calls Validate() automatically — no extra step needed.
var cfg DatabaseConfig
if err := rc.Unmarshal("databases.default", &cfg); err != nil {
    log.Fatal(err) // includes validation errors
}
```

---

## Default Values

Pre-initialize your struct before unmarshalling. Fields not present in any config source keep their default values:

```go
func DefaultDatabaseConfig() DatabaseConfig {
    return DatabaseConfig{
        Host:    "localhost",
        Port:    5432,
        MaxOpen: 25,
    }
}

cfg := DefaultDatabaseConfig()
rc.Unmarshal("databases.default", &cfg)
// cfg.MaxOpen is 25 unless overridden by config/env
```

---

## Environment Variables

Process environment variables use the `CREDO_` prefix (configurable via `config.WithPrefix()`). Naming convention:

- Strip prefix, lowercase
- `__` (double underscore) = nesting separator (becomes `.`)
- `_` (single underscore) = stays as-is within a segment

| Env Var                          | Config Key               |
| -------------------------------- | ------------------------ |
| `CREDO_SERVER__PORT`             | `server.port`            |
| `CREDO_SERVER__READ_TIMEOUT`     | `server.read_timeout`    |
| `CREDO_SERVER__TLS__CERT_FILE`   | `server.tls.cert_file`   |
| `CREDO_SERVER__TLS__KEY_FILE`    | `server.tls.key_file`    |
| `CREDO_DATABASES__DEFAULT__HOST` | `databases.default.host` |

`.env` file entries use the same normalization but **without** the prefix:

| .env Entry                           | Config Key               |
| ------------------------------------ | ------------------------ |
| `SERVER__PORT=8080`                  | `server.port`            |
| `SERVER__READ_TIMEOUT=30s`           | `server.read_timeout`    |
| `SERVER__TLS__CERT_FILE=/cert.pem`   | `server.tls.cert_file`   |
| `SERVER__TLS__KEY_FILE=/key.pem`     | `server.tls.key_file`    |
| `DATABASES__DEFAULT__HOST=localhost` | `databases.default.host` |

---

## TLS Server Config

File-based TLS can be configured through the `server.tls` section:

```json
{
  "server": {
    "host": "0.0.0.0",
    "port": 443,
    "tls": {
      "cert_file": "/etc/tls/server.crt",
      "key_file": "/etc/tls/server.key"
    }
  }
}
```

When both paths are set, `Run()` and `RunContext()` serve HTTPS automatically. The key pair is loaded and validated at startup; missing files, mismatched pairs, and partial config fail before the server starts accepting connections.

After startup the pair is served through `GetCertificate` backed by an atomic pointer, and **every reload re-reads it** — `app.Reload(ctx)`, or `systemctl reload` / `SIGHUP` under `Run()`. Rotating a certificate in place needs no config change at all; changing `server.tls.cert_file` / `key_file` in the config file moves the pair to the new paths on the next reload. New handshakes see the new certificate immediately, open connections are untouched, and a pair that fails to load keeps the current certificate serving while the failure surfaces through the reload error. An ACME deploy hook therefore reduces to one line:

```sh
# certbot renewal-hooks/deploy/credo.sh
systemctl reload myservice
```

TLS sources resolve by precedence:

```text
WithTLSConfig(*tls.Config) > WithTLSFiles(cert, key) > server.tls.* > plaintext
```

Each source is a whole-source override. For example, `WithTLSFiles` replaces both `server.tls.cert_file` and `server.tls.key_file`; it does not merge one path from the option with one path from config.

Use `WithTLSConfig` for embedded certificates, mTLS, SNI, custom TLS versions, or when you want to own certificate rotation yourself: Credo never touches a caller-supplied `*tls.Config`, so drive it through your own `GetCertificate` (optionally refreshed from an `OnReload` hook). To redirect plaintext HTTP callers to HTTPS, configure TLS and add `credo.WithHTTPRedirect(":80")`.

---

## Configuration Reference

Quick-lookup of the commonly used config keys.

### Server — `server`

Consumed automatically by `credo.New()`. The **Reloadable** column says what a [reload](#reloading-configuration) does with a change: `restart` keys are read once at startup and changing them logs `restart required`.

| Key | Type | Default | Reloadable | Description |
| --- | --- | --- | --- | --- |
| `host` | string | `""` (all interfaces) | restart | Listen address |
| `port` | int | `0` (OS-assigned) | restart | Listen port (0–65535) |
| `read_timeout` | duration | `0` | restart | Max duration for reading entire request |
| `write_timeout` | duration | `0` | restart | Max duration for writing response |
| `idle_timeout` | duration | `0` | restart | Max wait for next request (keep-alive) |
| `read_header_timeout` | duration | `0` | restart | Max duration for reading headers |
| `max_header_bytes` | int | `0` (Go default: 1 MB) | restart | Max header size in bytes |
| `max_header_value_count` | int | `0` (Go default: 500) | restart | Max header lines per request; over the limit the request is answered with `431` — written straight to the connection by `net/http`, so it never appears in the logs. Negative values are rejected at `credo.New()` |
| `max_body_bytes` | int64 | `4 MiB` | restart | Request body limit; `-1` disables it |
| `shutdown_timeout` | duration | `30s` | restart | Drain budget for signal- or context-triggered shutdown |
| `reload_timeout` | duration | `30s` | restart | Context budget for a `SIGHUP`-triggered reload under `Run()` |
| `redirect_trailing_slash` | bool | `true` | restart | Auto-redirect when trailing slash variant matches (301/308) |
| `debug` | bool | `false` | restart | Enable development warnings |
| `strict_bodies` | bool | `false` | restart | Reject unknown JSON object members in `BindBody` (400 `unknown_field`); `credo.WithStrictBodies()` wins over this key |
| `trusted_proxies` | []string | `[]` | restart | CIDR ranges allowed to influence forwarded headers for `Request.Scheme()` and `Request.RealIP()` |
| `tls.cert_file` | string | `""` | **yes** — re-read on every reload | PEM certificate file for HTTPS |
| `tls.key_file` | string | `""` | **yes** — re-read on every reload | PEM private key file for HTTPS |

Sections the application reads itself (`databases.*`, `i18n`, `auth.*`, and your own) are reloadable exactly when the application registers an `OnConfigChange[T]` subscriber for them; without one, a change is restart-only.

### Databases — `databases.<name>`

User-read via `rc.Unmarshal("databases.<name>", &cfg)`.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `driver` | string | `""` | `"postgres"`, `"mysql"`, `"sqlite"` |
| `dsn` | string | `""` | Raw DSN (overrides host/port/name) |
| `host` | string | `""` | Server hostname or IP |
| `port` | int | `0` | Server port |
| `name` | string | `""` | Database name |
| `user` | string | `""` | Auth username |
| `password` | string | `""` | Auth password |
| `connect_timeout` | duration | `0` | Connection establishment timeout |
| `max_open` | int | `0` (unlimited) | Max open connections |
| `max_idle` | int or null | unset | Max idle connections; omitted uses the stdlib policy (subject to `max_open`), explicit `0` retains none |
| `max_idle_time` | duration | `0` (disabled) | Max idle age before a connection is closed |
| `max_lifetime` | duration | `0` (disabled) | Max connection lifetime |
| `ssl_mode` | string | `""` | `"disable"`, `"require"`, `"verify-full"` |
| `options` | map | `{}` | Driver-specific params |

`max_open: 0` is not a conservative production limit: it preserves
`database/sql`'s unlimited-open behavior. The canonical `store.Register` path
logs `sqldb.pool.max_open_unlimited` after successful registration when the
effective pool maximum is still unlimited; choose a finite value from the
database connection budget divided across service replicas. When both are
explicit and `max_open > 0`, `max_idle` must not exceed `max_open`.

### i18n — `i18n`

Auto-read by `app.UseI18n()`.

| Key       | Type   | Default      | Description               |
| --------- | ------ | ------------ | ------------------------- |
| `dir`     | string | `"locales/"` | Locale file directory     |
| `default` | string | `"en"`       | Default language (BCP 47) |

An `i18n.dir` value is explicit configuration: a missing or message-empty
directory makes `UseI18n` fail. Only absent conventional `locales/` discovery
from zero-config setup is optional. `I18nConfig.Messages`, `Fields`, `DirFS`,
`Detect`, and `ResolveMessageKey` are Go-only startup inputs; programmatic maps
are copied snapshots and are not part of RawConfig or runtime reload.

### Auth — `auth.*`

User-read via `rc.Unmarshal("auth.<strategy>", &cfg)`.

**JWT** — `auth.jwt`:

| Key              | Type   | Default           | Description          |
| ---------------- | ------ | ----------------- | -------------------- |
| `header`         | string | `"Authorization"` | Token header         |
| `prefix`         | string | `"Bearer"`        | Scheme prefix        |
| `query`          | string | `""`              | Query param fallback |
| `cookie`         | string | `""`              | Cookie fallback      |
| `signing_method` | string | `"HS256"`         | Signing algorithm    |

**API Key** — `auth.api_key`:

| Key      | Type   | Default       | Description          |
| -------- | ------ | ------------- | -------------------- |
| `header` | string | `"X-API-Key"` | Key header           |
| `prefix` | string | `""`          | Scheme prefix        |
| `query`  | string | `""`          | Query param fallback |

**Basic** — `auth.basic`:

| Key     | Type   | Default        | Description            |
| ------- | ------ | -------------- | ---------------------- |
| `realm` | string | `"Restricted"` | WWW-Authenticate realm |

---

## Related Documents

- [Data Access Guide](data-access.md) — single DB and multi-DB wiring
- [Deployment Guide](deployment.md) — systemd `ExecReload`, containers, certificate rotation hooks
- [Configuration Spec](../specs/config.md) — API contracts, design rules
- [ADR-020](../adr/020-reload-and-partial-config-reload.md) — reload signal and partial config reload
- [ADR-005](../adr/005-configuration-architecture.md) — architecture decision

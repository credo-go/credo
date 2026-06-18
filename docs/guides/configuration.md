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
store, err := config.Load(config.WithFiles("myconfig.json"))
if err != nil {
    log.Fatal(err)
}
app, err := credo.New(credo.WithRawConfig(store))
```

Passing `WithRawConfig` bypasses `credo.New()`'s auto-load path. The provided `RawConfig` is registered in DI as-is, while framework server settings are still read from its `"server"` section when present.

### go:embed

```go
import "github.com/credo-go/credo/config"

//go:embed config.json
var configData []byte

func main() {
    store, err := config.LoadBytes(configData, config.FormatJSON)
    if err != nil {
        log.Fatal(err)
    }
    app, err := credo.New(credo.WithRawConfig(store))
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

---

## Environment-Based Config

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
store, err := config.Load(config.WithFiles("myapp.yaml"))
// Loads: myapp.yaml (required) + myapp.production.yaml (optional overlay)
```

`CREDO_ENV` can also be set in the `.env` file. Process env takes precedence.

### Custom .env Path

By default, Credo looks for `.env` in the working directory. For deployments where the binary runs from a different directory, use `WithDotenvPath`:

```go
store, err := config.Load(
    config.WithDotenvPath("/etc/myapp/.env"),
)
```

`WithDotenvPath` takes precedence over the `CREDO_ENV_FILE` environment variable. A missing file at the specified path is an error. To downgrade the missing-file error to a warning, combine with `WithDotenvOptional()`:

```go
store, err := config.Load(
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

The primary pattern is to unmarshal config once at the module boundary, then inject the typed struct via DI:

```go
type DatabaseConfig struct {
    Host     string        `credo:"host"`
    Port     int           `credo:"port"`
    MaxOpen  int           `credo:"max_open"`
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    // Resolve the auto-loaded RawConfig from DI.
    rc := credo.MustResolve[credo.RawConfig](app)

    // Unmarshal and register typed config.
    var dbCfg DatabaseConfig
    if err := rc.Unmarshal("databases.default", &dbCfg); err != nil {
        log.Fatal(err)
    }
    credo.MustProvideValue(app, &dbCfg)

    // Services receive *DatabaseConfig via constructor injection.
    credo.MustProvide[*MyService](app, NewMyService)
}

func NewMyService(infra credo.Infra, cfg *DatabaseConfig) *MyService {
    // cfg is fully typed — no string keys
    return &MyService{cfg: cfg}
}
```

String keys appear **once** at the module boundary. Beyond that, everything is typed and compile-time safe.

---

## Multi-Database Config

For multiple databases, keep each config section separate and unmarshal them independently at the module boundary:

```go
func setupDatabases(app *credo.App) error {
    rc := credo.MustResolve[credo.RawConfig](app)

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
    Host string `credo:"host"`
    Port int    `credo:"port"`
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
| `CREDO_DATABASES__DEFAULT__HOST` | `databases.default.host` |

`.env` file entries use the same normalization but **without** the prefix:

| .env Entry                           | Config Key               |
| ------------------------------------ | ------------------------ |
| `SERVER__PORT=8080`                  | `server.port`            |
| `SERVER__READ_TIMEOUT=30s`           | `server.read_timeout`    |
| `DATABASES__DEFAULT__HOST=localhost` | `databases.default.host` |

---

## Configuration Reference

Quick-lookup of the commonly used config keys.

### Server — `server`

Consumed automatically by `credo.New()`.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `host` | string | `""` (all interfaces) | Listen address |
| `port` | int | `0` (OS-assigned) | Listen port (0–65535) |
| `read_timeout` | duration | `0` | Max duration for reading entire request |
| `write_timeout` | duration | `0` | Max duration for writing response |
| `idle_timeout` | duration | `0` | Max wait for next request (keep-alive) |
| `read_header_timeout` | duration | `0` | Max duration for reading headers |
| `max_header_bytes` | int | `0` (Go default: 1 MB) | Max header size in bytes |
| `redirect_trailing_slash` | bool | `true` | Auto-redirect when trailing slash variant matches (301/308) |
| `debug` | bool | `false` | Enable development warnings |
| `trusted_proxies` | []string | `[]` | CIDR ranges allowed to influence forwarded headers for `Request.Scheme()` and `Request.RealIP()` |

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
| `max_idle` | int | `0` | Max idle connections |
| `max_lifetime` | duration | `0` | Max connection reuse duration |
| `ssl_mode` | string | `""` | `"disable"`, `"require"`, `"verify-full"` |
| `options` | map | `{}` | Driver-specific params |

### i18n — `i18n`

Auto-read by `app.UseI18n()`.

| Key       | Type   | Default      | Description               |
| --------- | ------ | ------------ | ------------------------- |
| `dir`     | string | `"locales/"` | Locale file directory     |
| `default` | string | `"en"`       | Default language (BCP 47) |

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
- [Configuration Spec](../specs/config.md) — API contracts, design rules
- [ADR-005](../adr/005-configuration-architecture.md) — architecture decision

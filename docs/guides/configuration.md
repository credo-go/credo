# Configuration Guide

This guide covers how to configure a Credo application. For internal design rationale, see [Configuration Spec](../specs/config.md).

All config examples in this guide use JSON for consistency. Credo also supports YAML/YML with the same structure. A YAML file holds one document: it may open with `---`, but a second document — even an empty one after a trailing `---` — fails the load instead of being ignored.

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

Because these inputs are read whether or not you wrote them, `credo.New()` logs what it found: one record naming the files, the `.env` file and the environment prefix, and never a key or a value.

```text
level=INFO msg="credo: configuration loaded automatically" sources.files=[/srv/app/config.yaml] sources.dotenv=/srv/app/.env sources.process_env=true sources.env_prefix=CREDO_
```

The record is written at Info when a file or a `.env` was read, and at Debug when the load found only the environment. If a setting has a value you did not expect, this line tells you which file to open; a `config.json` left behind in the working directory shows up here.

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

`credo.New()` writes no load record on this path, since it loaded nothing. Log the same record yourself from the Config you loaded:

```go
logger.Info("configuration loaded", "sources", rawCfg.Sources())
```

`Sources()` lists the files found (absolute paths, in merge order), the `.env` file read and the environment prefix. An application that wants no bootstrap input at all passes a `RawConfig` of its own: a `config.Load` with `WithFiles()`, `WithoutDotenv()` and `WithoutProcessEnv()` ([Hermetic Config](#hermetic-config)), or `config.LoadBytes` with an embedded document.

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

Decoding is strict by default. A configuration key that no field of the target struct decodes is an error naming its key path, nested sections included, so a typo fails instead of silently leaving a default:

```text
config: unknown key "server.prot": no field of the target decodes it; fix the configuration, or opt in to weak coercion with config.WithWeakDecoding()
```

A value is not converted across kinds: a boolean into a number, a number into a string or a scalar into a slice is an error that names the key and `config.WithWeakDecoding()`. Strings still parse by their target type, so environment and `.env` overrides keep working: `CREDO_SERVER__PORT=9090` decodes into an `int`, `"true"` into a `bool`, `"5s"` into a `time.Duration`, and an `encoding.TextUnmarshaler` field from its text. JSON numbers decode into int fields.

The policy applies to every decode from that store: `Unmarshal`, a section's `Decode`, the decoding of a reload's candidate, and the framework's own `server` and `log` sections read in `credo.New`, so a typo under `server.*` or `log.*` fails startup. Every struct that decodes a section must therefore cover all of that section's keys, full-tree decodes included (a config with a `server` section needs a `Server` field on a full-tree target).

A full-tree decode (`rawCfg.Unmarshal("", &cfg)`) is where this bites first. `.env` entries are read with no prefix, so every entry in `.env` lands in the tree beside the framework's `server` keys, and the target must cover each one. Decode the sections you own instead (`config.Section[AppConfig]("app").Decode(rawCfg)`), give the target fields for those keys, or load with `config.WithoutDotenv()`. An unknown key that a `.env` entry or an environment variable set names its source:

```text
config: unknown key "x_y" (from .env entry X_Y): no field of the target decodes it; fix the configuration, or opt in to weak coercion with config.WithWeakDecoding()
```

An application that relies on mapstructure's weak coercion (a number into a string field, a single value into a slice, unknown keys ignored) opts in for the whole store:

```go
rawCfg, err := config.Load(config.WithWeakDecoding())
```

### Logging the Config

`*config.Config` and its dereferenced copies format as metadata only (`config.Config(N keys, values redacted)`) through `%v`, `%+v`, `%#v`, and as an slog attribute, so passing the store to a log line, a panic message, or a debug dump can never leak a secret or a key name. To inspect values, decode the section into a typed struct and log the fields you mean to expose. To log where the configuration came from, pass `cfg.Sources()`, which names files and the environment prefix and holds no key and no value.

---

## Environment-Based Config

Complete, equivalent YAML and JSON starter files plus an `.env.example` are available under [`examples/references/config`](../../examples/references/config/). They are versioned copyable references; choose one base format unless layered file merging is intentional.

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

Name each section once, with a typed descriptor: its key and its type, `config.Section[T]("key")`. Every read of the section takes the descriptor, so the key is spelled in one place and business code receives the typed value through DI. Field names map to snake_case config keys automatically (e.g. `MaxOpen` → `max_open`), so struct tags are optional — add a `credo:"..."` tag only when the key differs from the field's snake_case name:

```go
type AppConfig struct {
    Name        string
    SupportMail string
}

var AppSection = config.Section[AppConfig]("app")

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    // The section's value, decoded and validated when first resolved.
    app.Provide(AppSection.Decode)

    // Services receive AppConfig via constructor injection.
    app.Provide(NewMyService)
}

func NewMyService(infra credo.Infra, cfg AppConfig) *MyService {
    // cfg is fully typed — no string keys
    return &MyService{cfg: cfg}
}
```

`AppSection.Decode` is an ordinary constructor over the configuration the App registers, so a missing section or a failed `Validate()` is a constructor error at `Resolve` or at start that names the key, and a second binding of `AppConfig` panics at registration with the container's message.

A section that only one constructor reads is decoded there and not bound. The database is what business code needs, so the constructor that opens it reads its section:

```go
var DatabaseSection = config.Section[sqldb.Config]("databases.default")

app.Provide(func(rc credo.RawConfig) (*sqldb.DB, error) {
    cfg, err := DatabaseSection.Decode(rc)
    if err != nil {
        return nil, err
    }
    return sqldb.Open(&cfg)
})
```

The key appears **once**, in the descriptor. Beyond that, everything is typed and compile-time safe.

### Reading a Section in the Composition Root

`app.GetConfig(section)` reads one section of the App's configuration in a single call, the auto-loaded one included — resolving `RawConfig` would finalize the container and end registration, while `GetConfig` never does, so it is how registration code reads a section it needs itself. When you hold a `*config.Config` directly (for example the value returned by `config.Load`), the descriptor reads it: `AppSection.Decode(cfg)`.

```go
app, err := credo.New()
if err != nil {
    log.Fatal(err)
}

appCfg, err := app.GetConfig(AppSection) // T is inferred from the descriptor
if err != nil {
    log.Fatal(err)
}
```

Use `app.MustGetConfig(AppSection)` to panic on a missing or invalid required section — fail-fast startup wiring, mirroring `MustResolve`. A literal key keeps compiling, `app.GetConfig[AppConfig]("app")`, since the untyped constant converts to the descriptor. These are composition-root sugar: a handler has no `App` accessor, so config reading stays out of business code, and services still receive typed values via DI.

---

## Reloading Configuration

A section is a snapshot unless you declare it live. A long-running service can pick up changes to its config files without restarting: `app.Reload(ctx)` (or `SIGHUP` under `app.Run()` on Unix; see the [Deployment Guide](deployment.md)) re-reads every source the configuration was built from and replaces the value of every live section the change touches. Every other change is reported as needing a restart.

```go
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

var (
    AppSection    = config.Section[AppConfig]("app")   // static: a change takes a restart
    LimitsSection = config.Section[Limits]("limits")   // live: a reload replaces its value
)

app.Provide(AppSection.Decode)  // AppConfig
app.Provide(LimitsSection.Live) // *config.Live[Limits]
app.Provide(NewThrottle)

type Throttle struct {
    limits *config.Live[Limits]
}

// The signature says the value can change.
func NewThrottle(limits *config.Live[Limits]) *Throttle {
    return &Throttle{limits: limits}
}

func (t *Throttle) Allow(key string) bool {
    l := t.limits.Load() // once per operation: one value no reload changes under you
    return t.bucket(key, l.RPS, l.Burst).Allow()
}
```

State derived from a live section — a limiter, a compiled pattern — follows it through `OnChange`, which runs after the reload has published the new value:

```go
type RedactConfig struct {
    Pattern string // a regular expression
}

func (c RedactConfig) Validate() error {
    _, err := regexp.Compile(c.Pattern)
    return err
}

var RedactSection = config.Section[RedactConfig]("redact")

// In the composition root, over the *config.Config handed to credo.WithRawConfig.
redactCfg, err := RedactSection.Live(rawCfg)
if err != nil {
    log.Fatal(err)
}
var pattern atomic.Pointer[regexp.Regexp]
pattern.Store(regexp.MustCompile(redactCfg.Load().Pattern))
redactCfg.OnChange(func(ctx context.Context, prev, next RedactConfig) error {
    pattern.Store(regexp.MustCompile(next.Pattern)) // Validate has accepted it
    return nil
})
```

The log level and format need none of this: they are the framework's [`log` section](#logging--log), which a reload applies by itself.

The rules that make this safe:

- **Validate before publish, all or nothing.** Every live section the change touches is decoded from the _candidate_ and validated (`Validate() error` when `T` has one). One failure aborts the reload before anything is published: every live section keeps its value, no callback runs, and the error names the section (`config: reload: live section "limits": …; nothing was published, and every live section keeps its value`). A bad YAML edit can never half-apply.
- **One generation.** The snapshot and the changed sections are published together: once a `Load` returns a reload's value for one section, later `Load` calls return that reload's values or newer ones. Two `Load` calls can still straddle a reload, so values that must agree within one operation belong to one section.
- **Callbacks run in order and never roll back.** `OnChange` callbacks run in registration order for the sections the reload changed, with the reload's context. An error or a panic is joined into the error `Reload` returns, the later callbacks still run, and none is skipped when the context has ended.
- **What the type does not enforce.** Call `Load` in each operation rather than once at construction; derive state in `OnChange` rather than from a value read once; and treat a value's slices and maps as read-only, since every `Load` of one reload shares them.
- **One nature per section.** A section is static (`Decode`, `GetConfig`, `MustGetConfig`, and the framework's own `server` section) or live (`Live`, and the framework's own `log` section) within one configuration, and a read of the other nature fails, naming both reads: `config: section "limits" is read both as static, by …, and as live, by …`. A section nested in another, or one containing it, counts as the same section, so put the keys that need a restart and the keys a reload applies in separate sections. A raw `Unmarshal` with a string key is outside that check.
- **Changes outside a live section are reported, never applied.** Every key that differs from the value the start read and that no live section covers is reported on every reload until it reverts — never its value — and so is a file key that `.env` or the process environment keeps.

`app.Reload` returns what it did beside its error, and logs it (`credo: reload complete` at Info, the restart keys at Warn):

```go
res, err := app.Reload(ctx)
// res.Published        true when the reload published its snapshot
// res.Aborted          why nothing was published, naming the live section
// res.Changed          the changed keys
// res.Replaced         the live sections whose value was replaced, e.g. ["limits"]
// res.RestartRequired  e.g. ["app.name"]: a restart applies them
// res.Overridden       e.g. [{Key: "app.name", Source: "environment variable CREDO_APP__NAME"}]
```

Its JSON form (`published`, `aborted`, `changed`, `replaced`, `restart_required`, `overridden`) is what an admin endpoint can answer with ([Deployment Guide](deployment.md#reload-without-a-signal)). In a consumer's unit test, `config.Fixed(v)` is a `*config.Live[T]` whose value never changes.

`Section.Live` and `app.Reload` need a configuration that can reload: the `*config.Config` that `credo.New()` loads or `config.Load` returns. Over `testutil`'s configuration or a custom `credo.WithRawConfig` store, `Live` fails at construction and `app.Reload` returns an error naming the type and `config.Load`.

What a reload does **not** do:

- It never switches environments: `CREDO_ENV` is fixed at first load, so `config.dev.yaml` stays the overlay even if the variable changes.
- It never applies server settings. `server.host`, ports, timeouts, body limits, proxies and the `server.tls.*` paths are read by `credo.New()` once; changing them is a restart, and the reload reports it. Certificate files rotate on their own when they change on disk (see [TLS Server Config](#tls-server-config)).
- It cannot see environment changes the process never received. The process environment is re-read, but a supervisor's `EnvironmentFile=` is handed over at process start, so values sourced from env still require a restart.

The [Configuration Spec](../specs/config.md#reload-and-its-result) has the contracts.

---

## Multi-Database Config

For multiple databases, keep each config section separate. Both sections decode into `sqldb.Config`, so neither is bound: each is read by the constructor that opens its database, and the databases are what the container binds:

```go
var (
    PrimarySection   = config.Section[sqldb.Config]("databases.primary")
    AnalyticsSection = config.Section[sqldb.Config]("databases.analytics")
)

func setupDatabases(app *credo.App) {
    // Each database is bound under its own wrapper type and named for /ready
    // and the lifecycle; the binding alone pings it at start and reports it.
    app.Provide(func(rc credo.RawConfig) (PrimaryDB, error) {
        cfg, err := PrimarySection.Decode(rc)
        if err != nil {
            return PrimaryDB{}, err
        }
        db, err := sqldb.Open(&cfg)
        return PrimaryDB{db}, err
    }, credo.ComponentName("primary"))

    app.Provide(func(rc credo.RawConfig) (AnalyticsDB, error) {
        cfg, err := AnalyticsSection.Decode(rc)
        if err != nil {
            return AnalyticsDB{}, err
        }
        db, err := sqldb.Open(&cfg)
        return AnalyticsDB{db}, err
    }, credo.ComponentName("analytics"))
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

Use one section per logical connection. The [Data Access Guide](data-access.md#multiple-databases) shows the wrapper types `PrimaryDB` and `AnalyticsDB` (`struct{ *sqldb.DB }`) and how consumers inject them.

---

## Validation

If your config struct implements `Validate() error`, it is called automatically by `Unmarshal`, and so by a section's `Decode`:

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

After startup the pair is served through `GetCertificate` backed by an atomic pointer, and **the files rotate when they change on disk**, with no signal and no reload: the handshake that finds the last check older than a second stats both files and, when either changed, loads the pair and swaps it in. `os.Stat` follows symbolic links, so the link swap of a Kubernetes Secret volume counts as a change. New handshakes see the new certificate, open connections are untouched, and a pair that fails to load — a half-written pair, a key that does not match its certificate — keeps the current certificate serving and is logged at Error once per change, then retried when its other file changes. A certificate renewed in place (ACME, an internal PKI) therefore needs no deploy hook. The paths themselves are read at start: changing `server.tls.cert_file` / `key_file` takes a restart, which a reload reports.

TLS sources resolve by precedence:

```text
WithTLSConfig(*tls.Config) > WithTLSFiles(cert, key) > server.tls.* > plaintext
```

Each source is a whole-source override. For example, `WithTLSFiles` replaces both `server.tls.cert_file` and `server.tls.key_file`; it does not merge one path from the option with one path from config.

Use `WithTLSConfig` for embedded certificates, mTLS, SNI, custom TLS versions, or when you want to own certificate rotation yourself: Credo never touches a caller-supplied `*tls.Config`, so drive it through your own `GetCertificate`. To redirect plaintext HTTP callers to HTTPS, configure TLS and add `credo.WithHTTPRedirect(":80")`.

---

## Configuration Reference

Quick-lookup of the commonly used config keys.

### Server — `server`

Consumed automatically by `credo.New()`. The **Reloadable** column says what a [reload](#reloading-configuration) does with a change: `restart` keys are read once at startup, and a reload reports a change to them in its `restart_required` keys.

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
| `tls.cert_file` | string | `""` | restart (the file itself rotates when it changes) | PEM certificate file for HTTPS |
| `tls.key_file` | string | `""` | restart (the file itself rotates when it changes) | PEM private key file for HTTPS |

### Logging — `log`

Consumed automatically by `credo.New()` when no logger is passed with `credo.WithLogger`. The section selects the App's logger, written to stderr, and a [reload](#reloading-configuration) applies both keys to it and to every logger derived from it.

| Key | Type | Default | Reloadable | Description |
| --- | --- | --- | --- | --- |
| `level` | string | `info` | yes | Lowest level written: `debug`, `info`, `warn` or `error` |
| `format` | string | `text` | yes | Handler: `text` or `json` |

```yaml
log:
  level: info
  format: json
```

The values are matched without regard to case, so `CREDO_LOG__LEVEL=DEBUG` overrides the file. Three things to know:

- **The name is the framework's.** An application section called `log` with other keys fails `credo.New()` with the unknown key. Rename it, or build the logger yourself and pass it with `credo.WithLogger`: the section is then not read at all.
- **Write the section from the start if you mean to reload it.** A `log` section that first appears in a reload is reported in `restart_required` and changes nothing, and removing the section from a running service aborts the reload. To go back to the defaults, leave the key with no value under it (`log:`).
- **Anything else takes `WithLogger`.** Another writer, a source location, attribute rewriting or a handler of your own is a logger you build; the level and format keys do not apply to it.

Sections the application reads itself (`databases.*`, `i18n`, `auth.*`, and your own) are reloadable exactly when the application declares them live with `Section.Live`; a section bound through `Decode` is restart-only, and a reload reports its changed keys.

### Databases — `databases.<name>`

User-read through a section descriptor, `config.Section[sqldb.Config]("databases.<name>")`, decoded by the constructor that opens the database ([Multi-Database Config](#multi-database-config)).

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
| `ping_timeout` | duration | `0` (= `sqldb.DefaultPingTimeout`, 5s) | Deadline of the ping `(*sqldb.DB).Start` sends; unlike `connect_timeout`, zero is not "no deadline", and a negative value fails `Open` |
| `max_open` | int | `0` (unlimited) | Max open connections |
| `max_idle` | int or null | unset | Max idle connections; omitted uses the stdlib policy (subject to `max_open`), explicit `0` retains none |
| `max_idle_time` | duration | `0` (disabled) | Max idle age before a connection is closed |
| `max_lifetime` | duration | `0` (disabled) | Max connection lifetime |
| `ssl_mode` | string | `""` | `"disable"`, `"require"`, `"verify-full"` |
| `options` | map | `{}` | Driver-specific params |

`max_open: 0` is not a conservative production limit: it preserves `database/sql`'s unlimited-open behavior. A database's `Start` logs `sqldb.pool.max_open_unlimited` once its ping succeeds when the effective pool maximum is still unlimited; choose a finite value from the database connection budget divided across service replicas. When both are explicit and `max_open > 0`, `max_idle` must not exceed `max_open`.

### i18n — `i18n`

Auto-read by `app.UseI18n()` at the call; the locale directory itself is read in the start phase.

| Key       | Type   | Default      | Description               |
| --------- | ------ | ------------ | ------------------------- |
| `dir`     | string | `"locales/"` | Locale file directory     |
| `default` | string | `"en"`       | Default language (BCP 47) |

An `i18n.dir` value is explicit configuration: a missing or message-empty directory fails the start. An `i18n` section that does not decode makes `UseI18n` panic at the call. Only absent conventional `locales/` discovery from zero-config setup is optional. `I18nConfig.Messages`, `Fields`, `DirFS`, `Detect`, and `ResolveMessageKey` are Go-only startup inputs; programmatic maps are copied snapshots and are not part of RawConfig or runtime reload.

### Auth — `auth.*`

User-read via `rc.Unmarshal("auth.<strategy>", &cfg)`.

**JWT** — `auth.jwt`, for `jwtauth.Config` (`auth/jwtauth`):

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

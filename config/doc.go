// Package config provides struct-centric configuration loading with
// deterministic merge order. A single-pass [Load] merges files, the .env
// file, and environment variables into one nested map, exposed through the
// high-level Credo API focused on type safety and developer ergonomics.
//
// # Source Precedence
//
// Configuration merges in this order (later overrides earlier):
//
//  1. Base config files — all found (config.json, config.yaml, config.yml) merged
//  2. Env-specific files — when CREDO_ENV is set (via process env or the .env file).
//     Discovery mode: config.{env}.*. Explicit mode ([WithFiles]): name.{env}.ext.
//  3. .env file — resolved via [WithDotenvPath], CREDO_ENV_FILE, or default ".env";
//     read and parsed exactly once per Load. Disable with [WithoutDotenv].
//  4. Process environment variables — CREDO_* prefix (configurable).
//     Disable with [WithoutProcessEnv].
//
// Each opt-out removes its source entirely, bootstrap keys included:
// [WithoutProcessEnv] also ignores the env-sourced CREDO_ENV and
// CREDO_ENV_FILE values, and [WithoutDotenv] never reads any .env file.
// Combining both with [WithFiles] (or [LoadBytes]) yields fully hermetic
// loading where only the listed files (or the embedded document) are read.
//
// # Quick Start
//
//	type AppConfig struct {
//	    Debug bool     // auto-mapped to key "debug"
//	    DB    DBConfig `credo:"db"` // decodes the nested "app.db" sub-tree
//	}
//
//	type DBConfig struct {
//	    Host string // auto-mapped to key "host"
//	}
//
//	var AppSection = config.Section[AppConfig]("app")
//
//	c, err := config.Load()
//	if err != nil {
//	    log.Fatal(err)
//	}
//	cfg, err := AppSection.Decode(c)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
// A section decodes its own keys only. A whole-tree decode,
// [Config.Unmarshal] with the empty key, sees every key: each .env entry,
// which is read with no prefix, each prefixed environment variable, and the
// framework's server keys. Under strict decoding its target must cover them
// all, or the Config is loaded with [WithoutDotenv].
//
// # Sub-Tree Access
//
// A [Section] names a section and its type once; its [Section.Decode] reads
// it from any [RawConfig], and its method value is the constructor that binds
// the section in the container:
//
//	var ServerSection = config.Section[ServerConfig]("server")
//
//	serverCfg, err := ServerSection.Decode(c)
//	app.Provide(ServerSection.Decode)
//
// [Config.Unmarshal] decodes into an existing value:
//
//	var serverCfg ServerConfig
//	c.Unmarshal("server", &serverCfg)
//
// # Reload
//
// A section is a snapshot unless it is declared live. [Section.Live] binds a
// [Live] handle whose value each reload that changes the section replaces
// whole; [Live.Load] reads it once per operation, and [Live.OnChange] keeps
// derived state in step:
//
//	var Limits = config.Section[LimitsConfig]("limits")
//
//	app.Provide(Limits.Live) // *config.Live[LimitsConfig]
//
// A section is static or live within one configuration: reading one section
// through [Section.Decode] and through [Section.Live] fails, naming both
// reads. A raw [Config.Unmarshal] with a string key is outside that check.
//
// [Config.Reload] re-reads every source the Config was built from, validates
// every live section the change touches before anything is published, then
// publishes the snapshot and the changed sections as one generation and runs
// their callbacks. It returns a [ReloadResult] naming the keys that changed,
// the sections it replaced, the keys whose change takes a restart and the file
// keys a later source keeps. It never switches CREDO_ENV, and a failed re-read
// or validation publishes nothing. The credo package calls it from SIGHUP and
// app.Reload (ADR-020).
//
// # Strict Decoding
//
// Every typed decode is strict: a key that maps to no field of the target is
// an error, and a value is not converted across kinds — a boolean into a
// number, a number into a string, a scalar into a list. Strings parse into
// the basic type of their target, so environment and .env overrides, which
// are always strings, decode into int, bool, float64 and duration fields, as
// they do into [encoding.TextUnmarshaler] targets. Each failure names its
// full key path. [WithWeakDecoding] opts in to mapstructure's weak coercion,
// under which unknown keys are ignored.
//
// # Custom Prefix
//
// Use [WithPrefix] to override the default "CREDO_" env var prefix:
//
//	c, err := config.Load(config.WithPrefix("MYAPP_"))
//
// # Adapted From
//
// Map-merge utilities adapted from koanf (MIT).
// See NOTICES file for full attribution.
//
// Maturity: beta
package config

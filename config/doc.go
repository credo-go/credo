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
//     read and parsed exactly once per Load
//  4. Process environment variables — CREDO_* prefix (configurable)
//
// # Quick Start
//
//	type AppConfig struct {
//	    Port  int    `credo:"server.port"`
//	    Debug bool   `credo:"debug"`
//	}
//
//	c, err := config.Load()
//	if err != nil {
//	    log.Fatal(err)
//	}
//	var cfg AppConfig
//	if err := c.Unmarshal("", &cfg); err != nil {
//	    log.Fatal(err)
//	}
//
// # Sub-Tree Access
//
// For granular key access, use [Config.Unmarshal]:
//
//	var serverCfg ServerConfig
//	c.Unmarshal("server", &serverCfg)
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
// Maturity: experimental
package config

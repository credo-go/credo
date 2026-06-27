package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Format constants for use with [LoadBytes].
const (
	FormatJSON = "json"
	FormatYAML = "yaml"
)

// bootstrapKeys are framework-internal keys that control loading behavior
// and must not leak into the merged configuration.
var bootstrapKeys = map[string]bool{
	"CREDO_ENV":      true,
	"CREDO_ENV_FILE": true,
}

// Load creates a Config instance and loads all configuration sources.
// Sources are merged in precedence order (lowest to highest):
//
//  1. Base config files — all found among candidates, merged in order
//  2. Env-specific files — config.{CREDO_ENV}.* when CREDO_ENV is set
//  3. .env file (CREDO_ENV_FILE or default ".env") — all entries loaded, no prefix filtering
//  4. Process environment variables (prefix-filtered, default "CREDO_")
//
// The .env file is read and parsed once: CREDO_ENV is taken from it before
// file loading (the process environment wins), and its entries are merged
// into the config tree after file loading.
//
// Load is NOT concurrency-safe; call it once at startup.
//
// The returned *Config satisfies [RawConfig]; use [Config.Get] for typed
// snapshot access or pass it to credo.WithRawConfig as-is.
func Load(opts ...Option) (*Config, error) {
	c := newConfig(opts...)
	dotenv, err := c.readDotenv()
	if err != nil {
		return nil, fmt.Errorf("config: load .env: %w", err)
	}
	if err := c.loadFiles(credoEnv(dotenv)); err != nil {
		return nil, fmt.Errorf("config: load config file: %w", err)
	}
	c.mergeDotenv(dotenv)
	c.mergeEnv(os.Environ())
	return c, nil
}

// LoadBytes creates a Config from raw bytes. After parsing, .env and
// environment variable layers are applied on top (same as [Load]).
// This is useful with go:embed to bundle config files in the binary.
//
//	//go:embed config.json
//	var configData []byte
//
//	rc, err := config.LoadBytes(configData, config.FormatJSON)
//
// The returned *Config satisfies [RawConfig]; use [Config.Get] for typed
// snapshot access or pass it to credo.WithRawConfig as-is.
func LoadBytes(data []byte, format string, opts ...Option) (*Config, error) {
	c := newConfig(opts...)
	m, err := parseConfig(data, format)
	if err != nil {
		return nil, fmt.Errorf("config: load bytes: %w", err)
	}
	c.merge(m)
	dotenv, err := c.readDotenv()
	if err != nil {
		return nil, fmt.Errorf("config: load .env: %w", err)
	}
	c.mergeDotenv(dotenv)
	c.mergeEnv(os.Environ())
	return c, nil
}

// parseConfig parses raw JSON or YAML bytes into a string-keyed nested map.
// format accepts bare names ("json", "yaml", "yml") and file extensions
// (".json", ".yaml", ".yml"), case-insensitively.
func parseConfig(data []byte, format string) (map[string]any, error) {
	switch strings.ToLower(strings.TrimPrefix(format, ".")) {
	case FormatJSON:
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return out, nil
	case FormatYAML, "yml":
		var out map[string]any
		if err := yaml.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		// YAML may produce map[any]any for non-string keys; normalize.
		return intfaceKeysToStrings(out), nil
	default:
		return nil, fmt.Errorf("unsupported format: %q", format)
	}
}

// --- config files ---

// loadFiles loads config files into the config tree. Both discovery mode
// (default) and explicit mode ([WithFiles]) merge env-specific files on
// top of base files when env (the effective CREDO_ENV) is non-empty.
func (c *Config) loadFiles(env string) error {
	if c.opts.explicit {
		return c.loadExplicitFiles(env)
	}
	return c.loadDiscoveryFiles(env)
}

// loadExplicitFiles loads all found files from the explicit list and
// merges them in order. At least one base file must exist. When env is
// non-empty, env-specific files are derived and merged on top (optional).
func (c *Config) loadExplicitFiles(env string) error {
	// Phase 1: base files (REQUIRED — at least one must exist).
	found := false
	for _, f := range c.opts.files {
		loaded, err := c.tryLoadFile(f)
		if err != nil {
			return err
		}
		if loaded {
			found = true
		}
	}
	if !found && len(c.opts.files) > 0 {
		return fmt.Errorf("none of the specified config files were found: %s",
			strings.Join(c.opts.files, ", "))
	}

	// Phase 2: env-specific derived files (OPTIONAL — missing silently skipped).
	if env != "" {
		for _, f := range c.opts.files {
			if _, err := c.tryLoadFile(deriveEnvFile(f, env)); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadDiscoveryFiles loads all found base config files, then merges
// env-specific files on top when CREDO_ENV is set.
func (c *Config) loadDiscoveryFiles(env string) error {
	// Base files — all found are merged in order.
	for _, f := range c.opts.files {
		if _, err := c.tryLoadFile(f); err != nil {
			return err
		}
	}

	// Env-specific files — only when CREDO_ENV is set.
	if env != "" {
		for _, f := range c.opts.files {
			if _, err := c.tryLoadFile(deriveEnvFile(f, env)); err != nil {
				return err
			}
		}
	}
	return nil
}

// tryLoadFile reads and merges a single config file. Returns (true, nil)
// when loaded, (false, nil) when the file does not exist (silent skip),
// or (false, err) on a read or parse error.
func (c *Config) tryLoadFile(f string) (bool, error) {
	data, err := os.ReadFile(f)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil // file not found — skip
		}
		return false, err
	}
	m, err := parseConfig(data, filepath.Ext(f))
	if err != nil {
		return false, fmt.Errorf("%s: %w", f, err)
	}
	c.merge(m)
	return true, nil
}

// deriveEnvFile inserts the environment name before the file extension.
// For example, deriveEnvFile("myapp.yaml", "production") returns
// "myapp.production.yaml". If the file has no extension, ".{env}" is
// appended (e.g., "config" becomes "config.production").
func deriveEnvFile(path, env string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return path + "." + env
	}
	return strings.TrimSuffix(path, ext) + "." + env + ext
}

// --- .env file ---

// credoEnv returns the effective CREDO_ENV value: the process environment
// wins over the .env file. Empty when neither source sets it.
func credoEnv(dotenv map[string]string) string {
	if env := os.Getenv("CREDO_ENV"); env != "" {
		return env
	}
	return dotenv["CREDO_ENV"]
}

// readDotenv resolves the .env path and parses the file once. The returned
// map is nil when no .env applies (missing default file, or missing
// explicit file with [WithDotenvOptional]).
//
// Resolution order: [WithDotenvPath] (programmatic) > CREDO_ENV_FILE
// (env var) > ".env" (default). When an explicit path is used (via either
// mechanism), a missing file is an error unless [WithDotenvOptional] was
// set. The default implicit ".env" is always optional.
func (c *Config) readDotenv() (map[string]string, error) {
	path := c.opts.dotenvPath
	explicit := path != ""
	if !explicit {
		path = os.Getenv("CREDO_ENV_FILE")
		explicit = path != ""
	}
	if !explicit {
		path = ".env"
	}

	data, err := os.ReadFile(path) //nolint:gosec // Dotenv path is intentionally caller/env configured at startup.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if !explicit {
				return nil, nil // default .env missing — silently ignore
			}
			if c.opts.dotenvOptional {
				c.logger().Warn("dotenv file not found, skipping", "path", path)
				return nil, nil
			}
		}
		return nil, err
	}

	pairs, err := parseDotenv(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return pairs, nil
}

// mergeDotenv merges parsed .env pairs into the config tree. Keys are normalized
// (lowercase, "__" → "."); no prefix filtering is applied — .env files are
// project-scoped and need no namespace isolation. Bootstrap keys
// (CREDO_ENV, CREDO_ENV_FILE) are excluded.
func (c *Config) mergeDotenv(pairs map[string]string) {
	if len(pairs) == 0 {
		return
	}
	flat := make(map[string]any, len(pairs))
	for key, val := range pairs {
		if bootstrapKeys[key] {
			continue
		}
		if k := normalizeKey(key); k != "" {
			flat[k] = val
		}
	}
	c.merge(unflatten(flat))
}

// --- process environment ---

// mergeEnv merges process environment variables matching the configured
// prefix into the config tree. The prefix is stripped and keys are normalized
// (lowercase, "__" → "."). Bootstrap keys are excluded.
//
//	CREDO_SERVER__PORT         → server.port
//	CREDO_SERVER__READ_TIMEOUT → server.read_timeout
//	CREDO_DB__MAX_OPEN_CONNS   → db.max_open_conns
func (c *Config) mergeEnv(environ []string) {
	flat := make(map[string]any)
	for _, kv := range environ {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if c.opts.prefix != "" && !strings.HasPrefix(key, c.opts.prefix) {
			continue
		}
		if bootstrapKeys[key] {
			continue
		}
		if k := normalizeKey(strings.TrimPrefix(key, c.opts.prefix)); k != "" {
			flat[k] = val
		}
	}
	c.merge(unflatten(flat))
}

// normalizeKey converts an environment-style key to a config key path:
// lowercase, with "__" as the nesting delimiter ("_" stays).
func normalizeKey(key string) string {
	return strings.ReplaceAll(strings.ToLower(key), "__", keyDelim)
}

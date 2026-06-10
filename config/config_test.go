package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/credo-go/credo/config"
)

// testAppConfig is a sample config struct for integration tests.
type testAppConfig struct {
	Server struct {
		Port        int           `credo:"port"`
		Host        string        `credo:"host"`
		ReadTimeout time.Duration `credo:"read_timeout"`
	} `credo:"server"`
	Debug bool   `credo:"debug"`
	Name  string `credo:"name"`
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
server:
  port: 8080
  host: localhost
  read_timeout: 30s
debug: false
name: testapp
`), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(config.WithFiles(yamlPath), config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg testAppConfig
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("Port: got %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.Host != "localhost" {
		t.Errorf("Host: got %q, want localhost", cfg.Server.Host)
	}
	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout: got %v, want 30s", cfg.Server.ReadTimeout)
	}
	if cfg.Debug != false {
		t.Errorf("Debug: got %v, want false", cfg.Debug)
	}
	if cfg.Name != "testapp" {
		t.Errorf("Name: got %q, want testapp", cfg.Name)
	}
}

func TestLoadFromJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(jsonPath, []byte(`{
		"server": {"port": 3000, "host": "0.0.0.0"},
		"name": "jsonapp"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(config.WithFiles(jsonPath), config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg testAppConfig
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Server.Port != 3000 {
		t.Errorf("Port: got %d, want 3000", cfg.Server.Port)
	}
	if cfg.Name != "jsonapp" {
		t.Errorf("Name: got %q, want jsonapp", cfg.Name)
	}
}

func TestLoadPrecedence(t *testing.T) {
	// Setup: config.yaml (port=3000) < .env (port=8080) < env var (port=9090)
	dir := t.TempDir()

	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
server:
  port: 3000
  host: from-yaml
name: yamlapp
`), 0o644); err != nil {
		t.Fatal(err)
	}

	dotenvPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(dotenvPath, []byte("SERVER__PORT=8080\nSERVER__HOST=from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CREDO_ENV_FILE", dotenvPath)
	t.Setenv("CREDO_SERVER__PORT", "9090")

	c, err := config.Load(config.WithFiles(yamlPath))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg testAppConfig
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Env var overrides .env overrides YAML.
	if cfg.Server.Port != 9090 {
		t.Errorf("Port: got %d, want 9090 (env var should override)", cfg.Server.Port)
	}
	// .env overrides YAML for host.
	if cfg.Server.Host != "from-dotenv" {
		t.Errorf("Host: got %q, want from-dotenv (.env should override yaml)", cfg.Server.Host)
	}
	// Name from YAML (not overridden).
	if cfg.Name != "yamlapp" {
		t.Errorf("Name: got %q, want yamlapp", cfg.Name)
	}
}

func TestLoadCascadeMerge(t *testing.T) {
	dir := t.TempDir()

	// Create both config.json and config.yaml with overlapping + unique keys.
	// Both should be loaded and merged; YAML overrides JSON for overlapping keys.
	jsonPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(jsonPath, []byte(`{"name":"from-json","port":3000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("name: from-yaml\ndebug: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(
		config.WithFiles(jsonPath, yamlPath),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name  string `credo:"name"`
		Port  int    `credo:"port"`
		Debug bool   `credo:"debug"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// YAML overrides JSON for overlapping "name" key.
	if cfg.Name != "from-yaml" {
		t.Errorf("Name: got %q, want from-yaml (later file wins)", cfg.Name)
	}
	// JSON-only key "port" is preserved.
	if cfg.Port != 3000 {
		t.Errorf("Port: got %d, want 3000 (non-overlapping key preserved)", cfg.Port)
	}
	// YAML-only key "debug" is preserved.
	if !cfg.Debug {
		t.Errorf("Debug: got false, want true (non-overlapping key preserved)")
	}
}

func TestLoadExplicitFileMissing(t *testing.T) {
	// WithFiles with a non-existent file must return an error.
	_, err := config.Load(
		config.WithFiles("/nonexistent/config.yaml"),
		config.WithPrefix("NOTSET_"),
	)
	if err == nil {
		t.Fatal("expected error when explicit config file is missing")
	}
}

func TestLoadExplicitFilesEmpty(t *testing.T) {
	// WithFiles() with an empty list explicitly disables file loading — no error.
	c, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Store is empty, keyed Unmarshal should fail.
	var cfg testAppConfig
	if err := c.Unmarshal("server", &cfg); err == nil {
		t.Fatal("expected error for missing key on empty store")
	}
}

func TestLoadDefaultDiscoveryMissingSilent(t *testing.T) {
	// Default discovery list (no WithFiles) — missing files are silently skipped.
	_, err := config.Load(config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v (default discovery missing should be silent)", err)
	}
}

// validatingConfig implements Validate() error.
type validatingConfig struct {
	Port int `credo:"port"`
}

func (c *validatingConfig) Validate() error {
	if c.Port == 0 {
		return errors.New("port is required")
	}
	return nil
}

func TestUnmarshalValidation(t *testing.T) {
	t.Run("validation passes", func(t *testing.T) {
		dir := t.TempDir()
		yamlPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(yamlPath, []byte("port: 8080\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		c, err := config.Load(config.WithFiles(yamlPath), config.WithPrefix("NOTSET_"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		var cfg validatingConfig
		if err := c.Unmarshal("", &cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if cfg.Port != 8080 {
			t.Errorf("Port: got %d, want 8080", cfg.Port)
		}
	})

	t.Run("validation fails", func(t *testing.T) {
		dir := t.TempDir()
		yamlPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(yamlPath, []byte("port: 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		c, err := config.Load(config.WithFiles(yamlPath), config.WithPrefix("NOTSET_"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		var cfg validatingConfig
		err = c.Unmarshal("", &cfg)
		if err == nil {
			t.Fatal("expected validation error")
		}
		if cfg.Port != 0 {
			t.Errorf("Port: got %d, want 0", cfg.Port)
		}
	})
}

func TestLoadReturnsRawConfig(t *testing.T) {
	// config.Load() returns credo.RawConfig interface.
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("name: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var rc config.RawConfig
	var err error
	rc, err = config.Load(config.WithFiles(yamlPath), config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Verify interface methods work.
	if !rc.Exists("name") {
		t.Error("expected 'name' key to exist")
	}

	var name string
	if err := rc.Unmarshal("name", &name); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if name != "test" {
		t.Errorf("got %q, want %q", name, "test")
	}
}

// validatingServerConfig validates a sub-tree config.
type validatingServerConfig struct {
	Port int    `credo:"port"`
	Host string `credo:"host"`
}

func (c *validatingServerConfig) Validate() error {
	if c.Port <= 0 {
		return errors.New("server port must be positive")
	}
	return nil
}

func TestUnmarshalKeyedPathValidation(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("server:\n  port: 8080\n  host: localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(config.WithFiles(yamlPath), config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Run("keyed path validation passes", func(t *testing.T) {
		var cfg validatingServerConfig
		if err := c.Unmarshal("server", &cfg); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if cfg.Port != 8080 {
			t.Errorf("Port: got %d, want 8080", cfg.Port)
		}
	})

	t.Run("keyed path validation fails", func(t *testing.T) {
		// No config files loaded → store is empty.
		emptyC, err := config.Load(
			config.WithFiles(),
			config.WithPrefix("NOTSET_"),
		)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// server key doesn't exist, Unmarshal should error (key not found).
		var cfg validatingServerConfig
		err = emptyC.Unmarshal("server", &cfg)
		if err == nil {
			t.Fatal("expected error for missing server key")
		}
	})
}

func TestLoadDotenvExplicitMissing(t *testing.T) {
	// When CREDO_ENV_FILE is set to a non-existent path, Load should error.
	t.Setenv("CREDO_ENV_FILE", "/nonexistent/.env.prod")

	_, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
	)
	if err == nil {
		t.Error("expected error when CREDO_ENV_FILE points to missing file")
	}
}

func TestLoadDotenvDefaultMissingSilent(t *testing.T) {
	// When CREDO_ENV_FILE is not set and default .env is missing, no error.
	// Ensure CREDO_ENV_FILE is not set.
	t.Setenv("CREDO_ENV_FILE", "")
	// Clear it completely by unsetting (t.Setenv with empty still sets it).
	os.Unsetenv("CREDO_ENV_FILE")

	_, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v (default .env missing should be silent)", err)
	}
}

func TestLoadWithDotenvPath(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "custom.env")
	os.WriteFile(envPath, []byte("MY_KEY=from_custom\n"), 0o644)

	os.Unsetenv("CREDO_ENV_FILE")

	c, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
		config.WithDotenvPath(envPath),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var got string
	if err := c.Unmarshal("my_key", &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != "from_custom" {
		t.Errorf("MY_KEY = %q, want from_custom", got)
	}
}

func TestLoadWithDotenvPathOverridesEnvVar(t *testing.T) {
	dir := t.TempDir()
	optionPath := filepath.Join(dir, "option.env")
	os.WriteFile(optionPath, []byte("SRC=option\n"), 0o644)
	envvarPath := filepath.Join(dir, "envvar.env")
	os.WriteFile(envvarPath, []byte("SRC=envvar\n"), 0o644)

	t.Setenv("CREDO_ENV_FILE", envvarPath)

	c, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
		config.WithDotenvPath(optionPath),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var got string
	c.Unmarshal("src", &got)
	if got != "option" {
		t.Errorf("SRC = %q, want option (WithDotenvPath takes precedence)", got)
	}
}

func TestLoadWithDotenvPathMissingIsError(t *testing.T) {
	os.Unsetenv("CREDO_ENV_FILE")

	_, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
		config.WithDotenvPath("/nonexistent/custom.env"),
	)
	if err == nil {
		t.Error("expected error when WithDotenvPath file is missing")
	}
}

func TestLoadWithDotenvOptionalMissingSilent(t *testing.T) {
	os.Unsetenv("CREDO_ENV_FILE")

	_, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
		config.WithDotenvPath("/nonexistent/custom.env"),
		config.WithDotenvOptional(),
	)
	if err != nil {
		t.Fatalf("Load: %v (WithDotenvOptional should skip missing file)", err)
	}
}

func TestLoadWithDotenvOptionalAndEnvFile(t *testing.T) {
	t.Setenv("CREDO_ENV_FILE", "/nonexistent/envfile.env")

	_, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
		config.WithDotenvOptional(),
	)
	if err != nil {
		t.Fatalf("Load: %v (WithDotenvOptional should skip missing CREDO_ENV_FILE)", err)
	}
}

func TestLoadCredoEnvFromDotenvPathOption(t *testing.T) {
	// CREDO_ENV inside a WithDotenvPath file triggers env-specific derivation.
	dir := t.TempDir()
	basePath := filepath.Join(dir, "myapp.yaml")
	if err := os.WriteFile(basePath, []byte("port: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envSpecific := filepath.Join(dir, "myapp.staging.yaml")
	if err := os.WriteFile(envSpecific, []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dotenvPath := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(dotenvPath, []byte("CREDO_ENV=staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	os.Unsetenv("CREDO_ENV")
	os.Unsetenv("CREDO_ENV_FILE")

	c, err := config.Load(
		config.WithFiles(basePath),
		config.WithPrefix("NOTSET_"),
		config.WithDotenvPath(dotenvPath),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Port int `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port: got %d, want 8080 (CREDO_ENV from WithDotenvPath file)", cfg.Port)
	}
}

func TestLoadMapFieldName(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("debug: true\napi_key: secret123\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(config.WithFiles(yamlPath), config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Debug  bool   `credo:"debug"`
		APIKey string // no credo tag -> MapFieldName converts to "api_key"
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !cfg.Debug {
		t.Error("Debug: got false, want true")
	}
	// MapFieldName converts "APIKey" to "api_key", matching the YAML key directly.
	if cfg.APIKey != "secret123" {
		t.Errorf("APIKey: got %q, want secret123", cfg.APIKey)
	}
}

func TestLoadWithCustomPrefix(t *testing.T) {
	t.Setenv("MYAPP_PORT", "7070")

	c, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("MYAPP_"),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Port int `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Port != 7070 {
		t.Errorf("Port: got %d, want 7070", cfg.Port)
	}
}

func TestLoadMapStringT(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
databases:
  default:
    host: localhost
    port: 5432
  analytics:
    host: analytics-server
    port: 9000
`), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(config.WithFiles(yamlPath), config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	type DBConfig struct {
		Host string `credo:"host"`
		Port int    `credo:"port"`
	}
	var cfg struct {
		Databases map[string]DBConfig `credo:"databases"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(cfg.Databases) != 2 {
		t.Fatalf("Databases: got %d entries, want 2", len(cfg.Databases))
	}
	if cfg.Databases["default"].Host != "localhost" {
		t.Errorf("default.host: got %q", cfg.Databases["default"].Host)
	}
	if cfg.Databases["analytics"].Port != 9000 {
		t.Errorf("analytics.port: got %d", cfg.Databases["analytics"].Port)
	}
}

func TestUnmarshalEmptyStore(t *testing.T) {
	// Unmarshal("", &cfg) on an empty store must return an error
	// instead of silently producing zero-value fields.
	c, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg testAppConfig
	err = c.Unmarshal("", &cfg)
	if err == nil {
		t.Fatal("expected error for Unmarshal on empty store")
	}
}

func TestEnvVarOverridesDotenvOverridesYAML(t *testing.T) {
	dir := t.TempDir()

	// YAML: port=1000
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("server:\n  port: 1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// .env: port=2000
	dotenvPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(dotenvPath, []byte("SERVER__PORT=2000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// env var: port=3000
	t.Setenv("CREDO_ENV_FILE", dotenvPath)
	t.Setenv("CREDO_SERVER__PORT", "3000")

	c, err := config.Load(config.WithFiles(yamlPath))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Server struct {
			Port int `credo:"port"`
		} `credo:"server"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Server.Port != 3000 {
		t.Errorf("Port: got %d, want 3000 (env > .env > yaml)", cfg.Server.Port)
	}
}

func TestLoadCascadeEnvSpecific(t *testing.T) {
	dir := t.TempDir()

	// Base config.
	basePath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(basePath, []byte("name: base\nport: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Env-specific config (production).
	envPath := filepath.Join(dir, "config.production.yaml")
	if err := os.WriteFile(envPath, []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CREDO_ENV", "production")

	c, err := config.Load(
		config.WithFiles(basePath),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
		Port int    `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Explicit mode now derives env-specific files.
	// config.production.yaml overrides port from 3000 to 8080.
	if cfg.Port != 8080 {
		t.Errorf("Port: got %d, want 8080 (env-specific derived in explicit mode)", cfg.Port)
	}
	// Base name preserved (not in env-specific file).
	if cfg.Name != "base" {
		t.Errorf("Name: got %q, want base", cfg.Name)
	}
}

func TestLoadDiscoveryCascadeEnvSpecific(t *testing.T) {
	// This test uses discovery mode where CREDO_ENV derivation IS applied.
	// We need config files in the current working directory, so we use
	// a temp dir and chdir into it.
	dir := t.TempDir()
	t.Chdir(dir)

	// Base config.
	if err := os.WriteFile("config.yaml", []byte("name: base\nport: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Env-specific config.
	if err := os.WriteFile("config.production.yaml", []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CREDO_ENV", "production")

	c, err := config.Load(config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
		Port int    `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Base name preserved.
	if cfg.Name != "base" {
		t.Errorf("Name: got %q, want base", cfg.Name)
	}
	// Env-specific overrides port.
	if cfg.Port != 8080 {
		t.Errorf("Port: got %d, want 8080 (env-specific overrides base)", cfg.Port)
	}
}

func TestLoadCascadeNoEnv(t *testing.T) {
	// When CREDO_ENV is not set, only base files are loaded.
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile("config.yaml", []byte("name: base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// This file should NOT be loaded without CREDO_ENV.
	if err := os.WriteFile("config.production.yaml", []byte("name: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ensure CREDO_ENV is not set.
	os.Unsetenv("CREDO_ENV")

	c, err := config.Load(config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Name != "base" {
		t.Errorf("Name: got %q, want base (no CREDO_ENV → no env-specific file)", cfg.Name)
	}
}

func TestCREDO_ENV_ExcludedFromStore(t *testing.T) {
	// CREDO_ENV should not leak into the config store as a key.
	t.Setenv("CREDO_ENV", "production")

	c, err := config.Load(config.WithFiles(), config.WithPrefix("CREDO_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.Exists("env") {
		t.Error("CREDO_ENV should be excluded from the config store")
	}
}

func TestLoadBytesJSON(t *testing.T) {
	data := []byte(`{"name":"embedded","port":4000}`)
	c, err := config.LoadBytes(data, config.FormatJSON, config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
		Port int    `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Name != "embedded" {
		t.Errorf("Name: got %q, want embedded", cfg.Name)
	}
	if cfg.Port != 4000 {
		t.Errorf("Port: got %d, want 4000", cfg.Port)
	}
}

func TestLoadBytesYAML(t *testing.T) {
	data := []byte("name: yaml-embed\nport: 5000\n")
	c, err := config.LoadBytes(data, config.FormatYAML, config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
		Port int    `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Name != "yaml-embed" {
		t.Errorf("Name: got %q, want yaml-embed", cfg.Name)
	}
	if cfg.Port != 5000 {
		t.Errorf("Port: got %d, want 5000", cfg.Port)
	}
}

func TestLoadBytesInvalidFormat(t *testing.T) {
	_, err := config.LoadBytes([]byte("data"), "toml")
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestLoadBytesEnvOverride(t *testing.T) {
	data := []byte(`{"port":3000}`)
	t.Setenv("CREDO_PORT", "9090")

	c, err := config.LoadBytes(data, config.FormatJSON)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	var cfg struct {
		Port int `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port: got %d, want 9090 (env var should override embedded)", cfg.Port)
	}
}

// --- CREDO_ENV resolution and explicit mode derivation tests ---

func TestDeriveEnvFile(t *testing.T) {
	tests := []struct {
		path string
		env  string
		want string
	}{
		{"config.yaml", "production", "config.production.yaml"},
		{"config.json", "staging", "config.staging.json"},
		{"config.yml", "test", "config.test.yml"},
		{"myapp.yaml", "production", "myapp.production.yaml"},
		{"app.config.yml", "dev", "app.config.dev.yml"},
		{"config", "production", "config.production"},
	}
	for _, tt := range tests {
		t.Run(tt.path+"_"+tt.env, func(t *testing.T) {
			got := config.ExportDeriveEnvFile(tt.path, tt.env)
			if got != tt.want {
				t.Errorf("deriveEnvFile(%q, %q) = %q, want %q", tt.path, tt.env, got, tt.want)
			}
		})
	}
}

func TestDeriveEnvFileAbsolutePath(t *testing.T) {
	got := config.ExportDeriveEnvFile("/abs/path/app.json", "prod")
	want := "/abs/path/app.prod.json"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLoadCredoEnvFromDotenv(t *testing.T) {
	// CREDO_ENV in .env (no process env var) triggers env-specific file loading.
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile("config.yaml", []byte("name: base\nport: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.production.yaml", []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".env", []byte("CREDO_ENV=production\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ensure CREDO_ENV is NOT set as process env var.
	os.Unsetenv("CREDO_ENV")
	os.Unsetenv("CREDO_ENV_FILE")

	c, err := config.Load(config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
		Port int    `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("Port: got %d, want 8080 (CREDO_ENV from .env should trigger env-specific load)", cfg.Port)
	}
	if cfg.Name != "base" {
		t.Errorf("Name: got %q, want base", cfg.Name)
	}
}

func TestLoadCredoEnvProcessOverridesDotenv(t *testing.T) {
	// Process env CREDO_ENV takes priority over .env CREDO_ENV.
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile("config.yaml", []byte("name: base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.staging.yaml", []byte("name: staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.production.yaml", []byte("name: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// .env says staging, process env says production.
	if err := os.WriteFile(".env", []byte("CREDO_ENV=staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	os.Unsetenv("CREDO_ENV_FILE")
	t.Setenv("CREDO_ENV", "production")

	c, err := config.Load(config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Name != "production" {
		t.Errorf("Name: got %q, want production (process env wins over .env)", cfg.Name)
	}
}

func TestLoadExplicitCredoEnvFromDotenv(t *testing.T) {
	// CREDO_ENV from .env triggers derivation in explicit mode too.
	dir := t.TempDir()

	basePath := filepath.Join(dir, "myapp.yaml")
	if err := os.WriteFile(basePath, []byte("port: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "myapp.production.yaml")
	if err := os.WriteFile(envPath, []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dotenvPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(dotenvPath, []byte("CREDO_ENV=production\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	os.Unsetenv("CREDO_ENV")
	t.Setenv("CREDO_ENV_FILE", dotenvPath)

	c, err := config.Load(
		config.WithFiles(basePath),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Port int `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("Port: got %d, want 8080 (CREDO_ENV from .env in explicit mode)", cfg.Port)
	}
}

func TestLoadExplicitDerivedFileMissingSilent(t *testing.T) {
	// Derived env-specific file missing is not an error.
	dir := t.TempDir()

	basePath := filepath.Join(dir, "myapp.yaml")
	if err := os.WriteFile(basePath, []byte("port: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No myapp.production.yaml — derived file is optional.

	t.Setenv("CREDO_ENV", "production")

	c, err := config.Load(
		config.WithFiles(basePath),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v (derived file missing should be silent)", err)
	}

	var cfg struct {
		Port int `credo:"port"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Port != 3000 {
		t.Errorf("Port: got %d, want 3000 (no derived file, base value preserved)", cfg.Port)
	}
}

func TestLoadExplicitMultipleFilesDerived(t *testing.T) {
	// Each explicit file gets its own env-specific derivation.
	dir := t.TempDir()

	base1 := filepath.Join(dir, "base.yaml")
	if err := os.WriteFile(base1, []byte("name: base1\nport: 1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base2 := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(base2, []byte(`{"debug": true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	env1 := filepath.Join(dir, "base.production.yaml")
	if err := os.WriteFile(env1, []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env2 := filepath.Join(dir, "overrides.production.json")
	if err := os.WriteFile(env2, []byte(`{"debug": false}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CREDO_ENV", "production")

	c, err := config.Load(
		config.WithFiles(base1, base2),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name  string `credo:"name"`
		Port  int    `credo:"port"`
		Debug bool   `credo:"debug"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Name != "base1" {
		t.Errorf("Name: got %q, want base1", cfg.Name)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port: got %d, want 8080 (from base.production.yaml)", cfg.Port)
	}
	if cfg.Debug {
		t.Error("Debug: got true, want false (from overrides.production.json)")
	}
}

func TestLoadNoCredoEnvNoEnvSpecific(t *testing.T) {
	// No CREDO_ENV anywhere (no process env, no .env) -> no derivation.
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile("config.yaml", []byte("name: base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.production.yaml", []byte("name: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	os.Unsetenv("CREDO_ENV")
	os.Unsetenv("CREDO_ENV_FILE")

	c, err := config.Load(config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var cfg struct {
		Name string `credo:"name"`
	}
	if err := c.Unmarshal("", &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if cfg.Name != "base" {
		t.Errorf("Name: got %q, want base (no CREDO_ENV -> no env-specific)", cfg.Name)
	}
}

func TestLoadExplicitEmptyFilesWithCredoEnv(t *testing.T) {
	// Empty file list + CREDO_ENV -> no derivation attempted, no error.
	t.Setenv("CREDO_ENV", "production")

	_, err := config.Load(
		config.WithFiles(),
		config.WithPrefix("NOTSET_"),
	)
	if err != nil {
		t.Fatalf("Load: %v (empty files + CREDO_ENV should not error)", err)
	}
}

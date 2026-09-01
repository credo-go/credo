package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeTestFile writes a file for source-opt-out tests, failing the test on error.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestWithoutProcessEnv verifies that the process environment is ignored as a
// whole: the merge layer, the env-sourced CREDO_ENV (no env-specific file
// derivation), and CREDO_ENV_FILE (no .env resolution through it).
func TestWithoutProcessEnv(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.json")
	writeTestFile(t, base, `{"app":{"name":"file"}}`)
	writeTestFile(t, filepath.Join(dir, "config.prod.json"), `{"app":{"name":"prod"}}`)
	customEnv := filepath.Join(dir, "custom.env")
	writeTestFile(t, customEnv, "APP__NAME=dotenv\n")
	t.Chdir(dir) // no default ".env" here

	t.Setenv("CREDO_APP__NAME", "env")
	t.Setenv("CREDO_ENV", "prod")
	t.Setenv("CREDO_ENV_FILE", customEnv)

	c, err := Load(WithFiles(base), WithoutProcessEnv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[string]any{"app": map[string]any{"name": "file"}}
	if !reflect.DeepEqual(c.data, want) {
		t.Errorf("data:\n  got  %v\n  want %v", c.data, want)
	}
}

// TestWithoutProcessEnvKeepsDotenvBootstrap pins the source boundary: with only
// the process environment disabled, an applicable .env file still supplies both
// its entries and the CREDO_ENV bootstrap value that drives env-specific file
// derivation.
func TestWithoutProcessEnvKeepsDotenvBootstrap(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "config.json"), `{"app":{"name":"file"}}`)
	writeTestFile(t, filepath.Join(dir, "config.prod.json"), `{"app":{"name":"prod"}}`)
	writeTestFile(t, filepath.Join(dir, ".env"), "CREDO_ENV=prod\nAPP__EXTRA=dotenv\n")
	t.Chdir(dir)
	t.Setenv("CREDO_ENV", "")      // neutralize ambient value
	t.Setenv("CREDO_ENV_FILE", "") // neutralize ambient value

	c, err := Load(WithoutProcessEnv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var name, extra string
	if err := c.Unmarshal("app.name", &name); err != nil || name != "prod" {
		t.Errorf("app.name = %q, %v; want %q (dotenv CREDO_ENV must derive env files)", name, err, "prod")
	}
	if err := c.Unmarshal("app.extra", &extra); err != nil || extra != "dotenv" {
		t.Errorf("app.extra = %q, %v; want %q (dotenv entries must still merge)", extra, err, "dotenv")
	}
}

// TestWithoutDotenv verifies that no .env file is read at all: not the default
// ".env", not a CREDO_ENV_FILE-resolved file, and no dotenv-sourced CREDO_ENV
// bootstrap (so no env-specific file derivation from it).
func TestWithoutDotenv(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "config.json"), `{"app":{"name":"file"}}`)
	writeTestFile(t, filepath.Join(dir, "config.prod.json"), `{"app":{"name":"prod"}}`)
	writeTestFile(t, filepath.Join(dir, ".env"), "CREDO_ENV=prod\nAPP__NAME=dotenv\n")
	otherEnv := filepath.Join(dir, "other.env")
	writeTestFile(t, otherEnv, "APP__NAME=otherenv\n")
	t.Chdir(dir)
	t.Setenv("CREDO_ENV", "") // neutralize ambient value
	t.Setenv("CREDO_ENV_FILE", otherEnv)

	// An unused prefix keeps ambient process env vars out of the comparison;
	// the env source itself stays enabled.
	c, err := Load(WithoutDotenv(), WithPrefix("OPTOUTTEST_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[string]any{"app": map[string]any{"name": "file"}}
	if !reflect.DeepEqual(c.data, want) {
		t.Errorf("data:\n  got  %v\n  want %v", c.data, want)
	}
}

// TestHermeticLoad verifies that both opt-outs together yield fully hermetic
// loading: with a hostile process environment and a .env present, the tree is
// exactly the parsed content of the listed file.
func TestHermeticLoad(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.json")
	writeTestFile(t, base, `{"app":{"name":"file","port":8080}}`)
	writeTestFile(t, filepath.Join(dir, "config.prod.json"), `{"app":{"name":"prod"}}`)
	writeTestFile(t, filepath.Join(dir, ".env"), "APP__NAME=dotenv\nCREDO_ENV=prod\n")
	t.Chdir(dir)
	t.Setenv("CREDO_APP__NAME", "env")
	t.Setenv("CREDO_ENV", "prod")
	t.Setenv("CREDO_ENV_FILE", filepath.Join(dir, ".env"))

	c, err := Load(WithFiles(base), WithoutProcessEnv(), WithoutDotenv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[string]any{"app": map[string]any{"name": "file", "port": float64(8080)}}
	if !reflect.DeepEqual(c.data, want) {
		t.Errorf("data:\n  got  %v\n  want %v", c.data, want)
	}
}

// TestHermeticLoadBytes verifies the embedded-document variant: LoadBytes with
// both opt-outs reads nothing beyond the document.
func TestHermeticLoadBytes(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".env"), "APP__NAME=dotenv\n")
	t.Chdir(dir)
	t.Setenv("CREDO_APP__NAME", "env")

	c, err := LoadBytes([]byte(`{"app":{"name":"embedded"}}`), FormatJSON,
		WithoutProcessEnv(), WithoutDotenv())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	want := map[string]any{"app": map[string]any{"name": "embedded"}}
	if !reflect.DeepEqual(c.data, want) {
		t.Errorf("data:\n  got  %v\n  want %v", c.data, want)
	}
}

// TestWithoutDotenvConflictsWithDotenvPath verifies the fail-loud rule for the
// contradictory combination, on both entry points, and that the redundant
// WithDotenvOptional combination stays allowed.
func TestWithoutDotenvConflictsWithDotenvPath(t *testing.T) {
	if _, err := Load(WithoutDotenv(), WithDotenvPath(".env.custom")); err == nil {
		t.Error("Load(WithoutDotenv, WithDotenvPath): expected error, got nil")
	}
	if _, err := LoadBytes([]byte(`{}`), FormatJSON, WithoutDotenv(), WithDotenvPath(".env.custom")); err == nil {
		t.Error("LoadBytes(WithoutDotenv, WithDotenvPath): expected error, got nil")
	}
	if _, err := LoadBytes([]byte(`{}`), FormatJSON, WithoutDotenv(), WithDotenvOptional()); err != nil {
		t.Errorf("LoadBytes(WithoutDotenv, WithDotenvOptional): unexpected error %v", err)
	}
}

// TestHermeticReloadReplay verifies that Reload replays the opt-outs: sources
// that were disabled at Load stay disabled, while the file layer stays live.
func TestHermeticReloadReplay(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.json")
	writeTestFile(t, base, `{"app":{"name":"file"}}`)
	t.Chdir(dir)

	c, err := Load(WithFiles(base), WithoutProcessEnv(), WithoutDotenv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Hostile sources appearing after Load must not leak in via Reload.
	t.Setenv("CREDO_APP__NAME", "env")
	writeTestFile(t, filepath.Join(dir, ".env"), "APP__NAME=dotenv\n")

	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !changes.Empty() {
		t.Errorf("Reload changes = %v, want empty (disabled sources must not leak in)", changes.Keys())
	}

	// The file layer itself stays live across Reload.
	writeTestFile(t, base, `{"app":{"name":"updated"}}`)
	if _, err := c.Reload(); err != nil {
		t.Fatalf("Reload after file change: %v", err)
	}
	var name string
	if err := c.Unmarshal("app.name", &name); err != nil || name != "updated" {
		t.Errorf("app.name after reload = %q, %v; want %q", name, err, "updated")
	}
}

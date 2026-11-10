package credo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
)

// autoLoadRecord is the record New writes about the configuration it loaded
// by itself.
const autoLoadRecord = "credo: configuration loaded automatically"

// TestNew_AutoLoadRecord pins the record of the automatic load: it names the
// files found, the .env file read and the environment prefix, never a key or
// a value, at Info when a file or a .env was read and at Debug when the load
// found only the environment.
func TestNew_AutoLoadRecord(t *testing.T) {
	const secret = "s3cr3t-value"
	tests := []struct {
		name       string
		files      map[string]string
		wantLevel  string
		wantFiles  []string
		wantDotenv string
	}{
		{
			name:      "only the environment",
			wantLevel: "DEBUG",
		},
		{
			name:      "a config file in the working directory",
			files:     map[string]string{"config.json": `{"app":{"token":"` + secret + `"}}`},
			wantLevel: "INFO",
			wantFiles: []string{"config.json"},
		},
		{
			name:       "a .env in the working directory",
			files:      map[string]string{".env": "APP__TOKEN=" + secret + "\n"},
			wantLevel:  "INFO",
			wantDotenv: ".env",
		},
		{
			name: "files and a .env",
			files: map[string]string{
				"config.yaml": "app:\n  token: " + secret + "\n",
				"config.json": `{"server":{"port":8081}}`,
				".env":        "APP__TOKEN=" + secret + "\n",
			},
			wantLevel:  "INFO",
			wantFiles:  []string{"config.json", "config.yaml"},
			wantDotenv: ".env",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			t.Chdir(dir)
			wd, err := os.Getwd()
			if err != nil {
				t.Fatalf("Getwd: %v", err)
			}
			t.Setenv("CREDO_ENV", "")
			t.Setenv("CREDO_ENV_FILE", "")
			t.Setenv("CREDO_APP__TOKEN", secret)

			logger, buf := newTestLogger(t)
			if _, err := credo.New(credo.WithLogger(logger)); err != nil {
				t.Fatalf("New: %v", err)
			}
			if n := strings.Count(buf.String(), autoLoadRecord); n != 1 {
				t.Fatalf("%d records of the automatic load, want 1:\n%s", n, buf.String())
			}
			if strings.Contains(buf.String(), secret) || strings.Contains(strings.ToLower(buf.String()), "token") {
				t.Fatalf("the record holds a key or a value: %s", buf.String())
			}
			rec := logLine(t, buf, autoLoadRecord)
			if rec["level"] != tt.wantLevel {
				t.Errorf("level = %v, want %s", rec["level"], tt.wantLevel)
			}
			sources, ok := rec["sources"].(map[string]any)
			if !ok {
				t.Fatalf("record has no sources group: %v", rec)
			}
			files, _ := sources["files"].([]any)
			if len(files) != len(tt.wantFiles) {
				t.Fatalf("files = %v, want %v", sources["files"], tt.wantFiles)
			}
			for i, name := range tt.wantFiles {
				if files[i] != filepath.Join(wd, name) {
					t.Errorf("files[%d] = %v, want %s", i, files[i], filepath.Join(wd, name))
				}
			}
			wantDotenv := ""
			if tt.wantDotenv != "" {
				wantDotenv = filepath.Join(wd, tt.wantDotenv)
			}
			if sources["dotenv"] != wantDotenv {
				t.Errorf("dotenv = %v, want %q", sources["dotenv"], wantDotenv)
			}
			if sources["process_env"] != true || sources["env_prefix"] != "CREDO_" {
				t.Errorf("sources = %v, want the process environment with the CREDO_ prefix", sources)
			}
		})
	}
}

// TestNew_AutoLoadRecord_BeforeAFailingSetting: a New that fails on a setting
// has already named the file the setting came from.
func TestNew_AutoLoadRecord_BeforeAFailingSetting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"server":{"port":-1}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("CREDO_ENV", "")
	t.Setenv("CREDO_ENV_FILE", "")

	logger, buf := newTestLogger(t)
	if _, err := credo.New(credo.WithLogger(logger)); err == nil {
		t.Fatal("New accepted server.port -1")
	}
	rec := logLine(t, buf, autoLoadRecord)
	if files, _ := rec["sources"].(map[string]any)["files"].([]any); len(files) != 1 {
		t.Fatalf("record = %v, want the one config file", rec)
	}
}

// TestNew_WithRawConfig_WritesNoAutoLoadRecord: New loads nothing when it is
// handed a RawConfig, a *config.Config the application loaded included, and
// the application logs the sources itself.
func TestNew_WithRawConfig_WritesNoAutoLoadRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"server":{"port":8081}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("CREDO_ENV", "")
	t.Setenv("CREDO_ENV_FILE", "")

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, rc := range map[string]credo.RawConfig{
		"a loaded Config":    loaded,
		"a custom RawConfig": newServerConfigRC(map[string]any{}),
	} {
		t.Run(name, func(t *testing.T) {
			logger, buf := newTestLogger(t)
			if _, err := credo.New(credo.WithLogger(logger), credo.WithRawConfig(rc)); err != nil {
				t.Fatalf("New: %v", err)
			}
			if strings.Contains(buf.String(), autoLoadRecord) {
				t.Fatalf("New(WithRawConfig) wrote the record of a load it did not make:\n%s", buf.String())
			}
		})
	}
	if got := loaded.Sources(); len(got.Files) != 1 || !got.FromFile() {
		t.Fatalf("the application's own Config does not name its file: %+v", got)
	}
}

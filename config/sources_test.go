package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/credo-go/credo/config"
)

// workingDir is the working directory as the loader resolves a relative path
// against it, which may spell a temporary directory differently from
// t.TempDir (a symlink, a short name).
func workingDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return wd
}

// TestSources pins what a load records about its inputs: the files it found,
// in merge order and as absolute paths, the .env file it read, and the
// process environment's prefix.
func TestSources(t *testing.T) {
	const secret = "s3cr3t-value"
	tests := []struct {
		name  string
		files map[string]string // written into the working directory
		env   map[string]string
		load  func() (*config.Config, error)
		want  func(dir string) config.Sources
	}{
		{
			name: "nothing but the environment",
			load: func() (*config.Config, error) { return config.Load() },
			want: func(string) config.Sources {
				return config.Sources{ProcessEnv: true, EnvPrefix: "CREDO_"}
			},
		},
		{
			name:  "discovered files in merge order, missing candidates left out",
			files: map[string]string{"config.yml": "a: 1\n", "config.json": `{"token":"` + secret + `"}`},
			load:  func() (*config.Config, error) { return config.Load() },
			want: func(dir string) config.Sources {
				return config.Sources{
					Files:      []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "config.yml")},
					ProcessEnv: true, EnvPrefix: "CREDO_",
				}
			},
		},
		{
			name:  "the environment's file follows the base file",
			files: map[string]string{"config.json": `{"a":1}`, "config.staging.json": `{"a":2}`},
			env:   map[string]string{"CREDO_ENV": "staging"},
			load:  func() (*config.Config, error) { return config.Load() },
			want: func(dir string) config.Sources {
				return config.Sources{
					Files:      []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "config.staging.json")},
					ProcessEnv: true, EnvPrefix: "CREDO_",
				}
			},
		},
		{
			name:  "the default .env",
			files: map[string]string{".env": "TOKEN=" + secret + "\n"},
			load:  func() (*config.Config, error) { return config.Load() },
			want: func(dir string) config.Sources {
				return config.Sources{Dotenv: filepath.Join(dir, ".env"), ProcessEnv: true, EnvPrefix: "CREDO_"}
			},
		},
		{
			name:  "an explicit .env path and explicit files",
			files: map[string]string{"deploy.env": "TOKEN=" + secret + "\n", "app.yaml": "a: 1\n"},
			load: func() (*config.Config, error) {
				return config.Load(config.WithFiles("app.yaml"), config.WithDotenvPath("deploy.env"))
			},
			want: func(dir string) config.Sources {
				return config.Sources{
					Files:      []string{filepath.Join(dir, "app.yaml")},
					Dotenv:     filepath.Join(dir, "deploy.env"),
					ProcessEnv: true, EnvPrefix: "CREDO_",
				}
			},
		},
		{
			name:  "a .env that is not read is not listed",
			files: map[string]string{".env": "TOKEN=" + secret + "\n"},
			load:  func() (*config.Config, error) { return config.Load(config.WithoutDotenv()) },
			want: func(string) config.Sources {
				return config.Sources{ProcessEnv: true, EnvPrefix: "CREDO_"}
			},
		},
		{
			name: "no process environment",
			load: func() (*config.Config, error) { return config.Load(config.WithoutProcessEnv()) },
			want: func(string) config.Sources { return config.Sources{} },
		},
		{
			name: "another prefix",
			load: func() (*config.Config, error) { return config.Load(config.WithPrefix("APP_")) },
			want: func(string) config.Sources {
				return config.Sources{ProcessEnv: true, EnvPrefix: "APP_"}
			},
		},
		{
			name:  "an embedded document lists no file",
			files: map[string]string{"config.json": `{"a":1}`},
			load: func() (*config.Config, error) {
				return config.LoadBytes([]byte("token: "+secret+"\n"), config.FormatYAML)
			},
			want: func(string) config.Sources {
				return config.Sources{Document: "yaml", ProcessEnv: true, EnvPrefix: "CREDO_"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				writeFile(t, filepath.Join(dir, name), content)
			}
			t.Chdir(dir)
			t.Setenv("CREDO_ENV", "")
			t.Setenv("CREDO_ENV_FILE", "")
			t.Setenv("CREDO_TOKEN", secret)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := tt.load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got, want := cfg.Sources(), tt.want(workingDir(t))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Sources() =\n %+v\nwant\n %+v", got, want)
			}
			if got.FromFile() != (len(want.Files) > 0 || want.Dotenv != "") {
				t.Errorf("FromFile() = %v for %+v", got.FromFile(), got)
			}

			// The record names paths and the prefix, never a key or a value.
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("configuration loaded", "sources", got)
			if strings.Contains(buf.String(), secret) || strings.Contains(strings.ToLower(buf.String()), "token") {
				t.Fatalf("the record holds a key or a value: %s", buf.String())
			}
			var line struct {
				Sources map[string]any `json:"sources"`
			}
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("record is not JSON: %v", err)
			}
			wantKeys := []string{"files", "dotenv", "process_env"}
			if want.Document != "" {
				wantKeys = append(wantKeys, "document")
			}
			if want.ProcessEnv {
				wantKeys = append(wantKeys, "env_prefix")
			}
			if len(line.Sources) != len(wantKeys) {
				t.Fatalf("record attributes = %v, want exactly %v", line.Sources, wantKeys)
			}
			for _, k := range wantKeys {
				if _, ok := line.Sources[k]; !ok {
					t.Errorf("record has no %q attribute: %v", k, line.Sources)
				}
			}
			if files, ok := line.Sources["files"].([]any); !ok || len(files) != len(want.Files) {
				t.Errorf("files = %v, want a list of %d", line.Sources["files"], len(want.Files))
			}
			if line.Sources["dotenv"] != want.Dotenv || line.Sources["process_env"] != want.ProcessEnv {
				t.Errorf("record = %v, want dotenv %q and process_env %v", line.Sources, want.Dotenv, want.ProcessEnv)
			}
		})
	}
}

// TestSources_FollowAReload: a reload that publishes replaces the sources
// with what it read, so a file that appeared since the start is listed.
func TestSources_FollowAReload(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CREDO_ENV", "")
	t.Setenv("CREDO_ENV_FILE", "")
	wd := workingDir(t)
	writeFile(t, filepath.Join(dir, "config.json"), `{"a":1}`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Sources(); len(got.Files) != 1 || got.Dotenv != "" {
		t.Fatalf("Sources() at the start = %+v", got)
	}

	writeFile(t, filepath.Join(dir, "config.yaml"), "b: 2\n")
	writeFile(t, filepath.Join(dir, ".env"), "C=3\n")
	if _, err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := cfg.Sources()
	want := config.Sources{
		Files:      []string{filepath.Join(wd, "config.json"), filepath.Join(wd, "config.yaml")},
		Dotenv:     filepath.Join(wd, ".env"),
		ProcessEnv: true, EnvPrefix: "CREDO_",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sources() after the reload =\n %+v\nwant\n %+v", got, want)
	}

	// The returned list is the caller's: changing it does not change the Config's.
	got.Files[0] = "changed"
	if again := cfg.Sources(); again.Files[0] != want.Files[0] {
		t.Fatalf("Sources() shares its file list with the caller: %v", again.Files)
	}
}

// TestSources_ZeroConfig: a Config no load built has read nothing.
func TestSources_ZeroConfig(t *testing.T) {
	var nilCfg *config.Config
	for name, cfg := range map[string]*config.Config{"nil": nilCfg, "zero": {}} {
		if got := cfg.Sources(); !reflect.DeepEqual(got, config.Sources{}) {
			t.Errorf("%s Config: Sources() = %+v, want the zero value", name, got)
		}
	}
}

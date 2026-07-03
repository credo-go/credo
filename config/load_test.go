package config

import (
	"reflect"
	"testing"
)

func TestMergeEnv(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		environ []string
		want    map[string]any
	}{
		{
			name:   "prefix filter and nesting",
			prefix: "CREDO_",
			environ: []string{
				"CREDO_SERVER__PORT=8080",
				"CREDO_SERVER__HOST=localhost",
				"CREDO_DEBUG=true",
				"OTHER_VAR=ignored",
				"PATH=/usr/bin",
			},
			want: map[string]any{
				"server": map[string]any{"port": "8080", "host": "localhost"},
				"debug":  "true",
			},
		},
		{
			name:   "normalization: single underscore stays, deep nesting",
			prefix: "CREDO_",
			environ: []string{
				"CREDO_READ_TIMEOUT=30s",
				"CREDO_DB__MAX_OPEN_CONNS=10",
				"CREDO_A__B__C__D=val",
			},
			want: map[string]any{
				"read_timeout": "30s",
				"db":           map[string]any{"max_open_conns": "10"},
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{"d": "val"},
					},
				},
			},
		},
		{
			name:   "bootstrap keys excluded",
			prefix: "CREDO_",
			environ: []string{
				"CREDO_PORT=8080",
				"CREDO_ENV_FILE=.env.prod",
				"CREDO_ENV=production",
			},
			want: map[string]any{"port": "8080"},
		},
		{
			name:    "empty prefix loads all",
			prefix:  "",
			environ: []string{"PORT=8080", "HOST=localhost"},
			want:    map[string]any{"port": "8080", "host": "localhost"},
		},
		{
			name:    "no matching vars",
			prefix:  "CREDO_",
			environ: []string{"OTHER_VAR=value"},
			want:    map[string]any{},
		},
		{
			name:    "malformed entry skipped",
			prefix:  "CREDO_",
			environ: []string{"CREDO_PORT=8080", "MALFORMED_NO_EQUALS"},
			want:    map[string]any{"port": "8080"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newConfig(WithPrefix(tt.prefix))
			c.mergeEnv(tt.environ)
			if !reflect.DeepEqual(c.data, tt.want) {
				t.Errorf("data:\n  got  %v\n  want %v", c.data, tt.want)
			}
		})
	}
}

func TestMergeDotenv(t *testing.T) {
	c := newConfig()
	c.mergeDotenv(map[string]string{
		"SERVER__PORT":   "8080",
		"DEBUG":          "true",
		"OTHER_VAR":      "hello",
		"CREDO_ENV":      "production",
		"CREDO_ENV_FILE": "/some/path",
	})

	// No prefix filtering (.env is project-scoped); bootstrap keys excluded.
	want := map[string]any{
		"server":    map[string]any{"port": "8080"},
		"debug":     "true",
		"other_var": "hello",
	}
	if !reflect.DeepEqual(c.data, want) {
		t.Errorf("data:\n  got  %v\n  want %v", c.data, want)
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		format  string
		want    map[string]any
		wantErr bool
	}{
		{
			name:   "json object",
			data:   `{"name":"app","server":{"port":8080}}`,
			format: "json",
			want: map[string]any{
				"name":   "app",
				"server": map[string]any{"port": float64(8080)},
			},
		},
		{
			name:   "json by extension",
			data:   `{"a":1}`,
			format: ".json",
			want:   map[string]any{"a": float64(1)},
		},
		{
			name:   "json empty object",
			data:   `{}`,
			format: "json",
			want:   map[string]any{},
		},
		{
			name:    "json invalid",
			data:    `{not json`,
			format:  "json",
			wantErr: true,
		},
		{
			name:   "yaml nested",
			data:   "server:\n  port: 8080\n",
			format: "yaml",
			want: map[string]any{
				"server": map[string]any{"port": 8080},
			},
		},
		{
			name:   "yaml by extension",
			data:   "a: 1\n",
			format: ".yaml",
			want:   map[string]any{"a": 1},
		},
		{
			name:   "yml bare",
			data:   "a: 1\n",
			format: "yml",
			want:   map[string]any{"a": 1},
		},
		{
			name:   "yml extension",
			data:   "a: 1\n",
			format: ".yml",
			want:   map[string]any{"a": 1},
		},
		{
			name:   "format is case-insensitive",
			data:   `{"a":1}`,
			format: "JSON",
			want:   map[string]any{"a": float64(1)},
		},
		{
			name:   "yaml non-string nested keys normalized",
			data:   "m:\n  42: answer\n",
			format: "yaml",
			want: map[string]any{
				"m": map[string]any{"42": "answer"},
			},
		},
		{
			name:    "yaml invalid",
			data:    "a: [unclosed",
			format:  "yaml",
			wantErr: true,
		},
		{
			name:    "unsupported format",
			data:    "x",
			format:  "toml",
			wantErr: true,
		},
		{
			name:    "empty format",
			data:    "x",
			format:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfig([]byte(tt.data), tt.format)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseConfig(%q): expected error, got %v", tt.format, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfig(%q): %v", tt.format, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseConfig:\n  got  %#v\n  want %#v", got, tt.want)
			}
		})
	}
}

func TestGetReturnsCopies(t *testing.T) {
	c := newConfig()
	c.merge(map[string]any{"db": map[string]any{"host": "localhost"}})

	// Mutating a sub-tree result must not affect the config tree.
	sub, _ := c.get("db")
	got := sub.(map[string]any)
	got["host"] = "modified"
	if host, _ := c.get("db.host"); host != "localhost" {
		t.Error("mutating get result affected the config tree")
	}

	// Mutating the full-tree result must not affect the config tree either.
	full, _ := c.get("")
	root := full.(map[string]any)
	root["db"].(map[string]any)["host"] = "also-modified"
	if host, _ := c.get("db.host"); host != "localhost" {
		t.Error("mutating full-tree result affected the config tree")
	}
}

func TestGetMissing(t *testing.T) {
	c := newConfig()
	c.merge(map[string]any{"a": 1})

	if _, ok := c.get("nonexistent"); ok {
		t.Error("nonexistent: got ok=true, want false")
	}
	if _, ok := c.get("a.b.c"); ok {
		t.Error("a.b.c: got ok=true, want false")
	}
}

// TestUnmarshalPresentNull verifies that a key explicitly set to JSON null is
// treated as present, not missing. Regression: get() collapsed present-null into
// the missing case, so Unmarshal reported "not found" for a key that Exists
// reported as existing.
func TestUnmarshalPresentNull(t *testing.T) {
	c, err := LoadBytes([]byte(`{"feature":{"enabled":null}}`), FormatJSON)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	if !c.Exists("feature.enabled") {
		t.Fatal("Exists(feature.enabled) = false, want true (present null)")
	}

	// A present null is a no-op: Unmarshal reports no error and leaves dst
	// unchanged (a null overlays nothing rather than zeroing the target).
	enabled := true
	if err := c.Unmarshal("feature.enabled", &enabled); err != nil {
		t.Fatalf("Unmarshal(feature.enabled) = %v, want nil (present null)", err)
	}
	if !enabled {
		t.Error("Unmarshal(feature.enabled) zeroed dst; want it unchanged (no-op)")
	}

	// A genuinely missing key still errors.
	if err := c.Unmarshal("feature.missing", new(bool)); err == nil {
		t.Error("Unmarshal(feature.missing) = nil, want not-found error")
	}
}

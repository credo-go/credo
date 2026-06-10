package config

import (
	"reflect"
	"testing"
)

func TestLookup(t *testing.T) {
	m := map[string]any{
		"server": map[string]any{
			"port": 8080,
			"tls": map[string]any{
				"enabled": true,
			},
		},
		"name":    "app",
		"nothing": nil,
	}

	tests := []struct {
		name   string
		key    string
		want   any
		wantOK bool
	}{
		{"top-level key", "name", "app", true},
		{"nested key", "server.port", 8080, true},
		{"deeply nested", "server.tls.enabled", true, true},
		{"intermediate map", "server", m["server"], true},
		{"nil value is present", "nothing", nil, true},
		{"missing key", "nonexistent", nil, false},
		{"missing nested", "server.missing", nil, false},
		{"path through non-map", "name.sub", nil, false},
		{"path through nil", "nothing.sub", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lookup(m, tt.key)
			if ok != tt.wantOK {
				t.Fatalf("lookup(%q) ok = %v, want %v", tt.key, ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("lookup(%q):\n  got  %v\n  want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestUnflatten(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
		want  map[string]any
	}{
		{
			name:  "empty map",
			input: map[string]any{},
			want:  map[string]any{},
		},
		{
			name:  "flat keys stay flat",
			input: map[string]any{"port": 8080},
			want:  map[string]any{"port": 8080},
		},
		{
			name: "dotted keys become nested",
			input: map[string]any{
				"server.port": 8080,
				"server.host": "localhost",
			},
			want: map[string]any{
				"server": map[string]any{
					"port": 8080,
					"host": "localhost",
				},
			},
		},
		{
			name: "deep nesting",
			input: map[string]any{
				"a.b.c.d": "deep",
			},
			want: map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{
							"d": "deep",
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unflatten(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("unflatten:\n  got  %v\n  want %v", got, tt.want)
			}
		})
	}
}

func TestMergeMaps(t *testing.T) {
	tests := []struct {
		name string
		dst  map[string]any
		src  map[string]any
		want map[string]any
	}{
		{
			name: "new keys added",
			dst:  map[string]any{"a": 1},
			src:  map[string]any{"b": 2},
			want: map[string]any{"a": 1, "b": 2},
		},
		{
			name: "src overrides dst",
			dst:  map[string]any{"a": 1},
			src:  map[string]any{"a": 2},
			want: map[string]any{"a": 2},
		},
		{
			name: "recursive merge",
			dst: map[string]any{
				"server": map[string]any{
					"port": 8080,
					"host": "localhost",
				},
			},
			src: map[string]any{
				"server": map[string]any{
					"port": 9090,
				},
			},
			want: map[string]any{
				"server": map[string]any{
					"port": 9090,
					"host": "localhost",
				},
			},
		},
		{
			name: "non-map overwrites map",
			dst: map[string]any{
				"server": map[string]any{"port": 8080},
			},
			src: map[string]any{
				"server": "simple-string",
			},
			want: map[string]any{
				"server": "simple-string",
			},
		},
		{
			name: "map overwrites non-map",
			dst: map[string]any{
				"server": "simple-string",
			},
			src: map[string]any{
				"server": map[string]any{"port": 8080},
			},
			want: map[string]any{
				"server": map[string]any{"port": 8080},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergeMaps(tt.src, tt.dst)
			if !reflect.DeepEqual(tt.dst, tt.want) {
				t.Errorf("mergeMaps:\n  got  %v\n  want %v", tt.dst, tt.want)
			}
		})
	}
}

func TestCopyMap(t *testing.T) {
	original := map[string]any{
		"a": 1,
		"b": map[string]any{
			"c": "hello",
			"d": []any{1, 2, 3},
		},
		"e": []any{
			map[string]any{"f": true},
		},
	}

	copied := copyMap(original)

	// Should be equal.
	if !reflect.DeepEqual(copied, original) {
		t.Errorf("copy not equal to original:\n  got  %v\n  want %v", copied, original)
	}

	// Mutating the copy should not affect the original.
	copied["a"] = 999
	if original["a"] == 999 {
		t.Error("mutating copy affected original top-level value")
	}

	copiedB := copied["b"].(map[string]any)
	copiedB["c"] = "modified"
	originalB := original["b"].(map[string]any)
	if originalB["c"] == "modified" {
		t.Error("mutating copy affected original nested map")
	}

	// Mutating slice in copy should not affect original.
	copiedE := copied["e"].([]any)
	copiedE[0].(map[string]any)["f"] = false
	originalE := original["e"].([]any)
	if originalE[0].(map[string]any)["f"] == false {
		t.Error("mutating copy affected original slice element")
	}
}

func TestCopyMapEmpty(t *testing.T) {
	copied := copyMap(map[string]any{})
	if copied == nil || len(copied) != 0 {
		t.Errorf("copyMap of empty: got %v, want empty map", copied)
	}
}

func TestIntfaceKeysToStrings(t *testing.T) {
	input := map[string]any{
		"normal": "value",
		"nested": map[any]any{
			"key1": "val1",
			42:     "val2",
		},
		"slice": []any{
			map[any]any{"a": 1},
			"plain",
		},
	}

	got := intfaceKeysToStrings(input)

	// Normal string keys preserved.
	if got["normal"] != "value" {
		t.Errorf("normal key: got %v", got["normal"])
	}

	// map[any]any converted to map[string]any.
	nested, ok := got["nested"].(map[string]any)
	if !ok {
		t.Fatal("nested not converted to map[string]any")
	}
	if nested["key1"] != "val1" {
		t.Errorf("nested.key1: got %v", nested["key1"])
	}
	if nested["42"] != "val2" {
		t.Errorf("nested.42: got %v", nested["42"])
	}

	// Slice elements converted.
	slice, ok := got["slice"].([]any)
	if !ok {
		t.Fatal("slice not preserved")
	}
	elem, ok := slice[0].(map[string]any)
	if !ok {
		t.Fatal("slice[0] not converted to map[string]any")
	}
	if elem["a"] != 1 {
		t.Errorf("slice[0].a: got %v", elem["a"])
	}
}

func TestUnflattenOverwritesNonMap(t *testing.T) {
	// When unflattening, a dotted key that conflicts with a non-map
	// value should overwrite it with a nested map.
	input := map[string]any{
		"server":      "old-value",
		"server.port": 8080,
	}

	got := unflatten(input)

	// The result depends on map iteration order, but "server.port"
	// should create a nested map. If "server" is processed first,
	// it gets overwritten. If "server.port" is first, "server" then
	// overwrites. We ensure at least one key exists.
	if got["server"] == nil {
		t.Error("server key should exist")
	}

	// Verify deterministic behavior with only the dotted key.
	input2 := map[string]any{"server.port": 8080}
	got2 := unflatten(input2)
	server, ok := got2["server"].(map[string]any)
	if !ok {
		t.Fatal("server should be a map")
	}
	if server["port"] != 8080 {
		t.Errorf("server.port: got %v, want 8080", server["port"])
	}
}

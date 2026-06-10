package testutil

import (
	"strings"
	"testing"
)

func TestMatchesEntry(t *testing.T) {
	base := map[string]any{
		"level":  "INFO",
		"msg":    "request completed",
		"method": "GET",
		"status": float64(200), // decoded form of a JSON number
	}

	tests := []struct {
		name string
		want LogEntry
		ok   bool
	}{
		{"empty matcher matches anything", LogEntry{}, true},
		{"level case-insensitive", LogEntry{Level: "info"}, true},
		{"level mismatch", LogEntry{Level: "ERROR"}, false},
		{"message exact", LogEntry{Message: "request completed"}, true},
		{"message mismatch", LogEntry{Message: "other"}, false},
		{"string attr match", LogEntry{Attrs: map[string]any{"method": "GET"}}, true},
		{"string attr mismatch", LogEntry{Attrs: map[string]any{"method": "POST"}}, false},
		{"numeric attr normalized", LogEntry{Attrs: map[string]any{"status": 200}}, true},
		{"missing attr", LogEntry{Attrs: map[string]any{"absent": 1}}, false},
		{
			"combined match",
			LogEntry{Level: "INFO", Message: "request completed", Attrs: map[string]any{"status": 200}},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesEntry(base, tc.want); got != tc.ok {
				t.Errorf("matchesEntry(%+v) = %v, want %v", tc.want, got, tc.ok)
			}
		})
	}
}

func TestSetNested(t *testing.T) {
	t.Run("nested path", func(t *testing.T) {
		root := map[string]any{}
		setNested(root, "a.b.c", 1)
		a, _ := root["a"].(map[string]any)
		b, _ := a["b"].(map[string]any)
		if b["c"] != 1 {
			t.Errorf("root[a][b][c] = %v, want 1", b["c"])
		}
	})

	t.Run("merge under same root", func(t *testing.T) {
		root := map[string]any{}
		setNested(root, "a.b", 1)
		setNested(root, "a.c", 2)
		a, _ := root["a"].(map[string]any)
		if a["b"] != 1 || a["c"] != 2 {
			t.Errorf("root[a] = %v, want {b:1, c:2}", a)
		}
	})

	t.Run("single key", func(t *testing.T) {
		root := map[string]any{}
		setNested(root, "x", 5)
		if root["x"] != 5 {
			t.Errorf("root[x] = %v, want 5", root["x"])
		}
	})

	t.Run("empty key is ignored", func(t *testing.T) {
		root := map[string]any{}
		setNested(root, "", 9)
		if len(root) != 0 {
			t.Errorf("root = %v, want empty", root)
		}
	})
}

func TestDescribeWant(t *testing.T) {
	got := describeWant(LogEntry{
		Level:   "INFO",
		Message: "hi",
		Attrs:   map[string]any{"k": "v"},
	})
	for _, want := range []string{"level=INFO", `msg="hi"`, "attrs=", `"k":"v"`} {
		if !strings.Contains(got, want) {
			t.Errorf("describeWant() = %q, want it to contain %q", got, want)
		}
	}
}

func TestDescribeEntries(t *testing.T) {
	if got := describeEntries(nil); got != "  (none)" {
		t.Errorf("describeEntries(nil) = %q, want %q", got, "  (none)")
	}

	entries := []map[string]any{{"msg": "a"}, {"msg": "b"}}
	got := describeEntries(entries)
	if !strings.Contains(got, `"msg":"a"`) || !strings.Contains(got, `"msg":"b"`) {
		t.Errorf("describeEntries() = %q, want it to contain both records", got)
	}
}

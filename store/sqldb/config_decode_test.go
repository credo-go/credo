package sqldb_test

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/store/sqldb"
)

func TestConfig_MaxIdlePresenceSurvivesUnmarshal(t *testing.T) {
	raw := []byte(`{
		"databases": {
			"omitted": {"driver": "sqlite", "dsn": ":memory:"},
			"zero": {"driver": "sqlite", "dsn": ":memory:", "max_idle": 0},
			"positive": {"driver": "sqlite", "dsn": ":memory:", "max_idle": 7}
		}
	}`)
	cfg, err := config.LoadBytes(
		raw,
		config.FormatJSON,
		config.WithPrefix("CREDO_TEST_MAX_IDLE_PRESENCE_"),
		config.WithDotenvPath(filepath.Join(t.TempDir(), ".env")),
		config.WithDotenvOptional(),
		config.WithLogger(slog.New(slog.DiscardHandler)),
	)
	if err != nil {
		t.Fatalf("LoadBytes() = %v", err)
	}

	tests := []struct {
		name string
		key  string
		want *int
	}{
		{name: "omitted", key: "databases.omitted"},
		{name: "explicit zero", key: "databases.zero", want: new(0)},
		{name: "positive", key: "databases.positive", want: new(7)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got sqldb.Config
			if err := cfg.Unmarshal(tt.key, &got); err != nil {
				t.Fatalf("Unmarshal(%q) = %v", tt.key, err)
			}
			if tt.want == nil {
				if got.MaxIdle != nil {
					t.Fatalf("MaxIdle = %d, want nil", *got.MaxIdle)
				}
				return
			}
			if got.MaxIdle == nil {
				t.Fatalf("MaxIdle = nil, want %d", *tt.want)
			}
			if *got.MaxIdle != *tt.want {
				t.Fatalf("MaxIdle = %d, want %d", *got.MaxIdle, *tt.want)
			}
		})
	}
}

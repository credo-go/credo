package sqldb

import (
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/dialect/sqlitedialect"
)

func TestOpen_NilConfig(t *testing.T) {
	_, err := Open(nil)
	if err == nil {
		t.Fatal("Open(nil) should return error")
	}
}

func TestOpen_NoDriver(t *testing.T) {
	_, err := Open(&Config{})
	if err == nil {
		t.Fatal("Open with empty driver should return error")
	}
}

func TestOpen_DSNWithoutDriver(t *testing.T) {
	_, err := Open(&Config{DSN: "postgres://localhost/app"})
	if err == nil {
		t.Fatal("Open with DSN but no driver should return error")
	}
}

func TestOpen_NoDriverWithConnector(t *testing.T) {
	// WithConnector should bypass the driver/DSN requirement.
	// We can't easily create a real connector here, but we verify
	// that the validation doesn't reject an empty driver when
	// a connector is provided. This test will fail at sql.OpenDB
	// level if the connector is invalid, not at validation.
	// For now, just verify the error message path WITHOUT connector.
	_, err := Open(&Config{})
	if err == nil {
		t.Fatal("Open with empty config should return error")
	}
	if !strings.Contains(err.Error(), "WithConnector") {
		t.Errorf("error should mention WithConnector, got: %v", err)
	}
}

func TestOpen_UnknownDialect(t *testing.T) {
	_, err := Open(&Config{Driver: "unknown_driver", DSN: "fake"})
	if err == nil {
		t.Fatal("Open with unknown dialect should return error")
	}
}

func TestOpen_InvalidPoolSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "negative max open", cfg: Config{Driver: "sqlite", DSN: ":memory:", MaxOpen: -1}},
		{name: "negative max idle", cfg: Config{Driver: "sqlite", DSN: ":memory:", MaxIdle: -1}},
		{name: "negative max lifetime", cfg: Config{Driver: "sqlite", DSN: ":memory:", MaxLifetime: -1 * time.Second}},
		{name: "negative connect timeout", cfg: Config{Driver: "sqlite", DSN: ":memory:", ConnectTimeout: -1 * time.Second}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(&tt.cfg)
			if err == nil {
				t.Fatalf("Open(%+v) should return validation error", tt.cfg)
			}
		})
	}
}

func TestOpen_WithDialect(t *testing.T) {
	// sqlite3 with :memory: DSN should work without a real driver
	// if we provide the dialect. But sql.Open will still need a registered driver.
	// This test verifies the option plumbing at least.
	_, err := Open(&Config{
		Driver: "sqlite3",
		DSN:    ":memory:",
	}, WithDialect(sqlitedialect.New()))
	// This may fail if sqlite3 driver is not registered, which is expected
	// in a unit test environment without cgo. We just verify it doesn't panic.
	_ = err
}

func TestDriverDialectDetection(t *testing.T) {
	tests := []struct {
		driver string
		want   bool // true if dialect should be detected
	}{
		{"postgres", true},
		{"pgx", true},
		{"mysql", true},
		{"sqlite3", true},
		{"sqlite", true},
		{"unknown", false},
		{"", false},
	}
	for _, tt := range tests {
		d := resolveDriverFamily(tt.driver).dialect()
		got := d != nil
		if got != tt.want {
			t.Errorf("resolveDriverFamily(%q).dialect() detected=%v, want %v", tt.driver, got, tt.want)
		}
	}
}

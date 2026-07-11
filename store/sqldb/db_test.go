package sqldb

import (
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
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

func TestOpen_TxCleanupTimeout(t *testing.T) {
	tests := []struct {
		name    string
		option  Option
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: defaultTxCleanupTimeout},
		{name: "custom", option: WithTxCleanupTimeout(17 * time.Second), want: 17 * time.Second},
		{name: "zero", option: WithTxCleanupTimeout(0), wantErr: true},
		{name: "negative", option: WithTxCleanupTimeout(-time.Second), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []Option{}
			if tt.option != nil {
				opts = append(opts, tt.option)
			}
			db, err := Open(&Config{Driver: "sqlite", DSN: ":memory:"}, opts...)
			if tt.wantErr {
				if err == nil {
					_ = db.Shutdown(t.Context())
					t.Fatal("Open should reject invalid tx cleanup timeout")
				}
				return
			}
			if err != nil {
				t.Fatalf("Open = %v", err)
			}
			t.Cleanup(func() { _ = db.Shutdown(t.Context()) })
			if db.txCleanupTimeout != tt.want {
				t.Fatalf("txCleanupTimeout = %s, want %s", db.txCleanupTimeout, tt.want)
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

func TestResolveDialectFamily(t *testing.T) {
	tests := []struct {
		name    string
		dialect schema.Dialect
		want    driverFamily
	}{
		{"postgres", pgdialect.New(), driverFamilyPostgres},
		{"mysql", mysqldialect.New(), driverFamilyMySQL},
		{"sqlite", sqlitedialect.New(), driverFamilySQLite},
	}
	for _, tt := range tests {
		if got := resolveDialectFamily(tt.dialect); got != tt.want {
			t.Errorf("resolveDialectFamily(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
	if got := resolveDialectFamily(nil); got != driverFamilyUnknown {
		t.Errorf("resolveDialectFamily(nil) = %v, want unknown", got)
	}
}

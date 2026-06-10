package sqldb

import (
	"strings"
	"testing"
	"time"
)

func mustBuildDSN(t *testing.T, cfg *Config) string {
	t.Helper()

	dsn, err := cfg.buildDSN(resolveDriverFamily(cfg.Driver))
	if err != nil {
		t.Fatalf("buildDSN() = %v", err)
	}
	return dsn
}

func TestConfig_BuildDSN_Postgres(t *testing.T) {
	cfg := &Config{
		Driver:   "postgres",
		Host:     "localhost",
		Port:     5432,
		Name:     "testdb",
		User:     "user",
		Password: "pass",
		SSLMode:  "disable",
	}

	dsn := mustBuildDSN(t, cfg)
	if !strings.Contains(dsn, "postgres://user:pass@localhost:5432/testdb") {
		t.Errorf("unexpected DSN: %s", dsn)
	}
	if !strings.Contains(dsn, "sslmode=disable") {
		t.Errorf("DSN missing sslmode: %s", dsn)
	}
}

func TestConfig_BuildDSN_MySQL(t *testing.T) {
	cfg := &Config{
		Driver:   "mysql",
		Host:     "localhost",
		Port:     3306,
		Name:     "testdb",
		User:     "root",
		Password: "secret",
	}

	dsn := mustBuildDSN(t, cfg)
	if !strings.Contains(dsn, "root:secret@tcp(localhost:3306)/testdb") {
		t.Errorf("unexpected DSN: %s", dsn)
	}
	if !strings.Contains(dsn, "parseTime=true") {
		t.Errorf("DSN missing parseTime: %s", dsn)
	}
}

func TestConfig_BuildDSN_MySQL_SpecialCharPassword(t *testing.T) {
	// go-sql-driver's DSN grammar resolves credentials as
	// [first ':' .. last '@' before the last '/'], so passwords with
	// delimiter characters must be written RAW (no URL-encoding) —
	// matching mysql.Config.FormatDSN byte for byte.
	cfg := &Config{
		Driver:   "mysql",
		Host:     "db.internal",
		Port:     3306,
		Name:     "app",
		User:     "svc",
		Password: "p@ss:w/o?rd",
	}

	dsn := mustBuildDSN(t, cfg)
	want := "svc:p@ss:w/o?rd@tcp(db.internal:3306)/app?parseTime=true"
	if dsn != want {
		t.Errorf("buildDSN() = %q, want %q", dsn, want)
	}
}

func TestConfig_BuildDSN_SQLite(t *testing.T) {
	cfg := &Config{
		Driver: "sqlite3",
		Name:   "test.db",
	}
	dsn := mustBuildDSN(t, cfg)
	if dsn != "test.db" {
		t.Errorf("buildDSN() = %q, want %q", dsn, "test.db")
	}
}

func TestConfig_BuildDSN_SQLiteMemory(t *testing.T) {
	cfg := &Config{
		Driver: "sqlite3",
	}
	dsn := mustBuildDSN(t, cfg)
	if dsn != ":memory:" {
		t.Errorf("buildDSN() = %q, want %q", dsn, ":memory:")
	}
}

func TestConfig_BuildDSN_Override(t *testing.T) {
	cfg := &Config{
		Driver: "postgres",
		DSN:    "custom-dsn-string",
		Host:   "should-be-ignored",
	}
	dsn := mustBuildDSN(t, cfg)
	if dsn != "custom-dsn-string" {
		t.Errorf("buildDSN() = %q, want %q", dsn, "custom-dsn-string")
	}
}

func TestConfig_BuildDSN_WithOptions(t *testing.T) {
	cfg := &Config{
		Driver:  "postgres",
		Host:    "localhost",
		Port:    5432,
		Name:    "testdb",
		Options: map[string]string{"application_name": "myapp"},
	}
	dsn := mustBuildDSN(t, cfg)
	if !strings.Contains(dsn, "application_name=myapp") {
		t.Errorf("DSN missing options: %s", dsn)
	}
}

func TestConfig_BuildDSN_WithConnectTimeout(t *testing.T) {
	cfg := &Config{
		Driver:         "postgres",
		Host:           "localhost",
		Port:           5432,
		Name:           "testdb",
		ConnectTimeout: 10 * time.Second,
	}
	dsn := mustBuildDSN(t, cfg)
	if !strings.Contains(dsn, "connect_timeout=10") {
		t.Errorf("DSN missing connect_timeout: %s", dsn)
	}
}

func TestConfig_BuildDSN_UnknownDriver(t *testing.T) {
	cfg := &Config{Driver: "custom"}

	_, err := cfg.buildDSN(resolveDriverFamily(cfg.Driver))
	if err == nil {
		t.Fatal("buildDSN() should fail for unknown driver family")
	}
}

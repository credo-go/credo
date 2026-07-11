package sqldb

import (
	"net/url"
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
	tests := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{name: "exact seconds", timeout: 10 * time.Second, want: "10"},
		{name: "sub-second rounds up", timeout: time.Nanosecond, want: "1"},
		{name: "fractional second rounds up", timeout: 1500 * time.Millisecond, want: "2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Driver:         "postgres",
				Host:           "localhost",
				Port:           5432,
				Name:           "testdb",
				ConnectTimeout: tt.timeout,
			}
			dsn := mustBuildDSN(t, cfg)
			parsed, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("url.Parse(%q) = %v", dsn, err)
			}
			if got := parsed.Query().Get("connect_timeout"); got != tt.want {
				t.Fatalf("connect_timeout = %q, want %q (DSN %q)", got, tt.want, dsn)
			}
		})
	}
}

func TestConfig_BuildDSN_IPv6Host(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		cfg := &Config{
			Driver: "postgres",
			Host:   "2001:db8::1",
			Port:   5432,
			Name:   "app",
		}
		dsn := mustBuildDSN(t, cfg)
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("url.Parse(%q) = %v", dsn, err)
		}
		if parsed.Host != "[2001:db8::1]:5432" {
			t.Fatalf("PostgreSQL host = %q, want bracketed IPv6 host", parsed.Host)
		}
	})

	t.Run("mysql", func(t *testing.T) {
		cfg := &Config{
			Driver: "mysql",
			Host:   "2001:db8::1",
			Port:   3306,
			Name:   "app",
		}
		dsn := mustBuildDSN(t, cfg)
		if !strings.Contains(dsn, "tcp([2001:db8::1]:3306)") {
			t.Fatalf("MySQL DSN does not bracket IPv6 host: %q", dsn)
		}
	})
}

func TestConfig_BuildDSN_PreservesEmptyHostAndName(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "postgres",
			cfg:  Config{Driver: "postgres", Port: 5432},
			want: "postgres://:5432",
		},
		{
			name: "mysql",
			cfg:  Config{Driver: "mysql", Port: 3306},
			want: "tcp(:3306)/?parseTime=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustBuildDSN(t, &tt.cfg); got != tt.want {
				t.Fatalf("buildDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfig_BuildDSN_RejectsZeroNetworkPort(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			cfg := &Config{Driver: driver, Host: "localhost", Name: "app"}
			_, err := cfg.buildDSN(resolveDriverFamily(cfg.Driver))
			if err == nil {
				t.Fatal("buildDSN() should reject Port=0 for a generated network DSN")
			}
			if !strings.Contains(err.Error(), "port") || !strings.Contains(err.Error(), "1 and 65535") {
				t.Fatalf("buildDSN() error = %q, want a bounded port error", err)
			}
		})
	}
}

func TestConfig_BuildDSN_RejectsStructuredOptionConflictsWithoutLeakingValues(t *testing.T) {
	const secret = "do-not-leak-option-value"
	tests := []struct {
		name string
		cfg  Config
		key  string
	}{
		{
			name: "postgres endpoint",
			cfg: Config{
				Driver:  "postgres",
				Host:    "localhost",
				Port:    5432,
				Options: map[string]string{"password": secret},
			},
			key: "password",
		},
		{
			name: "postgres ssl mode",
			cfg: Config{
				Driver:  "postgres",
				Host:    "localhost",
				Port:    5432,
				SSLMode: "verify-full",
				Options: map[string]string{"sslmode": secret},
			},
			key: "sslmode",
		},
		{
			name: "postgres timeout",
			cfg: Config{
				Driver:         "postgres",
				Host:           "localhost",
				Port:           5432,
				ConnectTimeout: time.Second,
				Options:        map[string]string{"connect_timeout": secret},
			},
			key: "connect_timeout",
		},
		{
			name: "mysql parse time",
			cfg: Config{
				Driver:  "mysql",
				Host:    "localhost",
				Port:    3306,
				Options: map[string]string{"parseTime": secret},
			},
			key: "parseTime",
		},
		{
			name: "mysql tls",
			cfg: Config{
				Driver:  "mysql",
				Host:    "localhost",
				Port:    3306,
				SSLMode: "true",
				Options: map[string]string{"tls": secret},
			},
			key: "tls",
		},
		{
			name: "mysql timeout",
			cfg: Config{
				Driver:         "mysql",
				Host:           "localhost",
				Port:           3306,
				ConnectTimeout: time.Second,
				Options:        map[string]string{"timeout": secret},
			},
			key: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.cfg.buildDSN(resolveDriverFamily(tt.cfg.Driver))
			if err == nil {
				t.Fatalf("buildDSN() should reject conflicting option %q", tt.key)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("buildDSN() error = %q, want option key %q", err, tt.key)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("buildDSN() leaked option value: %q", err)
			}
		})
	}
}

func TestConfig_BuildDSN_AllowsDriverOptionsWithoutStructuredSource(t *testing.T) {
	tests := []Config{
		{
			Driver:  "postgres",
			Host:    "localhost",
			Port:    5432,
			Options: map[string]string{"sslmode": "verify-full", "connect_timeout": "3"},
		},
		{
			Driver:  "mysql",
			Host:    "localhost",
			Port:    3306,
			Options: map[string]string{"tls": "preferred", "timeout": "3s"},
		},
	}

	for i := range tests {
		if _, err := tests[i].buildDSN(resolveDriverFamily(tests[i].Driver)); err != nil {
			t.Fatalf("buildDSN(%s) = %v", tests[i].Driver, err)
		}
	}
}

func TestConfig_BuildDSN_UnknownDriver(t *testing.T) {
	cfg := &Config{Driver: "custom"}

	_, err := cfg.buildDSN(resolveDriverFamily(cfg.Driver))
	if err == nil {
		t.Fatal("buildDSN() should fail for unknown driver family")
	}
}

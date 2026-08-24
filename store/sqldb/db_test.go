package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
)

type poolConfigCall struct {
	method string
	value  any
}

type recordingPool struct {
	calls []poolConfigCall
}

type customTestDriver struct{}

func (customTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("custom test driver connection is not used")
}

type customTestConnector struct{}

func (*customTestConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("custom test connector connection is not used")
}

func (*customTestConnector) Driver() driver.Driver {
	return customTestDriver{}
}

var registerCustomTestDriver sync.Once

func (p *recordingPool) SetMaxOpenConns(n int) {
	p.calls = append(p.calls, poolConfigCall{method: "SetMaxOpenConns", value: n})
}

func (p *recordingPool) SetMaxIdleConns(n int) {
	p.calls = append(p.calls, poolConfigCall{method: "SetMaxIdleConns", value: n})
}

func (p *recordingPool) SetConnMaxLifetime(d time.Duration) {
	p.calls = append(p.calls, poolConfigCall{method: "SetConnMaxLifetime", value: d})
}

func (p *recordingPool) SetConnMaxIdleTime(d time.Duration) {
	p.calls = append(p.calls, poolConfigCall{method: "SetConnMaxIdleTime", value: d})
}

func TestApplyPoolConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []poolConfigCall
	}{
		{
			name: "unset preserves database sql defaults",
			cfg:  Config{},
		},
		{
			name: "explicit zero max idle is applied",
			cfg:  Config{MaxIdle: new(0)},
			want: []poolConfigCall{{method: "SetMaxIdleConns", value: 0}},
		},
		{
			name: "all positive settings are applied in dependency order",
			cfg: Config{
				MaxOpen:     11,
				MaxIdle:     new(7),
				MaxLifetime: 30 * time.Minute,
				MaxIdleTime: 5 * time.Minute,
			},
			want: []poolConfigCall{
				{method: "SetMaxOpenConns", value: 11},
				{method: "SetMaxIdleConns", value: 7},
				{method: "SetConnMaxLifetime", value: 30 * time.Minute},
				{method: "SetConnMaxIdleTime", value: 5 * time.Minute},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := new(recordingPool)
			applyPoolConfig(pool, &tt.cfg)
			if !reflect.DeepEqual(pool.calls, tt.want) {
				t.Fatalf("calls = %#v, want %#v", pool.calls, tt.want)
			}
		})
	}
}

func TestDB_StoreRegistrationWarningCodesReflectEffectivePool(t *testing.T) {
	t.Run("unlimited max open", func(t *testing.T) {
		db, err := Open(&Config{Driver: "sqlite", DSN: ":memory:"})
		if err != nil {
			t.Fatalf("Open() = %v", err)
		}
		t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })

		got := db.StoreRegistrationWarningCodes()
		want := []string{maxOpenUnlimitedWarningCode}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("StoreRegistrationWarningCodes() = %v, want %v", got, want)
		}

		got[0] = "mutated"
		if next := db.StoreRegistrationWarningCodes(); !reflect.DeepEqual(next, want) {
			t.Fatalf("warning codes retained caller mutation: %v", next)
		}

		db.Client().DB.SetMaxOpenConns(5)
		if got := db.StoreRegistrationWarningCodes(); got != nil {
			t.Fatalf("StoreRegistrationWarningCodes() after finite override = %v, want nil", got)
		}
	})

	t.Run("bounded max open", func(t *testing.T) {
		db, err := Open(&Config{Driver: "sqlite", DSN: ":memory:", MaxOpen: 5})
		if err != nil {
			t.Fatalf("Open() = %v", err)
		}
		t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })

		if got := db.StoreRegistrationWarningCodes(); got != nil {
			t.Fatalf("StoreRegistrationWarningCodes() = %v, want nil", got)
		}

		db.Client().DB.SetMaxOpenConns(0)
		want := []string{maxOpenUnlimitedWarningCode}
		if got := db.StoreRegistrationWarningCodes(); !reflect.DeepEqual(got, want) {
			t.Fatalf("StoreRegistrationWarningCodes() after unlimited override = %v, want %v", got, want)
		}
	})
}

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

func TestOpen_NoDriverWithoutConnector(t *testing.T) {
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
		{name: "negative max idle", cfg: Config{Driver: "sqlite", DSN: ":memory:", MaxIdle: new(-1)}},
		{name: "negative max lifetime", cfg: Config{Driver: "sqlite", DSN: ":memory:", MaxLifetime: -1 * time.Second}},
		{name: "negative max idle time", cfg: Config{Driver: "sqlite", DSN: ":memory:", MaxIdleTime: -1 * time.Second}},
		{
			name: "max idle exceeds max open",
			cfg:  Config{Driver: "sqlite", DSN: ":memory:", MaxOpen: 2, MaxIdle: new(3)},
		},
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
			t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })
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

func TestOpen_CustomDriverWithExplicitDialect(t *testing.T) {
	const driverName = "credo-custom-driver-test"
	registerCustomTestDriver.Do(func() {
		sql.Register(driverName, customTestDriver{})
	})

	db, err := Open(
		&Config{Driver: driverName, DSN: "driver-native-dsn"},
		WithDialect(sqlitedialect.New()),
	)
	if err != nil {
		t.Fatalf("Open(custom driver, explicit dialect) = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })
	if db.family != driverFamilySQLite {
		t.Fatalf("custom driver family = %v, want SQLite from explicit dialect", db.family)
	}
}

func TestOpen_CustomConnectorWithExplicitDialect(t *testing.T) {
	db, err := Open(
		new(Config),
		WithConnector(new(customTestConnector)),
		WithDialect(sqlitedialect.New()),
	)
	if err != nil {
		t.Fatalf("Open(custom connector, explicit dialect) = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })
	if db.family != driverFamilySQLite {
		t.Fatalf("custom connector family = %v, want SQLite from explicit dialect", db.family)
	}
}

func TestOpen_ExplicitNilOptionsFailLoud(t *testing.T) {
	tests := []struct {
		name   string
		option Option
		want   string
	}{
		{name: "nil dialect", option: WithDialect(nil), want: "WithDialect"},
		{
			name:   "typed nil dialect",
			option: WithDialect((*pgdialect.Dialect)(nil)),
			want:   "WithDialect",
		},
		{name: "nil connector", option: WithConnector(nil), want: "WithConnector"},
		{
			name:   "typed nil connector",
			option: WithConnector((*customTestConnector)(nil)),
			want:   "WithConnector",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(&Config{Driver: "sqlite", DSN: ":memory:"}, tt.option)
			if err == nil {
				t.Fatalf("Open(%s) should reject an explicitly nil option", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "non-nil") {
				t.Fatalf("Open(%s) error = %q, want fail-loud option name", tt.name, err)
			}
		})
	}
}

func TestOpen_RejectsKnownDriverDialectMismatch(t *testing.T) {
	const rawDSN = "do-not-leak-raw-dsn"
	tests := []struct {
		name    string
		driver  string
		dialect schema.Dialect
	}{
		{name: "postgres driver with mysql dialect", driver: "postgres", dialect: mysqldialect.New()},
		{name: "mysql driver with sqlite dialect", driver: "mysql", dialect: sqlitedialect.New()},
		{name: "sqlite driver with postgres dialect", driver: "sqlite", dialect: pgdialect.New()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(&Config{Driver: tt.driver, DSN: rawDSN}, WithDialect(tt.dialect))
			if err == nil {
				t.Fatal("Open() should reject a known driver/dialect family mismatch")
			}
			if !strings.Contains(err.Error(), "WithDialect") ||
				!strings.Contains(err.Error(), tt.driver) {
				t.Fatalf("Open() error = %q, want driver/dialect mismatch context", err)
			}
			if strings.Contains(err.Error(), rawDSN) {
				t.Fatalf("Open() mismatch error leaked Config.DSN: %q", err)
			}
		})
	}
}

func TestDriverDialectDetection(t *testing.T) {
	tests := []struct {
		driver string
		want   driverFamily
	}{
		{"postgres", driverFamilyPostgres},
		{"pgx", driverFamilyPostgres},
		{"mysql", driverFamilyMySQL},
		{"sqlite3", driverFamilySQLite},
		{"sqlite", driverFamilySQLite},
		{"sqliteshim", driverFamilySQLite},
		{"POSTGRES", driverFamilyPostgres},
		{"notmysql", driverFamilyUnknown},
		{"postgres-proxy", driverFamilyUnknown},
		{"my-sqlite-wrapper", driverFamilyUnknown},
		{"unknown", driverFamilyUnknown},
		{"", driverFamilyUnknown},
	}
	for _, tt := range tests {
		got := resolveDriverFamily(tt.driver)
		if got != tt.want {
			t.Errorf("resolveDriverFamily(%q) = %v, want %v", tt.driver, got, tt.want)
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

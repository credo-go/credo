package sqldb_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun/dialect/sqlitedialect"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store/sqldb"
)

// gateConnector opens SQLite connections, refuses them, or hangs until the
// caller's context ends, and counts the pings its connections answer.
type gateConnector struct {
	drv     driver.Driver
	dsn     string
	mode    atomic.Int32 // gateAccept, gateRefuse or gateHang
	pings   atomic.Int32
	connect atomic.Int32
}

const (
	gateAccept int32 = iota
	gateRefuse
	gateHang
)

var errRefused = errors.New("dial tcp 127.0.0.1:5432: connection refused")

func newGateConnector(t *testing.T, mode int32) *gateConnector {
	t.Helper()
	probe, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	drv := probe.Driver()
	_ = probe.Close()
	g := &gateConnector{
		drv: drv,
		dsn: fmt.Sprintf("file:credo-gate-%d?mode=memory&cache=shared", sqliteMemorySequence.Add(1)),
	}
	g.mode.Store(mode)
	return g
}

func (g *gateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	g.connect.Add(1)
	switch g.mode.Load() {
	case gateRefuse:
		return nil, errRefused
	case gateHang:
		<-ctx.Done()
		return nil, ctx.Err()
	}
	conn, err := g.drv.Open(g.dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, pings: &g.pings}, nil
}

func (g *gateConnector) Driver() driver.Driver { return g.drv }

// countingConn counts the pings database/sql sends through it.
type countingConn struct {
	driver.Conn
	pings *atomic.Int32
}

func (c *countingConn) Ping(ctx context.Context) error {
	c.pings.Add(1)
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func openGateDB(t *testing.T, g *gateConnector, cfg sqldb.Config, opts ...sqldb.Option) *sqldb.DB {
	t.Helper()
	opts = append([]sqldb.Option{sqldb.WithConnector(g), sqldb.WithDialect(sqlitedialect.New())}, opts...)
	db, err := sqldb.Open(&cfg, opts...)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })
	return db
}

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns the JSON records whose message is msg.
func (b *syncBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func newComponentApp(t *testing.T, logs *syncBuffer) *credo.App {
	t.Helper()
	opts := []credo.Option{credo.WithRawConfig(emptyConfig{})}
	if logs != nil {
		opts = append(opts, credo.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	}
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatalf("credo.New() = %v", err)
	}
	return app
}

func shutdown(t *testing.T, app *credo.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

// readyBody serves /ready and returns its status code and decoded body.
func readyBody(t *testing.T, app *credo.App) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/ready body %q: %v", rec.Body, err)
	}
	return rec.Code, body
}

func entry(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	checks, _ := body["checks"].(map[string]any)
	e, ok := checks[name].(map[string]any)
	if !ok {
		t.Fatalf("/ready = %v, want an entry %q", body, name)
	}
	return e
}

// dbConsumer depends on the database; it records what it saw.
type dbConsumer struct {
	db             *sqldb.DB
	g              *gateConnector
	pingedAtStart  bool
	openAtShutdown bool
}

func (c *dbConsumer) Start(context.Context) error {
	c.pingedAtStart = c.g.pings.Load() > 0
	return nil
}

func (c *dbConsumer) Shutdown(ctx context.Context) error {
	c.openAtShutdown = c.db.Ping(ctx) == nil
	return nil
}

// TestComponent_BoundDatabase: a database bound with ProvideValue is pinged
// before its dependants start, reported by /ready under its type, and shut
// down after its consumers.
func TestComponent_BoundDatabase(t *testing.T) {
	g := newGateConnector(t, gateAccept)
	db := openGateDB(t, g, sqldb.Config{MaxOpen: 2})
	app := newComponentApp(t, nil)
	app.ProvideValue(db)
	var consumer *dbConsumer
	app.Provide(func(d *sqldb.DB) *dbConsumer {
		consumer = &dbConsumer{db: d, g: g}
		return consumer
	})
	app.UseHealth()
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if !consumer.pingedAtStart {
		t.Error("the consumer started before the database was pinged")
	}
	code, body := readyBody(t, app)
	if e := entry(t, body, "*sqldb.DB"); code != http.StatusOK || e["status"] != "up" || e["latency"] == "" {
		t.Errorf("/ready = %d %v, want 200 with *sqldb.DB up and its latency", code, body)
	}
	shutdown(t, app)
	if !consumer.openAtShutdown {
		t.Error("the database was closed before its consumer shut down")
	}
	if err := db.Ping(t.Context()); err == nil {
		t.Error("Ping() after Shutdown = nil, want the database closed")
	}
}

// TestComponent_FailedPing: a failing ping fails the start, rolls back and
// leaves the database closed; a ping that outlives PingTimeout fails with
// the deadline in the error.
func TestComponent_FailedPing(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode int32
		cfg  sqldb.Config
		want string
	}{
		{"refused", gateRefuse, sqldb.Config{}, "sqldb: ping: " + errRefused.Error()},
		{"deadline", gateHang, sqldb.Config{PingTimeout: 50 * time.Millisecond},
			"sqldb: ping did not answer within 50ms: context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateConnector(t, tc.mode)
			db := openGateDB(t, g, tc.cfg)
			app := newComponentApp(t, nil)
			app.ProvideValue(db)
			err := app.Start(t.Context())
			if _, ok := errors.AsType[*credo.LifecycleError](err); !ok || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start() = %v, want a lifecycle error containing %q", err, tc.want)
			}
			if pingErr := db.Ping(t.Context()); pingErr == nil || !strings.Contains(pingErr.Error(), "closed") {
				t.Errorf("Ping() after the failed start = %v, want the database closed", pingErr)
			}
		})
	}
}

// TestOpen_NegativePingTimeoutFails: a negative PingTimeout fails Open.
func TestOpen_NegativePingTimeoutFails(t *testing.T) {
	_, err := sqldb.Open(&sqldb.Config{Driver: "sqlite", DSN: ":memory:", PingTimeout: -time.Second})
	if want := "sqldb: ping timeout must be >= 0, got -1s"; err == nil || err.Error() != want {
		t.Fatalf("Open() = %v, want %q", err, want)
	}
}

// TestStart_LogsThePoolWarning: Start logs the pool warning once after a
// successful ping, with its message, its code and module=sqldb, without
// store: to the App's logger scoped to the component, or to WithLogger's
// logger. It follows the effective pool, and a failed ping logs none.
func TestStart_LogsThePoolWarning(t *testing.T) {
	const msg = "credo: store configuration warning"
	for _, tc := range []struct {
		name      string
		cfg       sqldb.Config
		adjust    func(db *sqldb.DB)
		own       bool
		mode      int32
		wantCount int
	}{
		{"unlimited pool, the App's logger", sqldb.Config{}, nil, false, gateAccept, 1},
		{"unlimited pool, WithLogger", sqldb.Config{}, nil, true, gateAccept, 1},
		{"bounded pool", sqldb.Config{MaxOpen: 5}, nil, false, gateAccept, 0},
		{"bounded pool reset to unlimited", sqldb.Config{MaxOpen: 5},
			func(db *sqldb.DB) { db.Client().DB.SetMaxOpenConns(0) }, false, gateAccept, 1},
		{"unlimited pool given a maximum", sqldb.Config{},
			func(db *sqldb.DB) { db.Client().DB.SetMaxOpenConns(5) }, false, gateAccept, 0},
		{"failed ping", sqldb.Config{}, nil, false, gateRefuse, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appLogs, ownLogs := &syncBuffer{}, &syncBuffer{}
			g := newGateConnector(t, tc.mode)
			var opts []sqldb.Option
			if tc.own {
				opts = append(opts, sqldb.WithLogger(slog.New(slog.NewJSONHandler(ownLogs, nil))))
			}
			db := openGateDB(t, g, tc.cfg, opts...)
			if tc.adjust != nil {
				tc.adjust(db)
			}
			app := newComponentApp(t, appLogs)
			app.ProvideValue(db)
			startErr := app.Start(t.Context())
			if (startErr == nil) != (tc.mode == gateAccept) {
				t.Fatalf("Start() = %v", startErr)
			}
			if startErr == nil {
				shutdown(t, app)
			}
			sink, other := appLogs, ownLogs
			if tc.own {
				sink, other = ownLogs, appLogs
			}
			recs := sink.records(t, msg)
			if len(recs) != tc.wantCount || len(other.records(t, msg)) != 0 {
				t.Fatalf("warnings = %v (other sink %v), want %d on the expected sink",
					recs, other.records(t, msg), tc.wantCount)
			}
			for _, rec := range recs {
				if rec["code"] != "sqldb.pool.max_open_unlimited" || rec["module"] != "sqldb" || rec["level"] != "WARN" {
					t.Errorf("warning = %v, want the code, module=sqldb, at Warn", rec)
				}
				if _, ok := rec["store"]; ok {
					t.Errorf("warning = %v carries store; the key is retired", rec)
				}
				wantComponent := any("*sqldb.DB")
				if tc.own {
					wantComponent = nil
				}
				if rec["component"] != wantComponent {
					t.Errorf("warning component = %v, want %v", rec["component"], wantComponent)
				}
			}
		})
	}
}

// ordersDB is a semantic wrapper: it embeds the handle and keeps no state
// of its own, so it shares the handle's resource identity.
type ordersDB struct{ *sqldb.DB }

// ordersConsumer depends on the wrapper.
type ordersConsumer struct {
	db             *ordersDB
	openAtShutdown bool
}

func (c *ordersConsumer) Shutdown(ctx context.Context) error {
	c.openAtShutdown = c.db.Ping(ctx) == nil
	return nil
}

// TestComponent_RawAndWrapperAreOneDatabase: a raw handle and a wrapper that
// embeds it give one ping, one readiness entry and one teardown, after the
// consumers of both, in either registration order; the entry carries a name
// given on either.
func TestComponent_RawAndWrapperAreOneDatabase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(app *credo.App, db *sqldb.DB)
		want     string
	}{
		{"raw first", func(app *credo.App, db *sqldb.DB) {
			app.ProvideValue(db)
			app.ProvideValue(&ordersDB{DB: db}, credo.ComponentName("orders-db"))
		}, "orders-db"},
		{"wrapper first", func(app *credo.App, db *sqldb.DB) {
			app.ProvideValue(&ordersDB{DB: db})
			app.ProvideValue(db, credo.ComponentName("primary"))
		}, "primary"},
		{"wrapper built by a constructor", func(app *credo.App, db *sqldb.DB) {
			app.ProvideValue(db)
			app.Provide(func(d *sqldb.DB) *ordersDB { return &ordersDB{DB: d} })
		}, "*sqldb.DB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateConnector(t, gateAccept)
			db := openGateDB(t, g, sqldb.Config{MaxOpen: 2})
			app := newComponentApp(t, nil)
			tc.register(app, db)
			var raw *dbConsumer
			var orders *ordersConsumer
			app.Provide(func(d *sqldb.DB) *dbConsumer { raw = &dbConsumer{db: d, g: g}; return raw })
			app.Provide(func(o *ordersDB) *ordersConsumer { orders = &ordersConsumer{db: o}; return orders })
			app.UseHealth()
			if err := app.Start(t.Context()); err != nil {
				t.Fatalf("Start() = %v", err)
			}
			if n := g.pings.Load(); n != 1 {
				t.Errorf("pings at start = %d, want 1", n)
			}
			// The consumers of both are built, as a running App's would be.
			app.MustResolve[*dbConsumer]()
			app.MustResolve[*ordersConsumer]()
			_, body := readyBody(t, app)
			checks, _ := body["checks"].(map[string]any)
			if len(checks) != 1 {
				t.Errorf("/ready = %v, want one entry", body)
			}
			entry(t, body, tc.want)
			shutdown(t, app)
			if !raw.openAtShutdown || !orders.openAtShutdown {
				t.Errorf("the database closed before a consumer shut down (raw %v, wrapper %v)",
					raw.openAtShutdown, orders.openAtShutdown)
			}
		})
	}
}

// TestComponent_RawAndWrapperAgreeOnTheEntry: credo.OptionalReadiness() on
// one of the raw handle and the wrapper alone, or two different names,
// panics at the second ProvideValue, and fails the start when a Provide
// constructor builds the second value, naming both registrations, the
// option and the remedies.
func TestComponent_RawAndWrapperAgreeOnTheEntry(t *testing.T) {
	const optionalMsg = "credo: App.ProvideValue[*sqldb_test.ordersDB]: *sqldb.DB and *sqldb_test.ordersDB " +
		"hold one resource, which has one readiness entry, but disagree on credo.OptionalReadiness(): " +
		"*sqldb.DB has it and *sqldb_test.ordersDB does not; give both the same options, or hold the handle " +
		"in a named field so that each is a resource of its own"
	const nameMsg = "credo: App.ProvideValue[*sqldb_test.ordersDB]: *sqldb.DB and *sqldb_test.ordersDB " +
		"hold one resource, which has one readiness entry and one teardown, but name it differently with " +
		"credo.ComponentName, \"primary\" and \"orders\"; give both the same options, or hold the handle in a " +
		"named field so that each is a resource of its own"
	for _, tc := range []struct {
		name    string
		rawOpts []credo.RegistrationOption
		wrapOpt []credo.RegistrationOption
		want    string
	}{
		{"OptionalReadiness on one", []credo.RegistrationOption{credo.OptionalReadiness()}, nil, optionalMsg},
		{"two names", []credo.RegistrationOption{credo.ComponentName("primary")},
			[]credo.RegistrationOption{credo.ComponentName("orders")}, nameMsg},
	} {
		t.Run(tc.name+", ProvideValue", func(t *testing.T) {
			db := openGateDB(t, newGateConnector(t, gateAccept), sqldb.Config{})
			app := newComponentApp(t, nil)
			app.ProvideValue(db, tc.rawOpts...)
			defer func() {
				if r := recover(); fmt.Sprint(r) != tc.want {
					t.Fatalf("panic = %v\nwant  %s", r, tc.want)
				}
			}()
			app.ProvideValue(&ordersDB{DB: db}, tc.wrapOpt...)
		})
		t.Run(tc.name+", Provide", func(t *testing.T) {
			db := openGateDB(t, newGateConnector(t, gateAccept), sqldb.Config{})
			app := newComponentApp(t, nil)
			app.ProvideValue(db, tc.rawOpts...)
			app.Provide(func(d *sqldb.DB) *ordersDB { return &ordersDB{DB: d} }, tc.wrapOpt...)
			err := app.Start(t.Context())
			want := strings.TrimPrefix(tc.want, "credo: App.ProvideValue[*sqldb_test.ordersDB]: ")
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Start() = %v\nwant it to contain %s", err, want)
			}
		})
	}
}

// TestComponent_BorrowedDatabase: a borrowed database is never pinged or
// shut down, and is reported by /ready; with credo.OptionalReadiness() it
// does not gate.
func TestComponent_BorrowedDatabase(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run(fmt.Sprintf("optional=%v", optional), func(t *testing.T) {
			g := newGateConnector(t, gateAccept)
			db := openGateDB(t, g, sqldb.Config{MaxOpen: 2})
			app := newComponentApp(t, nil)
			opts := []credo.RegistrationOption{credo.Borrowed()}
			if optional {
				opts = append(opts, credo.OptionalReadiness())
			}
			app.ProvideValue(db, opts...)
			app.UseHealth()
			if err := app.Start(t.Context()); err != nil {
				t.Fatalf("Start() = %v", err)
			}
			if n := g.pings.Load(); n != 0 {
				t.Errorf("pings at start = %d, want 0: the owner starts a borrowed database", n)
			}
			g.mode.Store(gateRefuse)
			_ = db.Client().DB.Close() // drop the pooled connections; the owner's pool is down
			code, body := readyBody(t, app)
			e := entry(t, body, "*sqldb.DB")
			wantCode, wantStatus := http.StatusServiceUnavailable, "down"
			if optional {
				wantCode, wantStatus = http.StatusOK, "degraded"
			}
			if code != wantCode || body["status"] != wantStatus || e["status"] != "down" {
				t.Errorf("/ready = %d %v, want %d %s with the database down", code, body, wantCode, wantStatus)
			}
			shutdown(t, app)
		})
	}
	t.Run("never shut down", func(t *testing.T) {
		db := openGateDB(t, newGateConnector(t, gateAccept), sqldb.Config{MaxOpen: 2})
		app := newComponentApp(t, nil)
		app.ProvideValue(db, credo.Borrowed())
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("Start() = %v", err)
		}
		shutdown(t, app)
		if err := db.Ping(t.Context()); err != nil {
			t.Errorf("Ping() after the App's shutdown = %v; a borrowed database stays open", err)
		}
	})
}

// TestComponent_DownDatabaseInReady: /ready renders the database down when
// its ping fails, under its given name, and logs the failure in the check
// shape; Ready's error wraps the ping's cause.
func TestComponent_DownDatabaseInReady(t *testing.T) {
	logs := &syncBuffer{}
	g := newGateConnector(t, gateAccept)
	db := openGateDB(t, g, sqldb.Config{MaxOpen: 2})
	app := newComponentApp(t, logs)
	app.ProvideValue(db, credo.ComponentName("primary"))
	app.UseHealth()
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() { shutdown(t, app) })
	g.mode.Store(gateRefuse)
	db.Client().DB.SetMaxIdleConns(0) // the pooled connection goes; the next ping dials
	code, body := readyBody(t, app)
	if e := entry(t, body, "primary"); code != http.StatusServiceUnavailable || e["status"] != "down" {
		t.Fatalf("/ready = %d %v, want 503 with primary down", code, body)
	}
	recs := logs.records(t, "credo: health check failed")
	if len(recs) != 1 || recs[0]["check"] != "primary" || recs[0]["kind"] != "readiness" ||
		!strings.Contains(fmt.Sprint(recs[0]["error"]), errRefused.Error()) || recs[0]["latency"] == nil {
		t.Errorf("failure log = %v, want one readiness record for primary with the error and latency", recs)
	}
	if err := db.Ready(t.Context()); !errors.Is(err, errRefused) {
		t.Errorf("Ready() = %v, want it to wrap the ping's cause", err)
	}
}

// TestComponent_WithOptionalStart: through a connector that refuses
// connections the start succeeds, the failed ping is logged at Warn with
// the component's name and the error, and the pool warnings are logged;
// /ready reports the database down, answering 503 without
// credo.OptionalReadiness() and 200 degraded with it; once the connector
// accepts, Ready passes and a query runs on the same pool. Without the
// option the same start fails and closes the database.
func TestComponent_WithOptionalStart(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run(fmt.Sprintf("OptionalReadiness=%v", optional), func(t *testing.T) {
			logs := &syncBuffer{}
			g := newGateConnector(t, gateRefuse)
			db := openGateDB(t, g, sqldb.Config{PingTimeout: time.Second}, sqldb.WithOptionalStart())
			app := newComponentApp(t, logs)
			var opts []credo.RegistrationOption
			if optional {
				opts = append(opts, credo.OptionalReadiness())
			}
			app.ProvideValue(db, opts...)
			app.UseHealth()
			if err := app.Start(t.Context()); err != nil {
				t.Fatalf("Start() = %v, want the start to succeed", err)
			}
			t.Cleanup(func() { shutdown(t, app) })
			warn := logs.records(t, "sqldb: ping failed; starting without the database, which gates readiness until it answers")
			if len(warn) != 1 || warn[0]["level"] != "WARN" || warn[0]["component"] != "*sqldb.DB" ||
				warn[0]["module"] != "sqldb" || warn[0]["ping_timeout"] == nil ||
				!strings.Contains(fmt.Sprint(warn[0]["error"]), errRefused.Error()) {
				t.Errorf("ping log = %v, want one Warn with the component, the deadline and the error", warn)
			}
			if pool := logs.records(t, "credo: store configuration warning"); len(pool) != 1 {
				t.Errorf("pool warnings = %v, want one", pool)
			}
			code, body := readyBody(t, app)
			wantCode, wantStatus := http.StatusServiceUnavailable, "down"
			if optional {
				wantCode, wantStatus = http.StatusOK, "degraded"
			}
			if code != wantCode || body["status"] != wantStatus || entry(t, body, "*sqldb.DB")["status"] != "down" {
				t.Errorf("/ready = %d %v, want %d %s", code, body, wantCode, wantStatus)
			}
			g.mode.Store(gateAccept)
			if err := db.Ready(t.Context()); err != nil {
				t.Fatalf("Ready() once the server accepts = %v", err)
			}
			var one int
			if err := db.Client().QueryRowContext(t.Context(), "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("query on the same pool = %d, %v", one, err)
			}
			if code, body := readyBody(t, app); code != http.StatusOK || body["status"] != "up" {
				t.Errorf("/ready after recovery = %d %v, want 200 up", code, body)
			}
		})
	}
	t.Run("without the option", func(t *testing.T) {
		db := openGateDB(t, newGateConnector(t, gateRefuse), sqldb.Config{})
		app := newComponentApp(t, nil)
		app.ProvideValue(db)
		if err := app.Start(t.Context()); err == nil {
			t.Fatal("Start() = nil, want the failed ping")
		}
		if err := db.Ping(t.Context()); err == nil {
			t.Error("Ping() after the failed start = nil, want the database closed")
		}
	})
}

// emptyConfig is a hermetic RawConfig.
type emptyConfig struct{}

func (emptyConfig) Unmarshal(string, any) error { return nil }
func (emptyConfig) Exists(string) bool          { return false }

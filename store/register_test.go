package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
)

// testDB is a mock database type for registration tests.
type testDB struct {
	*mockLifecycle
}

func newTestDB(lc *mockLifecycle) *testDB {
	return &testDB{mockLifecycle: lc}
}

func newTestApp(t *testing.T, opts ...credo.Option) *credo.App {
	t.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatalf("credo.New() = %v", err)
	}
	return app
}

// freePort returns a host and port that were free at probe time.
func freePort(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// runApp starts app.Run in the background and waits until it is running,
// so that app.Shutdown can exercise the real container shutdown path.
func runApp(t *testing.T, app *credo.App) {
	t.Helper()
	go app.Run()

	deadline := time.Now().Add(5 * time.Second)
	for !app.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("server did not reach running state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRegister_Success(t *testing.T) {
	app := newTestApp(t)
	db := newTestDB(&mockLifecycle{
		health: store.Health{Status: store.StatusUp},
	})

	if err := store.Register[*testDB](app, db); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	// Verify the value is in DI.
	resolved, err := credo.Resolve[*testDB](app)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if resolved != db {
		t.Error("Resolve returned different instance")
	}

	// Verify the registry is in DI.
	reg, err := credo.Resolve[*store.Registry](app)
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}

	health := reg.HealthAll(context.Background())
	if len(health) != 1 {
		t.Fatalf("HealthAll() = %d entries, want 1", len(health))
	}
}

func TestRegister_PingFailure_Cleanup(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{pingErr: fmt.Errorf("connection refused")}
	db := newTestDB(lc)

	err := store.Register[*testDB](app, db)
	if err == nil {
		t.Fatal("Register() should fail when ping fails")
	}

	// The caller still owns the lifecycle on registration failure.
	lc.mu.Lock()
	called := lc.shutCalled
	lc.mu.Unlock()
	if called {
		t.Error("Shutdown should not be called for caller-owned lifecycle on ping failure")
	}

	// Value should NOT be in DI.
	_, resolveErr := credo.Resolve[*testDB](app)
	if resolveErr == nil {
		t.Error("value should not be in DI after failed registration")
	}
}

func TestRegister_NilValue(t *testing.T) {
	app := newTestApp(t)
	if err := store.Register[*testDB](app, nil); err == nil {
		t.Fatal("Register(nil value) should return error")
	}
}

func TestRegister_NilApp(t *testing.T) {
	db := newTestDB(&mockLifecycle{})
	if err := store.Register[*testDB](nil, db); err == nil {
		t.Fatal("Register(nil app) should return error")
	}
}

func TestRegister_NoLifecycle(t *testing.T) {
	app := newTestApp(t)
	// string does not implement Lifecycle and no WithLifecycle provided.
	if err := store.Register[string](app, "not-a-db"); err == nil {
		t.Fatal("Register without Lifecycle should return error")
	}
}

func TestRegister_WithName(t *testing.T) {
	app := newTestApp(t)
	db := newTestDB(&mockLifecycle{
		health: store.Health{Status: store.StatusUp},
	})

	if err := store.Register[*testDB](app, db, store.WithName("custom-db")); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	reg, _ := credo.Resolve[*store.Registry](app)
	health := reg.HealthAll(context.Background())
	if _, ok := health["custom-db"]; !ok {
		t.Error("HealthAll should contain entry with custom name")
	}
}

func TestRegister_WithPingTimeout(t *testing.T) {
	app := newTestApp(t)
	db := newTestDB(&mockLifecycle{
		health: store.Health{Status: store.StatusUp},
	})

	if err := store.Register[*testDB](app, db, store.WithPingTimeout(1*time.Second)); err != nil {
		t.Fatalf("Register() = %v", err)
	}
}

func TestRegister_InvalidPingTimeout(t *testing.T) {
	app := newTestApp(t)
	db := newTestDB(&mockLifecycle{})

	if err := store.Register[*testDB](app, db, store.WithPingTimeout(-1*time.Second)); err == nil {
		t.Fatal("Register with negative ping timeout should return error")
	}
}

// wrapperDB does not implement Lifecycle — uses WithLifecycle.
type wrapperDB struct {
	inner *testDB
}

func TestRegister_WithLifecycle(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	inner := newTestDB(lc)
	wrapper := &wrapperDB{inner: inner}

	if err := store.Register[*wrapperDB](app, wrapper, store.WithLifecycle(lc)); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	resolved, err := credo.Resolve[*wrapperDB](app)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if resolved != wrapper {
		t.Error("Resolve returned different instance")
	}
}

func TestRegister_ShutdownExactlyOnceViaDI(t *testing.T) {
	host, port := freePort(t)
	app := newTestApp(t, credo.WithAddr(host, port))

	var seq []string
	lc := &mockLifecycle{name: "db", shutdownSeq: &seq, health: store.Health{Status: store.StatusUp}}
	if err := store.Register[*testDB](app, newTestDB(lc)); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	runApp(t, app)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}

	if len(seq) != 1 {
		t.Fatalf("lifecycle Shutdown ran %d times, want exactly 1 (DI owns closing; the Registry must not close)", len(seq))
	}
}

func TestRegister_WithLifecycle_CallerOwnsClosing(t *testing.T) {
	host, port := freePort(t)

	var logBuf bytes.Buffer
	app := newTestApp(t, credo.WithAddr(host, port),
		credo.WithLogger(slog.New(slog.NewJSONHandler(&logBuf, nil))))

	lc := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	wrapper := &wrapperDB{inner: newTestDB(lc)}
	if err := store.Register[*wrapperDB](app, wrapper, store.WithLifecycle(lc)); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	// The value has no Shutdowner — Register must warn about the missing
	// framework-owned closing path.
	if !strings.Contains(logBuf.String(), "will not be closed by the framework") {
		t.Errorf("Register should warn when value does not implement Shutdowner; log: %s", logBuf.String())
	}

	runApp(t, app)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}

	lc.mu.Lock()
	called := lc.shutCalled
	lc.mu.Unlock()
	if called {
		t.Error("WithLifecycle handle was closed by the framework; closing belongs to the caller")
	}
}

func TestRegister_DuplicateType(t *testing.T) {
	app := newTestApp(t)
	db1 := newTestDB(&mockLifecycle{health: store.Health{Status: store.StatusUp}})
	db2 := newTestDB(&mockLifecycle{health: store.Health{Status: store.StatusUp}})

	if err := store.Register[*testDB](app, db1, store.WithName("db1")); err != nil {
		t.Fatalf("first Register() = %v", err)
	}

	// Second registration of same type should fail (DI already has *testDB).
	err := store.Register[*testDB](app, db2, store.WithName("db2"))
	if err == nil {
		t.Fatal("second Register of same type should return error")
	}
	if db2.shutCalled {
		t.Fatal("duplicate type failure should not shut down caller-owned lifecycle")
	}
}

func TestRegister_HealthAppearsInReadiness(t *testing.T) {
	// End-to-end across the module-internal health seam: Register provides
	// the store-health collector via DI, UseHealth's readiness handler
	// resolves it lazily, and the store shows up in GET /ready.
	app := newTestApp(t)
	db := newTestDB(&mockLifecycle{
		health: store.Health{Status: store.StatusUp, Latency: 2 * time.Millisecond},
	})
	if err := store.Register[*testDB](app, db, store.WithName("pg")); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	app.UseHealth()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ready", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("/ready status = %d, want %d (body: %s)", w.Code, http.StatusOK, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("expected checks map in response, got: %s", w.Body.String())
	}
	pg, ok := checks["pg"].(map[string]any)
	if !ok {
		t.Fatalf("expected pg entry in checks, got: %s", w.Body.String())
	}
	if pg["status"] != "up" {
		t.Errorf("pg status = %v, want %q", pg["status"], "up")
	}
}

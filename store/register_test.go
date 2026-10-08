package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func up() store.Health { return store.Health{Status: store.StatusUp} }

// mustPanic runs fn and returns the panic message it raised.
func mustPanic(t *testing.T, fn func()) string {
	t.Helper()
	var msg string
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected a panic")
			}
			msg, _ = r.(string)
			if err, ok := r.(error); ok {
				msg = err.Error()
			}
		}()
		fn()
	}()
	return msg
}

// readyChecks serves GET /ready and returns the status code and the checks map.
func readyChecks(t *testing.T, app *credo.App) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal /ready: %v (body: %s)", err, w.Body.String())
	}
	checks, _ := body["checks"].(map[string]any)
	return w.Code, checks
}

// warningRecords returns the store configuration warnings logged as JSON.
func warningRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if record["msg"] == "credo: store configuration warning" {
			records = append(records, record)
		}
	}
	return records
}

func TestRegister_PerformsNoIOAndPingsAtStart(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: up()}
	app.ProvideValue(newTestDB(lc))
	store.Register[*testDB](app)

	if ping, _, _ := lc.calls(); ping != 0 {
		t.Fatalf("Ping calls after Register = %d, want 0", ping)
	}
	startApp(t, app)
	if ping, _, _ := lc.calls(); ping != 1 {
		t.Fatalf("Ping calls after Start = %d, want 1", ping)
	}
}

func TestRegister_HealthAppearsInReadiness(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: store.Health{Status: store.StatusUp, Latency: 2 * time.Millisecond}}
	app.ProvideValue(newTestDB(lc))
	store.Register[*testDB](app, store.WithName("pg"))
	app.UseHealth()
	startApp(t, app)

	for range 2 {
		code, checks := readyChecks(t, app)
		if code != http.StatusOK {
			t.Fatalf("/ready status = %d, want %d", code, http.StatusOK)
		}
		pg, ok := checks["pg"].(map[string]any)
		if !ok {
			t.Fatalf("expected pg entry in checks, got %v", checks)
		}
		if pg["status"] != "up" {
			t.Errorf("pg status = %v, want %q", pg["status"], "up")
		}
	}
	if _, _, health := lc.calls(); health < 2 {
		t.Fatalf("Health calls = %d, want one per readiness request", health)
	}
}

func TestRegister_DownStoreFailsReadiness(t *testing.T) {
	app := newTestApp(t)
	app.ProvideValue(newTestDB(&mockLifecycle{health: store.Health{Status: store.StatusDown}}))
	store.Register[*testDB](app, store.WithName("pg"))
	app.UseHealth()
	startApp(t, app)

	code, checks := readyChecks(t, app)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/ready status = %d, want %d", code, http.StatusServiceUnavailable)
	}
	if pg, _ := checks["pg"].(map[string]any); pg["status"] != "down" {
		t.Fatalf("pg check = %v, want status down", checks["pg"])
	}
}

func TestRegister_ReadinessReportsShuttingDown(t *testing.T) {
	app := newTestApp(t)
	release := make(chan struct{})
	app.ProvideValue(newTestDB(&mockLifecycle{health: up()}))
	store.Register[*testDB](app, store.WithName("pg"))
	app.UseHealth()
	entered := make(chan struct{})
	app.OnStop(func(context.Context) error {
		close(entered)
		<-release
		return nil
	})
	startApp(t, app)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- app.Shutdown(ctx)
	}()
	<-entered
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "shutting_down") {
		t.Fatalf("/ready during shutdown = %d %s, want 503 shutting_down", w.Code, w.Body.String())
	}
}

type warningDB struct {
	*mockLifecycle
	codes []string
}

func (db *warningDB) StoreRegistrationWarningCodes() []string {
	return db.codes
}

func TestRegister_EmitsConfigurationWarningsAfterPing(t *testing.T) {
	var logs bytes.Buffer
	app := newTestApp(t, credo.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	app.ProvideValue(&warningDB{
		mockLifecycle: &mockLifecycle{health: up()},
		codes:         []string{"sqldb.pool.max_open_unlimited"},
	})
	store.Register[*warningDB](app, store.WithName("primary"))

	if records := warningRecords(t, &logs); len(records) != 0 {
		t.Fatalf("warnings before Start = %v, want none", records)
	}
	startApp(t, app)

	records := warningRecords(t, &logs)
	if len(records) != 1 {
		t.Fatalf("warning records = %d, want 1\nlogs: %s", len(records), logs.String())
	}
	for key, want := range map[string]string{
		"level":     "WARN",
		"component": "store",
		"store":     "primary",
		"code":      "sqldb.pool.max_open_unlimited",
	} {
		if got := records[0][key]; got != want {
			t.Errorf("warning %s = %#v, want %q", key, got, want)
		}
	}
}

func TestRegister_DoesNotEmitConfigurationWarningsOnPingFailure(t *testing.T) {
	var logs bytes.Buffer
	app := newTestApp(t, credo.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	app.ProvideValue(&warningDB{
		mockLifecycle: &mockLifecycle{pingErr: errors.New("unavailable")},
		codes:         []string{"sqldb.pool.max_open_unlimited"},
	})
	store.Register[*warningDB](app)

	if err := app.Start(t.Context()); err == nil {
		t.Fatal("Start() should fail when Ping fails")
	}
	if records := warningRecords(t, &logs); len(records) != 0 {
		t.Fatalf("warnings on a failed ping = %v, want none", records)
	}
}

func TestRegister_RejectsInvalidConfigurationWarningBeforePingWithoutLeak(t *testing.T) {
	const secret = "super-secret-password"
	var logs bytes.Buffer
	lc := &mockLifecycle{}
	app := newTestApp(t, credo.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	app.ProvideValue(&warningDB{mockLifecycle: lc, codes: []string{"password=" + secret}})
	store.Register[*warningDB](app)

	err := app.Start(t.Context())
	if err == nil {
		t.Fatal("Start() should reject an invalid warning code")
	}
	if ping, _, _ := lc.calls(); ping != 0 {
		t.Fatalf("Ping calls = %d, want 0", ping)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("invalid warning code leaked secret: error=%q log=%q", err, logs.String())
	}
}

func TestRegister_DeduplicatesConfigurationWarningsInOrder(t *testing.T) {
	var logs bytes.Buffer
	app := newTestApp(t, credo.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	app.ProvideValue(&warningDB{
		mockLifecycle: &mockLifecycle{health: up()},
		codes: []string{
			"sqldb.pool.max_open_unlimited",
			"sqldb.pool.max_open_unlimited",
			"sqldb.pool.idle_disabled",
		},
	})
	store.Register[*warningDB](app)
	startApp(t, app)

	records := warningRecords(t, &logs)
	wantCodes := []string{"sqldb.pool.max_open_unlimited", "sqldb.pool.idle_disabled"}
	if len(records) != len(wantCodes) {
		t.Fatalf("warning records = %d, want %d\nlogs: %s", len(records), len(wantCodes), logs.String())
	}
	for i, record := range records {
		if got := record["code"]; got != wantCodes[i] {
			t.Errorf("warning %d code = %#v, want %q", i, got, wantCodes[i])
		}
	}
}

func TestRegister_PingFailureFailsStartAndRollsTheStoreBack(t *testing.T) {
	app := newTestApp(t)
	pingErr := errors.New("connection refused")
	lc := &mockLifecycle{pingErr: pingErr}
	app.ProvideValue(newTestDB(lc))
	store.Register[*testDB](app, store.WithName("pg"))

	err := app.Start(t.Context())
	if !errors.Is(err, pingErr) {
		t.Fatalf("Start() = %v, want the ping error", err)
	}
	if _, ok := errors.AsType[*credo.LifecycleError](err); !ok {
		t.Fatalf("Start() = %T, want *credo.LifecycleError", err)
	}
	if !strings.Contains(err.Error(), `"pg"`) {
		t.Errorf("Start() = %q, want it to name the store", err)
	}
	// The ping opened nothing the bound value had not: the rollback still
	// shuts the value down.
	if _, shut, _ := lc.calls(); shut != 1 {
		t.Fatalf("Shutdown calls after a failed ping = %d, want 1", shut)
	}
}

func TestRegister_PingHonorsTheTimeout(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: up()}
	app.ProvideValue(newTestDB(lc))
	store.Register[*testDB](app, store.WithPingTimeout(250*time.Millisecond))
	startApp(t, app)

	lc.mu.Lock()
	ctx := lc.pingCtx
	lc.mu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("ping context has no deadline")
	}
	if remaining := time.Until(deadline); remaining > 250*time.Millisecond {
		t.Fatalf("ping deadline is %s away, want at most 250ms", remaining)
	}
}

// bareDB is a store that names no resource of its own.
type bareDB struct{ store.Lifecycle }

func TestRegister_NilValueFailsStart(t *testing.T) {
	app := newTestApp(t)
	app.Provide[*bareDB](func() *bareDB { return nil })
	store.Register[*bareDB](app)

	err := app.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the bound value is nil") {
		t.Fatalf("Start() = %v, want a nil-value error", err)
	}
}

func TestRegister_ConstructorBindingIsBuiltByTheStartPhase(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: up()}
	built := 0
	app.Provide[*testDB](func() *testDB {
		built++
		return newTestDB(lc)
	})
	store.Register[*testDB](app)
	if built != 0 {
		t.Fatalf("constructor ran %d times at registration, want 0", built)
	}
	startApp(t, app)
	if ping, _, _ := lc.calls(); built != 1 || ping != 1 {
		t.Fatalf("(built, pinged) = (%d, %d), want (1, 1)", built, ping)
	}
}

func TestRegister_PingsTheOverride(t *testing.T) {
	app := newTestApp(t)
	original := &mockLifecycle{health: up()}
	override := &mockLifecycle{health: up()}
	app.ProvideValue(newTestDB(original))
	store.Register[*testDB](app)
	app.ProvideValue(newTestDB(override), credo.Override())
	startApp(t, app)

	if ping, _, _ := original.calls(); ping != 0 {
		t.Errorf("original pinged %d times, want 0", ping)
	}
	if ping, _, _ := override.calls(); ping != 1 {
		t.Errorf("override pinged %d times, want 1", ping)
	}
}

func TestRegister_BorrowedStoreIsPingedButNotShutDown(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: up()}
	app.ProvideValue(newTestDB(lc), credo.Borrowed())
	store.Register[*testDB](app)
	startApp(t, app)
	shutdownApp(t, app)

	if ping, shut, _ := lc.calls(); ping != 1 || shut != 0 {
		t.Fatalf("(ping, shutdown) = (%d, %d), want (1, 0)", ping, shut)
	}
}

// Primary is a named interface a store may be registered as.
type Primary interface{ store.Lifecycle }

func TestRegister_AliasedInterface(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: up()}
	app.ProvideValue(newTestDB(lc))
	app.Alias[Primary, *testDB]()
	store.Register[Primary](app, store.WithName("primary"))
	app.UseHealth()
	startApp(t, app)

	if ping, _, _ := lc.calls(); ping != 1 {
		t.Fatalf("Ping calls = %d, want 1", ping)
	}
	if _, checks := readyChecks(t, app); checks["primary"] == nil {
		t.Fatalf("readiness checks = %v, want a primary entry", checks)
	}
}

func TestRegister_MissingBindingFailsFinalize(t *testing.T) {
	app := newTestApp(t)
	store.Register[*testDB](app)

	err := app.Finalize()
	if err == nil {
		t.Fatal("Finalize() should fail for a registration without a binding")
	}
	for _, want := range []string{"store.Register[*store_test.testDB]", "has no binding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Finalize() = %q, want it to contain %q", err, want)
		}
	}
}

func TestRegister_ShutsTheStoreDownOnce(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: up()}
	app.ProvideValue(newTestDB(lc))
	store.Register[*testDB](app)
	startApp(t, app)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if _, shut, _ := lc.calls(); shut != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", shut)
	}
}

type shutdownOrderDBA struct{ *mockLifecycle }
type shutdownOrderDBB struct{ *mockLifecycle }

func TestRegister_IndependentStoresShutDownInReverseRegistrationOrder(t *testing.T) {
	app := newTestApp(t)
	var order []string
	first := &mockLifecycle{name: "first", shutdownSeq: &order, health: up()}
	second := &mockLifecycle{name: "second", shutdownSeq: &order, health: up()}
	app.ProvideValue(&shutdownOrderDBA{mockLifecycle: first})
	app.ProvideValue(&shutdownOrderDBB{mockLifecycle: second})
	store.Register[*shutdownOrderDBA](app, store.WithName("first"))
	store.Register[*shutdownOrderDBB](app, store.WithName("second"))
	startApp(t, app)
	shutdownApp(t, app)

	if got := strings.Join(order, ","); got != "second,first" {
		t.Fatalf("shutdown order = %q, want %q", got, "second,first")
	}
}

// replicaDB holds the same resource as testDB: both embed one mock.
type replicaDB struct{ *mockLifecycle }

func TestRegister_TwoHoldersOfOneResourceArePingedAndShareOneShutdown(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: up()}
	app.ProvideValue(newTestDB(lc))
	app.ProvideValue(&replicaDB{mockLifecycle: lc})
	store.Register[*testDB](app, store.WithName("primary"))
	store.Register[*replicaDB](app, store.WithName("replica"))
	startApp(t, app)
	shutdownApp(t, app)

	if ping, shut, _ := lc.calls(); ping != 2 || shut != 1 {
		t.Fatalf("(ping, shutdown) = (%d, %d), want (2, 1)", ping, shut)
	}
}

func TestRegister_DefaultNameIsOperatorFriendly(t *testing.T) {
	app := newTestApp(t)
	app.ProvideValue(newTestDB(&mockLifecycle{health: up()}))
	store.Register[*testDB](app)
	app.UseHealth()
	startApp(t, app)

	if _, checks := readyChecks(t, app); checks["store_test.testDB"] == nil {
		t.Fatalf("readiness checks = %v, want a store_test.testDB entry", checks)
	}
}

type otherDB struct{ *mockLifecycle }

func TestRegister_Misuse(t *testing.T) {
	tests := []struct {
		name string
		fn   func(app *credo.App)
		want string
	}{
		{"nil app", func(*credo.App) { store.Register[*testDB](nil) }, "app must not be nil"},
		{"nil option", func(app *credo.App) { store.Register[*testDB](app, nil) }, "nil RegisterOption"},
		{"zero timeout", func(app *credo.App) {
			store.Register[*testDB](app, store.WithPingTimeout(0))
		}, "ping timeout must be > 0"},
		{"empty name", func(app *credo.App) {
			store.Register[*testDB](app, store.WithName(""))
		}, "invalid store name"},
		{"padded name", func(app *credo.App) {
			store.Register[*testDB](app, store.WithName(" pg "))
		}, "invalid store name"},
		{"reserved name", func(app *credo.App) {
			store.Register[*testDB](app, store.WithName("credo.pg"))
		}, "invalid store name"},
		{"no default name", func(app *credo.App) {
			store.Register[interface{ store.Lifecycle }](app)
		}, "no stable default name"},
		{"duplicate type", func(app *credo.App) {
			store.Register[*testDB](app, store.WithName("a"))
			store.Register[*testDB](app, store.WithName("b"))
		}, "store.Register[*store_test.testDB]"},
		{"duplicate name", func(app *credo.App) {
			store.Register[*testDB](app, store.WithName("pg"))
			store.Register[*otherDB](app, store.WithName("pg"))
		}, `"pg"`},
		{"after finalize", func(app *credo.App) {
			if err := app.Finalize(); err != nil {
				t.Fatalf("Finalize() = %v", err)
			}
			store.Register[*testDB](app)
		}, "store.Register[*store_test.testDB]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t)
			msg := mustPanic(t, func() { tt.fn(app) })
			if !strings.Contains(msg, tt.want) {
				t.Fatalf("panic = %q, want it to contain %q", msg, tt.want)
			}
		})
	}
}

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

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

// runApp starts ServeContext on an already-bound test listener and waits until
// it is running, so shutdown tests avoid a free-port probe race.
func runApp(t *testing.T, app *credo.App) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	serveCtx, cancelServe := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- app.ServeContext(serveCtx, listener) }()
	t.Cleanup(func() {
		cancelServe()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("ServeContext() = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ServeContext did not stop")
		}
	})

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
	resolved, err := app.Resolve[*testDB]()
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if resolved != db {
		t.Error("Resolve returned different instance")
	}

	// Verify the registry is in DI.
	reg, err := app.Resolve[*store.Registry]()
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
	_, resolveErr := app.Resolve[*testDB]()
	if resolveErr == nil {
		t.Error("value should not be in DI after failed registration")
	}
}

func TestRegister_NilValue(t *testing.T) {
	app := newTestApp(t)
	if err := store.Register[*testDB](app, nil); err == nil {
		t.Fatal("Register(nil value) should return error")
	}

	var typedNil *testDB
	var lifecycle store.Lifecycle = typedNil
	if err := store.Register[store.Lifecycle](app, lifecycle); err == nil {
		t.Fatal("Register(interface containing typed-nil value) should return error")
	}
}

func TestRegister_NilUnsafePointerValue(t *testing.T) {
	var value unsafe.Pointer
	lifecycle := &mockLifecycle{}
	if err := store.Register[unsafe.Pointer](
		newTestApp(t),
		value,
		store.WithLifecycle(lifecycle),
		store.WithCallerOwnedLifecycle(),
	); err == nil {
		t.Fatal("Register should reject a nil unsafe-pointer value")
	}
}

func TestRegister_NilMapAndSliceValues(t *testing.T) {
	lifecycle := &mockLifecycle{}

	var nilMap map[string]int
	if err := store.Register[map[string]int](newTestApp(t), nilMap, store.WithLifecycle(lifecycle)); err == nil {
		t.Fatal("Register(nil map) should return error")
	}

	var nilSlice []int
	if err := store.Register[[]int](newTestApp(t), nilSlice, store.WithLifecycle(lifecycle)); err == nil {
		t.Fatal("Register(nil slice) should return error")
	}
}

func TestRegister_NonNilLifecycleInterface(t *testing.T) {
	app := newTestApp(t)
	var lifecycle store.Lifecycle = newTestDB(&mockLifecycle{
		health: store.Health{Status: store.StatusUp},
	})
	if err := store.Register[store.Lifecycle](app, lifecycle, store.WithName("interface-db")); err != nil {
		t.Fatalf("Register(non-nil Lifecycle interface) = %v", err)
	}
	resolved, err := app.Resolve[store.Lifecycle]()
	if err != nil || resolved != lifecycle {
		t.Fatalf("Resolve[Lifecycle]() = (%v, %v), want original interface", resolved, err)
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

	reg, _ := app.Resolve[*store.Registry]()
	health := reg.HealthAll(context.Background())
	if _, ok := health["custom-db"]; !ok {
		t.Error("HealthAll should contain entry with custom name")
	}
}

func TestRegister_DefaultNameIsOperatorFriendly(t *testing.T) {
	app := newTestApp(t)
	db := newTestDB(&mockLifecycle{health: store.Health{Status: store.StatusUp}})

	if err := store.Register[*testDB](app, db); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	if _, exists := registry.HealthAll(t.Context())["store_test.testDB"]; !exists {
		t.Fatalf("default store name must be package-qualified without pointer syntax")
	}
}

func TestRegister_InvalidNameFailsBeforePing(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "explicit empty", value: ""},
		{name: "leading whitespace", value: " primary"},
		{name: "trailing whitespace", value: "primary "},
		{name: "reserved prefix", value: "credo.primary"},
		{name: "control character", value: "primary\nreplica"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lc := &mockLifecycle{}
			err := store.Register[*testDB](newTestApp(t), newTestDB(lc), store.WithName(tt.value))
			if err == nil {
				t.Fatalf("Register(WithName(%q)) should fail", tt.value)
			}
			lc.mu.Lock()
			pingCalls := lc.pingCalls
			lc.mu.Unlock()
			if pingCalls != 0 {
				t.Fatalf("Ping calls = %d, want 0 for invalid local input", pingCalls)
			}
		})
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

type unstableIdentityLifecycle struct {
	values    []int
	pingCalls *int
}

type nanIdentityLifecycle float64

func (nanIdentityLifecycle) Ping(context.Context) error     { return nil }
func (nanIdentityLifecycle) Shutdown(context.Context) error { return nil }
func (nanIdentityLifecycle) Health(context.Context) store.Health {
	return store.Health{Status: store.StatusUp}
}

func (l unstableIdentityLifecycle) Ping(context.Context) error {
	*l.pingCalls++
	return nil
}

func (unstableIdentityLifecycle) Shutdown(context.Context) error { return nil }

func (unstableIdentityLifecycle) Health(context.Context) store.Health {
	return store.Health{Status: store.StatusUp}
}

func TestRegister_UnstableLifecycleIdentityFailsBeforeInfrastructureOrPing(t *testing.T) {
	app := newTestApp(t)
	pingCalls := 0
	value := unstableIdentityLifecycle{values: []int{1}, pingCalls: &pingCalls}
	if err := store.Register[unstableIdentityLifecycle](app, value); err == nil {
		t.Fatal("Register should reject a non-comparable value Lifecycle")
	}
	if pingCalls != 0 {
		t.Fatalf("Ping calls = %d, want 0", pingCalls)
	}
	if _, err := app.Resolve[*store.Registry](); err == nil {
		t.Fatal("invalid lifecycle identity should not create Registry infrastructure")
	}
}

func TestRegister_NonReflexiveLifecycleIdentityFailsBeforeInfrastructure(t *testing.T) {
	app := newTestApp(t)
	if err := store.Register[nanIdentityLifecycle](app, nanIdentityLifecycle(math.NaN())); err == nil {
		t.Fatal("Register should reject a non-reflexive NaN lifecycle identity")
	}
	if _, err := app.Resolve[*store.Registry](); err == nil {
		t.Fatal("non-reflexive lifecycle identity should not create Registry infrastructure")
	}
}

// wrapperDB does not implement Lifecycle — uses WithLifecycle.
type wrapperDB struct {
	inner *testDB
}

type concurrentDBA struct{ *mockLifecycle }
type concurrentDBB struct{ *mockLifecycle }
type sameNameDBA struct{ *mockLifecycle }
type sameNameDBB struct{ *mockLifecycle }
type conflictingTypeDB struct{ *mockLifecycle }

func TestRegister_WithLifecycleRequiresExplicitCallerOwnership(t *testing.T) {
	app := newTestApp(t)
	lc := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	inner := newTestDB(lc)
	wrapper := &wrapperDB{inner: inner}

	if err := store.Register[*wrapperDB](app, wrapper, store.WithLifecycle(lc)); err == nil {
		t.Fatal("Register() should reject implicit caller-owned lifecycle")
	}
	lc.mu.Lock()
	pingCalls := lc.pingCalls
	lc.mu.Unlock()
	if pingCalls != 0 {
		t.Fatalf("Ping calls = %d, want 0 for ownership conflict", pingCalls)
	}
}

func TestRegister_WithCallerOwnedLifecycle(t *testing.T) {
	tests := []struct {
		name string
		opts func(store.Lifecycle) []store.RegisterOption
	}{
		{
			name: "lifecycle then ownership",
			opts: func(lc store.Lifecycle) []store.RegisterOption {
				return []store.RegisterOption{store.WithLifecycle(lc), store.WithCallerOwnedLifecycle()}
			},
		},
		{
			name: "ownership then lifecycle",
			opts: func(lc store.Lifecycle) []store.RegisterOption {
				return []store.RegisterOption{store.WithCallerOwnedLifecycle(), store.WithLifecycle(lc)}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t)
			lc := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
			wrapper := &wrapperDB{inner: newTestDB(lc)}
			if err := store.Register[*wrapperDB](app, wrapper, tt.opts(lc)...); err != nil {
				t.Fatalf("Register() = %v", err)
			}
			resolved, err := app.Resolve[*wrapperDB]()
			if err != nil || resolved != wrapper {
				t.Fatalf("Resolve[*wrapperDB]() = (%p, %v), want original %p", resolved, err, wrapper)
			}
		})
	}
}

func TestRegister_WithTypedNilLifecycle(t *testing.T) {
	app := newTestApp(t)
	var lifecycle *mockLifecycle
	wrapper := &wrapperDB{}

	if err := store.Register[*wrapperDB](
		app,
		wrapper,
		store.WithLifecycle(lifecycle),
		store.WithCallerOwnedLifecycle(),
	); err == nil {
		t.Fatal("Register with typed-nil lifecycle should return error")
	}
}

func TestRegister_LifecycleValueRejectsExplicitLifecycle(t *testing.T) {
	tests := []struct {
		name     string
		explicit func(*testDB) store.Lifecycle
	}{
		{name: "same", explicit: func(db *testDB) store.Lifecycle { return db }},
		{name: "different", explicit: func(*testDB) store.Lifecycle { return &mockLifecycle{} }},
		{name: "typed nil", explicit: func(*testDB) store.Lifecycle {
			var lifecycle *mockLifecycle
			return lifecycle
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valueLifecycle := &mockLifecycle{}
			db := newTestDB(valueLifecycle)
			explicit := tt.explicit(db)
			err := store.Register[*testDB](newTestApp(t), db, store.WithLifecycle(explicit))
			if err == nil {
				t.Fatal("Register should reject explicit lifecycle when value implements Lifecycle")
			}
			valueLifecycle.mu.Lock()
			valuePingCalls := valueLifecycle.pingCalls
			valueLifecycle.mu.Unlock()
			if valuePingCalls != 0 {
				t.Fatalf("value Ping calls = %d, want 0", valuePingCalls)
			}
			if other, ok := explicit.(*mockLifecycle); ok && other != nil {
				other.mu.Lock()
				otherPingCalls := other.pingCalls
				other.mu.Unlock()
				if otherPingCalls != 0 {
					t.Fatalf("explicit Ping calls = %d, want 0", otherPingCalls)
				}
			}
		})
	}
}

func TestRegister_LifecycleValueRejectsCallerOwnedOptOut(t *testing.T) {
	lc := &mockLifecycle{}
	err := store.Register[*testDB](
		newTestApp(t),
		newTestDB(lc),
		store.WithCallerOwnedLifecycle(),
	)
	if err == nil {
		t.Fatal("Register should reject caller-owned opt-out for a Lifecycle value")
	}
	if lc.pingCalled {
		t.Fatal("ownership error must be found before Ping")
	}
}

type shutdownOnlyDB struct {
	shutdownCalls int
}

func (db *shutdownOnlyDB) Shutdown(context.Context) error {
	db.shutdownCalls++
	return nil
}

func TestRegister_RejectsSplitHealthAndShutdownObjects(t *testing.T) {
	value := &shutdownOnlyDB{}
	healthLifecycle := &mockLifecycle{}
	err := store.Register[*shutdownOnlyDB](
		newTestApp(t),
		value,
		store.WithLifecycle(healthLifecycle),
		store.WithCallerOwnedLifecycle(),
	)
	if err == nil {
		t.Fatal("Register should reject Shutdowner value with a separate Lifecycle")
	}
	if healthLifecycle.pingCalled || value.shutdownCalls != 0 {
		t.Fatal("split ownership error must not Ping or Shutdown either object")
	}
}

func TestRegister_ShutdownOnceViaDIWithLiveDeadline(t *testing.T) {
	app := newTestApp(t)

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

type shutdownOrderDBA struct{ *mockLifecycle }
type shutdownOrderDBB struct{ *mockLifecycle }

func TestRegister_FrameworkOwnedStoresShutdownInReverseRegistrationOrder(t *testing.T) {
	app := newTestApp(t)
	var order []string
	first := &mockLifecycle{
		name: "first", shutdownSeq: &order, health: store.Health{Status: store.StatusUp},
	}
	second := &mockLifecycle{
		name: "second", shutdownSeq: &order, health: store.Health{Status: store.StatusUp},
	}
	if err := store.Register[*shutdownOrderDBA](
		app,
		&shutdownOrderDBA{mockLifecycle: first},
		store.WithName("first"),
	); err != nil {
		t.Fatalf("Register(first) = %v", err)
	}
	if err := store.Register[*shutdownOrderDBB](
		app,
		&shutdownOrderDBB{mockLifecycle: second},
		store.WithName("second"),
	); err != nil {
		t.Fatalf("Register(second) = %v", err)
	}

	runApp(t, app)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := strings.Join(order, ","); got != "second,first" {
		t.Fatalf("shutdown order = %q, want %q", got, "second,first")
	}
	first.mu.Lock()
	firstCalls := first.shutCalls
	first.mu.Unlock()
	second.mu.Lock()
	secondCalls := second.shutCalls
	second.mu.Unlock()
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("Shutdown calls = (%d, %d), want (1, 1)", firstCalls, secondCalls)
	}
}

func TestRegister_WithLifecycle_CallerOwnsClosing(t *testing.T) {
	app := newTestApp(t)

	lc := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	wrapper := &wrapperDB{inner: newTestDB(lc)}
	if err := store.Register[*wrapperDB](
		app,
		wrapper,
		store.WithLifecycle(lc),
		store.WithCallerOwnedLifecycle(),
	); err != nil {
		t.Fatalf("Register() = %v", err)
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
		t.Error("caller-owned lifecycle was closed by the framework")
	}
	if err := lc.Shutdown(t.Context()); err != nil {
		t.Fatalf("caller Shutdown() = %v", err)
	}
	lc.mu.Lock()
	shutdownCalls := lc.shutCalls
	lc.mu.Unlock()
	if shutdownCalls != 1 {
		t.Fatalf("caller Shutdown calls = %d, want 1", shutdownCalls)
	}
}

type callerOwnedFailureDB struct {
	inner *testDB
}

func TestRegister_CallerOwnedPingFailureRetainsOwnershipAndCanRetry(t *testing.T) {
	app := newTestApp(t)
	lifecycle := &mockLifecycle{pingErr: fmt.Errorf("offline")}
	wrapper := &callerOwnedFailureDB{inner: newTestDB(lifecycle)}
	register := func() error {
		return store.Register[*callerOwnedFailureDB](
			app,
			wrapper,
			store.WithName("caller-owned-failure"),
			store.WithLifecycle(lifecycle),
			store.WithCallerOwnedLifecycle(),
		)
	}
	if err := register(); err == nil {
		t.Fatal("Register should fail Ping")
	}
	lifecycle.mu.Lock()
	shutdownCalls := lifecycle.shutCalls
	lifecycle.pingErr = nil
	lifecycle.health = store.Health{Status: store.StatusUp}
	lifecycle.mu.Unlock()
	if shutdownCalls != 0 {
		t.Fatalf("Shutdown calls after failed registration = %d, want 0", shutdownCalls)
	}
	if _, err := app.Resolve[*callerOwnedFailureDB](); err == nil {
		t.Fatal("failed caller-owned registration left a DI value")
	}
	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	if got := len(registry.HealthAll(t.Context())); got != 0 {
		t.Fatalf("Registry entries after failed registration = %d, want 0", got)
	}
	if err := register(); err != nil {
		t.Fatalf("retry Register() = %v", err)
	}
}

type callerOwnedHookDB struct {
	inner *testDB
}

func TestRegister_CallerOwnedLifecycleCanCloseThroughShutdownHook(t *testing.T) {
	app := newTestApp(t)
	lifecycle := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	wrapper := &callerOwnedHookDB{inner: newTestDB(lifecycle)}
	if err := store.Register[*callerOwnedHookDB](
		app,
		wrapper,
		store.WithLifecycle(lifecycle),
		store.WithCallerOwnedLifecycle(),
	); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	app.OnShutdown(lifecycle.Shutdown)
	runApp(t, app)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	lifecycle.mu.Lock()
	shutdownCalls := lifecycle.shutCalls
	lifecycle.mu.Unlock()
	if shutdownCalls != 1 {
		t.Fatalf("caller-owned hook Shutdown calls = %d, want 1", shutdownCalls)
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
	db2.mu.Lock()
	pingCalls := db2.pingCalls
	db2.mu.Unlock()
	if pingCalls != 0 {
		t.Fatalf("duplicate type Ping calls = %d, want 0", pingCalls)
	}
}

func TestRegister_RejectsSameLifecycleUnderDifferentDITypes(t *testing.T) {
	app := newTestApp(t)
	lifecycle := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	db := newTestDB(lifecycle)
	if err := store.Register[*testDB](app, db, store.WithName("concrete")); err != nil {
		t.Fatalf("Register[*testDB]() = %v", err)
	}
	var asInterface store.Lifecycle = db
	if err := store.Register[store.Lifecycle](app, asInterface, store.WithName("interface")); err == nil {
		t.Fatal("Register should reject the same lifecycle under another DI type")
	}
	lifecycle.mu.Lock()
	pingCalls := lifecycle.pingCalls
	lifecycle.mu.Unlock()
	if pingCalls != 1 {
		t.Fatalf("Ping calls = %d, want only the first registration's call", pingCalls)
	}
	if err := app.Alias[store.Lifecycle, *testDB](); err != nil {
		t.Fatalf("Alias[Lifecycle, *testDB]() = %v", err)
	}
	resolved, err := app.Resolve[store.Lifecycle]()
	if err != nil || resolved != db {
		t.Fatalf("Resolve[Lifecycle]() = (%v, %v), want original db", resolved, err)
	}

	runApp(t, app)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	lifecycle.mu.Lock()
	shutdownCalls := lifecycle.shutCalls
	lifecycle.mu.Unlock()
	if shutdownCalls != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", shutdownCalls)
	}
}

type mixedOwnershipDB struct {
	inner *testDB
}

func TestRegister_RejectsMixedOwnershipForSameLifecycle(t *testing.T) {
	app := newTestApp(t)
	lifecycle := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	db := newTestDB(lifecycle)
	if err := store.Register[*testDB](app, db, store.WithName("owned")); err != nil {
		t.Fatalf("Register(framework-owned) = %v", err)
	}
	wrapper := &mixedOwnershipDB{inner: db}
	err := store.Register[*mixedOwnershipDB](
		app,
		wrapper,
		store.WithName("caller-owned"),
		store.WithLifecycle(db),
		store.WithCallerOwnedLifecycle(),
	)
	if err == nil {
		t.Fatal("Register should reject caller ownership for an already framework-owned lifecycle")
	}
	lifecycle.mu.Lock()
	pingCalls := lifecycle.pingCalls
	lifecycle.mu.Unlock()
	if pingCalls != 1 {
		t.Fatalf("Ping calls = %d, want only the framework-owned registration's call", pingCalls)
	}
}

func TestRegister_ProtectsStoreAndRegistryBindingsFromReplace(t *testing.T) {
	app := newTestApp(t)
	original := newTestDB(&mockLifecycle{health: store.Health{Status: store.StatusUp}})
	if err := store.Register[*testDB](app, original, store.WithName("protected")); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	if err := app.Replace[*testDB](newTestDB(&mockLifecycle{})); err == nil {
		t.Fatal("Replace should reject a registered store binding")
	}
	if err := app.Replace[*store.Registry](&store.Registry{}); err == nil {
		t.Fatal("Replace should reject the Registry binding")
	}
	resolved, err := app.Resolve[*testDB]()
	if err != nil || resolved != original {
		t.Fatalf("Resolve[*testDB]() = (%p, %v), want original %p", resolved, err, original)
	}
	resolvedRegistry, err := app.Resolve[*store.Registry]()
	if err != nil || resolvedRegistry != registry {
		t.Fatalf("Resolve[*Registry]() = (%p, %v), want original %p", resolvedRegistry, err, registry)
	}
}

func TestRegister_ProtectsCallerOwnedWrapperBindingFromReplace(t *testing.T) {
	app := newTestApp(t)
	lifecycle := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	wrapper := &wrapperDB{inner: newTestDB(lifecycle)}
	if err := store.Register[*wrapperDB](
		app,
		wrapper,
		store.WithLifecycle(lifecycle),
		store.WithCallerOwnedLifecycle(),
	); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if err := app.Replace[*wrapperDB](&wrapperDB{}); err == nil {
		t.Fatal("Replace should reject a registered caller-owned wrapper binding")
	}
}

func TestRegister_PreProvidedDIValueFailsBeforePing(t *testing.T) {
	app := newTestApp(t)
	app.MustProvideValue[*testDB](newTestDB(&mockLifecycle{}))
	candidate := &mockLifecycle{}
	if err := store.Register[*testDB](app, newTestDB(candidate)); err == nil {
		t.Fatal("Register should reject a pre-provided DI value")
	}
	candidate.mu.Lock()
	pingCalls := candidate.pingCalls
	candidate.mu.Unlock()
	if pingCalls != 0 {
		t.Fatalf("pre-provided DI value Ping calls = %d, want 0", pingCalls)
	}
	if _, err := app.Resolve[*store.Registry](); err == nil {
		t.Fatal("duplicate DI preflight must not create Registry infrastructure")
	}
}

func TestRegister_FinalizedAppFailsBeforePing(t *testing.T) {
	app := newTestApp(t)
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
	lc := &mockLifecycle{}
	if err := store.Register[*testDB](app, newTestDB(lc)); err == nil {
		t.Fatal("Register after Finalize should fail")
	}
	lc.mu.Lock()
	pingCalls := lc.pingCalls
	lc.mu.Unlock()
	if pingCalls != 0 {
		t.Fatalf("finalized app Ping calls = %d, want 0", pingCalls)
	}
	if _, err := app.Resolve[*store.Registry](); err == nil {
		t.Fatal("finalized registration must not create Registry infrastructure")
	}
}

type duplicateNameDB struct{ *mockLifecycle }

func TestRegister_DuplicateNameFailsBeforePing(t *testing.T) {
	app := newTestApp(t)
	if err := store.Register[*testDB](
		app,
		newTestDB(&mockLifecycle{health: store.Health{Status: store.StatusUp}}),
		store.WithName("primary"),
	); err != nil {
		t.Fatalf("first Register() = %v", err)
	}
	second := &mockLifecycle{}
	err := store.Register[*duplicateNameDB](
		app,
		&duplicateNameDB{mockLifecycle: second},
		store.WithName("primary"),
	)
	if err == nil {
		t.Fatal("duplicate store name should fail")
	}
	second.mu.Lock()
	pingCalls := second.pingCalls
	second.mu.Unlock()
	if pingCalls != 0 {
		t.Fatalf("duplicate name Ping calls = %d, want 0", pingCalls)
	}
}

type retryAfterPingFailureDB struct{ *mockLifecycle }

func TestRegister_PingFailureReleasesReservations(t *testing.T) {
	app := newTestApp(t)
	failed := &mockLifecycle{pingErr: fmt.Errorf("offline")}
	if err := store.Register[*retryAfterPingFailureDB](
		app,
		&retryAfterPingFailureDB{mockLifecycle: failed},
		store.WithName("retryable"),
	); err == nil {
		t.Fatal("first Register should fail Ping")
	}
	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	if got := len(registry.HealthAll(t.Context())); got != 0 {
		t.Fatalf("Registry entries after failed Ping = %d, want 0", got)
	}

	failed.mu.Lock()
	failed.pingErr = nil
	failed.health = store.Health{Status: store.StatusUp}
	failed.mu.Unlock()
	if err := store.Register[*retryAfterPingFailureDB](
		app,
		&retryAfterPingFailureDB{mockLifecycle: failed},
		store.WithName("retryable"),
	); err != nil {
		t.Fatalf("retry Register() = %v", err)
	}
	if got := len(registry.HealthAll(t.Context())); got != 1 {
		t.Fatalf("Registry entries after retry = %d, want 1", got)
	}
}

type finalizeDuringPingDB struct {
	app           *credo.App
	pingCalls     int
	shutdownCalls int
}

func (db *finalizeDuringPingDB) Ping(context.Context) error {
	db.pingCalls++
	return db.app.Finalize()
}

func (db *finalizeDuringPingDB) Shutdown(context.Context) error {
	db.shutdownCalls++
	return nil
}

func (*finalizeDuringPingDB) Health(context.Context) store.Health {
	return store.Health{Status: store.StatusUp}
}

func TestRegister_FinalPublicationFailureLeavesNoHealthEntry(t *testing.T) {
	app := newTestApp(t)
	db := &finalizeDuringPingDB{app: app}
	err := store.Register[*finalizeDuringPingDB](app, db, store.WithName("finalize-race"))
	if err == nil {
		t.Fatal("Register should fail when Finalize wins before DI publication")
	}
	if db.pingCalls != 1 {
		t.Fatalf("Ping calls = %d, want 1", db.pingCalls)
	}
	if db.shutdownCalls != 0 {
		t.Fatalf("Shutdown calls = %d, want 0 for failed registration", db.shutdownCalls)
	}
	registry, resolveErr := app.Resolve[*store.Registry]()
	if resolveErr != nil {
		t.Fatalf("Resolve[*Registry]() = %v", resolveErr)
	}
	if got := len(registry.HealthAll(t.Context())); got != 0 {
		t.Fatalf("Registry entries after final publication failure = %d, want 0", got)
	}
	if _, resolveErr := app.Resolve[*finalizeDuringPingDB](); resolveErr == nil {
		t.Fatal("failed final publication left the store value in DI")
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

func TestRegister_PreProvidedRegistryStillWiresReadiness(t *testing.T) {
	app := newTestApp(t)
	provided := &store.Registry{}
	if err := app.ProvideValue[*store.Registry](provided); err != nil {
		t.Fatalf("ProvideValue[*Registry]: %v", err)
	}

	db := newTestDB(&mockLifecycle{
		health: store.Health{Status: store.StatusUp},
	})
	if err := store.Register[*testDB](app, db, store.WithName("preprovided")); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	resolved, err := app.Resolve[*store.Registry]()
	if err != nil || resolved != provided {
		t.Fatalf("resolved Registry = (%p, %v), want pre-provided %p", resolved, err, provided)
	}
	if err := app.Replace[*store.Registry](&store.Registry{}); err == nil {
		t.Fatal("Register should protect a pre-provided Registry from replacement")
	}

	app.UseHealth()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"preprovided"`) {
		t.Fatalf("/ready = %d %s, want wired pre-provided registry", w.Code, w.Body.String())
	}
}

type constructorProvidedRegistryDB struct{ *mockLifecycle }

func TestRegister_ConstructorProvidedRegistryIsAdoptedAndProtected(t *testing.T) {
	app := newTestApp(t)
	provided := &store.Registry{}
	if err := app.Provide[*store.Registry](func() *store.Registry { return provided }); err != nil {
		t.Fatalf("Provide[*Registry]() = %v", err)
	}
	value := &constructorProvidedRegistryDB{mockLifecycle: &mockLifecycle{
		health: store.Health{Status: store.StatusUp},
	}}
	if err := store.Register[*constructorProvidedRegistryDB](app, value); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	resolved, err := app.Resolve[*store.Registry]()
	if err != nil || resolved != provided {
		t.Fatalf("Resolve[*Registry]() = (%p, %v), want constructor value %p", resolved, err, provided)
	}
	if err := app.Replace[*store.Registry](&store.Registry{}); err == nil {
		t.Fatal("Register should protect a constructor-provided Registry")
	}
}

func TestRegister_TypedNilPreProvidedRegistryFailsBeforePing(t *testing.T) {
	app := newTestApp(t)
	if err := app.ProvideValue[*store.Registry](nil); err != nil {
		t.Fatalf("ProvideValue[*Registry](nil) = %v", err)
	}
	lc := &mockLifecycle{}
	err := store.Register[*testDB](app, newTestDB(lc), store.WithName("nil-registry"))
	if err == nil {
		t.Fatal("Register should reject a typed-nil pre-provided Registry")
	}
	lc.mu.Lock()
	pingCalls := lc.pingCalls
	lc.mu.Unlock()
	if pingCalls != 0 {
		t.Fatalf("typed-nil Registry Ping calls = %d, want 0", pingCalls)
	}
	replacement := &store.Registry{}
	if err := app.Replace[*store.Registry](replacement); err != nil {
		t.Fatalf("Replace valid Registry after typed-nil rejection = %v", err)
	}
	if err := store.Register[*testDB](app, newTestDB(lc), store.WithName("nil-registry")); err != nil {
		t.Fatalf("Register() after Registry repair = %v", err)
	}
	resolved, err := app.Resolve[*store.Registry]()
	if err != nil || resolved != replacement {
		t.Fatalf("Resolve[*Registry]() = (%p, %v), want repaired %p", resolved, err, replacement)
	}
}

type constructorRegistryDB struct{ *mockLifecycle }

func TestRegister_FailingRegistryConstructorRemainsRepairable(t *testing.T) {
	app := newTestApp(t)
	if err := app.Provide[*store.Registry](func() (*store.Registry, error) {
		return nil, fmt.Errorf("registry unavailable")
	}); err != nil {
		t.Fatalf("Provide[*Registry]() = %v", err)
	}
	lifecycle := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	value := &constructorRegistryDB{mockLifecycle: lifecycle}
	if err := store.Register[*constructorRegistryDB](app, value); err == nil {
		t.Fatal("Register should fail when Registry construction fails")
	}
	if lifecycle.pingCalled {
		t.Fatal("Registry construction error must happen before Ping")
	}
	replacement := &store.Registry{}
	if err := app.Replace[*store.Registry](replacement); err != nil {
		t.Fatalf("Replace valid Registry after constructor failure = %v", err)
	}
	if err := store.Register[*constructorRegistryDB](app, value); err != nil {
		t.Fatalf("Register() after Registry repair = %v", err)
	}
}

func TestRegister_ConcurrentFirstStoresWireReadiness(t *testing.T) {
	app := newTestApp(t)
	errCh := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Go(func() {
		errCh <- store.Register[*concurrentDBA](app, &concurrentDBA{mockLifecycle: &mockLifecycle{
			health: store.Health{Status: store.StatusUp},
		}}, store.WithName("concurrent-a"))
	})
	wait.Go(func() {
		errCh <- store.Register[*concurrentDBB](app, &concurrentDBB{mockLifecycle: &mockLifecycle{
			health: store.Health{Status: store.StatusUp},
		}}, store.WithName("concurrent-b"))
	})
	wait.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Register: %v", err)
		}
	}

	app.UseHealth()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"concurrent-a"`) ||
		!strings.Contains(w.Body.String(), `"concurrent-b"`) {
		t.Fatalf("/ready = %d %s, want both concurrent stores", w.Code, w.Body.String())
	}
}

func TestRegister_ConcurrentSameNameRunsOnePing(t *testing.T) {
	app := newTestApp(t)
	first := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	second := &mockLifecycle{health: store.Health{Status: store.StatusUp}}
	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Go(func() {
		<-start
		errCh <- store.Register[*sameNameDBA](
			app,
			&sameNameDBA{mockLifecycle: first},
			store.WithName("shared"),
		)
	})
	wait.Go(func() {
		<-start
		errCh <- store.Register[*sameNameDBB](
			app,
			&sameNameDBB{mockLifecycle: second},
			store.WithName("shared"),
		)
	})
	close(start)
	wait.Wait()
	close(errCh)

	successes := 0
	for err := range errCh {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful registrations = %d, want 1", successes)
	}
	first.mu.Lock()
	firstCalls := first.pingCalls
	first.mu.Unlock()
	second.mu.Lock()
	secondCalls := second.pingCalls
	second.mu.Unlock()
	if firstCalls+secondCalls != 1 {
		t.Fatalf("total Ping calls = %d, want 1", firstCalls+secondCalls)
	}
	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	if got := len(registry.HealthAll(t.Context())); got != 1 {
		t.Fatalf("Registry entries = %d, want 1", got)
	}
}

func TestRegister_PendingNameIsInvisibleAndRejectsLoserBeforePing(t *testing.T) {
	app := newTestApp(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	winnerLifecycle := &mockLifecycle{
		health:      store.Health{Status: store.StatusUp},
		pingStarted: started,
		pingRelease: release,
	}
	winnerErr := make(chan error, 1)
	go func() {
		winnerErr <- store.Register[*sameNameDBA](
			app,
			&sameNameDBA{mockLifecycle: winnerLifecycle},
			store.WithName("pending-name"),
		)
	}()
	<-started

	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	if got := len(registry.HealthAll(t.Context())); got != 0 {
		t.Fatalf("pending Registry entries = %d, want 0", got)
	}
	loserLifecycle := &mockLifecycle{}
	if err := store.Register[*sameNameDBB](
		app,
		&sameNameDBB{mockLifecycle: loserLifecycle},
		store.WithName("pending-name"),
	); err == nil {
		t.Fatal("same-name loser should fail while winner is pending")
	}
	loserLifecycle.mu.Lock()
	loserPingCalls := loserLifecycle.pingCalls
	loserLifecycle.mu.Unlock()
	if loserPingCalls != 0 {
		t.Fatalf("loser Ping calls = %d, want 0", loserPingCalls)
	}

	close(release)
	if err := <-winnerErr; err != nil {
		t.Fatalf("winner Register() = %v", err)
	}
	if got := len(registry.HealthAll(t.Context())); got != 1 {
		t.Fatalf("committed Registry entries = %d, want 1", got)
	}
}

func TestRegister_PendingTypeRejectsLoserBeforePing(t *testing.T) {
	app := newTestApp(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	winnerLifecycle := &mockLifecycle{
		health:      store.Health{Status: store.StatusUp},
		pingStarted: started,
		pingRelease: release,
	}
	winner := &conflictingTypeDB{mockLifecycle: winnerLifecycle}
	winnerErr := make(chan error, 1)
	go func() {
		winnerErr <- store.Register[*conflictingTypeDB](app, winner, store.WithName("winner"))
	}()
	<-started

	loserLifecycle := &mockLifecycle{}
	loser := &conflictingTypeDB{mockLifecycle: loserLifecycle}
	if err := store.Register[*conflictingTypeDB](app, loser, store.WithName("loser")); err == nil {
		t.Fatal("same-type loser should fail while winner is pending")
	}
	loserLifecycle.mu.Lock()
	loserPingCalls := loserLifecycle.pingCalls
	loserLifecycle.mu.Unlock()
	if loserPingCalls != 0 {
		t.Fatalf("loser Ping calls = %d, want 0", loserPingCalls)
	}

	close(release)
	if err := <-winnerErr; err != nil {
		t.Fatalf("winner Register() = %v", err)
	}
	resolved, err := app.Resolve[*conflictingTypeDB]()
	if err != nil || resolved != winner {
		t.Fatalf("Resolve[*conflictingTypeDB]() = (%p, %v), want winner %p", resolved, err, winner)
	}
}

func TestRegister_ConcurrentSameTypeKeepsDIAndRegistryWinnerAligned(t *testing.T) {
	app := newTestApp(t)
	first := &conflictingTypeDB{mockLifecycle: &mockLifecycle{health: store.Health{
		Status: store.StatusUp, Details: map[string]any{"id": "first"},
	}}}
	second := &conflictingTypeDB{mockLifecycle: &mockLifecycle{health: store.Health{
		Status: store.StatusUp, Details: map[string]any{"id": "second"},
	}}}
	type outcome struct {
		name  string
		value *conflictingTypeDB
		err   error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	register := func(name string, value *conflictingTypeDB) {
		<-start
		outcomes <- outcome{
			name:  name,
			value: value,
			err:   store.Register[*conflictingTypeDB](app, value, store.WithName(name)),
		}
	}
	wait.Go(func() { register("first", first) })
	wait.Go(func() { register("second", second) })
	close(start)
	wait.Wait()
	close(outcomes)

	var winner outcome
	successes := 0
	for result := range outcomes {
		if result.err == nil {
			successes++
			winner = result
		}
	}
	if successes != 1 {
		t.Fatalf("successful registrations = %d, want 1", successes)
	}
	first.mu.Lock()
	firstCalls := first.pingCalls
	first.mu.Unlock()
	second.mu.Lock()
	secondCalls := second.pingCalls
	second.mu.Unlock()
	if firstCalls+secondCalls != 1 {
		t.Fatalf("total Ping calls = %d, want 1", firstCalls+secondCalls)
	}
	resolved, err := app.Resolve[*conflictingTypeDB]()
	if err != nil || resolved != winner.value {
		t.Fatalf("resolved value = (%p, %v), want winning value %p", resolved, err, winner.value)
	}
	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	health := registry.HealthAll(t.Context())
	if len(health) != 1 {
		t.Fatalf("Registry entries = %d, want 1", len(health))
	}
	entry, exists := health[winner.name]
	if !exists || entry.Details["id"] != winner.name {
		t.Fatalf("Registry winner = %#v, want name/id %q", health, winner.name)
	}
}

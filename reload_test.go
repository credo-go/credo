package credo_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
)

// reloadFixture is a running App backed by a YAML file the test can rewrite.
type reloadFixture struct {
	app  *credo.App
	path string
	logs *bytes.Buffer
	errC chan error
}

// newReloadFixture writes content to a config file, builds an App on it (plus
// opts), and returns the fixture without running it so hooks can still be
// registered. Call start to run it.
func newReloadFixture(t *testing.T, content string, opts ...credo.Option) *reloadFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeYAML(t, path, content)
	cfg, err := config.Load(config.WithFiles(path), config.WithPrefix("RELOADAPP_"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	host, port, _ := freePort(t)
	all := append([]credo.Option{
		credo.WithRawConfig(cfg),
		credo.WithAddr(host, port),
		credo.WithLogger(logger),
		credo.WithoutAccessLog(),
	}, opts...)
	return &reloadFixture{app: mustNew(t, all...), path: path, logs: logs, errC: make(chan error, 1)}
}

func writeYAML(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// start runs the app and waits until it is running; Cleanup shuts it down.
func (f *reloadFixture) start(t *testing.T) {
	t.Helper()
	go func() { f.errC <- f.app.RunContext(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.app.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("app did not reach running state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.app.Shutdown(ctx)
		<-f.errC
	})
}

// rewrite replaces the config file content.
func (f *reloadFixture) rewrite(t *testing.T, content string) {
	t.Helper()
	writeYAML(t, f.path, content)
}

type limits struct {
	RPS   int `credo:"rps"`
	Burst int `credo:"burst"`
}

type validatedLimits struct {
	RPS int `credo:"rps"`
}

func (v *validatedLimits) Validate() error {
	if v.RPS <= 0 {
		return errors.New("rps must be positive")
	}
	return nil
}

func TestReload_NotRunning(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	if err := f.app.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), `expected "running"`) {
		t.Fatalf("Reload before Run: err = %v, want state error", err)
	}
	if err := f.app.Reload(nil); err == nil { //nolint:staticcheck // nil ctx is the condition under test
		t.Fatal("Reload(nil) must error")
	}
}

func TestReload_NotifiesAffectedSubscribersOnly(t *testing.T) {
	f := newReloadFixture(t, "limits:\n  rps: 10\n  burst: 20\nother:\n  x: 1\n")

	var gotLimits []limits
	var otherCalls int
	f.app.OnConfigChange[limits]("limits", func(_ context.Context, next limits) error {
		gotLimits = append(gotLimits, next)
		return nil
	})
	f.app.OnConfigChange[map[string]any]("other", func(context.Context, map[string]any) error {
		otherCalls++
		return nil
	})
	f.start(t)

	f.rewrite(t, "limits:\n  rps: 50\n  burst: 20\nother:\n  x: 1\n")
	if err := f.app.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(gotLimits) != 1 || gotLimits[0] != (limits{RPS: 50, Burst: 20}) {
		t.Errorf("limits subscriber got %+v, want one call with rps=50", gotLimits)
	}
	if otherCalls != 0 {
		t.Errorf("unaffected subscriber called %d times", otherCalls)
	}

	// No change → nobody is notified, and the snapshot is still readable.
	if err := f.app.Reload(context.Background()); err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	if len(gotLimits) != 1 {
		t.Errorf("unchanged reload must not notify; calls = %d", len(gotLimits))
	}
	if got := f.app.MustGetConfig[int]("limits.rps"); got != 50 {
		t.Errorf("GetConfig after reload = %d, want 50", got)
	}
}

func TestReload_NestedKeysAreIndependentSubscriptions(t *testing.T) {
	f := newReloadFixture(t, "databases:\n  primary:\n    dsn: a\n  replica:\n    dsn: b\n")
	var all, primary, replica int
	f.app.OnConfigChange[map[string]any]("databases", func(context.Context, map[string]any) error { all++; return nil })
	f.app.OnConfigChange[map[string]any]("databases.primary", func(context.Context, map[string]any) error { primary++; return nil })
	f.app.OnConfigChange[map[string]any]("databases.replica", func(context.Context, map[string]any) error { replica++; return nil })
	f.start(t)

	f.rewrite(t, "databases:\n  primary:\n    dsn: changed\n  replica:\n    dsn: b\n")
	if err := f.app.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if all != 1 || primary != 1 || replica != 0 {
		t.Errorf("calls all=%d primary=%d replica=%d, want 1/1/0", all, primary, replica)
	}
}

func TestReload_DecodeFailureAbortsBeforePublish(t *testing.T) {
	f := newReloadFixture(t, "limits:\n  rps: 10\nname: app\n")
	var limitCalls, nameCalls, hookCalls int
	f.app.OnConfigChange[limits]("limits", func(context.Context, limits) error { limitCalls++; return nil })
	f.app.OnConfigChange[string]("name", func(context.Context, string) error { nameCalls++; return nil })
	f.app.OnReload(func(context.Context) error { hookCalls++; return nil })
	f.start(t)

	// rps becomes undecodable while name also changes: the whole reload aborts.
	f.rewrite(t, "limits:\n  rps: not-a-number\nname: renamed\n")
	err := f.app.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), `OnConfigChange[0] "limits"`) {
		t.Fatalf("Reload err = %v, want decode failure for limits", err)
	}
	if limitCalls+nameCalls+hookCalls != 0 {
		t.Errorf("nothing may run after an aborted reload: limits=%d name=%d hooks=%d", limitCalls, nameCalls, hookCalls)
	}
	if got := f.app.MustGetConfig[string]("name"); got != "app" {
		t.Errorf("previous snapshot must stay current; name = %q", got)
	}
	if !strings.Contains(f.logs.String(), "reload aborted before publish") {
		t.Errorf("expected abort log, got:\n%s", f.logs)
	}
}

func TestReload_ValidationFailureAbortsBeforePublish(t *testing.T) {
	f := newReloadFixture(t, "limits:\n  rps: 10\n")
	var calls int
	f.app.OnConfigChange[validatedLimits]("limits", func(context.Context, validatedLimits) error { calls++; return nil })
	f.start(t)

	f.rewrite(t, "limits:\n  rps: 0\n")
	err := f.app.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rps must be positive") {
		t.Fatalf("Reload err = %v, want validation failure", err)
	}
	if calls != 0 {
		t.Errorf("subscriber must not run on validation failure; calls = %d", calls)
	}
	if got := f.app.MustGetConfig[int]("limits.rps"); got != 10 {
		t.Errorf("previous snapshot must stay current; rps = %d", got)
	}
}

func TestReload_SubscriberErrorDoesNotStopOthers(t *testing.T) {
	f := newReloadFixture(t, "a: 1\nb: 1\n")
	var bCalls, hookCalls int
	f.app.OnConfigChange[int]("a", func(context.Context, int) error { return errors.New("a failed") })
	f.app.OnConfigChange[int]("b", func(context.Context, int) error { bCalls++; return nil })
	f.app.OnConfigChange[int]("b", func(context.Context, int) error { panic("b panicked") })
	f.app.OnReload(func(context.Context) error { hookCalls++; return nil })
	f.start(t)

	f.rewrite(t, "a: 2\nb: 2\n")
	err := f.app.Reload(context.Background())
	if err == nil {
		t.Fatal("expected joined errors")
	}
	for _, want := range []string{"a failed", "panic: b panicked", `OnConfigChange[0] "a"`, `OnConfigChange[2] "b"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if bCalls != 1 || hookCalls != 1 {
		t.Errorf("later steps must still run: b=%d hooks=%d", bCalls, hookCalls)
	}
	// The snapshot was published despite the subscriber failures (no rollback).
	if got := f.app.MustGetConfig[int]("a"); got != 2 {
		t.Errorf("a after reload = %d, want 2 (no rollback)", got)
	}
}

func TestReload_OnReloadOrderAndPanicRecovery(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	var order []string
	f.app.OnConfigChange[int]("a", func(context.Context, int) error { order = append(order, "sub"); return nil })
	f.app.OnReload(func(context.Context) error { order = append(order, "hook1"); return nil })
	f.app.OnReload(func(context.Context) error { order = append(order, "hook2"); panic("boom") })
	f.app.OnReload(func(context.Context) error { order = append(order, "hook3"); return errors.New("hook3 failed") })
	f.start(t)

	f.rewrite(t, "a: 2\n")
	err := f.app.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "OnReload[1]: panic: boom") || !strings.Contains(err.Error(), "OnReload[2]: hook3 failed") {
		t.Fatalf("Reload err = %v", err)
	}
	if got := strings.Join(order, ","); got != "sub,hook1,hook2,hook3" {
		t.Errorf("order = %s, want sub,hook1,hook2,hook3", got)
	}
	if !f.app.IsRunning() {
		t.Error("a failed reload must never stop the server")
	}
}

func TestReload_WarnsForUnsubscribedChanges(t *testing.T) {
	f := newReloadFixture(t, "limits:\n  rps: 1\nserver:\n  read_timeout: 1s\nfeature:\n  flag: false\n")
	f.app.OnConfigChange[limits]("limits", func(context.Context, limits) error { return nil })
	f.start(t)

	f.rewrite(t, "limits:\n  rps: 2\nserver:\n  read_timeout: 2s\nfeature:\n  flag: true\n")
	if err := f.app.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "restart required") {
		t.Fatalf("expected restart-required warning, got:\n%s", logs)
	}
	for _, key := range []string{"feature.flag", "server.read_timeout"} {
		if !strings.Contains(logs, key) {
			t.Errorf("warning should name %s:\n%s", key, logs)
		}
	}
	if strings.Contains(logs, "limits.rps") {
		t.Errorf("subscribed key must not be reported as restart-only:\n%s", logs)
	}
	if strings.Contains(logs, "true") && strings.Contains(logs, "keys=") && strings.Contains(logs, "flag: true") {
		t.Errorf("values must never be logged:\n%s", logs)
	}
	if !strings.Contains(logs, "reload complete") {
		t.Errorf("expected completion summary:\n%s", logs)
	}
}

// staticConfig is a RawConfig that cannot reload.
type staticConfig struct{ data map[string]any }

func (s staticConfig) Unmarshal(key string, dst any) error {
	v, ok := s.data[key]
	if !ok {
		return errors.New("missing " + key)
	}
	if p, ok := dst.(*int); ok {
		*p = v.(int)
	}
	return nil
}
func (s staticConfig) Exists(key string) bool { _, ok := s.data[key]; return ok }

func TestReload_NonReloadableStore(t *testing.T) {
	host, port, _ := freePort(t)
	app := mustNew(t, credo.WithRawConfig(staticConfig{data: map[string]any{"a": 1}}), credo.WithAddr(host, port), credo.WithoutAccessLog())

	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(r.(string), "neither config.Stager nor config.Reloader") {
				t.Errorf("OnConfigChange on a static store should panic, got %v", r)
			}
		}()
		app.OnConfigChange[int]("a", func(context.Context, int) error { return nil })
	}()

	var hookCalls int
	app.OnReload(func(context.Context) error { hookCalls++; return nil })

	errC := make(chan error, 1)
	go func() { errC <- app.RunContext(context.Background()) }()
	for !app.IsRunning() {
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Shutdown(ctx)
		<-errC
	})

	if err := app.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if hookCalls != 1 {
		t.Errorf("OnReload must still run without config reload; calls = %d", hookCalls)
	}
}

// reloadOnlyStore implements config.Reloader but not config.Stager, so the
// App publishes first and validates afterwards.
type reloadOnlyStore struct {
	mu   sync.Mutex
	cur  map[string]any
	next map[string]any
}

func (s *reloadOnlyStore) Unmarshal(key string, dst any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cur[key]
	if !ok {
		return errors.New("missing " + key)
	}
	p, ok := dst.(*int)
	if !ok {
		return errors.New("unsupported target")
	}
	n, ok := v.(int)
	if !ok {
		return errors.New("not an int")
	}
	*p = n
	return nil
}
func (s *reloadOnlyStore) Exists(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.cur[key]
	return ok
}
func (s *reloadOnlyStore) Reload() (config.Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = s.next
	return changesOf("a", "b"), nil
}

// changesOf builds a Changes value through a real config diff: every key
// moves from 1 to 2.
func changesOf(keys ...string) config.Changes {
	var before, after strings.Builder
	for _, k := range keys {
		before.WriteString(k + ": 1\n")
		after.WriteString(k + ": 2\n")
	}
	dir, err := os.MkdirTemp("", "changes")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "c.yaml")
	if werr := os.WriteFile(p, []byte(before.String()), 0o644); werr != nil {
		panic(werr)
	}
	c, err := config.Load(config.WithFiles(p), config.WithPrefix("NONE_"))
	if err != nil {
		panic(err)
	}
	if werr := os.WriteFile(p, []byte(after.String()), 0o644); werr != nil {
		panic(werr)
	}
	ch, err := c.Reload()
	if err != nil {
		panic(err)
	}
	return ch
}

func TestReload_ReloaderOnlyStoreValidatesAfterPublish(t *testing.T) {
	store := &reloadOnlyStore{cur: map[string]any{"a": 1, "b": 1}, next: map[string]any{"a": 2, "b": "bad"}}
	host, port, _ := freePort(t)
	app := mustNew(t, credo.WithRawConfig(store), credo.WithAddr(host, port), credo.WithoutAccessLog())
	var gotA int
	app.OnConfigChange[int]("a", func(_ context.Context, v int) error { gotA = v; return nil })
	app.OnConfigChange[int]("b", func(context.Context, int) error { t.Error("b must not be applied"); return nil })

	errC := make(chan error, 1)
	go func() { errC <- app.RunContext(context.Background()) }()
	for !app.IsRunning() {
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Shutdown(ctx)
		<-errC
	})

	err := app.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), `OnConfigChange[1] "b": not an int`) {
		t.Fatalf("Reload err = %v, want post-publish decode failure for b", err)
	}
	if gotA != 2 {
		t.Errorf("a subscriber got %d, want 2 (published before validation)", gotA)
	}
}

func TestReload_ConcurrentCallsAreSerialized(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	var inFlight, maxInFlight int
	var mu sync.Mutex
	f.app.OnReload(func(context.Context) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})
	f.start(t)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if err := f.app.Reload(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if maxInFlight != 1 {
		t.Errorf("reloads overlapped: max in flight = %d", maxInFlight)
	}
}

func TestReload_RegistrationGuards(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	mustPanic(t, "nil OnReload", func() { f.app.OnReload(nil) })
	mustPanic(t, "nil OnConfigChange", func() { f.app.OnConfigChange[int]("a", nil) })
	f.start(t) // compiles and freezes
	mustPanic(t, "frozen OnReload", func() { f.app.OnReload(func(context.Context) error { return nil }) })
	mustPanic(t, "frozen OnConfigChange", func() {
		f.app.OnConfigChange[int]("a", func(context.Context, int) error { return nil })
	})
}

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic", name)
		}
	}()
	fn()
}

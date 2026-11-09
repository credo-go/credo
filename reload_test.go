package credo_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/testutil"
)

// reloadFixture is a running App backed by a YAML file the test can rewrite.
type reloadFixture struct {
	app  *credo.App
	cfg  *config.Config
	path string
	logs *syncBuffer
	errC chan error
}

// newReloadFixture writes content to a config file, builds an App on it (plus
// opts), and returns the fixture without running it so sections can still be
// bound. Call start to run it.
func newReloadFixture(t *testing.T, content string, opts ...credo.Option) *reloadFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeYAML(t, path, content)
	cfg, err := config.Load(config.WithFiles(path), config.WithPrefix("RELOADAPP_"), config.WithoutDotenv())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	host, port, _ := freePort(t)
	all := append([]credo.Option{
		credo.WithRawConfig(cfg),
		credo.WithAddr(host, port),
		credo.WithLogger(logger),
	}, opts...)
	return &reloadFixture{app: mustNew(t, all...), cfg: cfg, path: path, logs: logs, errC: make(chan error, 1)}
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

// onChange declares the fixture's "a" key a live section whose callback runs
// fn. The callback first rewrites the file with the next value of a, so every
// reload, including one queued behind a running callback, changes the section
// again. The fixture's file must hold only "a: <int>".
func (f *reloadFixture) onChange(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()
	live, err := config.Section[int]("a").Live(f.cfg)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	live.OnChange(func(ctx context.Context, _, next int) error {
		writeYAML(t, f.path, "a: "+strconv.Itoa(next+1)+"\n")
		return fn(ctx)
	})
	f.rewrite(t, "a: 2\n")
}

type limits struct {
	RPS   int `credo:"rps"`
	Burst int `credo:"burst"`
}

type validatedLimits struct {
	RPS int `credo:"rps"`
}

func (v validatedLimits) Validate() error {
	if v.RPS <= 0 {
		return errors.New("rps must be positive")
	}
	return nil
}

var limitsLiveSection = config.Section[limits]("limits")

// limitedService consumes a live section, as a business service would.
type limitedService struct{ limits *config.Live[limits] }

func newLimitedService(l *config.Live[limits]) *limitedService { return &limitedService{limits: l} }

func TestReload_NotRunning(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	if _, err := f.app.Reload(t.Context()); err == nil || !strings.Contains(err.Error(), `expected "running"`) {
		t.Fatalf("Reload before Run: err = %v, want state error", err)
	}
	if _, err := f.app.Reload(nil); err == nil { //nolint:staticcheck // nil ctx is the condition under test
		t.Fatal("Reload(nil) must error")
	}
}

// TestReload_LiveSectionThroughProvide: a live section bound with
// Provide(section.Live) reaches its consumer, and a reload that changes it is
// visible through Load; a reload that changes another key leaves it alone.
func TestReload_LiveSectionThroughProvide(t *testing.T) {
	f := newReloadFixture(t, "limits:\n  rps: 10\n  burst: 1\nother: 1\n", credo.WithoutReloadSignals())
	f.app.Provide(limitsLiveSection.Live)
	f.app.Provide(newLimitedService)
	svc := f.app.MustResolve[*limitedService]()
	f.start(t)

	f.rewrite(t, "limits:\n  rps: 20\n  burst: 1\nother: 1\n")
	res, err := f.app.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := svc.limits.Load().RPS; got != 20 {
		t.Errorf("RPS after reload = %d, want 20", got)
	}
	if !slices.Equal(res.Replaced, []string{"limits"}) {
		t.Errorf("Replaced = %v, want [limits]", res.Replaced)
	}

	f.rewrite(t, "limits:\n  rps: 20\n  burst: 1\nother: 2\n")
	res, err = f.app.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(res.Replaced) != 0 || svc.limits.Load().RPS != 20 {
		t.Errorf("a reload of another key replaced %v; RPS %d", res.Replaced, svc.limits.Load().RPS)
	}
}

// TestReload_ResultReportsRestartKeys: App.Reload returns the result
// Config.Reload computes; a changed key outside every live section — a
// Decode-bound section's key and a server.tls.* path included — needs a
// restart, a live key does not, and the report repeats until a key reverts.
func TestReload_ResultReportsRestartKeys(t *testing.T) {
	const start = "limits:\n  rps: 10\napp:\n  name: a\nserver:\n  tls:\n    cert_file: \"\"\n    key_file: \"\"\n"
	f := newReloadFixture(t, start)
	f.app.Provide(limitsLiveSection.Live)
	f.app.Provide(config.Section[struct{ Name string }]("app").Decode)
	f.app.Provide(newLimitedService)
	f.app.Provide(func(app struct{ Name string }) *appNameHolder { return &appNameHolder{name: app.Name} })
	_ = f.app.MustResolve[*limitedService]()
	holder := f.app.MustResolve[*appNameHolder]()
	f.start(t)

	f.rewrite(t, "limits:\n  rps: 20\napp:\n  name: b\nserver:\n  tls:\n    cert_file: \"/new.pem\"\n    key_file: \"\"\n")
	res, err := f.app.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if want := []string{"app.name", "server.tls.cert_file"}; !slices.Equal(res.RestartRequired, want) {
		t.Errorf("RestartRequired = %v, want %v", res.RestartRequired, want)
	}
	if holder.name != "a" {
		t.Errorf("a Decode-bound section changed with a reload: %q", holder.name)
	}
	logs := f.logs.String()
	for _, part := range []string{"restart to apply it", "app.name", "server.tls.cert_file", "reload complete"} {
		if !strings.Contains(logs, part) {
			t.Errorf("logs do not contain %q:\n%s", part, logs)
		}
	}
	if strings.Contains(logs, "/new.pem") {
		t.Errorf("a configuration value was logged:\n%s", logs)
	}

	res, err = f.app.Reload(t.Context())
	if err != nil || !slices.Equal(res.RestartRequired, []string{"app.name", "server.tls.cert_file"}) {
		t.Errorf("a reload that changes nothing = %+v, %v; want the restart keys again", res, err)
	}

	f.rewrite(t, start)
	res, err = f.app.Reload(t.Context())
	if err != nil || len(res.RestartRequired) != 0 {
		t.Errorf("a reload that reverts every key = %+v, %v; want no restart keys", res, err)
	}
}

type appNameHolder struct{ name string }

// TestReload_InvalidLiveSectionAborts: a reload that a live section's
// validation aborts returns a result that applied nothing, naming the
// section, with its error, and logs the abort.
func TestReload_InvalidLiveSectionAborts(t *testing.T) {
	f := newReloadFixture(t, "limits:\n  rps: 10\n")
	live, err := config.Section[validatedLimits]("limits").Live(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)

	f.rewrite(t, "limits:\n  rps: 0\n")
	res, err := f.app.Reload(t.Context())
	if err == nil || !strings.Contains(err.Error(), `live section "limits"`) || !strings.Contains(err.Error(), "rps must be positive") {
		t.Fatalf("Reload = %v, want the live section's validation error", err)
	}
	if res.Published || !strings.Contains(res.Aborted, `live section "limits"`) {
		t.Errorf("result = %+v, want unpublished, naming the section", res)
	}
	if live.Load().RPS != 10 {
		t.Errorf("an aborted reload replaced the value: %+v", live.Load())
	}
	if !strings.Contains(f.logs.String(), "reload aborted; nothing was published") {
		t.Errorf("the abort was not logged:\n%s", f.logs.String())
	}
	if !f.app.IsRunning() {
		t.Error("a failed reload must never stop the server")
	}
}

// TestReload_CallbackErrorsAreJoined: OnChange errors and panics come back
// joined from App.Reload, after the publish, and are logged.
func TestReload_CallbackErrorsAreJoined(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	f.onChange(t, func(context.Context) error { return errors.New("derived state refused") })
	live, err := config.Section[int]("a").Live(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	live.OnChange(func(context.Context, int, int) error { panic("boom") })
	f.start(t)

	res, err := f.app.Reload(t.Context())
	if err == nil || !strings.Contains(err.Error(), "derived state refused") || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("Reload = %v, want both callback failures", err)
	}
	if !res.Published || live.Load() != 2 {
		t.Errorf("callback failures undid the publish: %+v, a = %d", res, live.Load())
	}
	if !strings.Contains(f.logs.String(), "reload callback failed") {
		t.Errorf("callback failures were not logged:\n%s", f.logs.String())
	}
}

// TestReload_ConfigurationThatCannotReload: over testutil's empty
// configuration or a custom RawConfig, Section.Live fails at construction and
// App.Reload returns an error, each naming the type and config.Load.
func TestReload_ConfigurationThatCannotReload(t *testing.T) {
	t.Run("testutil's empty configuration", func(t *testing.T) {
		app := testutil.NewApp(t)
		app.Provide(limitsLiveSection.Live)
		_, err := app.Resolve[*config.Live[limits]]()
		if err == nil {
			t.Fatal("Resolve of a live section: expected an error")
		}
		for _, part := range []string{"testutil.emptyConfig", "cannot reload", "config.Load"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("Resolve error %q does not contain %q", err, part)
			}
		}
	})

	t.Run("a custom RawConfig", func(t *testing.T) {
		host, port, _ := freePort(t)
		app := mustNew(t, credo.WithRawConfig(staticConfig{data: map[string]any{"a": 1}}), credo.WithAddr(host, port),
			credo.WithLogger(slog.New(slog.DiscardHandler)))
		errC := make(chan error, 1)
		go func() { errC <- app.RunContext(context.Background()) }()
		for !app.IsRunning() {
			time.Sleep(5 * time.Millisecond)
		}
		t.Cleanup(func() { _ = app.Shutdown(context.Background()); <-errC })
		_, err := app.Reload(t.Context())
		if err == nil {
			t.Fatal("Reload over a custom RawConfig: expected an error")
		}
		for _, part := range []string{"credo_test.staticConfig", "cannot reload", "config.Load"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("Reload error %q does not contain %q", err, part)
			}
		}
	})
}

// TestReload_LiveServerSectionFails: the framework reads "server" as static,
// so a live read of it, or of a section nested in it, fails naming both reads.
func TestReload_LiveServerSectionFails(t *testing.T) {
	f := newReloadFixture(t, "server:\n  read_timeout: 1s\n  tls:\n    cert_file: \"\"\n")
	for _, key := range []string{"server", "server.tls"} {
		_, err := config.Section[map[string]any](key).Live(f.cfg)
		if err == nil {
			t.Fatalf("Live(%q): expected an error", key)
		}
		for _, part := range []string{`config.Section[credo.serverConfig]("server").Decode`, "Live", "static or live"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("Live(%q) error %q does not contain %q", key, err, part)
			}
		}
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

func TestReload_ConcurrentCallsAreSerialized(t *testing.T) {
	f := newReloadFixture(t, "a: 1\n")
	var inFlight, maxInFlight atomic.Int32
	f.onChange(t, func(context.Context) error {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	f.start(t)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, err := f.app.Reload(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("reloads overlapped: max in flight = %d", got)
	}
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

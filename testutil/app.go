package testutil

import (
	"context"
	jsonv2 "encoding/json/v2"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
)

// shutdownTimeout bounds the best-effort cleanup shutdown registered by NewApp.
const shutdownTimeout = 5 * time.Second

// Option configures a test App built by [NewApp].
type Option func(*options)

type options struct {
	wiring      []func(*credo.App)
	overrides   []func(*credo.App)
	configPairs []configPair
	logBuffer   *LogBuffer
}

type configPair struct {
	key string
	val any
}

// WithWiring registers functions that wire dependencies into the container
// (typically [credo.App.Provide] / [credo.App.ProvideValue] calls). They run after
// the App is constructed but before any [WithOverride], so an override can
// replace a binding established here.
func WithWiring(fns ...func(*credo.App)) Option {
	return func(o *options) { o.wiring = append(o.wiring, fns...) }
}

// WithOverride replaces the binding for type T with value v, through
// [credo.App.ProvideValue] with [credo.Override]. Overrides run after
// [WithWiring], making them the right tool for swapping a real dependency for
// a stub or fake. An override needs an earlier binding of T and panics without
// one, so an override that no longer matches the wiring fails instead of
// adding a binding nothing resolves; adding a binding is [WithWiring]'s job.
// The replaced value never becomes the App's: whoever built it releases it.
//
//	testutil.WithOverride[UserRepo](fakeRepo)
func WithOverride[T any](v T) Option {
	return func(o *options) {
		o.overrides = append(o.overrides, func(app *credo.App) {
			app.ProvideValue[T](v, credo.Override())
		})
	}
}

// WithConfig sets a single configuration value at a dotted key path (for
// example "server.port"). Repeated calls merge into one nested document that is
// injected as the App's RawConfig. Using WithConfig switches NewApp from its
// empty config to the real config loader, which decodes that document and
// nothing else: no .env file is read and no environment variable is merged,
// so the test sees exactly the values it set.
func WithConfig(key string, val any) Option {
	return func(o *options) {
		o.configPairs = append(o.configPairs, configPair{key: key, val: val})
	}
}

// WithLogBuffer routes the App's logger to buf so tests can assert on
// structured log output, including the request ID and access log records
// once those features are installed with app.UseRequestID and
// app.UseAccessLog. Without this option the test App uses a silent logger.
func WithLogBuffer(buf *LogBuffer) Option {
	return func(o *options) { o.logBuffer = buf }
}

// NewApp constructs a *credo.App for tests. Unlike [credo.New], it never loads
// configuration from disk or from the environment: by default it injects an
// empty RawConfig, and the values given with [WithConfig] are the only ones
// loaded, so tests are hermetic. Provide values with [WithConfig], wire
// dependencies with [WithWiring], swap them with [WithOverride], and capture
// logs with [WithLogBuffer].
//
// NewApp registers a graceful shutdown via tb.Cleanup: it tears down every
// singleton the test created whether or not the App was run. The App is not
// finalized, so tests may still register routes, providers, and overrides.
// Constructors run only after [credo.App.Finalize], so call it (or serve a
// request, which finalizes implicitly) before resolving services:
//
//	app := testutil.NewApp(t, testutil.WithOverride[UserRepo](fakeRepo))
//	if err := app.Finalize(); err != nil {
//		t.Fatal(err)
//	}
//	svc := app.MustResolve[*UserService]()
func NewApp(tb testing.TB, opts ...Option) *credo.App {
	tb.Helper()

	o := options{}
	for _, opt := range opts {
		opt(&o)
	}

	// Default to a silent logger so unit tests stay quiet; WithLogBuffer
	// opts into capturing structured output for assertions.
	logger := slog.New(slog.DiscardHandler)
	if o.logBuffer != nil {
		logger = slog.New(o.logBuffer.Handler())
	}
	credoOpts := []credo.Option{
		credo.WithRawConfig(buildConfig(tb, o.configPairs)),
		credo.WithLogger(logger),
	}

	app, err := credo.New(credoOpts...)
	if err != nil {
		tb.Fatalf("testutil: new app: %v", err)
	}

	// Wiring runs before overrides so WithOverride can replace a wired binding.
	for _, fn := range o.wiring {
		fn(app)
	}
	for _, fn := range o.overrides {
		fn(app)
	}

	// Registered first, this cleanup runs after every cleanup the test adds
	// later — a test server closed through tb.Cleanup is drained before the
	// components stop — so Start adds none for this App.
	builtApps.Store(app, struct{}{})
	tb.Cleanup(func() {
		builtApps.Delete(app)
		shutdown(app)
	})

	return app
}

// builtApps holds the Apps NewApp built whose shutdown cleanup is registered.
var builtApps sync.Map

// shutdown is the best-effort cleanup shutdown: a never-run App takes the
// bootstrap teardown path; a started App drains first. Either way the
// components that exist are shut down. A state error (already stopped) is
// fine.
func shutdown(app *credo.App) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = app.Shutdown(ctx)
}

// Start runs app's start phase with [credo.App.Start] — each component's
// Start in dependency order, then the start hooks — so the App can be served
// through ServeHTTP, by httptest or a recorder, and fails the test when the
// start fails. The App is shut down when the test ends.
//
// An App built by [NewApp] already has its shutdown cleanup registered first,
// so it runs after every cleanup the test adds later: a test server closed
// through tb.Cleanup is drained before the components stop, whether it was
// created before or after Start. Any other App gets a shutdown cleanup here,
// so its test server is created after Start or closed with defer.
//
//	app := testutil.NewApp(t, testutil.WithWiring(wire))
//	testutil.Start(t, app)
//	srv := httptest.NewServer(app)
//	t.Cleanup(srv.Close)
func Start(tb testing.TB, app *credo.App) {
	tb.Helper()
	if _, built := builtApps.Load(app); !built {
		tb.Cleanup(func() { shutdown(app) })
	}
	if err := app.Start(tb.Context()); err != nil {
		tb.Fatalf("testutil: start app: %v", err)
	}
}

// buildConfig returns the RawConfig for a test App. With no pairs it is an
// empty, hermetic config; otherwise the pairs are merged into a nested JSON
// document and parsed by the real loader with its .env and process-environment
// sources switched off, bootstrap keys (CREDO_ENV, CREDO_ENV_FILE) included.
func buildConfig(tb testing.TB, pairs []configPair) credo.RawConfig {
	tb.Helper()
	if len(pairs) == 0 {
		return emptyConfig{}
	}
	root := map[string]any{}
	for _, p := range pairs {
		setNested(root, p.key, p.val)
	}
	data, err := jsonv2.Marshal(root)
	if err != nil {
		tb.Fatalf("testutil: marshal config: %v", err)
	}
	rc, err := config.LoadBytes(data, config.FormatJSON,
		config.WithoutDotenv(), config.WithoutProcessEnv())
	if err != nil {
		tb.Fatalf("testutil: load config: %v", err)
	}
	return rc
}

// setNested assigns val at a dotted key path within root, creating intermediate
// maps as needed. An empty key is ignored.
func setNested(root map[string]any, key string, val any) {
	if key == "" {
		return
	}
	parts := strings.Split(key, ".")
	m := root
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = val
}

// emptyConfig is a RawConfig with no values. NewApp injects it by default so a
// test App does not auto-load configuration from the working directory.
type emptyConfig struct{}

func (emptyConfig) Unmarshal(string, any) error { return nil }
func (emptyConfig) Exists(string) bool          { return false }

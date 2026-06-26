package testutil

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
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
// (typically [credo.App.Provide] / [credo.App.MustProvideValue] calls). They run after
// the App is constructed but before any [WithOverride], so an override can
// replace a binding established here.
func WithWiring(fns ...func(*credo.App)) Option {
	return func(o *options) { o.wiring = append(o.wiring, fns...) }
}

// WithOverride replaces the binding for type T with value v via [credo.App.Replace].
// Overrides run after [WithWiring], making them the right tool for swapping a
// real dependency for a stub or fake. Because Replace adds the binding when it
// is absent, WithOverride works whether or not T was previously wired.
//
//	testutil.WithOverride[UserRepo](fakeRepo)
func WithOverride[T any](v T) Option {
	return func(o *options) {
		o.overrides = append(o.overrides, func(app *credo.App) {
			app.MustReplace[T](v)
		})
	}
}

// WithConfig sets a single configuration value at a dotted key path (for
// example "server.port"). Repeated calls merge into one nested document that is
// injected as the App's RawConfig. Using WithConfig switches NewApp from its
// hermetic empty config to the real config loader.
func WithConfig(key string, val any) Option {
	return func(o *options) {
		o.configPairs = append(o.configPairs, configPair{key: key, val: val})
	}
}

// WithLogBuffer routes the App's logger to buf so tests can assert on
// structured log output, including the built-in request ID and access log
// records. Without this option the test App uses a silent logger.
func WithLogBuffer(buf *LogBuffer) Option {
	return func(o *options) { o.logBuffer = buf }
}

// NewApp constructs a *credo.App for tests. Unlike [credo.New], it never loads
// configuration from disk: by default it injects an empty RawConfig, so tests
// are hermetic. Provide values with [WithConfig], wire dependencies with
// [WithWiring], swap them with [WithOverride], and capture logs with
// [WithLogBuffer].
//
// NewApp registers a best-effort graceful shutdown via tb.Cleanup. The App is
// not finalized, so tests may register additional routes, providers, or
// overrides, and may resolve services directly.
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

	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		// Best-effort: Shutdown returns a state error when the App was never
		// Run, which is the common case for unit tests. When the App was Run,
		// this drains in-flight requests and shuts down registered singletons.
		_ = app.Shutdown(ctx)
	})

	return app
}

// buildConfig returns the RawConfig for a test App. With no pairs it is an
// empty, hermetic config; otherwise the pairs are merged into a nested JSON
// document and parsed by the real loader.
func buildConfig(tb testing.TB, pairs []configPair) credo.RawConfig {
	tb.Helper()
	if len(pairs) == 0 {
		return emptyConfig{}
	}
	root := map[string]any{}
	for _, p := range pairs {
		setNested(root, p.key, p.val)
	}
	data, err := json.Marshal(root)
	if err != nil {
		tb.Fatalf("testutil: marshal config: %v", err)
	}
	rc, err := config.LoadBytes(data, config.FormatJSON)
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

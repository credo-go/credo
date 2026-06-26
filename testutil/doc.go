// Package testutil provides helpers for testing Credo applications: building a
// hermetic test App, overriding dependencies with fakes, injecting config, and
// asserting on structured log output.
//
// # Building a test App
//
// [NewApp] constructs a *credo.App that, unlike [credo.New], never loads
// configuration from disk. It injects an empty config by default, registers a
// best-effort shutdown via tb.Cleanup, and leaves the container un-finalized so
// the test can add routes, providers, or overrides:
//
//	func TestPingHandler(t *testing.T) {
//		app := testutil.NewApp(t)
//		app.GET("/ping", pingHandler)
//
//		rec := httptest.NewRecorder()
//		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
//		// ... assert on rec ...
//	}
//
// # Overriding dependencies
//
// Use [WithWiring] to register the dependencies under test and [WithOverride]
// to swap any of them for a fake. Overrides run after wiring, so they win;
// [WithOverride] also adds a binding when none was wired. It is built on
// [credo.App.Replace].
//
//	app := testutil.NewApp(t,
//		testutil.WithWiring(func(app *credo.App) {
//			app.MustProvide[*UserService](NewUserService)
//			app.MustProvide[UserRepo](NewPostgresRepo)
//		}),
//		testutil.WithOverride[UserRepo](fakeRepo),
//	)
//	svc := app.MustResolve[*UserService]() // built with fakeRepo
//
// # Injecting config
//
// [WithConfig] sets values at dotted key paths. Repeated calls merge into one
// document that is injected as the App's RawConfig:
//
//	app := testutil.NewApp(t,
//		testutil.WithConfig("app.name", "checkout"),
//		testutil.WithConfig("app.timeout", "5s"),
//	)
//
// # Asserting on logs
//
// Wire a [LogBuffer] with [WithLogBuffer] to capture structured output,
// including the built-in request ID and access log records, then match records
// with [LogBuffer.AssertHas]:
//
//	buf := testutil.NewLogBuffer()
//	app := testutil.NewApp(t, testutil.WithLogBuffer(buf))
//	app.GET("/ping", pingHandler)
//
//	rec := httptest.NewRecorder()
//	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
//
//	buf.AssertHas(t, testutil.LogEntry{
//		Level:   "INFO",
//		Message: "request completed",
//		Attrs:   map[string]any{"method": "GET", "status": 200},
//	})
package testutil

// Package testutil provides helpers for testing Credo applications: building a
// hermetic test App, overriding dependencies with fakes, starting an App served
// through ServeHTTP, injecting config, and asserting on structured log output.
//
// # Building a test App
//
// [NewApp] constructs a *credo.App that, unlike [credo.New], never loads
// configuration from disk or from the environment. It injects an empty config
// by default, registers a best-effort shutdown via tb.Cleanup, and leaves the
// container un-finalized so the test can add routes, providers, or overrides:
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
// to swap any of them for a fake. Overrides run after wiring, so they win.
// [WithOverride] is built on [credo.Override]: it replaces an earlier binding
// and panics when there is none, so an override that no longer matches the
// wiring fails loudly; adding a binding is [WithWiring]'s job.
//
//	app := testutil.NewApp(t,
//		testutil.WithWiring(func(app *credo.App) {
//			app.Provide[*UserService](NewUserService)
//			app.Provide[UserRepo](NewPostgresRepo)
//		}),
//		testutil.WithOverride[UserRepo](fakeRepo),
//	)
//	svc := app.MustResolve[*UserService]() // built with fakeRepo
//
// # Starting an App
//
// An App with something to start — a component with Start or Ready, a start
// hook — refuses ServeHTTP until its start phase has run. [Start] runs it with
// [credo.App.Start] and shuts the App down when the test ends; for an App
// built by [NewApp] that shutdown runs after every cleanup the test adds, so a
// test server closed through t.Cleanup is drained first:
//
//	app := testutil.NewApp(t, testutil.WithWiring(wire))
//	testutil.Start(t, app)
//	srv := httptest.NewServer(app)
//	t.Cleanup(srv.Close)
//
// # Injecting config
//
// [WithConfig] sets values at dotted key paths. Repeated calls merge into one
// document that is injected as the App's RawConfig. Only these values are
// loaded; a .env file in the working directory and CREDO_* environment
// variables do not reach the test App:
//
//	app := testutil.NewApp(t,
//		testutil.WithConfig("app.name", "checkout"),
//		testutil.WithConfig("app.timeout", "5s"),
//	)
//
// # Asserting on logs
//
// Wire a [LogBuffer] with [WithLogBuffer] to capture structured output —
// the access log records included once the feature is installed — then match
// records with [LogBuffer.AssertHas]:
//
//	buf := testutil.NewLogBuffer()
//	app := testutil.NewApp(t, testutil.WithLogBuffer(buf))
//	app.UseAccessLog()
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
//
// Maturity: beta
package testutil

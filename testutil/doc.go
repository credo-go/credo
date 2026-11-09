// Package testutil provides helpers for testing Credo applications: building a
// hermetic test App, overriding dependencies with fakes, starting an App served
// through ServeHTTP, injecting config, asserting on structured log output, and
// checking that every message key is in the catalogs.
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
//			app.Provide(NewUserService)
//			app.Provide(NewPostgresRepo) // binds *PostgresRepo
//			app.Alias[UserRepo, *PostgresRepo]()
//		}),
//		testutil.WithOverride[UserRepo](fakeRepo),
//	)
//	svc := app.MustResolve[*UserService]() // built with fakeRepo
//
// The override replaces the alias, and Finalize drops *PostgresRepo when
// nothing else uses it, so the real repository is never built.
//
// # Testing modules
//
// An application split into [credo.Module] values is tested with
// [WithModules], which hands the modules to [credo.App.UseModules] with one
// more module last whose Services applies the [WithOverride] values. The
// overrides land after every module's services and before any module's
// handlers resolve, so the handlers a module registers are built with the
// fake:
//
//	app := testutil.NewApp(t,
//		testutil.WithModules(users.Module{}, billing.Module{}),
//		testutil.WithOverride[billing.Gateway](fakeGateway),
//	)
//	rec := httptest.NewRecorder()
//	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invoices", body))
//
// An override given beside a UseModules call in a [WithWiring] function would
// come after the modules' handlers had finalized the container and panics; a
// UseModules call in the wiring beside [WithModules] is a second call and
// panics too.
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
// # Checking message keys
//
// A message key is a catalog key, never rendered as text: a key the catalogs
// do not define renders the code's built-in message. [CheckMessageKeys] parses
// the application's sources and fails the test for each key written as a
// literal that the catalogs do not define, and for each key that is not a
// literal:
//
//	func TestMessageKeys(t *testing.T) {
//		testutil.CheckMessageKeys(t, "locales", ".")
//	}
//
// Maturity: beta
package testutil

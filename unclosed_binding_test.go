package credo_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/testutil"
)

// unclErrConn, unclVoidConn and unclCtxConn have one Close shape each and
// record a call to it.
type (
	unclErrConn  struct{ j *journal }
	unclVoidConn struct{ j *journal }
	unclCtxConn  struct{ j *journal }
)

func (c *unclErrConn) Close() error {
	c.j.add("close")
	return nil
}

func (*unclErrConn) Exec() {}

func (c *unclVoidConn) Close() { c.j.add("close") }

func (c *unclCtxConn) Close(context.Context) error {
	c.j.add("close")
	return nil
}

// unclCloseExecer shows Close; unclExecer hides it.
type (
	unclCloseExecer interface {
		Exec()
		Close() error
	}
	unclExecer interface{ Exec() }
)

// unclComp is a component that also has Close.
type unclComp struct{ j *journal }

func (c *unclComp) Shutdown(context.Context) error {
	c.j.add("shutdown")
	return nil
}

func (c *unclComp) Close() error {
	c.j.add("close")
	return nil
}

// unclFailing is a component whose Start fails.
type unclFailing struct{}

func (*unclFailing) Start(context.Context) error    { return errors.New("start failed") }
func (*unclFailing) Shutdown(context.Context) error { return nil }

const unclosedText = "and the App does not close it at shutdown"

// unclosedWarnings returns the records of the unclosed-binding warning.
func unclosedWarnings(t *testing.T, entries []map[string]any) []map[string]any {
	t.Helper()
	var warnings []map[string]any
	for _, e := range entries {
		if msg, _ := e["msg"].(string); strings.Contains(msg, unclosedText) {
			warnings = append(warnings, e)
		}
	}
	return warnings
}

// unclosedServe registers, starts and shuts down an App — in debug mode
// when debug is set — running each of after once it has started, and
// returns the warnings its logger received.
func unclosedServe(t *testing.T, debug bool, register func(app *credo.App), after ...func(app *credo.App)) []map[string]any {
	t.Helper()
	logger, buf := newTestLogger(t)
	opts := []credo.Option{credo.WithLogger(logger)}
	if debug {
		opts = append(opts, credo.WithDebug())
	}
	app := mustNew(t, opts...)
	register(app)
	startComponents(t, app)
	for _, fn := range after {
		fn(app)
	}
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	return unclosedWarnings(t, parseJSONLines(t, buf.Bytes()))
}

// wantUnclosed fails the test unless warnings is the one warning msg, at
// Warn, with component=name and no module key.
func wantUnclosed(t *testing.T, warnings []map[string]any, msg, name string) {
	t.Helper()
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	w := warnings[0]
	if w["msg"] != msg {
		t.Errorf("msg = %q\nwant  %q", w["msg"], msg)
	}
	if w["level"] != "WARN" || w["component"] != name {
		t.Errorf("level = %v, component = %v; want WARN, %s", w["level"], w["component"], name)
	}
	if _, ok := w["module"]; ok {
		t.Errorf("the warning carries module=%v, want no module key", w["module"])
	}
}

const (
	unclosedValueRemedies = "give it credo.CloseOnShutdown() when the App should close it or its Close releases " +
		"nothing, credo.View() when another registration owns what it holds, or credo.Borrowed() when the caller " +
		"closes it"
	unclosedProvideRemedies = "give it credo.CloseOnShutdown() when the App should close it or its Close " +
		"releases nothing, or credo.View() when another registration owns what it holds"
)

// TestUnclosedBinding_EachCloseShapeWarnsOnce: in debug mode, a binding
// whose type shows Close() error, Close() or Close(ctx) error, that claims
// nothing and whose resource nothing owns, gets one warning when the App
// shuts down, naming the binding, its Close shape and the remedies — with
// credo.Borrowed() for a ProvideValue binding alone. The App does not close
// it, warning or not.
func TestUnclosedBinding_EachCloseShapeWarnsOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		shape   string
		value   func(app *credo.App, j *journal)
		provide func(app *credo.App, j *journal)
		resolve func(app *credo.App)
	}{
		{"*credo_test.unclErrConn", "Close() error",
			func(app *credo.App, j *journal) { app.ProvideValue(&unclErrConn{j: j}) },
			func(app *credo.App, j *journal) { app.Provide(func() *unclErrConn { return &unclErrConn{j: j} }) },
			func(app *credo.App) { app.MustResolve[*unclErrConn]() }},
		{"*credo_test.unclVoidConn", "Close()",
			func(app *credo.App, j *journal) { app.ProvideValue(&unclVoidConn{j: j}) },
			func(app *credo.App, j *journal) { app.Provide(func() *unclVoidConn { return &unclVoidConn{j: j} }) },
			func(app *credo.App) { app.MustResolve[*unclVoidConn]() }},
		{"*credo_test.unclCtxConn", "Close(ctx) error",
			func(app *credo.App, j *journal) { app.ProvideValue(&unclCtxConn{j: j}) },
			func(app *credo.App, j *journal) { app.Provide(func() *unclCtxConn { return &unclCtxConn{j: j} }) },
			func(app *credo.App) { app.MustResolve[*unclCtxConn]() }},
	} {
		t.Run(tc.shape+", ProvideValue", func(t *testing.T) {
			j := &journal{}
			warnings := unclosedServe(t, true, func(app *credo.App) { tc.value(app, j) })
			wantUnclosed(t, warnings, "credo: the ProvideValue binding of "+tc.name+" has "+tc.shape+", "+
				unclosedText+"; "+unclosedValueRemedies, tc.name)
			ownEvents(t, j)
		})
		t.Run(tc.shape+", Provide", func(t *testing.T) {
			j := &journal{}
			warnings := unclosedServe(t, true, func(app *credo.App) { tc.provide(app, j) }, tc.resolve)
			wantUnclosed(t, warnings, "credo: the Provide binding of "+tc.name+" has "+tc.shape+", "+
				unclosedText+"; "+unclosedProvideRemedies, tc.name)
			ownEvents(t, j)
		})
	}
	t.Run("a plain holder whose lazily built owner was never built", func(t *testing.T) {
		conn := &unclErrConn{j: &journal{}}
		warnings := unclosedServe(t, true, func(app *credo.App) {
			app.ProvideValue[unclCloseExecer](conn)
			app.Provide(func() *unclErrConn { return conn }, credo.CloseOnShutdown())
		})
		wantUnclosed(t, warnings, "credo: the ProvideValue binding of credo_test.unclCloseExecer has Close() "+
			"error, "+unclosedText+"; "+unclosedValueRemedies, "credo_test.unclCloseExecer")
	})
}

// TestUnclosedBinding_NoWarning: no warning for a binding the App closes
// (credo.CloseOnShutdown()), one its caller closes (credo.Borrowed()), a
// view, a component, a plain holder beside an owner in either registration
// order — the owner a value handed in or a binding built lazily after the
// start — a binding never built, a binding an override replaced, a binding
// whose type hides Close, or any binding without debug mode.
func TestUnclosedBinding_NoWarning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		debug    bool
		register func(app *credo.App, conn *unclErrConn)
		after    func(app *credo.App)
		closed   bool
	}{
		{"CloseOnShutdown", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue(conn, credo.CloseOnShutdown())
		}, nil, true},
		{"Borrowed", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue(conn, credo.Borrowed())
		}, nil, false},
		{"a view", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue[unclCloseExecer](conn, credo.View())
			app.ProvideValue(conn, credo.CloseOnShutdown())
		}, nil, true},
		{"a plain holder, owner first", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue(conn, credo.CloseOnShutdown())
			app.ProvideValue[unclCloseExecer](conn)
		}, nil, true},
		{"a plain holder, holder first", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue[unclCloseExecer](conn)
			app.ProvideValue(conn, credo.CloseOnShutdown())
		}, nil, true},
		{"a plain holder beside a lazily built owner, owner first", true, func(app *credo.App, conn *unclErrConn) {
			app.Provide(func() *unclErrConn { return conn }, credo.CloseOnShutdown())
			app.ProvideValue[unclCloseExecer](conn)
		}, func(app *credo.App) { app.MustResolve[*unclErrConn]() }, true},
		{"a plain holder beside a lazily built owner, holder first", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue[unclCloseExecer](conn)
			app.Provide(func() *unclErrConn { return conn }, credo.CloseOnShutdown())
		}, func(app *credo.App) { app.MustResolve[*unclErrConn]() }, true},
		{"a binding never built", true, func(app *credo.App, conn *unclErrConn) {
			app.Provide(func() *unclErrConn { return conn })
		}, nil, false},
		{"a binding an override replaced", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue(conn)
			app.ProvideValue(&unclErrConn{j: &journal{}}, credo.CloseOnShutdown(), credo.Override())
		}, nil, false},
		{"a type that hides Close", true, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue[unclExecer](conn)
		}, nil, false},
		{"without debug mode", false, func(app *credo.App, conn *unclErrConn) {
			app.ProvideValue(conn)
			app.Provide(func() *unclVoidConn { return &unclVoidConn{j: &journal{}} })
		}, func(app *credo.App) { app.MustResolve[*unclVoidConn]() }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &journal{}
			conn := &unclErrConn{j: j}
			var after []func(app *credo.App)
			if tc.after != nil {
				after = append(after, tc.after)
			}
			warnings := unclosedServe(t, tc.debug, func(app *credo.App) { tc.register(app, conn) }, after...)
			if len(warnings) != 0 {
				t.Fatalf("warnings = %v, want none", warnings)
			}
			if tc.closed {
				ownEvents(t, j, "close")
			} else {
				ownEvents(t, j)
			}
		})
	}
	t.Run("a component", func(t *testing.T) {
		j := &journal{}
		warnings := unclosedServe(t, true, func(app *credo.App) { app.ProvideValue(&unclComp{j: j}) })
		if len(warnings) != 0 {
			t.Fatalf("warnings = %v, want none", warnings)
		}
		ownEvents(t, j, "shutdown")
	})
}

// TestUnclosedBinding_EveryTeardown: the warning is logged once the drain
// begins, whatever begins it — the cleanup testutil.Start registers, a
// Shutdown of an App that never started, the rollback of a failed start —
// and once only: a later Shutdown adds none.
func TestUnclosedBinding_EveryTeardown(t *testing.T) {
	const want = "credo: the ProvideValue binding of *credo_test.unclErrConn has Close() error, " + unclosedText +
		"; " + unclosedValueRemedies
	t.Run("testutil.Start cleanup", func(t *testing.T) {
		buf := testutil.NewLogBuffer()
		t.Run("app", func(t *testing.T) {
			app := testutil.NewApp(t,
				testutil.WithLogBuffer(buf),
				testutil.WithConfig("server.debug", true),
				testutil.WithWiring(func(app *credo.App) { app.ProvideValue(&unclErrConn{j: &journal{}}) }),
			)
			testutil.Start(t, app)
			if got := unclosedWarnings(t, buf.Entries()); len(got) != 0 {
				t.Fatalf("warnings before shutdown = %v, want none", got)
			}
		})
		wantUnclosed(t, unclosedWarnings(t, buf.Entries()), want, "*credo_test.unclErrConn")
	})
	t.Run("Shutdown before the start", func(t *testing.T) {
		logger, buf := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger), credo.WithDebug())
		app.ProvideValue(&unclErrConn{j: &journal{}})
		if err := shutdownApp(t, app, 2*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
		_ = shutdownApp(t, app, 2*time.Second)
		wantUnclosed(t, unclosedWarnings(t, parseJSONLines(t, buf.Bytes())), want, "*credo_test.unclErrConn")
	})
	t.Run("a failed start", func(t *testing.T) {
		logger, buf := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger), credo.WithDebug())
		app.ProvideValue(&unclErrConn{j: &journal{}})
		app.ProvideValue(&unclFailing{})
		if err := app.Start(t.Context()); err == nil {
			t.Fatal("Start() = nil, want the failed start")
		}
		_ = shutdownApp(t, app, 2*time.Second)
		wantUnclosed(t, unclosedWarnings(t, parseJSONLines(t, buf.Bytes())), want, "*credo_test.unclErrConn")
	})
}

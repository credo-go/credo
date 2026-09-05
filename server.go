package credo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ServeHTTP implements http.Handler. The first call prepares the App —
// Finalize, compile, publish — and stores the result; a preparation error is
// a developer error and panics on every request rather than being retried.
// Requests are admitted by lifecycle state: a stopped App, or a stopping App
// that was never prepared, receives the default 503 envelope without touching
// DI or any configured callback, while an already-prepared handler keeps
// serving during the managed drain. Direct ServeHTTP never claims the managed
// server's start slot; an external http.Server stays its owner's job to drain
// before [App.Shutdown].
func (app *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	state := app.lifecycle.currentState()
	if state == stateStopped {
		app.rejectUnavailable(w, r, state)
		return
	}
	p := app.prep.Load()
	if p == nil {
		if state >= stateStopping {
			app.rejectUnavailable(w, r, state)
			return
		}
		if p = app.prepare(); p == nil {
			app.rejectUnavailable(w, r, app.lifecycle.currentState())
			return
		}
	}
	if p.err != nil {
		panic(p.err)
	}
	// http.NoBody (every bodyless request the stdlib server delivers) has
	// nothing to limit; skipping the wrap saves an allocation per request.
	if app.serverCfg.MaxBodyBytes > 0 && r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, app.serverCfg.MaxBodyBytes)
	}
	c := app.ctxPool.get()
	c.reset(w, r)
	// Errors are handled inside the compiled handler chain by
	// builtinErrorHandler (non-panic) and builtinRecover (panic).
	// The chain always returns nil.
	_ = p.handler(c)
	app.ctxPool.put(c)
}

// Run starts the HTTP server and blocks until an interrupt (Ctrl+C) or
// SIGTERM is received, then performs graceful shutdown using the deadline set
// by [WithShutdownTimeout]. An [App.OnPreDrain] hook that ignores that deadline
// remains a hard teardown barrier and may delay return. A second signal during
// shutdown force-kills the process. Returns nil on graceful shutdown.
//
// On Unix, Run also handles SIGHUP: each signal triggers [App.Reload] under
// the [WithReloadTimeout] budget (systemctl reload, logrotate postrotate).
// Reloads run one at a time, signals that arrive during a reload coalesce
// into at most one follow-up, and a failed reload is logged but never stops
// the server. There is no SIGHUP on Windows; there, and under [App.RunContext]
// on every platform, the programmatic [App.Reload] is the only trigger.
//
// Run serves HTTPS automatically when TLS is configured via [WithTLSFiles],
// [WithTLSConfig], or the server.tls.* config keys; otherwise it serves
// plaintext. A misconfigured certificate (missing file, mismatched pair, or a
// WithTLSConfig with no certificate source) fails fast before the server
// accepts connections, rolling the lifecycle back so the App can run again.
//
// Run is the safe default for a process whose lifetime is the server's. For
// explicit lifecycle control — tests, embedding, or caller-driven
// cancellation — use [App.RunContext].
func (app *App) Run() error {
	lm := app.lifecycle
	preflight, serveFn := app.serveFuncs()
	return lm.runSignal(func(ctx context.Context) error {
		return lm.serve(ctx, "Run", preflight, tcpListen, serveFn, app.httpRedirectAddr)
	})
}

// RunContext starts the HTTP server and blocks until ctx is cancelled, the
// server stops, or a programmatic [App.Shutdown]. Unlike [App.Run] it installs
// no signal handler; cancellation is entirely the caller's. On ctx
// cancellation the drain keeps ctx's values but drops its cancellation and
// applies the [WithShutdownTimeout] deadline. An [App.OnPreDrain] hook that
// ignores that deadline remains a hard teardown barrier and may delay return.
// Returns nil on graceful shutdown.
//
// Like [App.Run], RunContext serves HTTPS when TLS is configured (via
// [WithTLSFiles], [WithTLSConfig], or server.tls.*) and plaintext otherwise,
// with the same fail-fast certificate validation.
//
// Cancelling ctx during startup does not abort an in-progress [App.OnStart]
// hook: hooks receive the lifecycle context, not ctx, so the cancellation takes
// effect only after all hooks complete.
func (app *App) RunContext(ctx context.Context) error {
	preflight, serveFn := app.serveFuncs()
	return app.lifecycle.serve(ctx, "RunContext", preflight, tcpListen, serveFn, app.httpRedirectAddr)
}

// ServeContext serves on a caller-provided listener, sharing the same
// lifecycle as [App.RunContext]. It is the escape hatch for listeners the
// framework does not create itself — Unix sockets, a preconfigured test
// listener, or an externally managed listener. It supplies the listener only;
// the server itself is still the one the framework builds, so protocol-level
// settings such as H2C come from [WithHTTPServer]:
//
//	credo.WithHTTPServer(func(s *http.Server) {
//		s.Protocols = new(http.Protocols)
//		s.Protocols.SetHTTP1(true)
//		s.Protocols.SetUnencryptedHTTP2(true)
//	})
//
// ServeContext takes ownership of l: it is closed when the server stops,
// matching net/http.Server.Serve semantics. Returns nil on graceful shutdown.
//
// ServeContext serves l exactly as given and is TLS-exempt: TLS configured via
// [WithTLSFiles] or [WithTLSConfig] does not apply here, nor does the
// [WithHTTPRedirect] listener. For HTTPS on a custom listener, wrap it yourself
// — e.g. tls.NewListener(l, cfg).
func (app *App) ServeContext(ctx context.Context, l net.Listener) error {
	if l == nil {
		return errors.New("credo: ServeContext: nil listener")
	}
	return app.lifecycle.serve(ctx, "ServeContext", nil,
		func(*http.Server) (net.Listener, error) { return l, nil },
		plainServe, "",
	)
}

// Shutdown gracefully shuts down the server: it withdraws readiness, runs
// [App.OnPreDrain], cancels the lifecycle context, drains in-flight HTTP
// requests and [App.OnDrain] subsystem hooks in parallel, tears down DI
// singletons in dependency order, then runs OnShutdown hooks (LIFO). The
// caller's ctx carries the shared absolute deadline; [WithShutdownTimeout]
// does not replace it. An OnPreDrain hook that ignores ctx remains a hard
// teardown barrier and may delay return beyond that deadline. Returns an error
// if any shutdown step fails or remains incomplete (joined via errors.Join).
//
// Shutdown is also accepted on an App that was never run: bootstrap teardown
// closes route and DI registration, runs the same drain with no managed
// server, and tears down every singleton that exists — including values
// registered by a composition root whose [App.Finalize] never ran or failed.
// This is the cleanup path for tests and for Apps served through an external
// http.Server; that server's admission and drain remain its owner's job and
// must complete before Shutdown. The App is single-use: the terminal state is
// stopped even when cleanup was incomplete. Shutdown returns an error when the
// App is starting, or has already stopped.
func (app *App) Shutdown(ctx context.Context) error {
	lm := app.lifecycle
	err := lm.initiateShutdown(ctx)
	if !errors.Is(err, errShutdownNotRunning) {
		return err
	}
	if claimed, bootstrapErr := lm.initiateBootstrapShutdown(ctx); claimed {
		return bootstrapErr
	}
	return fmt.Errorf("credo: Shutdown: server in state %q, expected %q or %q",
		lm.currentState(), stateBuilding, stateRunning)
}

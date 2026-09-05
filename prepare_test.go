package credo_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// prepDep is a constructor-backed service whose construction is observable.
type prepDep struct{}

// prepMissing depends on a type nobody registers, so Finalize fails.
type prepMissing struct{}

// prepCloser records container-driven Shutdown calls.
type prepCloser struct {
	closed atomic.Bool
	err    error
}

func (c *prepCloser) Shutdown(context.Context) error {
	c.closed.Store(true)
	return c.err
}

// assertLifecycle503 checks the callback-free default envelope written to a
// request the lifecycle rejected.
func assertLifecycle503(t *testing.T, w *httptest.ResponseRecorder, head bool) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if head {
		if w.Body.Len() != 0 {
			t.Fatalf("HEAD body = %q, want empty", w.Body.String())
		}
		return
	}
	var env credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q is not the error envelope: %v", w.Body.String(), err)
	}
	if env.Success || env.Error.Code != credo.MsgKeyServiceUnavailable || env.Error.Message != "Service Unavailable" {
		t.Fatalf("envelope = %+v, want success=false, code service_unavailable, default message", env)
	}
}

func mustPanicWith(t *testing.T, fn func()) (recovered any) {
	t.Helper()
	defer func() {
		recovered = recover()
		if recovered == nil {
			t.Fatal("expected a panic")
		}
	}()
	fn()
	return nil
}

// --- Preparation: stored result, terminal failures ---

func TestServeHTTP_ImplicitFinalizeThenServe(t *testing.T) {
	app := mustNew(t)
	var built atomic.Int32
	app.MustProvide[*prepDep](func() *prepDep {
		built.Add(1)
		return &prepDep{}
	})
	app.GET("/x", func(ctx *credo.Context) error {
		dep, err := app.Resolve[*prepDep]()
		if err != nil || dep == nil {
			return err
		}
		return ctx.Response().Text(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK || built.Load() != 1 {
		t.Fatalf("status = %d, built = %d; want 200 and one construction", w.Code, built.Load())
	}
	if err := app.Provide[*prepMissing](func() *prepMissing { return &prepMissing{} }); err == nil {
		t.Fatal("Provide after implicit Finalize should be rejected")
	}
}

func TestFinalize_IsDIOnly_RoutesStayOpen(t *testing.T) {
	app := mustNew(t)
	app.MustProvide[*prepDep](func() *prepDep { return &prepDep{} })
	mustFinalize(t, app)

	// Composition after an explicit Finalize: resolve, then wire routes.
	dep := app.MustResolve[*prepDep]()
	app.GET("/x", func(ctx *credo.Context) error {
		if dep == nil {
			return errors.New("nil dep")
		}
		return ctx.Response().Text(http.StatusOK, "ok")
	})
	app.OnShutdown(func(context.Context) error { return nil })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Preparation admission closes HTTP registration.
	r := mustPanicWith(t, func() { app.GET("/late", func(*credo.Context) error { return nil }) })
	if !strings.Contains(r.(string), "compiled or shut down") {
		t.Fatalf("panic = %v, want the frozen diagnostic", r)
	}
}

func TestServeHTTP_PreparationError_IsTerminal(t *testing.T) {
	app := mustNew(t)
	app.MustProvide[*prepMissing](func(*prepDep) *prepMissing { return &prepMissing{} })
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	first := mustPanicWith(t, func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	})
	second := mustPanicWith(t, func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	})
	firstErr, ok := first.(error)
	if !ok || !strings.Contains(firstErr.Error(), "DI finalize") {
		t.Fatalf("first panic = %v, want the stored preparation error", first)
	}
	if second != first {
		t.Fatalf("second panic = %v, want the same stored error (no retry)", second)
	}
	if got := app.State(); got != "building" {
		t.Fatalf("State() = %q, want building (direct ServeHTTP never claims the start slot)", got)
	}

	// Managed serving reports the same stored error and releases the slot.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := app.ServeContext(t.Context(), l)
	if serveErr == nil || !errors.Is(serveErr, firstErr) {
		t.Fatalf("ServeContext = %v, want it to wrap the stored preparation error", serveErr)
	}
	if got := app.State(); got != "building" {
		t.Fatalf("State() after failed preparation = %q, want building", got)
	}
	// Bootstrap cleanup is still available; repair is not.
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown after preparation failure = %v", err)
	}
	// After stopped the lifecycle 503 wins over the cached error.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	assertLifecycle503(t, w, false)
}

func TestServe_CompilePanic_RecordedAndRolledBack(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	var wraps atomic.Int32
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		wraps.Add(1)
		panic("middleware construction failed")
	})
	app.GET("/x", func(ctx *credo.Context) error { return nil })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := app.ServeContext(t.Context(), l)
	if serveErr == nil || !strings.Contains(serveErr.Error(), "compile panicked: middleware construction failed") {
		t.Fatalf("ServeContext = %v, want the recorded compile panic", serveErr)
	}
	if got := app.State(); got != "building" {
		t.Fatalf("State() = %q, want building after rollback", got)
	}
	// A second managed start reports the stored failure without recompiling.
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if again := app.ServeContext(t.Context(), l2); again == nil || again.Error() != serveErr.Error() {
		t.Fatalf("second ServeContext = %v, want %v", again, serveErr)
	}
	if wraps.Load() != 1 {
		t.Fatalf("middleware constructor ran %d times, want 1", wraps.Load())
	}
	// Direct ServeHTTP panics with the same stored error; no partial handler runs.
	r := mustPanicWith(t, func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	})
	if rerr, ok := r.(error); !ok || !strings.Contains(rerr.Error(), "compile panicked") {
		t.Fatalf("ServeHTTP panic = %v, want the stored compile error", r)
	}
	var sawStack bool
	for _, e := range parseJSONLines(t, logs.Bytes()) {
		if e["msg"] == "credo: preparation failed" && e["stack"] != nil {
			sawStack = true
		}
	}
	if !sawStack {
		t.Fatal("preparation failure should be logged once with its stack")
	}
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown after compile panic = %v", err)
	}
}

// --- Lifecycle 503 ---

func TestServeHTTP_StoppedNeverPrepared_503WithoutCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []credo.Option
	}{
		{name: "recover enabled"},
		{name: "recover disabled", opts: []credo.Option{credo.WithoutRecover()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t, tc.opts...)
			var constructed, middleware, handler, renderer, status atomic.Int32
			app.MustProvide[*prepDep](func() *prepDep {
				constructed.Add(1)
				return &prepDep{}
			})
			app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
				middleware.Add(1)
				return next
			})
			app.SetErrorRenderer(func(*credo.Context, *credo.ErrorInfo) any {
				renderer.Add(1)
				return nil
			})
			app.StatusHandler(http.StatusNotFound, func(*credo.Context) error {
				status.Add(1)
				return nil
			})
			app.GET("/x", func(*credo.Context) error {
				handler.Add(1)
				return nil
			})
			if err := app.Shutdown(t.Context()); err != nil {
				t.Fatalf("bootstrap Shutdown = %v", err)
			}

			for _, path := range []string{"/x", "/missing"} {
				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				assertLifecycle503(t, w, false)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/x", nil))
			assertLifecycle503(t, w, true)

			if n := constructed.Load() + middleware.Load() + handler.Load() + renderer.Load() + status.Load(); n != 0 {
				t.Fatalf("rejected requests ran %d constructors/callbacks, want 0", n)
			}
			if got := app.State(); got != "stopped" {
				t.Fatalf("State() = %q, want stopped (the 503 never reopens the App)", got)
			}
		})
	}
}

func TestServeHTTP_PreparedThenStopped_503TakesPrecedence(t *testing.T) {
	app := mustNew(t)
	var handler atomic.Int32
	app.GET("/x", func(ctx *credo.Context) error {
		handler.Add(1)
		return ctx.Response().Text(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("prepared request status = %d", w.Code)
	}
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	assertLifecycle503(t, w, false)
	if handler.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1 (cached handler is not dispatched after stopped)", handler.Load())
	}
}

func TestServeHTTP_LatePublicationLosesToShutdown(t *testing.T) {
	app := mustNew(t)
	compileStarted := make(chan struct{})
	release := make(chan struct{})
	var handler atomic.Int32
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		close(compileStarted)
		<-release
		return next
	})
	app.GET("/x", func(*credo.Context) error {
		handler.Add(1)
		return nil
	})

	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	}()
	<-compileStarted

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- app.Shutdown(t.Context()) }()
	// Shutdown claims stopping first, then waits for the preparation in flight.
	deadline := time.Now().Add(2 * time.Second)
	for app.State() != "stopping" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := app.State(); got != "stopping" {
		t.Fatalf("State() = %q, want stopping while preparation is in flight", got)
	}
	close(release)

	<-served
	assertLifecycle503(t, w, false)
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if handler.Load() != 0 {
		t.Fatal("a preparation that lost publication must not dispatch")
	}
	// Nothing was published: later requests take the stopped path too.
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	assertLifecycle503(t, w, false)
}

func TestServeHTTP_PreparedHandlerServesDuringManagedDrain(t *testing.T) {
	app := mustNew(t)
	app.UseHealth()
	inHandler := make(chan struct{})
	release := make(chan struct{})
	app.GET("/slow", func(ctx *credo.Context) error {
		close(inHandler)
		<-release
		return ctx.Response().Text(http.StatusOK, "done")
	})
	app.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "pong")
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.ServeContext(context.Background(), l) }()
	waitRunning(t, app)

	slowDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + l.Addr().String() + "/slow")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		slowDone <- err
	}()
	<-inHandler

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- app.Shutdown(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for app.State() != "stopping" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// The prepared handler stays available during the drain; readiness reports
	// the drain and liveness stays up.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if w.Code != http.StatusOK || w.Body.String() != "pong" {
		t.Fatalf("/ping during stopping = %d %q, want the prepared handler", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "shutting_down") {
		t.Fatalf("/ready during stopping = %d %q, want 503 shutting_down", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/health during stopping = %d, want 200", w.Code)
	}

	close(release)
	if err := <-slowDone; err != nil {
		t.Fatalf("in-flight request: %v", err)
	}
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("ServeContext = %v", err)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	assertLifecycle503(t, w, false)
}

// --- Bootstrap Shutdown ---

func TestShutdown_Bootstrap_TearsDownRegisteredValues(t *testing.T) {
	app := mustNew(t)
	value := &prepCloser{}
	app.MustProvideValue[*prepCloser](value)
	var built atomic.Int32
	app.MustProvide[*prepDep](func() *prepDep {
		built.Add(1)
		return &prepDep{}
	})
	var drained, shut atomic.Bool
	app.OnDrain(func(context.Context) error { drained.Store(true); return nil })
	app.OnShutdown(func(context.Context) error { shut.Store(true); return nil })

	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if !value.closed.Load() {
		t.Fatal("a ProvideValue Shutdowner must be closed by bootstrap teardown")
	}
	if built.Load() != 0 {
		t.Fatal("teardown must not construct an unbuilt singleton")
	}
	if !drained.Load() || !shut.Load() {
		t.Fatalf("hook phases ran: OnDrain=%v OnShutdown=%v, want both", drained.Load(), shut.Load())
	}
	if got := app.State(); got != "stopped" {
		t.Fatalf("State() = %q, want stopped", got)
	}
	if err := app.Provide[*prepMissing](func() *prepMissing { return nil }); err == nil ||
		!strings.Contains(err.Error(), "frozen") {
		t.Fatalf("Provide after Shutdown = %v, want frozen error", err)
	}
	if _, err := app.Resolve[*prepCloser](); !errors.Is(err, credo.ErrDIClosed) {
		t.Fatalf("Resolve after Shutdown = %v, want ErrDIClosed", err)
	}
	r := mustPanicWith(t, func() { app.GET("/late", func(*credo.Context) error { return nil }) })
	if !strings.Contains(r.(string), "compiled or shut down") {
		t.Fatalf("route registration panic = %v", r)
	}
	if err := app.Shutdown(t.Context()); err == nil {
		t.Fatal("second Shutdown should report the stopped state")
	}
}

func TestShutdown_Bootstrap_WorksAfterFailedFinalize(t *testing.T) {
	app := mustNew(t)
	value := &prepCloser{}
	app.MustProvideValue[*prepCloser](value)
	app.MustProvide[*prepMissing](func(*prepDep) *prepMissing { return &prepMissing{} })
	if err := app.Finalize(); err == nil {
		t.Fatal("Finalize should fail on the missing dependency")
	}
	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown after failed Finalize = %v", err)
	}
	if !value.closed.Load() {
		t.Fatal("cleanup must derive from frozen registrations even when Seal failed")
	}
}

func TestShutdown_Bootstrap_ReportsShutdownerFailure(t *testing.T) {
	app := mustNew(t)
	cause := errors.New("flush failed")
	app.MustProvideValue[*prepCloser](&prepCloser{err: cause})
	err := app.Shutdown(t.Context())
	if !errors.Is(err, cause) {
		t.Fatalf("Shutdown = %v, want the Shutdowner error in the chain", err)
	}
	report, ok := errors.AsType[*credo.DIShutdownError](err)
	if !ok {
		t.Fatalf("Shutdown = %v, want a DIShutdownError", err)
	}
	var failed int
	for _, e := range report.Entries {
		if e.State == credo.DIShutdownFailed && errors.Is(e.Err, cause) {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("report entries = %v, want exactly one failed entry for the closer", report.Entries)
	}
	if got := app.State(); got != "stopped" {
		t.Fatalf("State() = %q, want stopped even when cleanup failed", got)
	}
}

func TestShutdown_Bootstrap_RacesManagedStart(t *testing.T) {
	for i := range 20 {
		app := mustNew(t)
		app.GET("/x", func(*credo.Context) error { return nil })
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		serveDone := make(chan error, 1)
		shutdownDone := make(chan error, 1)
		go func() { serveDone <- app.ServeContext(context.Background(), l) }()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdownDone <- app.Shutdown(ctx)
		}()

		shutdownErr := <-shutdownDone
		if shutdownErr != nil {
			// Shutdown observed the transient starting state: the managed start
			// owns the App now, so stop it once it is running.
			if !strings.Contains(shutdownErr.Error(), "starting") {
				t.Fatalf("iteration %d: Shutdown = %v, want the starting-state error", i, shutdownErr)
			}
			waitRunning(t, app)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := app.Shutdown(ctx); err != nil {
				t.Fatalf("iteration %d: follow-up Shutdown = %v", i, err)
			}
			cancel()
		}
		serveErr := <-serveDone
		if shutdownErr == nil && serveErr != nil && !strings.Contains(serveErr.Error(), "cannot be run after shutdown") {
			t.Fatalf("iteration %d: serve after bootstrap Shutdown = %v", i, serveErr)
		}
		if shutdownErr != nil && serveErr != nil {
			t.Fatalf("iteration %d: managed serve = %v", i, serveErr)
		}
		if got := app.State(); got != "stopped" {
			t.Fatalf("iteration %d: State() = %q, want stopped", i, got)
		}
		l.Close()
	}
}

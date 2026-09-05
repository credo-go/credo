package credo

import (
	jsonv2 "encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	internalobserve "github.com/credo-go/credo/internal/observe"
)

// preparation is the stored result of the one-time Finalize → compile →
// publish step shared by every serve path. Exactly one of handler and err is
// set. A preparation error is terminal: the DI plan is frozen by Finalize and
// the HTTP registrations are frozen at admission, so a retry could only
// reproduce the same failure.
type preparation struct {
	handler Handler
	err     error
}

// preparationPanicStackSize bounds the stack captured when compile panics.
const preparationPanicStackSize = 8192

// prepare returns the App's stored preparation, running it on first call.
// It returns nil when lifecycle admission rejected the attempt: a first
// preparation attempted in stopping or stopped, or an unfinished preparation
// that lost publication to a bootstrap Shutdown. Callers translate nil into
// the lifecycle 503 (ServeHTTP) or a state error (managed serve).
//
// Admission closes HTTP route/hook/feature/renderer registration (app.frozen)
// before any compile work. prepMu serialises preparation against bootstrap
// shutdown admission: Shutdown moves the state to stopping first, then takes
// prepMu, so a preparation already in flight either publishes before the
// state changed or observes the change and withholds its result.
func (app *App) prepare() *preparation {
	if p := app.prep.Load(); p != nil {
		return p
	}
	app.prepMu.Lock()
	defer app.prepMu.Unlock()
	if p := app.prep.Load(); p != nil {
		return p
	}
	if app.lifecycle.currentState() >= stateStopping {
		return nil
	}
	app.frozen.Store(true)
	p := &preparation{}
	p.handler, p.err = app.buildHandler()
	if app.lifecycle.currentState() >= stateStopping {
		// Shutdown won admission while the handler was being built: nothing
		// may publish after it, so the drain sees an unprepared App.
		return nil
	}
	app.prep.Store(p)
	return p
}

// buildHandler runs the DI-only Finalize and compiles the handler chain,
// converting a compile panic (duplicate routes, a panicking middleware
// constructor) into a recorded preparation error. The panic origin is logged
// once with its stack; the stored error is what every later request or serve
// call reports, so a partly compiled handler is never executed.
func (app *App) buildHandler() (handler Handler, err error) {
	if finalizeErr := app.Finalize(); finalizeErr != nil {
		return nil, fmt.Errorf("credo: prepare: DI finalize: %w", finalizeErr)
	}
	defer func() {
		if r := recover(); r != nil {
			if cause, ok := r.(error); ok {
				err = fmt.Errorf("credo: prepare: compile panicked: %w", cause)
			} else {
				err = fmt.Errorf("credo: prepare: compile panicked: %v", r)
			}
			handler = nil
			app.logger.LogAttrs(nil, slog.LevelError, "credo: preparation failed",
				slog.Any("error", err),
				slog.String("stack", internalobserve.StackTrace(preparationPanicStackSize)))
		}
	}()
	return app.compile(), nil
}

// lifecycleUnavailableBody is the default 503 envelope written to requests
// the lifecycle rejects. It is encoded once with the framework default JSON
// profile: the rejection path must work without preparation, live DI, or any
// configured renderer, message resolver or JSON options.
var lifecycleUnavailableBody = func() []byte {
	body, err := jsonv2.Marshal(ErrorResponse{
		Success: false,
		Error: ErrorBody{
			Code:    MsgKeyServiceUnavailable,
			Message: builtInMessages[MsgKeyServiceUnavailable],
		},
	}, defaultJSONOptions)
	if err != nil {
		panic(fmt.Sprintf("credo: encode lifecycle 503 body: %v", err))
	}
	return body
}()

// rejectUnavailable answers a request that lifecycle admission refused —
// the App has stopped, or is stopping without a prepared handler — with the
// callback-free default 503. It never compiles, resolves, dispatches, or runs
// user middleware, status handlers, renderers or access-log filters, because
// any of those may depend on singletons that are already shut down.
func (app *App) rejectUnavailable(w http.ResponseWriter, r *http.Request, state appState) {
	app.logger.LogAttrs(r.Context(), slog.LevelDebug, "credo: request rejected: app is not serving",
		slog.String("state", state.String()),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path))
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(lifecycleUnavailableBody)))
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(lifecycleUnavailableBody)
}

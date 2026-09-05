package credo

import "fmt"

// Built-in HTTP features are installed once, before the App prepares to
// serve, through the App.Use* registrations in this package: UseRequestID,
// UseAccessLog, UseCompress, UseDecompress, UseI18n, UseErrorRenderer and
// UseSuccessRenderer. Recovery is the one feature that is on by default; it is
// customized with WithRecoverConfig and disabled with WithoutRecover.
//
// Every feature has exactly one public path: a config-based Use* method takes
// zero configs for the defaults or one config for customization, a renderer
// registration takes its renderer directly, and there are no parallel
// constructor options or setters for the same feature. Calling Use* installs
// the feature; leaving it out leaves the feature off. Each registration
// succeeds at most once — a second call is a programming error and panics —
// and the configuration is validated and normalized off to the side, so a
// rejected call leaves nothing installed.
//
// Use* call order never determines request execution order: the request
// executor (see executor.go) applies the installed features in one fixed,
// framework-defined plan.

// oneConfig returns the single optional config of a Use* call, or its zero
// value when none was given. More than one config is a programming error.
func oneConfig[C any](what string, cfgs []C) C {
	switch len(cfgs) {
	case 0:
		var zero C
		return zero
	case 1:
		return cfgs[0]
	default:
		panic(fmt.Sprintf("credo: %s accepts at most one config", what))
	}
}

// installFeature publishes a validated feature under the preparation mutex.
// The registration window closes at HTTP preparation or shutdown admission
// (checkFrozen); re-checking under prepMu guarantees that a preparation
// admitted while the caller was validating can never be followed by a late
// publication, because prepare freezes the App while holding the same mutex.
// publish itself must be cheap and must not block: it runs under the lock
// that serializes preparation and bootstrap shutdown.
func (app *App) installFeature(what string, publish func()) {
	app.checkFrozen(what)
	app.prepMu.Lock()
	defer app.prepMu.Unlock()
	app.checkFrozen(what)
	publish()
}

// UseErrorRenderer installs the renderer that shapes error response bodies.
// The framework handles error classification, logging, the status code, HEAD
// handling, and committed-response guards internally; the renderer receives a
// request-scoped [ErrorInfo] containing normalized status, code, message key,
// resolved message, details, and violations, and returns the body to encode —
// or nil for the default Credo body. It is the error-side mirror of
// [App.UseSuccessRenderer]: install both to give every response, success and
// failure alike, one envelope.
//
// The renderer may be a method of a service resolved from DI after
// [App.Finalize]; registration stays open until the App prepares to serve.
// Omitting the call keeps the default JSON renderer. UseErrorRenderer panics
// for a nil renderer, when called twice, or after the App was prepared or
// shut down.
func (app *App) UseErrorRenderer(r ErrorRenderer) {
	if r == nil {
		panic("credo: App.UseErrorRenderer: nil renderer")
	}
	app.installFeature("App.UseErrorRenderer", func() {
		if app.errorRenderer != nil {
			panic("credo: App.UseErrorRenderer called twice")
		}
		app.errorRenderer = r
	})
}

// UseSuccessRenderer installs the renderer that shapes successful responses
// sent through [Context.Render]. It is opt-in: with no renderer installed,
// Render falls back to plain JSON and the framework imposes no response
// envelope. The renderer receives a [RenderInfo] (status, data, and any
// [RenderOption] side channels) and returns the body to encode — nil writes
// the data plain; the framework owns the write, mirroring
// [App.UseErrorRenderer]'s shape-only contract. The raw [Response] helpers
// ([Response.JSON] and friends) are never routed through it, so an enterprise
// envelope ({code,message,data}, HAL, JSON:API, …) applies only where handlers
// opt in via Render.
//
// Like UseErrorRenderer, it may be a DI-resolved method registered after
// Finalize, it is installed at most once, and it panics for a nil renderer,
// a second call, or a call after the App was prepared or shut down.
func (app *App) UseSuccessRenderer(r SuccessRenderer) {
	if r == nil {
		panic("credo: App.UseSuccessRenderer: nil renderer")
	}
	app.installFeature("App.UseSuccessRenderer", func() {
		if app.successRenderer != nil {
			panic("credo: App.UseSuccessRenderer called twice")
		}
		app.successRenderer = r
	})
}

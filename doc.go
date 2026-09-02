// Package credo is a batteries-included Go web framework that combines
// the best patterns from Chi (router), Echo (context), Goyave (architecture
// & components), and GoFr (enterprise toolkit).
//
// It targets Go 1.27+ and leverages generics for type-safe dependency
// injection without reflection.
//
// # Quick Start
//
//	package main
//
//	import (
//		"log"
//
//		"github.com/credo-go/credo"
//	)
//
//	func main() {
//	    app, err := credo.New()
//	    if err != nil {
//	        panic(err)
//	    }
//
//	    app.GET("/", func(ctx *credo.Context) error {
//	        return ctx.Response().JSON(200, map[string]string{"message": "Hello, Credo!"})
//	    })
//
//	    if err := app.Run(); err != nil {
//	        log.Fatal(err)
//	    }
//	}
//
// # Key Concepts
//
//   - Handler: func(*credo.Context) error — all handlers return errors
//   - Context: request-scoped struct with Request/Response accessors
//   - Middleware: func(credo.Handler) credo.Handler — wraps Handlers.
//     Four tiers run in order: built-in → global → group → route. Group
//     middleware is collected from the group parent chain when the app
//     compiles, so registration order affects execution order only —
//     middleware added to a group after its routes still applies to them.
//   - Route: fluent API with Name(), SetMeta(), Middleware()
//   - QUERY: first-class RFC 10008 routes via App.QUERY/Group.QUERY; request
//     content requires Content-Type and is commonly decoded with BindBody
//   - ErrorRenderer: receives normalized *ErrorInfo and shapes error response bodies via App.SetErrorRenderer — returns the body (nil = default Credo envelope); classification, localization, logging, status, and writing handled by framework; RFC 9457 is an opt-in renderer
//   - SuccessRenderer: opt-in uniform success envelope via App.SetSuccessRenderer, applied only at the Context.Render seam (raw Response helpers stay un-enveloped) — shape-only like ErrorRenderer: returns the body for a RenderInfo, the framework writes it
//
// # API Naming
//
// Credo uses a consistent verb convention so a method's name signals when and
// how it runs:
//
//   - With<X> / Without<X> — construction-time [Option] values passed to [New].
//     They only set configuration and perform no I/O, so their order does not
//     matter (e.g. [WithLogger], [WithAccessLogMinLevel], [WithoutAccessLog]).
//   - Use<X> — post-construction setup that mounts a subsystem: it registers
//     routes or an engine and may read files. It therefore can fail — panicking
//     on developer misuse, or returning an error when it touches the outside
//     world (e.g. [App.UseHealth], [App.UseI18n]).
//   - Set<X> / Remove<X> — imperative mutators for a single value or a
//     replaceable component (e.g. [App.SetErrorRenderer], [Route.SetMeta]).
//   - On<X> — registers a lifecycle hook (e.g. [App.OnStart], [App.OnPreDrain],
//     [App.OnDrain], [App.OnShutdown]).
//
// Request logging is on by default (see [WithLogger]). Silence individual
// routes or whole groups with the [MetaAccessLog] route meta, noisy paths with
// [WithAccessLogSkipper], or result classes with [WithAccessLogMinLevel] and
// [WithAccessLogResultFilter]; health probes are silent by default
// ([HealthConfig.LogRequests] re-enables them).
//
// # Panics and Errors
//
// Credo separates developer errors from runtime failures:
//
//   - Startup configuration (registering routes, hosts, middleware, names,
//     static files, health checks) panics on misuse — nil handlers, malformed
//     patterns, duplicates, or registration after the handler chain has
//     compiled. The route table is code written by the developer, so a
//     mistake there is a bug best caught at startup, not a condition to
//     handle.
//   - Anything that can legitimately fail at runtime — request handling,
//     server lifecycle, or operations touching the outside world (file I/O,
//     network) — returns an error.
//
// This is why [App.UseHealth] panics on misuse (it only registers in-process
// state) while [App.UseI18n] returns an error (it loads locale files). The
// same split applies to reload: registering [App.OnConfigChange] on a store
// that cannot reload panics, while [App.Reload] itself — which re-reads files
// and runs user hooks — returns an error.
//
// The split is drawn by cause, not by execution phase: a developer invariant
// violation panics even when the offending call happens to run during a
// request. [NewHTTPError] with an out-of-domain status or a malformed machine
// code is a bug in a constant, not a runtime condition — it panics on first
// execution, and built-in recovery renders a generic 500 that never publishes
// the invalid value. Request-derived text must map onto predeclared codes;
// it is never passed through as a wire identity.
//
// # Stability
//
// Credo is Beta: shipped packages are usable for real development, with breaking
// changes possible before v1. See the project README's "Maturity by Area" table
// for per-area status, including which features are experimental or still planned.
//
// Maturity: beta
package credo

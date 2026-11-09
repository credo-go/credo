// Package credo is a batteries-included Go web framework that combines
// the best patterns from Chi (router), Echo (context), Goyave (architecture
// & components), and GoFr (enterprise toolkit).
//
// It targets Go 1.27+ and leverages generics for type-safe dependency
// injection with no reflection on the request path.
//
// The package is large because the App's subsystems live in it: routing,
// dependency injection, components and their lifecycle, health, i18n, the
// HTTP features and the workers. The sections below group the overview by
// subject; the symbol index stays alphabetical, so [ContinuousConfig],
// [RestartPolicy], [Schedule] and [ScheduledConfig] stand apart from
// [Worker], [WorkerInfo] and [Workers].
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
//	    app.GET("/", func(c *credo.Context) error {
//	        return c.Response().JSON(200, map[string]string{"message": "Hello, Credo!"})
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
//     Three tiers run in order: global → group → route. Group middleware is
//     collected from the group parent chain when the app compiles, so
//     registration order affects execution order only — middleware added to a
//     group after its routes still applies to them. Panic recovery, request
//     IDs, access logging, compression, decompression and locale detection
//     are framework features that wrap the whole chain, not middleware.
//   - Route: fluent API with Name(), SetMeta(), Middleware()
//   - QUERY: first-class RFC 10008 routes via App.QUERY/Group.QUERY; request
//     content requires Content-Type and is commonly decoded with BindBody
//   - ErrorRenderer: receives normalized *ErrorInfo and shapes error response bodies via App.UseErrorRenderer — returns the body (nil = default Credo envelope); classification, localization, logging, status, and writing handled by framework; RFC 9457 is an opt-in renderer
//   - SuccessRenderer: opt-in uniform success envelope via App.UseSuccessRenderer, applied only at the Context.Render seam (raw Response helpers stay un-enveloped) — shape-only like ErrorRenderer: returns the body for a RenderInfo, the framework writes it
//
// # API Naming
//
// Credo uses a consistent verb convention so a method's name signals when and
// how it runs:
//
//   - With<X> / Without<X> — construction-time [Option] values passed to [New].
//     They only set configuration and perform no I/O, so their order does not
//     matter (e.g. [WithLogger], [WithRecoverConfig], [WithoutRecover]).
//   - Use<X> — post-construction setup that installs a subsystem or an HTTP
//     feature once: it registers routes, an engine or a feature configuration
//     and performs no I/O; what reads the outside world, such as locale files,
//     runs in the start phase. It panics on developer misuse — duplicate or
//     late registration, invalid config (e.g. [App.UseHealth], [App.UseI18n],
//     [App.UseAccessLog], [App.UseErrorRenderer]).
//   - Set<X> / Remove<X> — imperative mutators for a single request-state or
//     route value (e.g. [Context.SetUser], [Route.SetMeta]).
//   - On<X> — registers a lifecycle hook (e.g. [App.OnStart], [App.OnStop]).
//   - Add<X> — adds one of several registrations of a kind (e.g.
//     [App.AddLivenessCheck], [App.AddReadinessCheck]).
//   - A noun accessor returns the App's one registry of a subsystem that
//     takes many registrations (e.g. [App.Workers]).
//
// Panic recovery is on by default; request IDs, access logging, response
// compression and request decompression are explicit ([App.UseRequestID],
// [App.UseAccessLog], [App.UseCompress], [App.UseDecompress]). With access
// logging installed, silence individual routes or whole groups with the
// [MetaAccessLog] route meta, noisy paths with [AccessLogConfig.Skipper], or
// result classes with [AccessLogConfig.MinLevel] and
// [AccessLogConfig.ResultFilter]; health probes are silent by default
// ([HealthConfig.LogRequests] re-enables them).
//
// # Dependency Injection
//
// The container binds one singleton per type. [App.Provide] registers a
// constructor, whose first result type is the binding's type and whose
// parameters are resolved from the container — [Infra] first by convention,
// produced by the container for the binding it builds; [App.ProvideValue]
// binds a value built by the application, such as an opened database;
// [App.Alias] binds an interface to a bound type, and [App.BindMany] adds a
// type to the collection [App.ResolveAll] returns. Nothing is built at
// registration. The first [App.Resolve], [App.ResolveAll], [App.MustResolve]
// or [App.MustResolveAll] finalizes the container — validating the whole
// graph — and then resolves; [App.Finalize] is an optional early check that
// returns the same report as an error. [App.Has] probes a binding without
// building or finalizing anything. Framework components are never bound
// automatically: the one framework-made binding is [RawConfig].
//
// [RegistrationOption] values shape a registration: [Override] replaces an
// earlier binding or alias of the type, [Borrowed] binds a value whose start
// and teardown its owner keeps, [CloseOnShutdown] makes a value with Close a
// component, [ComponentName] names a component in logs, /ready and
// lifecycle errors — it is not a DI key — [Ingress] places a component in
// the ingress tier, and [OptionalReadiness] reports its readiness entry
// without letting it gate readiness. Each call checks the options it
// accepts and panics on the others.
//
// # Components
//
// A [Component] is a value whose teardown the App owns: a binding whose type
// shows Shutdown, or a value or constructor handed to [App.Manage], which
// adds a component that is not a binding. The binding's type, not its value,
// plans the rest: a component whose type shows [Starter] is started before
// its dependants, one that shows [Readier] enters /ready, and one that
// implements [IngressComponent] is placed in the ingress tier. A type that
// hides its value's Start, Ready or ingress tier is refused. The App starts
// components in dependency order before it accepts requests and shuts them
// down after their consumers in two tiers ([Tier]): the ingress tier with the
// HTTP drain, then the internal tier. Holders that share a resource
// ([ResourceIdentifier]) start it once and tear it down once, through its
// holder of record. Every call the App makes on a component carries its
// scoped [Infra], read with [InfraFrom], whose logger writes
// component=<name>. Start and drain failures are reported as a
// [*LifecycleError].
//
// # Workers
//
// [App.Workers] returns the App's one worker registry. [Workers.Continuous]
// registers a worker that runs until the App stops, restarted under its
// [RestartPolicy]; [Workers.Scheduled] registers one that runs on a
// [Schedule]. Each takes a worker — a [Worker] or a func(context.Context)
// error — a run function whose other parameters the container supplies, or
// a constructor of a worker that the start walk builds. Each worker is a
// component named "worker:<name>", started and stopped in its tier;
// [Workers.Snapshot] and [Workers.Lookup] report every worker's
// [WorkerInfo].
//
// # Panics and Errors
//
// Credo separates developer errors from runtime failures:
//
//   - Startup configuration (registering dependencies, routes, hosts,
//     middleware, names, static files, health checks) panics on misuse — nil
//     handlers, malformed patterns, duplicates, or registration after the
//     handler chain has compiled. The route table and the dependency graph are
//     code written by the developer, so a mistake there is a bug best caught
//     at startup, not a condition to handle.
//   - Anything that can legitimately fail at runtime — request handling,
//     server lifecycle, or operations touching the outside world (file I/O,
//     network) — returns an error.
//
// Bootstrap is sequential: registration calls — dependency bindings, feature
// mounts, satellite registrations, routes and hooks — come from the goroutine
// that builds the [App], before it runs, and are not safe for concurrent use.
// The documented order is:
//
//  1. configuration — [New];
//  2. the application's bindings — [App.Provide], [App.ProvideValue], a
//     database it opened among them, [App.Alias] and [App.BindMany];
//  3. feature mounts and component registrations, in any order among
//     themselves — [App.UseI18n], [App.UseHealth], workers and [App.Manage];
//  4. [App.Resolve], routes and anything built from a resolved value, a
//     readiness check included: the first resolving call finalizes the
//     container, and [App.Finalize], called before it, is an optional early
//     check of the graph that returns its report as an error;
//  5. [App.Run].
//
// A mistake surfaces in one of three phases:
//
//   - Registration panics on misuse known at the call site, with a message
//     naming the call and the remedy: a constructor of the wrong shape, a
//     duplicate binding, an [App.Alias] or [App.BindMany] whose types do not
//     fit, and a dependency registration after shutdown began or after the
//     container was finalized, naming what finalized it — [App.Finalize],
//     the first resolving call, or the App's start or first request — where,
//     and the calls that led there.
//   - Finalizing returns what only the whole dependency graph reveals —
//     every missing dependency with its whole path from the registration that
//     needs it, every cycle — joined in registration order, with the same
//     text on every run: [App.Finalize] returns it, and so does the first
//     resolving call when it is the one that finalizes.
//   - Starting and serving return errors for what touches the outside world.
//
// This is why [App.UseI18n] panics on misuse at the call while a missing or
// malformed locale file fails the start: the files are read in the start
// phase. Reload follows the same split by cause: it re-reads files and runs
// the live sections' callbacks, so [App.Reload] returns an error, and so does
// declaring a section live over a configuration that cannot reload, a
// constructor error at Resolve or at start.
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

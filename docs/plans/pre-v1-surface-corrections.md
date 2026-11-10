# Pre-v1 Surface Corrections

This plan corrects the public surface before v1 freezes it. It covers five deliveries, in this order:

- **Part A**: fixes and tests that change no API.
- **Part B**: additions.
- **Part C**: breaking changes in the root package.
- **Part D**: breaking changes in `middleware`, `validation`, `auth` and `store`.
- **Part E**: a spike on resource ownership and, if it passes, the change.

**What decides a change.** Credo is in beta, and the applications that exist are a small sample of what it is for. An API is not removed because nothing calls it yet. A name goes only when it is wrong or misleading as implemented, when it is a second name for one concept, or when a recorded decision already replaces it; a feature that is needed and implemented wrongly is fixed, not deleted.

Each part is one branch and one pull request, with one commit per item; a part whose pull request outgrows a review is split at an item boundary. Everything lands on `main` under the CHANGELOG's `[Unreleased]` heading and cuts no release ([CONTRIBUTING](../../CONTRIBUTING.md#releasing)). When the last part lands, what remains of this plan is folded into the ADRs, the specs and `TODO.md`, and this file is deleted.

Progress is tracked by the boxes below.

**Rules for every item.**

- An item's **Decided** line states a choice that is settled, with the alternative it rejects; the ADR the item writes records both. The one decision still open is Part E's (E2), which is taken on the spike's evidence.
- A break gets a CHANGELOG entry under its own **BREAKING** heading in the short form ([CONTRIBUTING](../../CONTRIBUTING.md#changelog-and-release-notes)) and a row in the Unreleased section of the [migration guide](../guides/pre-v1-migration.md).
- An item updates the ADR and spec it names, the guides and `examples/` that use the changed API, and the quick reference in `CLAUDE.md`.
- Every error and panic an item adds or changes names what failed and the remedy, and the test that triggers it asserts both.
- Every commit passes `go vet`, `gofmt`, the CI linter configuration and `go test -race` over the root module, and over `store/sqldb` in its own module when the item touches `store`.
- An item that touches the request path carries a benchstat table in the pull request ([CONTRIBUTING](../../CONTRIBUTING.md#performance-changes)).

## Part A: Fixes and tests

- [x] **A1. A disabled health endpoint accepts checks.**
  - **Problem:** `UseHealth(HealthConfig{Enabled: new(false)})` returns before it creates the engine, so a later `AddLivenessCheck` or `AddReadinessCheck` panics with `credo: UseHealth() must be called before AddReadinessCheck`. The message is false, and a second `UseHealth` after a disabled one is not refused. An application that maps a configuration key to `Enabled` and registers a check fails at registration in the deployment that turns health off.
  - **Fix:** record the registration before the early return. Checks are accepted and never served, and a second `UseHealth` panics as it does after an enabled one.
  - **Tests:** a check registered after a disabled `UseHealth` does not panic and both endpoints answer 404; a second `UseHealth` panics.
  - **Docs:** the health spec; CHANGELOG under Fixed, and as **BREAKING (behavior)** for the second call, which the spec had accepted. C3 removes the cause; this fix does not wait for it.
- [x] **A2. The middleware guide's complete example sends `nosniff`.**
  - **Problem:** the example passes `SecureConfig{HSTSMaxAge: 31536000, XFrameOptions: "DENY"}`, and a `SecureConfig` that is passed drops the defaults of the fields it leaves out, so the example serves without `X-Content-Type-Options`.
  - **Fix:** add `ContentTypeNosniff: "nosniff"` to the example. D1 removes the cause and then takes the line out again.
- [x] **A3. Stale godoc.** `WithLogger` says access and request logging are on by default; the access log is `app.UseAccessLog()`. The `App` type's doc describes a middleware stack and an error handler it no longer holds. `Context.RequestID` names a request ID middleware; the feature is `app.UseRequestID()`.
- [x] **A4. A request with another method to a static prefix.**
  - **Problem:** `Static` registers two routes, the prefix itself and `prefix/{_static...}`, and the pair has to answer as one route. The test for a request with another method covers the catch-all route only.
  - **Fix:** a test that a `POST` to the prefix itself is a 405 with `Allow`, with and without SPA mode, on the App and on a group. If the test fails, the fix is in this commit.
- [x] **A5. The verb convention names noun registrations.**
  - **Problem:** the documented verbs (`With`, `Use`, `Set`, `On`, `Add`, a noun accessor) do not cover a registration named by a noun: `Middleware`, `PreRouting`, `StatusHandler`, `Name`.
  - **Fix:** one rule added where the convention is written: the root `doc.go`, ADR-023, `CLAUDE.md` and `AGENTS.md`. No rename.
- [x] **A6. `TODO.md` reflects what shipped.**
  - **Problem:** five open boxes describe work that is done in whole or in part, and three deferred boxes cite a document that no longer mentions them.
  - **Fix:** no code changes.
    - Checked: the duplicate-route diagnostics (the panic names the call site of both registrations); "Detect duplicate route patterns" under registration-time route validation (a duplicate panics at registration); `example_test.go` for the root package and `config` (both have their examples).
    - Narrowed: "Optional/critical stores and bounded low-cardinality tags" keeps the tags, since `credo.OptionalReadiness()` shipped the rest. The `connection_id` box no longer waits for the access log's `Attrs` option, which exists; it names what is left, a way for a connection's identifier to reach the `Exchange`.
    - `app.Container()` is removed: the DI surface is ADR-004's seven methods on `App`. `BuildProxyURL` and the optional `Validate` on a route stay deferred, without the reference to a document that does not record them.

## Part B: Additions

- [ ] **B1. `T` and `TPlural` warn in debug mode when a key is missing.**
  - **Problem:** a miss returns the key and leaves no trace in the log.
  - **Fix:** the key is still returned, and debug mode logs a warning with the key, the locale and the route, in the shape of the error pipeline's warning for a missing message key.
  - **Not changed:** what a miss returns. The result goes into a field of a response the application builds, where an empty string would blank the field without a trace and a key is seen at once. The error pipeline can refuse to render a key because it has other text to fall back on; `T` has none.
  - **Docs:** the i18n spec and the localization guide; CHANGELOG under Added.
- [ ] **B2. `New` reports what the automatic load read.**
  - **Problem:** `credo.New` without `WithRawConfig` reads `config.json`, `.yaml` or `.yml`, a `.env` and the `CREDO_*` environment and logs nothing about it. An application that passes only options takes these inputs whether or not its author knows of them, and a file left in the working directory changes `server.*` without a trace.
  - **Fix:** one record at construction naming the files found, whether a `.env` was read and from which path, and the environment prefix; never a value. The loader keeps the list of sources it read and `*config.Config` exposes it, so an application that loads explicitly can log the same record. `New` with `WithRawConfig` writes no record.
  - **Decided:** the record is written at Info when a file or a `.env` was read, and at Debug when the load found only the environment.
  - **Not changed:** ADR-005's rejection of a `WithoutAutoConfig` option. An application that wants no bootstrap input passes a `RawConfig` of its own.
  - **Docs:** ADR-005, the config spec and the configuration guide; CHANGELOG under Added.
- [ ] **B3. A framework `log` section.**
  - **Problem:** the default logger is a text handler on stderr at the default level, and no configuration key changes it. An application that wants JSON records or a configured level has to build a logger itself, or load the configuration before `New` to read a section of its own, with a `slog.LevelVar` it rewires by hand on reload, as `examples/saas` does.
  - **Fix:** `New` builds the default logger from `log.level` (`debug`, `info`, `warn`, `error`) and `log.format` (`text`, `json`). `WithLogger` stays the whole-source override, as `WithTLSConfig` is for TLS: when it is set, the section is not read.
  - **Decided:**
    - *The section's name.* Top-level `log`, a live section, so a reload applies a new level. Rejected: `server.log`, which collides with no application section but cannot be live, because `server` is static and a section has one nature.
    - *What a reload applies.* Both keys, the level through a `slog.LevelVar` and the format by swapping the handler behind one atomic load.
  - **Break:** `log` becomes a framework section decoded strictly. An application with a `log` section of its own fails construction, naming the unknown key, until it renames the section.
  - **Check first:** how the framework reads `server` from a `RawConfig` that is not a `*config.Config`. `log` follows the same path; a configuration that cannot hold a live section gets the section decoded once.
  - **Tests:** each level and format; an unknown level fails `New`; a reload changes the level and the format; `WithLogger` wins; an unknown `log.*` key fails `New`.
  - **Docs:** ADR-005 and ADR-014, the config spec, the configuration guide's key table, `examples/saas`, which drops its own wiring. CHANGELOG as **BREAKING** and under Added.
- [ ] **B4. XML binding has a written contract.**
  - **Problem:** an XML body is decoded with `xml.NewDecoder(body).Decode(target)`. A decode failure becomes a `BindError`, but nothing says what the binder does with content after the root element, with a repeated element, or with an unknown element under `WithStrictBodies`, each of which the JSON branch decides.
  - **Check first:** write the three tests and record what happens today.
  - **Decided:** content after the root element other than white space, comments and processing instructions is refused with the trailing-data reason, as a JSON body's is; a repeated element keeps `encoding/xml`'s meaning, since repetition is how XML spells a slice; unknown elements are ignored in both modes, and the contract says so.
  - **Docs:** ADR-008, with the reason XML stays (feeds and sitemaps on the way out; banking, e-invoicing and SAML bodies on the way in), and the context spec. CHANGELOG as **BREAKING (behavior)**: an XML body with content after its root element is refused.
- [ ] **B5. Debug mode names a binding with `Close` that the App does not close.**
  - **Problem:** `Close` is never discovered: a binding whose type has one is closed only with `credo.CloseOnShutdown()` (ADR-024). A binding that needed the option and lacks it stays open at shutdown, and nothing says so.
  - **Fix:** with `WithDebug()`, the start logs one warning for each binding whose type shows one of the three `Close` shapes, is not a component and is not borrowed. The warning names the binding and both remedies: `credo.CloseOnShutdown()` when the App should close it, `credo.Borrowed()` when the caller does.
  - **Not changed:** what the App closes. Discovery stays rejected, for ADR-024's reason: the method is too common to mean that the App owns the value.
  - **Check first:** that `credo.Borrowed()` is accepted on a `ProvideValue` binding that is not a component, and silences the warning there; what silences it for a binding a constructor builds, which takes no `Borrowed()`; and that a binding sharing its resource with a registration that has a teardown is not warned about.
  - **Tests:** each `Close` shape warns once, naming the binding and both options; no warning with `CloseOnShutdown()`, with `Borrowed()`, for a component, or without debug mode.
  - **Docs:** the container spec and ADR-024, the dependency-injection guide; CHANGELOG under Added. The box in `TODO.md` is checked.

## Part C: The root package

- [ ] **C1. A route has one write handle and one read view.**
  - **Problem:** `Context.Route()` returns `*Route`, the handle registration returns, so its setters are in reach at request time and panic when called. A read view exists, `RouteInfo`, yet one datum has three spellings: `Route.GetPattern()`, `RouteInfo.Pattern`, and `Exchange.Route()`, which returns the pattern as a string.
  - **Fix:** `*Route` is what a registration returns and nothing else. `Context.Route()`, `Exchange.Route()`, `App.GetRoute(name)` and `App.Routes()` give the same `RouteInfo`. `GetRoute` returns `(RouteInfo, bool)`, so `Get` means a lookup that reports presence everywhere except `GetConfig`. `BuildURI` and `BuildURL` move to `RouteInfo`.
  - **Removed:** `Route.GetName`, `GetMethod`, `GetPattern`, `GetHost`; `Exchange.RouteName`.
  - **Shape:** a route's `RouteInfo` is built once at compile and returned by value, so a request allocates nothing for it. `Context.HasRoute` stays.
  - **Tests:** a middleware reads `c.Route().GetMeta(key)` unchanged; the three accessors return equal values for one request; a mount's view; no route gives the zero view and `HasRoute` false. Benchstat on the `ServeHTTP` benchmarks.
  - **Docs:** ADR-007, the router and context specs, the routing and middleware guides.
- [ ] **C2. `BuildURL` returns a valid reference.**
  - **Problem:** for a host-scoped route `BuildURL` returns the host joined to the path, `acme.myapp.com/users/42`. That is not a URL, and a client resolves it as a relative path. The feature is right to have, since a path alone cannot address a host-scoped route, and wrong as implemented.
  - **Fix:** it returns the network-path reference `//acme.myapp.com/users/42` (RFC 3986 §4.2), which is valid as it stands in a `Location` header or a link. A caller that needs an absolute URL prefixes the scheme it knows.
  - **Decided:** the reference. Rejected: `BuildURL(scheme string, params ...string)`, which asks for a scheme where a reference would do.
  - **Tests:** the reference parses with `net/url` to the host and path; a host-less route returns what `BuildURI` returns; host parameters first, then path parameters, as today.
  - **Docs:** ADR-007, the router spec and the routing guide. Lands with C1, since both move the method.
- [ ] **C3. Health: checks in any order, as functions, with plain bools.**
  - **Problem:** a check registered before `UseHealth` panics, the one registration in the root whose validity depends on call order without that being its meaning. A check is a `HealthChecker` with a `HealthCheckFunc` adapter, beside `Readier.Ready(ctx) error`, the same contract under another name. `Enabled`, `Liveness` and `Readiness` are `*bool` with nil meaning true.
  - **Fix:**
    - Checks are collected whenever they are registered. `UseHealth` mounts the endpoints and preparation joins the two. Checks registered on an App that never calls `UseHealth` are accepted, and the start logs one warning naming their count.
    - A check is a `func(context.Context) error`. A value with a `Check` method is passed as `v.Check`.
    - `HealthConfig.Disabled`, `DisableLiveness` and `DisableReadiness` replace the three pointers, as `RestartPolicy.Disabled` and `RecoverConfig.DisableStackTrace` already spell a default-on switch.
  - **Removed:** `HealthChecker`, `HealthCheckFunc`, `HealthConfig.Enabled`, `Liveness`, `Readiness`.
  - **Docs:** ADR-016 (the `*bool` decision is reversed; an inverted bool tells "not set" from false with a zero value) and ADR-017's sentence that reserves the pointer pattern; the health spec; the deployment guide.
- [ ] **C4. `App.State()` returns a typed state.**
  - **Fix:** `type State string` with `StateBuilding`, `StateStarting`, `StateRunning`, `StateStopping`, `StateStopped`, the string values unchanged, so a comparison with a literal still compiles. `IsRunning` stays.
  - **Break:** code that assigns the result to a `string` variable converts it.
  - **Docs:** the lifecycle spec.
- [ ] **C5. `ConfigExists` takes a section.**
  - **Problem:** `App.ConfigExists(key string)` is the one configuration method on `App` that takes a string key.
  - **Fix:** `config.Section[T]` gains `Exists(src RawConfig) bool`, and `App.ConfigExists` takes a section as `GetConfig` does. Like the other two, it never finalizes the container.
  - **Docs:** ADR-005, the config spec and the configuration guide.
- [ ] **C6. The panic error is a root type.**
  - **Problem:** `DIPanicError`, `DIPanicPhase` and its constants are aliases of `internal/di` types, so their documentation lives at an internal type, the reason ADR-023 gives for defining the worker types in the root. The `DI` prefix also misnames a panic in a component's `Start` or `Shutdown`.
  - **Fix:** a root-defined `PanicError{Type, Phase, Value, Stack}`, converted at the root's boundary wherever the container's error surfaces: `Resolve`, the value `MustResolve` panics with, and a `LifecycleError`'s entries.
  - **Decided:** the phase is a root-defined `PanicPhase` with today's four values (construction, start, shutdown, late cleanup). Rejected: folding them into `LifecyclePhase`, which has two values and would lose which of them a panic came from, or gain two values that no `LifecycleEntry` carries.
  - **Tests:** `errors.AsType[*credo.PanicError]` finds the error from `Resolve` and through a `LifecycleError`; a panic value that is an error is still reachable through `errors.Is`.
  - **Docs:** ADR-022 and ADR-024; the container and lifecycle specs.
- [ ] **C7. `T` takes a `MessageKey`, and the key check covers it.**
  - **Problem:** `Context.T` and `TPlural` take a `string`, so the call an application writes catalog keys into by hand is the one the key type and `testutil.CheckMessageKeys` do not cover.
  - **Fix:** the key parameter is a `credo.MessageKey`; an untyped literal still compiles. `CheckMessageKeys` also collects the first argument of a `T` or `TPlural` call.
  - **Decided:** computed keys. A key built from a prefix and a code is written `c.T(credo.MessageKey(prefix + code))`, and the check reports every conversion whose operand is not a literal, so an application with such key families could not adopt the check. The check therefore accepts a concatenation that begins with a literal or a string constant ending in `.`, treats that text as a family, and fails when no catalog defines a key under it; a computed key with no literal prefix is still reported. Rejected: leaving the check as it is and asking the application for a map from each code to a typed constant.
  - **Tests:** a literal in `T` that no catalog defines fails the check; a family with catalog keys passes and one without fails, naming the prefix; a key with no literal prefix is reported.
  - **Docs:** ADR-013, the i18n spec, the localization guide and the `testutil` godoc. Lands after B1.
- [ ] **C8. `HTTPError` loses its JSON tags.**
  - **Problem:** the tags present the type as a wire shape, and it is not one: nothing marshals it, the envelope is built from `ErrorInfo`, and `Details` is tagged `json:"-"`, so marshalling it directly yields a flat object that is not the envelope and omits the details.
  - **Fix:** remove the tags. A test pins that the default and the RFC 9457 bodies are unchanged.
  - **Docs:** ADR-009 if it shows the struct; CHANGELOG as **BREAKING** for code that marshalled the type.

## Part D: Packages

- [x] **D1. One rule for config structs: a zero field selects its default.**
  - **Problem:** the middleware spec says a config that is passed replaces the defaults. In the code every normalizer already fills its zero fields — `CORS`, `CSRF`, `ContractGuard`, `Rewrite`, `Timeout`, `ratelimit` and `httpclient`'s retry — except `Secure`, whose three header fields are defaulted only when no config is passed. `Secure(SecureConfig{HSTSMaxAge: 31536000})` therefore sends neither `X-Content-Type-Options` nor `X-Frame-Options`.
  - **Fix:**
    - `Secure` fills its zero fields like the others, and the spec states the one rule: a zero field selects its default, and turning a default off takes an explicit value.
    - `SecureConfig.ContentTypeNosniff string` becomes `DisableContentTypeNosniff bool`, since the header has one value. `XFrameOptions` keeps its string, with `SAMEORIGIN` for the zero value, and gains `DisableXFrameOptions bool` for an application that uses `frame-ancestors` instead.
    - `X-XSS-Protection` is no longer sent by default. Browsers removed the auditor it controlled; the field stays for an application that wants to send `0`.
  - **Removed:** `DefaultContractConfig`, `DefaultCORSConfig`, `DefaultCSRFConfig`, `DefaultRewriteConfig`, `DefaultSecureConfig`, `DefaultTimeoutConfig` and `DefaultSkipper`. Under the rule each is a second spelling of the zero value.
  - **Decided:** `ratelimit.DefaultConfig` and `httpclient.DefaultRetryConfig` go with them, since both configs already follow the rule; `httpclient.DefaultRetryIf` stays, as the function a custom `RetryIf` composes with.
  - **Tests:** each middleware with a partial config keeps its other defaults; the two `Disable` fields; no `X-XSS-Protection` unless set. A2's line comes out of the guide.
  - **Docs:** ADR-010, the middleware and httpclient specs, the middleware guide. Release notes, first: a `SecureConfig` that relied on a field left out being off now sends that header.
- [x] **D2. `validation`'s error types are named for what they carry.**
  - **Problem:** the wire name is `violations`, chosen in ADR-009 because `errors` stutters; the Go types stutter the same way (`validation.ValidationError`, `validation.Errors`), and a bind failure travels in the same type without being a validation error.
  - **Fix:** `ValidationError` becomes `Violation`, `Errors` becomes `Violations`, `NewError` becomes `NewViolation`. No behaviour changes.
  - **Docs:** ADR-011 and ADR-009, the validation spec and guide, the error-handling guide.
- [x] **D3. `auth.Middleware` takes a config; its callback takes the context first.**
  - **Problem:** `auth.ErrorFunc` is `func(err error, ctx *credo.Context) error`, and `auth.Middleware(a, onError)` takes it as a required positional argument, so the common call is `auth.Middleware[User](a, nil)`. Every other callback in the framework takes the context first, and every other middleware takes an optional config struct.
  - **Fix:** `auth.Middleware[T](a, cfg ...auth.Config)` with `Config.ErrorHandler func(*credo.Context, error) error`, the field name and signature `TimeoutConfig` and `CSRFConfig` use. `ErrorFunc` goes.
  - **Docs:** ADR-012 (the callback over a subtype stands; its position changes), the auth spec, the examples.
- [x] **D4. One name for each error kind.**
  - **Problem:** `store.Kind` and its eleven `Kind*` constants are a one-to-one copy of `fault.Kind` and its constants.
  - **Fix:** `store` and `store/sqldb` use `fault.Kind*`; the aliases go. `store.Error`, the sentinels and the classifiers are unchanged.
  - **Docs:** ADR-015, the store spec and the data-access guide.

## Part E: Resource ownership

Any number of registrations may hold one resource — one pointer under two bindings, or a wrapper and the handle it embeds — and the framework works out the rest: a holder of record, a deferral rule for a value a constructor builds without becoming a binding, an agreement among the holders on the tier and on the readiness entry's options, and an exemption for a borrowed resource. The proposal is that the application says it instead: one registration owns a resource, and a second registration that holds the same identity declares itself a view, with `Alias` for an interface view or an option on the registration, and is refused, naming the owner and both remedies, when it does not.

The rule covers a resource the App does something for: a start, a readiness entry or a teardown. A value with no lifecycle raises no ownership question and is bound under as many types as the application likes, with no declaration, as today.

- [ ] **E1. Spike.** A branch that is never merged. It implements the refusal behind the existing tests and runs each scenario below; the output is a table of scenario, today's behaviour, the behaviour under the rule, and what the application writes.
  - A wrapper bound beside the handle it embeds.
  - One pointer with a lifecycle under two bindings.
  - An interface view returned by an adapter's constructor.
  - A worker constructor's value that holds a bound resource.
  - `parent.Manage(child)`.
  - One pointer with no lifecycle under several interface bindings, where the concrete type is never bound: accepted with no declaration. `Alias` needs its target registered, so a rule without this scope would force that application to bind the concrete type.
  - One pointer with `Close` under two bindings, one of which carries `credo.CloseOnShutdown()`.
- [ ] **E2. Decision.** The rule is accepted only if it holds in every scenario and deletes more rules than it adds. The decision settles:
  - whether the view marker is `credo.Borrowed()`, which today means "start and shutdown are the caller's", or an option of its own;
  - whether a binding beside a `CloseOnShutdown()` owner is a plain holder or a view;
  - what a value built by a constructor does when it turns out to hold an owned resource, since its identity is known only when the start walk builds it.

  If the rule fails, ADR-024 gains the alternative with the scenario that defeated it, and E3 is dropped.
- [ ] **E3. Implementation.** One owner per resource; the entry's options (`ComponentName`, `OptionalReadiness`, the tier) are the owner's alone. Deleted: the selection of a holder of record, the deferral rule, the agreement checks and the borrowed exemption. Kept: identity detection, `ResourceIdentifier`, one teardown and one readiness entry per resource.
  - **Docs:** ADR-024, the container and lifecycle specs, the dependency-injection guide. Release notes, first: a second undeclared registration of a resource with a lifecycle now fails.

## Not in this plan

- **`httpclient`'s manual `traceparent` API.** The observability release decides whether the manual transport stays as the stdlib-only option beside the OpenTelemetry adapter (ADR-014). The decision is due before v1 freezes the six names.
- **`WithRedirectTrailingSlash(bool)`.** It overrides `server.redirect_trailing_slash` in either direction, which a `Without` option could not; it is an override of a configured value, like `WithAddr`, not a default-on switch like `WithoutRecover`.
- **One option type per registration call.** `RegistrationOption` is accepted by seven calls, each a different subset, checked when the call runs. A type per call would move the check into the compiler at the price of more exported types. E3 narrows the table by itself.
- **Static file serving in a package.** It is a route registration (ADR-017), and the one call that registers both of its routes is what A4 pins.
- **Kept as they are:** `Infra`; automatic configuration loading, `GetConfig` and `MustGetConfig`; `IsRunning`; route names, `BuildURI` and `GetRoute`; the worker readiness conditions; `RecoverConfig`; `IsDebug` and `HasRequestLogger`; `NewProblemDetails`; `Context.SetLogger`.

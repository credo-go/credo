# ADR-019: WebSocket Integration and Drain

**Status:** Accepted; v0.24.0 decisions accepted, pending implementation ([plan](../plans/components-and-sequential-bootstrap.md)) **Date:** 2026-07-12 **Depends on:** ADR-006, ADR-008, ADR-009, ADR-010, ADR-024

## Context

WebSocket support crosses routing, middleware, HTTP upgrade semantics, pooled
request contexts, observability, and graceful shutdown. Implementing only an
upgrade helper would leave applications to rebuild the security and lifecycle
parts independently. Copying a protocol engine into the framework would also
make Credo responsible for a fast-moving wire protocol and its security fixes.

Credo therefore needs an integrated server API without owning RFC 6455 frame
parsing. It must preserve the normal `func(*credo.Context) error` route chain,
fail visibly before an upgrade, and guarantee that successful application
drain does not tear down DI resources while WebSocket handlers still use them.

## Decision

### Wrap and pin the protocol engine

Credo imports exact-pinned `github.com/coder/websocket v1.8.15` under its ISC
license. The upstream library owns the wire protocol, framing, masking, close
handshake, ping/pong, and per-message deflate. Package `websocket` owns the
public types, secure defaults, error normalization, logging, admission, and App
lifecycle integration.

The upstream connection is reachable only through `Conn.Unwrap` as an expert
escape hatch. It remains borrowed: raw calls do not transfer ownership and
bypass Credo's validation, normalized errors, close policy, and logging.

### Keep routing explicit

The canonical API is:

```go
ws := websocket.Use(app, cfg)
app.GET("/events", ws.Handler(handler))
```

`Use` validates configuration and registers lifecycle hooks. `Server.Handler`
adapts a synchronous WebSocket handler into the one Credo route-handler type,
so built-in, global, group, route, auth, rewrite, and other middleware retain
their existing ordering. A second `app.WebSocket` or `Server.Handle` route API
is not added before usage evidence shows that explicit `GET` is inadequate.

### Treat the handshake as a transaction

The adapter validates mechanical HTTP requirements, origin, subprotocol, and
drain admission before calling the upstream `Accept`. A conditional response
writer captures upstream pre-upgrade status, headers, and body. Before 101,
failures return through Credo's centralized error renderer; only protocol-relevant
headers are published. Once 101 is committed, an actual Hijack failure is a
transport failure: the adapter logs it and must not attempt a second HTTP
status or body.

The effective precedence is:

1. HTTP/1.1 upgrade mechanics;
2. Credo-owned browser origin authorization;
3. Credo-owned subprotocol selection;
4. admission token while the server is open;
5. real Hijacker capability;
6. upstream Accept and actual Hijack.

Auto-generated HEAD routes reach the adapter and fail with 405 plus
`Allow: GET`. HTTP/2 extended CONNECT (RFC 8441) is not implemented. A response
writer without a real Hijacker fails before I/O with 501.

### Own origin and subprotocol policy

The zero configuration allows browser requests only when Origin is same-origin.
Additional origins are exact `http`/`https` origins or a single left-most DNS
label wildcard such as `https://*.example.com`. Origin absence is accepted for
non-browser clients. `InsecureSkipOriginCheck` is an explicit footgun and
cannot be combined with an allowlist. Origin authorization prevents browser
cross-site WebSocket abuse; it is not authentication.

Subprotocols are validated at startup and selected in server preference order.
`RequireSubprotocol` turns absence or mismatch into a pre-upgrade 400 rather
than silently continuing without a protocol.

### Borrow pooled request state only synchronously

`Handler` runs synchronously for the full connection lifetime. The
`*credo.Context` and `*websocket.Conn` must not be retained after it returns.
The connection context is derived with `context.WithoutCancel` so request
cancellation and deadlines do not terminate an accepted connection, while
request values such as authenticated user and logger fields remain available.

This preservation also means a transaction stored in the request context can
accidentally live for the entire connection. Long-lived handlers should
snapshot the small immutable values they need and open short repository or
transaction scopes per operation.

### Drain WebSockets before infrastructure

ADR-006's `App.OnDrain` is the canonical pre-infrastructure subsystem seam.
`websocket.Use` registers `Server.Shutdown` there and captures the lifecycle
context in `OnStart`. After readiness is withdrawn and every `OnPreDrain` hook
returns, the App cancels its lifecycle context, then drains HTTP servers and
all `OnDrain` hooks concurrently. WebSocket drain:

1. closes admission before Accept;
2. sends active peers 1001 Going Away;
3. cancels connection contexts and waits for every synchronous handler,
   connection record, and close task;
4. returns only when DI-dependent handler cleanup is finished.

The first `Server.Shutdown` caller owns the drain budget. Concurrent callers do
not replace it: they wait for the owner's result unless their own context ends
first. Calls made after the owner finishes return its stable result. If the
owner context is cancelled or its absolute deadline expires, the server reports
an incomplete error with remaining counts, cancels connections, and attempts
`CloseNow`; late handlers may finish afterward. The App continues DI and
`OnShutdown` with the same, possibly expired context. It never calls an
incomplete drain graceful success. A close task may also fail after all tracked
work settles; that is a closed, complete-with-error outcome rather than an
incomplete drain.

WebSocket is the first concrete `OnDrain` consumer. This does not introduce a
general restartable `Service` taxonomy: workers continue to use lifecycle
context plus DI `Shutdowner`; future gRPC/pubsub consumers may use `OnDrain`
without implying restartability.

### Construct with `New`, register as an ingress component

**Accepted, pending implementation (v0.24.0, W5).** When it ships, this section replaces the canonical API and the `Use` paragraph under "Keep routing explicit" and the whole "Drain WebSockets before infrastructure" section.

The canonical API becomes:

```go
ws := websocket.New(app.NewInfra("websocket"), cfg)
app.Manage(ws, credo.Ingress())
app.GET("/events", ws.Handler(handler))
```

`websocket.New(infra credo.Infra, cfg ...Config) *Server` validates the configuration, builds the server and registers nothing; misuse — an invalid configuration or more than one — panics. It takes `credo.Infra` instead of the App because today's `Use` took the App only for its logger, which `Infra` carries. The server adds the `module=websocket` attribute to the logger it is given, so log queries do not change.

**Why `New` and not `Use`.** `Use` names a call that attaches a subsystem to the App, and this one no longer does: with the start and drain hooks gone, the server attaches only when the application registers it. Keeping the name would promise an attachment the call no longer makes; renamed, a caller of today's `websocket.Use` gets a compile error instead of a server that silently skips its drain. `worker.Use` keeps its name, because the supervisor it returns registers each worker as a component of that App.

**The application registers the server as an ingress component** ([ADR-024](024-lifecycle-components.md)): the ingress tier is where work enters the process, and a WebSocket connection is such an entry. Either form works:

- `app.Manage(websocket.New(app.NewInfra("websocket"), cfg), credo.Ingress())` when only routes use the server;
- a binding with `credo.Ingress()` when controllers take the server as a dependency to mount their handlers — `ProvideValue`, or `Provide` with a constructor over `credo.Infra`. The binding is the one component; handing a server that `ProvideValue` binds to `Manage` as well panics, since DI already holds it.

**Drain.** The ingress tier stops first, concurrently with the HTTP drain and with the other ingress components that no dependency orders, and before any internal component — so every WebSocket handler, which may use repositories and clients, has returned before those stop. The server's `Shutdown` is today's drain: it closes admission before Accept, sends active peers 1001 Going Away, cancels connection contexts, and returns only when every synchronous handler, connection record and close task has finished. The first caller still owns the drain budget, and concurrent callers still wait for its result. An incomplete drain is reported in the App's one shutdown error, and what the App does with the components the server depends on follows ADR-024's deadline rule.

**Start.** The server's `Start` derives the server's own context from `context.WithoutCancel(ctx)` with its own cancel, which `Shutdown` calls; the context `Start` receives ends when `Start` returns, as every component's does. A server that was never started refuses every upgrade before 101 with an error that names the missing registration, so a forgotten `Manage` or `credo.Ingress()` binding fails at the first upgrade instead of silently skipping the drain.

**An internal component that depends on the server fails `Finalize`.** No dependency crosses the tiers backwards ([ADR-024](024-lifecycle-components.md)). The case that seems to need one does not: the server exports only `Handler`, `Start` and `Shutdown`, so an in-process consumer that broadcasts depends on the application's own connection registry — an internal component that the server's handlers fill and its drain empties — and what it sends after the ingress drain finds no peers. A controller that is not a component creates no edge to order, so injecting the server into controllers is unaffected.

**Served through `ServeHTTP`.** An App served by an external `http.Server` or by `httptest` is started with `App.Start`; its owner drains that server before calling `App.Shutdown`, which then stops the WebSocket server in the ingress tier — hijacked connections are not tracked by `http.Server.Shutdown`, so they are the server component's to drain.

WebSocket thus leaves the hook-based lifecycle entirely, and with it the role of first `OnDrain` consumer; the restartable `Service` taxonomy stays out of scope, as ADR-024 records.

### Keep defaults bounded and observable

- Per-message read limit: 32 KiB.
- Compression: disabled.
- Subprotocol: optional.
- Normal application return: close 1000.
- App drain: close 1001.
- Application error, transport error, or panic: close 1011 with a generic wire
  reason; arbitrary errors and panic values are not logged.

The normal HTTP access log remains the single Info-level request record with
status 101 and zero response bytes. Connection lifecycle records use structured
Debug/Warn/Error classifications and safe identifiers; peer reason, message
payload, cookies, authorization, query values, and raw Origin are excluded.

## Rejected Alternatives

| Alternative | Reason rejected |
| --- | --- |
| Copy/fork coder/websocket | Expands Credo's security and protocol ownership without a demonstrated upstream seam failure. |
| Put `Upgrade` on `credo.Context` | Couples the root package to a feature dependency and hides policy/lifecycle choices behind a transport helper. |
| Add `app.WebSocket` / `Server.Handle` now | Creates a second route-registration API before real usage proves the canonical `GET(..., Handler(...))` form insufficient. |
| Retain `*credo.Context` asynchronously | The context is pooled and may be reset for another request; retaining it is a correctness and race bug. |
| Use request cancellation for connection lifetime | `net/http` request cancellation after Hijack is not an application WebSocket lifetime contract. |
| Trust arbitrary `Origin` or query token by default | Enables browser cross-site abuse or credential leakage through proxy/access logs. |
| Tear down through DI only | Reverse registration order cannot prove that every active handler stopped before its repositories and clients. |
| Public hub/room/client/heartbeat/quota abstractions | These require concrete application demand and, for distributed fan-out, pubsub semantics not present in the MVP. |
| RFC 8441 support in the first release | Classic HTTP/1.1 Hijack and HTTP/2 extended CONNECT have different server and middleware mechanics. |

## Consequences

Applications get one Credo-shaped server API with secure defaults, normal
middleware behavior, visible pre-upgrade failures, and DI-safe graceful drain.
The package deliberately remains server-side and low-level: applications own
authentication, authorization, heartbeat policy, message schemas, and fan-out.

The adapter depends on specific public and handshake behavior of the pinned
upstream version. Every dependency update must run the permanent upstream,
origin, handshake, real-network, and lifecycle conformance suites documented in
`SECURITY-UPSTREAMS.md`.

## References

- [coder/websocket v1.8.15](https://github.com/coder/websocket/tree/v1.8.15)
- [RFC 6455](https://www.rfc-editor.org/rfc/rfc6455.html)
- [RFC 8441](https://www.rfc-editor.org/rfc/rfc8441.html)
- [WHATWG WebSockets](https://websockets.spec.whatwg.org/)
- [ADR-006: Application Lifecycle](006-application-lifecycle.md)
- [ADR-008: Context Design](008-context-design.md)
- [ADR-009: Handler and Error Handling](009-handler-and-error-handling.md)
- [ADR-010: Middleware Architecture](010-middleware-architecture.md)

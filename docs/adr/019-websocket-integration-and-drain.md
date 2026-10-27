# ADR-019: WebSocket Integration and Drain

**Status:** Accepted **Date:** 2026-07-12 **Depends on:** ADR-006, ADR-008, ADR-009, ADR-010, ADR-024 **Related:** ADR-023 **Specification:** [WebSocket spec](../specs/websocket.md) **Guide:** [WebSocket guide](../guides/websocket.md)

## Context

WebSocket support crosses routing, middleware, HTTP upgrade semantics, pooled request contexts, observability, and graceful shutdown. Implementing only an upgrade helper would leave applications to rebuild the security and lifecycle parts independently. Copying a protocol engine into the framework would also make Credo responsible for a fast-moving wire protocol and its security fixes.

Credo therefore needs an integrated server API without owning RFC 6455 frame parsing. It must preserve the normal `func(*credo.Context) error` route chain, fail visibly before an upgrade, and guarantee that successful application drain does not tear down DI resources while WebSocket handlers still use them.

## Decision

### Wrap and pin the protocol engine

Credo imports exact-pinned `github.com/coder/websocket v1.8.15` under its ISC license. The upstream library owns the wire protocol, framing, masking, close handshake, ping/pong, and per-message deflate. Package `websocket` owns the public types, secure defaults, error normalization, logging, admission, and App lifecycle integration.

The upstream connection is reachable only through `Conn.Unwrap` as an expert escape hatch. It remains borrowed: raw calls do not transfer ownership and bypass Credo's validation, normalized errors, close policy, and logging.

### Keep routing explicit

The canonical API is:

```go
ws := websocket.New(app.NewInfra("websocket"), cfg)
app.Manage(ws, credo.Ingress())
app.GET("/events", ws.Handler(handler))
```

`New` validates the configuration and builds the server; the application registers it as an ingress component (below). `Server.Handler` adapts a synchronous WebSocket handler into the one Credo route-handler type, so built-in, global, group, route, auth, rewrite, and other middleware retain their existing ordering.

### Treat the handshake as a transaction

The adapter validates mechanical HTTP requirements, origin, subprotocol, and drain admission before calling the upstream `Accept`, so every Credo-owned check precedes the upstream handshake. A conditional response writer captures upstream pre-upgrade status, headers, and body. Before 101, failures return through Credo's centralized error renderer; only protocol-relevant headers are published. Once 101 is committed, an actual Hijack failure is a transport failure: the adapter logs it and must not attempt a second HTTP status or body. The [spec](../specs/websocket.md#handshake-failure-contract) holds the check order and the status of each failure.

### Own origin and subprotocol policy

The zero configuration allows browser requests only when Origin is same-origin; further origins are allowlisted explicitly ([origin grammar](../specs/websocket.md#origin-authorization)). Origin absence is accepted for non-browser clients. `InsecureSkipOriginCheck` is an explicit footgun and cannot be combined with an allowlist. Origin authorization prevents browser cross-site WebSocket abuse; it is not authentication.

Subprotocols are validated at startup and selected in server preference order. A client offer that shares no protocol with the server's list fails pre-upgrade with 400; `RequireSubprotocol` also turns an absent offer into 400 rather than continuing without a protocol.

### Borrow pooled request state only synchronously

`Handler` runs synchronously for the full connection lifetime. The `*credo.Context` and `*websocket.Conn` must not be retained after it returns. The connection context is derived with `context.WithoutCancel` so request cancellation and deadlines do not terminate an accepted connection, while request values such as authenticated user and logger fields remain available.

This preservation also means a transaction stored in the request context can accidentally live for the entire connection. Long-lived handlers should snapshot the small immutable values they need and open short repository or transaction scopes per operation.

### Construct with `New`, register as an ingress component

`New` builds and validates the server without I/O and registers nothing; the [spec](../specs/websocket.md#canonical-registration) holds the signature and the misuse panics.

**Why `New`.** Under the API verb convention `Use` attaches a subsystem to the App; `New` attaches nothing, and the server attaches only when the application registers it, so the name promises no attachment the call does not make. It takes `credo.Infra` rather than the App because the server needs only a logger, which `Infra` carries; taking the App would imply the attachment that `New` leaves to the registration. `worker.Use` keeps the verb because the supervisor it returns registers each worker as a component of that App ([ADR-023](023-worker-system.md)).

**The application registers the server as an ingress component** ([ADR-024](024-lifecycle-components.md)): the ingress tier is where work enters the process, and a WebSocket connection is such an entry. It hands the server to `app.Manage` with `credo.Ingress()` when only routes use it, or binds it with `credo.Ingress()` when controllers take it as a dependency.

**Start.** The server's `Start` opens admission. A server that was never started refuses every upgrade with a 500 — a missing registration is a developer error, not an availability outcome — so a forgotten registration fails at the first upgrade instead of silently skipping the drain. The [spec](../specs/websocket.md#lifecycle) gives the state rules and the status of each refusal.

### Drain WebSockets before infrastructure

Hijacked connections are not tracked by `http.Server.Shutdown`, so nothing ends them unless the server's own drain does, and their handlers may use repositories and clients. The server is therefore an ingress component: the ingress tier stops concurrently with the HTTP drain and before any internal component, so every WebSocket handler has returned before the infrastructure it uses stops. Running beside the HTTP drain rather than after it lets the two drains spend the shared deadline concurrently, and peers receive 1001 Going Away — the RFC 6455 code for a server going down — while the HTTP drain runs rather than after it, so a client can tell a shutdown from a failure (1011). The server's `Shutdown` closes admission, sends 1001, cancels connection contexts and returns only when every handler and its cleanup have finished ([drain mechanics](../specs/websocket.md#lifecycle)).

The first `Shutdown` caller owns the drain budget; an incomplete drain is reported with remaining counts in the App's `*credo.LifecycleError` and is never treated as graceful success. The [spec](../specs/websocket.md#lifecycle) covers concurrent callers, stable results and the complete-with-error outcome; the deadline rule, under which no teardown starts once the deadline has passed, is the [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers)'s.

**An internal component that depends on the server fails `Finalize`.** No dependency crosses the tiers backwards ([ADR-024](024-lifecycle-components.md)). The case that seems to need one does not: the server exports only `Handler`, `Start` and `Shutdown`, so an in-process consumer that broadcasts depends on the application's own connection registry — an internal component that the server's handlers fill and its drain empties — and what it sends after the ingress drain finds no peers. A controller that is not a component creates no edge to order, so injecting the server into controllers is unaffected.

An App served through `ServeHTTP` follows ADR-024's owner rule: its owner drains the external server, and the [spec](../specs/websocket.md#lifecycle) gives the sequence. The restartable `Service` taxonomy stays out of scope, as ADR-024 records.

### Keep defaults bounded and observable

Defaults are bounded: a per-message read limit with no unlimited setting, compression disabled, and an optional subprotocol ([configuration](../specs/websocket.md#configuration)). Close codes report the outcome to the peer without leaking it: application errors, transport errors and panics close with 1011 and a generic wire reason ([close outcomes](../specs/websocket.md#errors-and-close-outcomes)).

The normal HTTP access log remains the single Info-level request record with status 101 and zero response bytes. Connection lifecycle records use structured Debug/Warn/Error classifications and safe identifiers; peer reason, message payload, cookies, authorization, query values, raw Origin, and arbitrary error and panic values are excluded ([observability](../specs/websocket.md#observability)).

## Rejected Alternatives

| Alternative | Reason rejected |
| --- | --- |
| Copy/fork coder/websocket | Expands Credo's security and protocol ownership without a demonstrated upstream seam failure. |
| Put `Upgrade` on `credo.Context` | Couples the root package to a feature dependency and hides policy/lifecycle choices behind a transport helper. |
| Add `app.WebSocket` / `Server.Handle` now | Creates a second route-registration API before real usage proves the canonical `GET(..., Handler(...))` form insufficient. |
| Retain `*credo.Context` asynchronously | The context is pooled and may be reset for another request; retaining it is a correctness and race bug. |
| Use request cancellation for connection lifetime | `net/http` request cancellation after Hijack is not an application WebSocket lifetime contract. |
| Trust arbitrary `Origin` or query token by default | Enables browser cross-site abuse or credential leakage through proxy/access logs. |
| Tear down as an internal component | It would drain only after the HTTP drain, and dependency order sees no edge from the server to the repositories its handlers use, so it cannot prove every handler returned before they stop. |
| Public hub/room/client/heartbeat/quota abstractions | These require concrete application demand and, for distributed fan-out, pubsub semantics that Credo does not provide. |
| RFC 8441 (HTTP/2 extended CONNECT) | Classic HTTP/1.1 Hijack and HTTP/2 extended CONNECT have different server and middleware mechanics. |

## Consequences

Applications get one Credo-shaped server API with secure defaults, normal middleware behavior, visible pre-upgrade failures, and DI-safe graceful drain. The package deliberately remains server-side and low-level: applications own authentication, authorization, heartbeat policy, message schemas, and fan-out.

The adapter depends on specific public and handshake behavior of the pinned upstream version, so every update of the pin runs the conformance suites that [SECURITY-UPSTREAMS.md](../../SECURITY-UPSTREAMS.md) lists.

## References

- [coder/websocket v1.8.15](https://github.com/coder/websocket/tree/v1.8.15)
- [RFC 6455](https://www.rfc-editor.org/rfc/rfc6455.html)
- [RFC 8441](https://www.rfc-editor.org/rfc/rfc8441.html)
- [WHATWG WebSockets](https://websockets.spec.whatwg.org/)
- [ADR-006: Application Lifecycle](006-application-lifecycle.md)
- [ADR-008: Context Design](008-context-design.md)
- [ADR-009: Handler and Error Handling](009-handler-and-error-handling.md)
- [ADR-010: Middleware Architecture](010-middleware-architecture.md)
- [ADR-023: Worker System](023-worker-system.md)
- [ADR-024: Lifecycle Components](024-lifecycle-components.md)

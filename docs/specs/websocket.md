# WebSocket Spec

**Status:** Implemented (beta) **Package:** `websocket/` **ADR:** [ADR-019](../adr/019-websocket-integration-and-drain.md) **Guide:** [WebSocket guide](../guides/websocket.md) **Sources:** wraps coder/websocket v1.8.15 (ISC)

## Scope

Package `github.com/credo-go/credo/websocket` is Credo's server-side adapter over exact-pinned `github.com/coder/websocket v1.8.15`. It provides route integration, security policy, Credo-owned message/close types, normalized errors, structured lifecycle logging, and managed or explicit graceful drain. The App's drain sequence is the [lifecycle spec](lifecycle.md#shutdown-in-tiers)'s; this spec states where the server sits in it.

It does not provide a client, hub/room registry, broadcast, reconnect, heartbeat scheduler, distributed fan-out, message codec, SSE, or HTTP/2 extended CONNECT.

## Canonical Registration

```go
ws := websocket.New(app.NewInfra("websocket"), websocket.Config{
    AllowedOrigins:     []string{"https://app.example.com"},
    Subprotocols:       []string{"events.v1"},
    RequireSubprotocol: true,
})
app.Manage(ws, credo.Ingress())

app.GET("/events", ws.Handler(func(req *credo.Context, conn *websocket.Conn) error {
    typ, payload, err := conn.Read(conn.Context())
    if err != nil {
        return err
    }
    return conn.Write(conn.Context(), typ, payload)
}))
```

`New` accepts zero or one `Config`, validates it, copies its slices defensively — caller mutation does not alter the server policy — and returns a `*Server`; it registers nothing and performs no I/O. An invalid configuration panics (`credo/websocket: invalid Config: …`), and so does more than one `Config` (`credo/websocket: New accepts at most one Config`), as startup misuse.

The application registers the server as an ingress [component](lifecycle.md#components-and-the-start-phase), by value as above, or as a binding when controllers depend on it:

```go
app.Provide[*websocket.Server](func(infra credo.Infra) *websocket.Server {
    return websocket.New(infra, cfg)
}, credo.Ingress())
```

A server bound with `ProvideValue` or `Provide` and `credo.Ingress()` is one component; also handing a `ProvideValue`-bound server to `Manage` panics. Without `credo.Ingress()` the server would take the internal tier and drain after the HTTP drain; the option is what places it beside the listener. `Manage` and the binding follow the App's [registration guards](lifecycle.md#registration-guards). The rationale for construction with `New` and registration as a component is in [ADR-019](../adr/019-websocket-integration-and-drain.md#construct-with-new-register-as-an-ingress-component).

`Server.Handler(nil)` also panics as registration misuse. `Server.Shutdown(nil)` returns an error without starting the drain. Runtime handshake, network, application, cancellation, and deadline failures return errors or protocol close outcomes; they are not panics.

## Public API

```go
type Handler func(req *credo.Context, conn *Conn) error

func New(infra credo.Infra, cfg ...Config) *Server
func (s *Server) Handler(h Handler) credo.Handler
func (s *Server) Start(ctx context.Context) error
func (s *Server) Shutdown(ctx context.Context) error

func (c *Conn) Context() context.Context
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error)
func (c *Conn) Write(ctx context.Context, typ MessageType, data []byte) error
func (c *Conn) Ping(ctx context.Context) error
func (c *Conn) CloseRead(ctx context.Context) context.Context
func (c *Conn) Close(code StatusCode, reason string) error
func (c *Conn) Subprotocol() string
func (c *Conn) Unwrap() *coderwebsocket.Conn

func CloseStatus(err error) StatusCode
```

`MessageType` exports `MessageText` and `MessageBinary`.

`CompressionMode` exports `CompressionDisabled`, `CompressionNoContextTakeover`, and `CompressionContextTakeover`.

`StatusCode` exports the standard 1000–1015 status names used by the adapter. A server may send the explicitly supported statuses, registered codes (3000–3999), and private-use codes (4000–4999). Synthetic/not-on-wire statuses 1005, 1006, and 1015, plus the client-only status 1010, are rejected before a close frame is written.

## Configuration

| Field | Zero/default | Contract |
| --- | --- | --- |
| `AllowedOrigins []string` | same-origin only | Adds exact origins or one-left-label wildcards such as `https://*.example.com`. Only `http`/`https`; no path, query, fragment, userinfo, Unicode host, empty port, or IP wildcard. |
| `Subprotocols []string` | none | Valid HTTP tokens in server preference order; duplicates are invalid. |
| `RequireSubprotocol bool` | `false` | When true, a client that offers no subprotocol also fails pre-upgrade with 400 ([negotiation](#subprotocol-negotiation)). Requires a non-empty server list. |
| `ReadLimit int64` | `32 << 10` | Maximum bytes in one message. Positive values override; negative is invalid. There is no public unlimited setting. |
| `CompressionMode` | disabled | Controls RFC 7692 negotiation independently of HTTP response compression middleware. |
| `CompressionThreshold int` | mode default | Disabled ignores non-negative values. No-context default is 512 bytes; context-takeover default is 128 bytes; positive overrides; negative is invalid. |
| `InsecureSkipOriginCheck bool` | `false` | Explicitly disables browser Origin authorization. Cannot be combined with `AllowedOrigins`. |

## Origin Authorization

Origin is canonicalized as `(lowercase scheme, lowercase ASCII hostname, effective port)`. Default ports 80/443 are equivalent to omission. IPv4 and IPv6 literals are accepted only for exact origins. A wildcard matches exactly one label: `https://*.example.com` matches `https://app.example.com`, not `https://example.com` or `https://a.b.example.com`.

Browser requests with Origin must be same-origin or allowlisted. Requests with more than one Origin, `null`, malformed values, or unauthorized origins receive 403 before admission. Requests without Origin are accepted for non-browser clients. Origin policy is not user authentication or tenant authorization.

## Subprotocol Negotiation

The client offer is parsed as HTTP tokens and matched case-sensitively. Credo selects the first configured server protocol present in the client set; client order does not override server preference. The selected value is the only value passed to the protocol engine.

An invalid client token, or a client offer that shares no token with the server list, fails before 101 with 400 whether or not `RequireSubprotocol` is set; this includes any offer to a server that lists no subprotocols. An absent offer is accepted without a subprotocol unless `RequireSubprotocol` is set, in which case it fails with 400 as well.

## Routing and Middleware

WebSocket routes use normal `app.GET`, group, and route APIs. Execution order is framework features → Global → Group → Route → WebSocket Handler. Auth middleware must run before `Server.Handler`. Global rewrite and trailing-slash behavior remain router concerns and execute before the upgrade.

The GET route's auto-generated HEAD twin executes the same middleware but the adapter rejects HEAD with 405 and `Allow: GET`; it never upgrades. Classic HTTP/1.1 upgrade is supported. HTTP/2 requests never produce 101 and RFC 8441 extended CONNECT is outside this contract.

The final response writer chain must expose a real `http.Hijacker`. Credo resolves nested `Unwrap` chains with cycle/depth guards. A non-Hijacker or a wrapper that merely forwards a false capability receives 501 before upgrade. The `app.UseCompress` writer preserves Hijacker and is supported. Third-party timeout, buffering, recorder, or response-replacement middleware may not.

## Handshake Failure Contract

Validation occurs in this order:

1. HTTP protocol and upgrade headers;
2. method, version 13, and WebSocket key;
3. Origin authorization;
4. subprotocol selection;
5. server admission state (started, not draining);
6. Hijacker capability;
7. upstream Accept and actual Hijack.

| Failure | HTTP result before 101 |
| --- | --- |
| Missing/invalid upgrade | 426; protocol `Connection`/`Upgrade` headers where applicable |
| HEAD or another method | 405 with `Allow: GET` |
| Bad version/key | 400; bad version includes `Sec-WebSocket-Version: 13` |
| Unauthorized Origin | 403 |
| Invalid client subprotocol token, no common subprotocol, or missing required subprotocol | 400 |
| Server never started | 500 through the centralized error pipeline (`internal_server_error` in the [default error envelope](../adr/009-handler-and-error-handling.md#default-error-envelope)); the message naming the missing registration is logged, not exposed |
| Server draining or shut down | 503 |
| No real Hijacker | 501 |

The status body is rendered by Credo's centralized error pipeline; raw upstream plain text is not exposed. If actual Hijack fails after 101 was committed, HTTP is not a usable error channel. The adapter records a structured transport failure and returns nil to the HTTP error renderer so no second status/body is attempted.

## Handler and Connection Lifetime

The application `Handler` runs synchronously until the connection ends. Both arguments are borrowed and must not be retained after return. `*credo.Context` is pooled and may be reused for another request after the handler exits.

`Conn.Context()` is the connection-lifetime context. It preserves request values but drops the HTTP request cancellation and deadline. It is cancelled when adapter cleanup finishes. WebSocket I/O should use it or a child deadline, not `req.Context()`.

Exactly one `Read` may be active. Concurrent `Write` calls are supported and serialized. `CloseRead` permanently owns the read side for write-only handlers, continues processing control frames, and closes unexpected data with 1008. Its returned context belongs to the upstream reader and may be cancelled only after the upstream bounded close guard; it is not `Conn.Context()`.

Every connection must have an active `Read` or `CloseRead`, otherwise pong and close processing can stall. A cancelled Read/Write/Ping context may terminate the underlying connection; application code should treat I/O deadlines as connection-fatal rather than retrying the same connection blindly.

`Unwrap` returns the borrowed upstream connection. Raw operations bypass Credo mapping, logging, and policy; the raw connection must not escape the handler or be closed/managed independently from the adapter.

## Errors and Close Outcomes

`CloseError` contains a `Code` and untrusted peer `Reason`. It deliberately does not unwrap the upstream concrete error. `CloseStatus` traverses wrapped and joined Credo close errors and returns `StatusCode(-1)` for non-close errors.

| Handler outcome | Adapter action |
| --- | --- |
| `nil` | Send 1000 Normal Closure. |
| Peer `CloseError` | Preserve it as the connection terminal cause; do not replace the peer close. |
| Non-close Read/Write/Ping transport error | Send 1011 with generic `internal error`; preserve cause through private wrapping. |
| Application error | Send 1011 with generic `internal error`. |
| Panic | Recover inside the adapter and send 1011 with generic `internal error`. |
| App drain | Send 1001 Going Away, cancel the connection, and wait for cleanup. |

`Conn.Close` validates sendable codes, UTF-8, and the RFC control-frame reason limit of 123 bytes before I/O. After a successful 101, application errors do not flow into the HTTP renderer.

## Lifecycle

**Start.** The start walk calls `Start` in the ingress tier. `Start` opens admission; it does no I/O and starts no goroutine, so it keeps no context — connection contexts derive from the request context as described under [Handler and Connection Lifetime](#handler-and-connection-lifetime). A second `Start`, or `Start` after `Shutdown`, returns an error.

**Never started.** Until `Start` has run, the handler refuses every upgrade before 101 with a plain error that names the missing registration — `credo/websocket: the server was never started; register it as an ingress component with app.Manage(server, credo.Ingress()) or bind it with credo.Ingress()` — which the centralized error pipeline renders as a 500 and logs. The check is the admission step of the handshake order, so mechanical, origin and subprotocol failures still win. A forgotten registration therefore fails at the first upgrade instead of silently skipping the drain.

**Shutdown order.** The server is an ingress component: in `Run`, `RunContext`, `ServeContext` and `App.Shutdown` after `App.Start`, its `Shutdown` runs concurrently with the HTTP drain and before any internal stop hook or component ([drain in tiers](lifecycle.md#shutdown-in-tiers)).

WebSocket shutdown closes admission before new Accepts, sends 1001 to active peers, and waits for admission tokens, connection records, synchronous handlers, and tracked close tasks. The first caller owns the budget. Concurrent callers cannot replace it: they receive the owner's result when it finishes, or their own context error if their wait ends first. Calls made after the owner finishes receive its stable result.

If the owner context is cancelled or its deadline expires before cleanup finishes, `Server.Shutdown` returns an error that unwraps that context error and reports remaining handler/connection/close-task counts. It applies best-effort force close and remains draining until late work finishes; it does not report `closed` early. The App reports the incomplete drain in its `*credo.LifecycleError` and moves on to the internal tier under the same deadline; once it has passed, no internal stop hook or teardown starts and each one left is reported abandoned ([drain in tiers](lifecycle.md#shutdown-in-tiers)).

A non-nil result does not always mean incomplete. All tracked work may finish, the server may become `closed`, and a failed close task may still be returned as a complete-with-error result. Only a nil result means error-free graceful completion.

**Dependents.** An internal component that depends on the server, directly or through bindings that are not components, fails `Finalize` with the path and both remedies ([ADR-024](../adr/024-lifecycle-components.md)). Controllers that take the server are not components and add no edge. A consumer that broadcasts depends on the application's own connection registry instead ([ADR-019](../adr/019-websocket-integration-and-drain.md#drain-websockets-before-infrastructure)).

**External servers.** An App served through `ServeHTTP` by an external `http.Server` or `httptest` is started with `App.Start` before it serves ([the start gate](lifecycle.md#the-start-gate-on-servehttp)). At shutdown the owner drains its `http.Server` and calls the WebSocket server's `Shutdown` concurrently, as managed serving does, and then calls `App.Shutdown`. `http.Server.Shutdown` does not wait for hijacked connections, and an HTTP handler and a WebSocket handler may each wait for work the other owns, so draining one after the other can spend the whole deadline on the first. `App.Shutdown` still stops the WebSocket server in its ingress tier — returning the owner's stored result, or draining it when the owner did not — before the internal tier stops the resources its handlers use.

## Observability

The HTTP access log records one request-completed event for the upgraded request: status 101, zero response bytes, and full handler lifetime. Normal connection open/close events are Debug so they do not duplicate the access-log Info record. Policy/read-limit outcomes are Warn; application, transport, panic, close-failure, handshake-transport, and incomplete-shutdown outcomes are Error.

Connection records — opened, closed, failed, close failed, and a handshake transport failure — go to the request's logger with `connection_id` added. Only `websocket: shutdown incomplete` goes to the server's logger: `infra.Logger` (`slog.Default()` when nil) with `module=websocket` added.

Safe fields include the request ID the request's logger carries, generated connection ID, route, negotiated subprotocol, classification, close code, lifetime, incomplete counts, the Go type of the error or panic (`error_type`, `panic_type`) and, for a panic, its stack. Payloads, authorization, cookies, URL query, raw Origin, peer close reason, and arbitrary error/panic values are not logged.

## Dependency Update Gate

The pin's monitoring and update procedure are in [SECURITY-UPSTREAMS.md](../../SECURITY-UPSTREAMS.md). Every bump runs the compile surface, upstream handshake precedence, origin/subprotocol, real TCP/WSS/HTTP2-negative, compression/read-limit, error/logging, and lifecycle/race suites.

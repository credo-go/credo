# websocket

Package `websocket` is Credo's server-side adapter over the exact-pinned
`coder/websocket` protocol engine. Credo owns the public message, close,
compression, origin, subprotocol, read-limit, and lifecycle policy while the
upstream library owns wire-protocol implementation.

Status: **Beta / implemented**. See the canonical
[spec](../docs/specs/websocket.md), [operations guide](../docs/guides/websocket.md),
and [ADR-019](../docs/adr/019-websocket-integration-and-drain.md).

The default policy is browser same-origin, optional subprotocol negotiation,
disabled compression, and a 32 KiB per-message read limit. Origin checks are a
browser-CSRF boundary, not authentication.

Minimal managed usage:

```go
ws := websocket.Use(app, websocket.Config{
    AllowedOrigins: []string{"https://app.example.com"},
    Subprotocols:   []string{"events.v1"},
})

app.GET("/events", ws.Handler(func(req *credo.Context, conn *websocket.Conn) error {
    typ, data, err := conn.Read(conn.Context())
    if err != nil {
        return err
    }
    return conn.Write(conn.Context(), typ, data)
}))
```

`Use` integrates connection drain with the App lifecycle. When the App is used
only as an `http.Handler`, the caller owns shutdown and must coordinate
`Server.Shutdown` with its `http.Server.Shutdown` before tearing down shared
infrastructure. The guide includes both managed and external-server examples,
successful/incomplete outcomes, and shutdown-budget sizing.

## Operational boundaries

- Origin authorization is a browser-CSRF boundary, not authentication.
  Browsers cannot attach arbitrary authorization headers to the WebSocket
  constructor; prefer secure cookies or a short-lived, single-use ticket.
  Query-string tokens can appear in proxy logs and should be avoided.
- Use `conn.Context()` for connection work. Values copied from the request
  remain available, so a request-scoped database transaction can accidentally
  stay open for the full connection lifetime; do not put such middleware on a
  WebSocket route by default. Snapshot immutable user/service values at handler
  entry and use short repository scopes.
- Every connection needs an active `Read` or `CloseRead` so ping, pong, and
  close frames are processed. `CloseRead` rejects unexpected application data
  with 1008. Credo does not provide an automatic heartbeat; applications should
  size Ping deadlines against their proxy idle timeout.
- HTTP timeout/buffering middleware may remove Hijacker support and produces a
  pre-upgrade 501. HTTP compression middleware is supported, while WebSocket
  frame compression is controlled separately by `CompressionMode`.
- Raw stdlib middleware that hijacks outside Credo tracking, RFC 8441/HTTP/2
  WebSockets, hubs/rooms, outbound clients, reconnect, and distributed fan-out
  are outside the MVP contract.
- `Conn.Unwrap` is a borrowed expert escape hatch. Raw calls bypass Credo's
  validation, error normalization, logging, and close policy; the raw
  connection must not outlive the synchronous Handler.

# websocket

Package `websocket` is Credo's server-side adapter over the exact-pinned
`coder/websocket` protocol engine. Credo owns the public message, close,
compression, origin, subprotocol, read-limit, and lifecycle policy while the
upstream library owns wire-protocol implementation.

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
infrastructure.

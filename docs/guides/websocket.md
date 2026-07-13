# WebSocket Guide

Credo's WebSocket package adapts `coder/websocket` to the normal router,
middleware, logging, and graceful-shutdown model. The application owns message
semantics, authentication, authorization, and heartbeat policy.

See the [spec](../specs/websocket.md) for the exact contract and
[ADR-019](../adr/019-websocket-integration-and-drain.md) for the architecture.

## Echo Endpoint

```go
package main

import (
    "log"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/websocket"
)

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    ws := websocket.Use(app, websocket.Config{
        AllowedOrigins:     []string{"https://app.example.com"},
        Subprotocols:       []string{"echo.v1"},
        RequireSubprotocol: true,
        ReadLimit:          64 << 10,
    })

    app.GET("/ws/echo", ws.Handler(func(_ *credo.Context, conn *websocket.Conn) error {
        for {
            typ, payload, err := conn.Read(conn.Context())
            if err != nil {
                return err
            }
            if err := conn.Write(conn.Context(), typ, payload); err != nil {
                return err
            }
        }
    })).Name("websocket.echo")

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

`echo.v1` is an application-defined subprotocol identifier, not a Credo
keyword. Choose a stable identifier only when client and server need to agree
on a message contract. Omit `Subprotocols` if no negotiation is needed.

## Authentication and Browser Clients

Origin checking prevents a malicious page from opening a credentialed
cross-site WebSocket. It does not identify the user. Apply ordinary Credo auth
middleware before `Server.Handler`:

```go
app.GET("/events", ws.Handler(eventsHandler)).Middleware(
    auth.Middleware[User](sessionAuthenticator, nil),
)
```

The browser `WebSocket` constructor cannot attach arbitrary Authorization
headers. Common choices are:

- a Secure, HttpOnly, SameSite cookie authenticated by route middleware;
- a short-lived, audience-bound, single-use ticket acquired over normal HTTPS;
- an application protocol message that authenticates immediately after
  upgrade, with a strict unauthenticated timeout and message limit.

Avoid long-lived credentials in the URL query. URLs routinely reach proxy,
load-balancer, browser-history, and access logs. If a ticket must use the query,
make it short-lived and single-use and configure every intermediary to redact
it.

Configure `AllowedOrigins` for every trusted browser deployment origin:

```go
ws := websocket.Use(app, websocket.Config{
    AllowedOrigins: []string{
        "https://app.example.com",
        "https://*.tenant.example.com", // exactly one wildcard label
    },
})
```

Do not enable `InsecureSkipOriginCheck` merely to make a failed deployment
work. Diagnose scheme, host, port, TLS termination, and trusted proxy settings
first. A missing Origin is accepted for non-browser clients.

## Context and Data Access

Use `conn.Context()` for connection work. It is independent of request
cancellation but retains request values such as the authenticated user and
scoped logger.

Do not retain `*credo.Context` or `*websocket.Conn` after the handler returns.
Credo pools request contexts, and adapter cleanup owns the connection. Snapshot
small immutable values at handler entry:

```go
func eventsHandler(req *credo.Context, conn *websocket.Conn) error {
    user, err := req.RequireUser[User]()
    if err != nil {
        return err
    }
    userID := user.ID
    logger := req.Logger().With("user_id", userID)

    return serveEvents(conn.Context(), conn, logger, userID)
}
```

`context.WithoutCancel` preserves all request values. Consequently, a database
transaction installed by request middleware is also preserved and could stay
open for hours. Do not put request-wide transaction middleware on WebSocket
routes. Open short repository/transaction scopes for each command or message.

## Reading, Writing, and Heartbeats

Only one goroutine may call `Read`; concurrent `Write` calls are supported.
Every connection needs either an active `Read` loop or `CloseRead`, otherwise
ping, pong, and close control frames may not be processed.

For a write-oriented stream that rejects client data:

```go
func streamHandler(_ *credo.Context, conn *websocket.Conn) error {
    readDone := conn.CloseRead(conn.Context())
    ticker := time.NewTicker(20 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-readDone.Done():
            return nil
        case <-ticker.C:
            pingCtx, cancel := context.WithTimeout(conn.Context(), 5*time.Second)
            err := conn.Ping(pingCtx)
            cancel()
            if err != nil {
                return err
            }
        }
    }
}
```

`CloseRead` treats unexpected data as a policy violation (1008). Its returned
context can be delayed by the upstream bounded close guard, so it is a control
reader completion signal, not a precise connection deadline.

Choose heartbeat intervals from the shortest proxy/load-balancer/NAT idle
timeout in the path. For example, with a 60-second proxy idle timeout, a
20-second ping and a 5-second pong deadline leaves recovery margin. A failed or
timed-out Read/Write/Ping should normally end the handler; operation-context
cancellation can close the underlying connection.

## Message Limits and Compression

The secure default read limit is 32 KiB per message. Set an explicit limit from
the largest legitimate application message plus modest protocol growth—not
from available server memory:

```go
ws := websocket.Use(app, websocket.Config{
    ReadLimit: 256 << 10,
})
```

Compression is disabled by default because it adds CPU/memory cost and can
amplify secret-compression side channels. Enable it only after measuring the
payload and threat model:

```go
ws := websocket.Use(app, websocket.Config{
    CompressionMode:      websocket.CompressionNoContextTakeover,
    CompressionThreshold: 1024,
})
```

No-context-takeover is the safer general default when compression is required.
Context takeover can improve ratios for repetitive streams but retains
compression state across messages. Credo's `middleware.Compress` concerns HTTP
responses and is separate from WebSocket frame compression.

## Middleware and Protocol Boundaries

Global, group, route, rewrite, authentication, authorization, request ID, and
access-log middleware work normally. Keep these boundaries in mind:

- Auto-generated HEAD is rejected with 405 and `Allow: GET`; only GET upgrades.
- HTTP/2 requests do not upgrade. RFC 8441 extended CONNECT is unsupported.
- The final response writer must expose a real `http.Hijacker`. Buffering,
  recorder, and some timeout middleware remove it and produce pre-upgrade 501.
- `middleware.Compress` forwards Hijacker correctly, but that does not enable
  WebSocket frame compression.
- After 101, HTTP status/body rendering is over. Handler failures become close
  frames and structured connection logs.

When a reverse proxy terminates TLS, forward the original scheme/host using a
trusted-proxy configuration so same-origin comparison sees the public origin.
Configure the proxy to support HTTP/1.1 Upgrade and set idle/read timeouts above
your heartbeat and drain budgets. Do not apply a short request timeout to the
long-lived route; Credo detaches the accepted connection from request
cancellation, but third-party middleware may still buffer or remove Hijacker.

## Managed Shutdown

`websocket.Use` integrates automatically with `app.Run`, `RunContext`, and
`ServeContext`. At shutdown, Credo marks readiness down, cancels the lifecycle
context, and drains HTTP plus WebSocket concurrently. WebSocket admission
closes, peers receive 1001 Going Away, and Credo waits for every synchronous
handler before DI resources are shut down.

Size `WithShutdownTimeout` for the whole shared absolute deadline:

```text
max(HTTP drain, slowest OnDrain/WebSocket drain)
+ DI cleanup
+ OnShutdown hooks
+ safety margin
```

If HTTP and WebSocket can take 20 seconds, DI takes 3 seconds, hooks take 2
seconds, and the deployment needs 5 seconds of margin, use at least 30 seconds.
An `OnDrain` hook that consumes the whole budget leaves DI and `OnShutdown` an
expired context. Explicit `app.Shutdown(ctx)` ignores `WithShutdownTimeout` and
uses the caller's deadline exactly.

A nil result is error-free graceful completion. A non-nil result has two
possible shapes: teardown may have completed with a close or hook error, or the
owner context may have ended while work was still pending. Only the latter is
an incomplete drain:

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := app.Shutdown(shutdownCtx); err != nil {
    if shutdownCtx.Err() != nil && errors.Is(err, shutdownCtx.Err()) {
        // The joined error identifies pending HTTP/OnDrain work; WebSocket
        // diagnostics include remaining handler, connection, and close-task
        // counts.
        logger.Error("shutdown incomplete", "error", err)
    } else {
        // All work settled, but one or more teardown operations failed.
        logger.Error("shutdown completed with errors", "error", err)
    }
}
```

On an incomplete drain, Credo makes a best-effort force close and continues
infrastructure teardown with the same deadline. It does not pretend late
handlers have stopped. Fix handlers that ignore cancellation; do not hide the
error with retries.

## External `http.Server`

Using `app` only as an `http.Handler` freezes routes but does not run Credo's
App lifecycle. The owner must drain HTTP and WebSocket in parallel, then close
application resources:

```go
httpServer := &http.Server{Addr: ":8080", Handler: app}

// ... serve httpServer and wait for the owner's shutdown signal ...

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

httpDone := make(chan error, 1)
wsDone := make(chan error, 1)
go func() { httpDone <- httpServer.Shutdown(ctx) }()
go func() { wsDone <- ws.Shutdown(ctx) }()

if err := errors.Join(<-httpDone, <-wsDone); err != nil {
    logger.Error("network drain failed", "error", err)
}

// Only now close repositories, clients, and other shared infrastructure.
```

Do not call the two drains sequentially: either side can wait for work owned by
the other and consume the entire deadline.

## Expert Escape Hatch

`conn.Unwrap()` exposes the borrowed `*coderwebsocket.Conn` for a capability not
yet represented by Credo. Raw calls bypass validation, normalized errors,
logging, and close policy. Do not retain the raw connection, start an
independently-owned lifecycle, or call it after the handler returns. If the
same raw operation appears repeatedly, propose a small Credo façade addition
instead of spreading `Unwrap` throughout application code.

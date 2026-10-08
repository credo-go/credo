// Package websocket provides Credo's server-side WebSocket adapter over the
// exact-pinned github.com/coder/websocket protocol engine.
//
// Create a Server with [New] and register it as an ingress component with
// app.Manage(server, credo.Ingress()) — or bind it with credo.Ingress() when
// controllers take it as a dependency — so the App starts it in the start
// phase and drains it beside the HTTP drain, before the internal components
// its handlers use. Then register [Server.Handler] through the normal Credo
// GET route API. Global, group, route, authentication, rewrite, and
// access-log middleware retain their normal ordering.
//
// The zero Config is secure and bounded: browser same-origin authorization,
// optional subprotocol negotiation, disabled compression, and a 32 KiB
// per-message read limit. Origin checks protect browser handshakes from
// cross-site abuse, but they are not authentication; applications must still
// authenticate and authorize connections.
//
// A Conn is borrowed for the synchronous lifetime of a Handler. Its Context is
// the connection-lifetime context and should be used for WebSocket I/O instead
// of the HTTP request context. Neither the pooled *credo.Context nor Conn may
// be retained after the Handler returns. Every connection needs an active Read
// or CloseRead so control frames are processed.
//
// Maturity: beta
package websocket

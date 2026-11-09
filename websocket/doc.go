// Package websocket provides Credo's server-side WebSocket adapter over the
// exact-pinned github.com/coder/websocket protocol engine.
//
// Create a Server with [New] and hand it to the App with app.Manage(server) —
// or bind it with app.ProvideValue(server) when controllers take it as a
// dependency. The server declares the ingress tier itself
// ([credo.IngressComponent]), so the App starts it in the start phase and
// drains it beside the HTTP drain, before the internal components its
// handlers use. Then register [Server.Handler] through the normal Credo
// GET route API. PreRouting, routed, group, route, authentication, rewrite,
// and access-log middleware retain their normal ordering.
//
// The zero Config is secure and bounded: browser same-origin authorization,
// optional subprotocol negotiation, disabled compression, and a 32 KiB
// per-message read limit. Origin checks protect browser handshakes from
// cross-site abuse, but they are not authentication; applications must still
// authenticate and authorize connections.
//
// A Conn is borrowed for the synchronous lifetime of a Handler. Its Context is
// the connection-lifetime context and should be used for WebSocket I/O instead
// of the HTTP request context. Neither the *credo.Context nor Conn may be used
// after the Handler returns: the response cannot be written then, and the
// adapter's cleanup owns the connection. Every connection needs an active Read
// or CloseRead so control frames are processed.
//
// Maturity: beta
package websocket

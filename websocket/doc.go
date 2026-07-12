// Package websocket provides Credo's server-side WebSocket adapter over the
// exact-pinned github.com/coder/websocket protocol engine.
//
// Create one Server with [Use], then register [Server.Handler] through the
// normal Credo GET route API. Global, group, route, authentication, rewrite,
// and access-log middleware retain their normal ordering. Use integrates with
// the App's pre-infrastructure drain; applications that use an App only as an
// http.Handler must coordinate [Server.Shutdown] themselves.
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
package websocket

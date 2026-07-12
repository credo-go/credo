// Package websocket provides Credo's server-side WebSocket adapter.
//
// The package wraps github.com/coder/websocket behind Credo-owned message,
// close, compression, configuration, and lifecycle boundaries. Origin checks
// protect browser handshakes by default, but they are not authentication;
// applications must still authenticate and authorize connections.
//
// A Conn is borrowed for the synchronous lifetime of a Handler. Its Context is
// the connection-lifetime context and should be used for WebSocket I/O instead
// of the HTTP request context.
package websocket

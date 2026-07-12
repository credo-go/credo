# websocket

Package `websocket` is Credo's server-side adapter over the exact-pinned
`coder/websocket` protocol engine. Credo owns the public message, close,
compression, origin, subprotocol, read-limit, and lifecycle policy while the
upstream library owns wire-protocol implementation.

The default policy is browser same-origin, optional subprotocol negotiation,
disabled compression, and a 32 KiB per-message read limit. Origin checks are a
browser-CSRF boundary, not authentication.

The route handler and managed connection lifecycle are being implemented in the
next plan phase; the package is not yet ready for application use.

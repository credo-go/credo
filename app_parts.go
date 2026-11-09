package credo

import (
	"crypto/tls"
	"net/http"
)

// App's fields are grouped by subsystem. Each group is a struct embedded in
// App by value, so its fields are promoted and read at a fixed offset, with no
// indirection added on the request path. Bootstrap writes every group; what
// the request executor reads, it reads after preparation has published the
// handler.

// appServer holds the HTTP server and its TLS: the server configuration, the
// TLS sources, the redirect listener's address, the WithHTTPServer callback
// and the reload-signal switch. New writes it from the options and the server
// section; the lifecycle reads it when it builds, serves and drains the server,
// and the file-based key pair is loaded at preflight.
type appServer struct {
	// serverCfg holds the server configuration (host, port, timeouts).
	serverCfg serverConfig

	// tlsConfig is the *tls.Config set via WithTLSConfig; it has the highest
	// TLS precedence. nil means TLS comes from server.tls.* / WithTLSFiles
	// (serverCfg.TLS) or, if those are empty too, the server serves plaintext.
	// Resolved and cloned at preflight (see resolveTLSConfig).
	tlsConfig *tls.Config

	// tlsConfigSet records that WithTLSConfig was called (even with nil). It lets
	// preflight reject an explicit WithTLSConfig(nil) as a fail-fast error rather
	// than silently falling through to the lower-precedence file sources.
	tlsConfigSet bool

	// tlsFilesSet records that WithTLSFiles was called (even with empty paths).
	// It lets preflight reject explicit empty paths as a fail-fast error rather
	// than silently falling through to the server.tls.* config keys or plaintext.
	tlsFilesSet bool

	// tlsFiles serves the file-based key pair (WithTLSFiles / server.tls.*)
	// through GetCertificate and rotates it when the files change on disk
	// (see tls_rotation.go). Inactive when TLS comes from WithTLSConfig or the
	// server is plaintext.
	tlsFiles fileCertSource

	// httpRedirectAddr, when non-empty, is the plaintext listen address for the
	// HTTP→HTTPS redirect listener (set via WithHTTPRedirect). Requires TLS;
	// applies to Run/RunContext only (not ServeContext).
	httpRedirectAddr string

	// disableReloadSignals makes Run ignore (and log) reload signals instead
	// of triggering App.Reload. Set via WithoutReloadSignals option.
	disableReloadSignals bool

	// configureServer is the WithHTTPServer callback, applied at the end of
	// buildServer. nil when the option was not used.
	configureServer func(*http.Server)
}

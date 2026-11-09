package credo

import (
	"crypto/tls"
	jsonv2 "encoding/json/v2"
	"net/http"
	"net/netip"
	"sync/atomic"
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

// appRouting holds the routes: the default mux and the host-scoped ones, the
// root group, the named routes, the mounts, the status handlers, the
// trailing-slash redirect switch and the pre-routing tier. Registration writes
// it during bootstrap; dispatch reads it once preparation has published the
// handler.
type appRouting struct {
	// mux is the underlying radix-tree router.
	mux *mux

	// root is the root route group: empty prefix, default mux, no host
	// scope. App's route-registration surface (HTTP verbs, Group, Static,
	// File, SetMeta) delegates to it so registration logic — path joining,
	// HEAD-twin wiring — lives in Group alone.
	root *Group

	// preRoutingMW holds the pre-routing tier: it runs on every request,
	// before the match, the router's own 404 and 405 included.
	preRoutingMW []Middleware

	// hosts holds registered host patterns for domain-based routing.
	// Empty when no host-specific routing is configured.
	hosts []*hostEntry

	// staticHosts maps exact host names to host entries for O(1) dispatch.
	// Param and regex host patterns remain in hosts and use specificity order.
	staticHosts map[string]*hostEntry

	// namedRoutes maps route names to Route pointers.
	namedRoutes map[string]*Route

	// mounts records mounted prefixes (App.Mount) for route introspection.
	// Appended only after a mount's radix registration succeeds.
	mounts []mountInfo

	// statusHandlers holds the app-level custom handlers set via
	// StatusHandler. Dispatch consults the entries for 404 and 405 only.
	statusHandlers map[int]Handler

	// redirectTrailingSlash controls automatic trailing-slash redirects.
	// Resolved once from config/option in New(); default true.
	redirectTrailingSlash bool
}

// appFeatures holds the built-in HTTP features: recovery, request ID, access
// log, decompression and compression, the observers, the error and success
// renderers and i18n. New and the Use methods write it during bootstrap; the
// request executor reads it once preparation has published the handler.
type appFeatures struct {
	// errorRenderer shapes error response bodies, set via UseErrorRenderer.
	// nil = default Credo JSON error envelope.
	errorRenderer ErrorRenderer

	// successRenderer formats successful responses sent via Context.Render,
	// set via UseSuccessRenderer. nil = default JSON (no envelope). Raw
	// Response helpers (JSON/XML/Text/Blob/...) never consult it.
	successRenderer SuccessRenderer

	// i18n is the installed internationalization feature: the loaded bundle,
	// the message-key resolver and the lazy locale detector. nil when i18n is
	// inactive.
	i18n *i18nFeature

	// i18nRegistered records that UseI18n completed (an inactive conventional
	// setup included) so a second call is rejected.
	i18nRegistered bool

	// recover is the panic recovery configuration; nil when WithoutRecover
	// disabled recovery. Resolved from options in New.
	recover *recoverFeature

	// requestID, accessLog, compress and decompress are the HTTP features
	// installed through the Use* methods; nil means the feature is off. They
	// are written during sequential bootstrap and read by the request
	// executor once preparation has published the handler.
	requestID  *requestIDFeature
	accessLog  *accessLogFeature
	compress   *compressFeature
	decompress *decompressFeature

	// observers run after each request's final response, in registration
	// order; the access log is among them when installed.
	observers []Observer
}

// appRequest holds the settings every request reads: debug mode, strict
// bodies, the multipart memory threshold, the JSON profiles and the trusted
// proxies. New writes them; the request path reads them once preparation has
// published the handler. foreignRequestReported is the one field a request
// writes.
type appRequest struct {
	// foreignRequestReported records that the debug report of a replaced
	// embedded request was made for a request no route matched.
	foreignRequestReported atomic.Bool

	// debug enables development-mode warnings.
	// Set via WithDebug option or server.debug config key.
	debug bool

	// strictBodies makes BindBody reject unknown JSON object members.
	// Set via WithStrictBodies option or server.strict_bodies config key.
	strictBodies bool

	// multipartMaxMemory overrides the in-memory threshold BindBody passes to
	// ParseMultipartForm; zero selects defaultMultipartMaxMemory. It has no
	// option: tests lower it (export_test.go) to make small uploads spill to
	// temporary files.
	multipartMaxMemory int64

	// jsonOpts is the response encoding profile: the framework default
	// (see defaultJSONOptions) with any WithJSONOptions overrides applied
	// after it. Read through App.jsonOptions, which is nil-safe.
	jsonOpts jsonv2.Options

	// errorJSONOpts is jsonOpts with deterministic ordering re-imposed, the
	// profile of every framework error body; joined once at construction.
	errorJSONOpts jsonv2.Options

	// trustedProxies contains parsed CIDR ranges allowed to influence forwarded headers.
	trustedProxies []netip.Prefix
}

package credo

import (
	"crypto/tls"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// serverConfig holds HTTP server settings.
type serverConfig struct {
	// Host is the listen address (default: "" = all interfaces).
	Host string `credo:"host"`

	// Port is the listen port (default: 0 = OS-assigned).
	Port int `credo:"port"`

	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body.
	ReadTimeout time.Duration `credo:"read_timeout"`

	// WriteTimeout is the maximum duration before timing out
	// writes of the response.
	WriteTimeout time.Duration `credo:"write_timeout"`

	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alives are enabled.
	IdleTimeout time.Duration `credo:"idle_timeout"`

	// ReadHeaderTimeout is the amount of time allowed to read
	// request headers.
	ReadHeaderTimeout time.Duration `credo:"read_header_timeout"`

	// ShutdownTimeout bounds graceful shutdown triggered by a signal
	// (Run) or by context cancellation (RunContext):
	// the drain has this long to finish before its deadline fires. Zero
	// (the default) applies a 30s budget. An explicit Shutdown(ctx) call
	// ignores this and honours the caller's context deadline instead.
	ShutdownTimeout time.Duration `credo:"shutdown_timeout"`

	// ReloadTimeout bounds a reload triggered by SIGHUP under Run: the
	// configuration re-read, subscribers, and OnReload hooks share this budget.
	// Zero (the default) applies 30s. A programmatic Reload(ctx) ignores this
	// and uses the caller's context instead.
	ReloadTimeout time.Duration `credo:"reload_timeout"`

	// MaxHeaderBytes controls the maximum number of bytes the
	// server will read parsing the request header's keys and values.
	MaxHeaderBytes int `credo:"max_header_bytes"`

	// MaxHeaderValueCount caps the number of header lines the server accepts
	// in a request. Zero (the default) applies net/http's own default of 500;
	// a positive value replaces it. Requests that exceed the limit receive
	// 431 Request Header Fields Too Large, written straight to the connection
	// by net/http — such rejections never reach the application logger.
	MaxHeaderValueCount int `credo:"max_header_value_count"`

	// MaxBodyBytes caps the number of bytes read from a request body
	// (via http.MaxBytesReader), mitigating memory-exhaustion DoS.
	// Zero (the default) applies a 4 MiB limit; a negative value disables it.
	MaxBodyBytes int64 `credo:"max_body_bytes"`

	// RedirectTrailingSlash controls automatic trailing-slash redirects.
	// When enabled, a 404 triggers a probe with the trailing slash toggled;
	// if the alternate path matches, the router issues a 301 (GET/HEAD) or
	// 308 (other methods) redirect. nil (default) = true.
	RedirectTrailingSlash *bool `credo:"redirect_trailing_slash"`

	// Debug enables development-mode warnings such as logging when
	// BindBody/BindQuery targets do not implement Validatable.
	Debug bool `credo:"debug"`

	// StrictBodies makes BindBody reject JSON object members that do not
	// map to a field of the target (400, reason unknown_field). Default
	// false: unknown members are ignored.
	StrictBodies bool `credo:"strict_bodies"`

	// TrustedProxies configures CIDR ranges whose forwarded headers are trusted.
	TrustedProxies []string `credo:"trusted_proxies"`

	// TLS holds the certificate and key file paths for serving HTTPS. When both
	// are set — via WithTLSFiles or the server.tls.cert_file / key_file keys —
	// Run and RunContext serve TLS. WithTLSFiles takes precedence over the config
	// keys; both are shadowed by WithTLSConfig.
	TLS serverTLS `credo:"tls"`
}

// serverTLS holds file-based TLS material, also populated from the
// server.tls.cert_file and server.tls.key_file config keys.
type serverTLS struct {
	CertFile string `credo:"cert_file"`
	KeyFile  string `credo:"key_file"`
}

// Option configures the App during construction.
type Option func(*appOptions)

// setting is a construction-time option value that remembers whether it was
// set explicitly. Server settings from With* options live in settings rather
// than in the decoded serverConfig, so a "server" config section decoded in
// New cannot silently overwrite an explicit programmatic value; overlayServer
// copies every explicitly set value over the decoded config afterwards, giving
// the option precedence over config. Remembering "set" separately from the
// value also lets an explicit zero (WithTLSConfig(nil), WithTLSFiles("", ""))
// fail loud instead of silently falling back.
type setting[T any] struct {
	value T
	isSet bool
}

// set records an explicit value.
func (s *setting[T]) set(v T) { s.value, s.isSet = v, true }

// overlay writes the value into dst when it was set explicitly.
func (s setting[T]) overlay(dst *T) {
	if s.isSet {
		*dst = s.value
	}
}

// listenAddr is the WithAddr host/port pair, overlaid as one unit.
type listenAddr struct {
	host string
	port int
}

// appOptions collects all App construction options.
type appOptions struct {
	rawConfig            RawConfig
	logger               *slog.Logger
	disableRecover       bool
	disableRequestID     bool
	disableAccessLog     bool
	disableReloadSignals bool
	accessLogLogger      *slog.Logger
	accessLogMinLevel    slog.Leveler
	accessLogSkipper     func(*Context) bool
	accessLogFilter      AccessLogResultFilter
	debug                bool
	strictBodies         bool
	jsonOptions          []jsonv2.Options
	httpRedirectAddr     string
	configureServer      func(*http.Server)

	// tlsConfig has the highest TLS precedence and is resolved at preflight,
	// not overlaid onto serverConfig. isSet is remembered even for nil.
	tlsConfig setting[*tls.Config]

	// Server settings that also have a "server" config key. See setting.
	addr                  setting[listenAddr]
	shutdownTimeout       setting[time.Duration]
	reloadTimeout         setting[time.Duration]
	maxBodyBytes          setting[int64]
	redirectTrailingSlash setting[bool]
	trustedProxies        setting[[]string]
	tlsFiles              setting[serverTLS]
}

// overlayServer re-applies the explicitly set programmatic server options over
// cfg after the "server" config section was decoded into it, so an explicit
// With* setting always wins over config (which would otherwise overwrite it —
// including resetting a field to zero, which applyServerDefaults then replaces
// with a framework default, silently undoing an intentional value such as
// WithMaxBodyBytes(-1)).
//
// WithTLSFiles overrides the server.tls.* keys as a whole pair (not merged) and
// fires whenever the option was set — even with empty paths, which preflight
// then rejects rather than letting them silently fall back to the config keys.
// WithTLSConfig outranks both and is resolved later at preflight.
func (o *appOptions) overlayServer(cfg *serverConfig) {
	if o.addr.isSet {
		cfg.Host, cfg.Port = o.addr.value.host, o.addr.value.port
	}
	o.shutdownTimeout.overlay(&cfg.ShutdownTimeout)
	o.reloadTimeout.overlay(&cfg.ReloadTimeout)
	o.maxBodyBytes.overlay(&cfg.MaxBodyBytes)
	if o.redirectTrailingSlash.isSet {
		cfg.RedirectTrailingSlash = new(o.redirectTrailingSlash.value)
	}
	if o.trustedProxies.isSet {
		cfg.TrustedProxies = slices.Clone(o.trustedProxies.value)
	}
	o.tlsFiles.overlay(&cfg.TLS)
}

// WithRawConfig sets the RawConfig for the application. When provided,
// New does not auto-load configuration from files, .env, or environment
// variables; the given RawConfig is registered in DI as-is. The framework
// still reads its internal server settings from the "server" key when present.
//
// Use this option when config has already been loaded explicitly, for example
// via config.Load(config.WithFiles(...)) or config.LoadBytes(...).
func WithRawConfig(rc RawConfig) Option {
	return func(o *appOptions) { o.rawConfig = rc }
}

// GetConfig decodes the configuration value or sub-tree at the given dotted key
// path into a value of type T and returns it. It is a convenience wrapper over
// the application's [RawConfig] (auto-loaded or supplied via [WithRawConfig]),
// saving an explicit app.MustResolve[RawConfig]() plus Unmarshal:
//
//	db, err := app.GetConfig[DatabaseConfig]("database")
//
// Like config.(*Config).Get, this is a bootstrap/composition-root helper: read
// config here and inject typed structs into services via DI rather than reading
// string keys inside business code (a handler cannot reach this method — there
// is no App accessor on *Context). A missing key or decode failure returns an
// error; on error the zero value of T is returned.
func (app *App) GetConfig[T any](key string) (T, error) {
	var dst T
	if err := app.rawConfig.Unmarshal(key, &dst); err != nil {
		var zero T
		return zero, err
	}
	return dst, nil
}

// MustGetConfig is like [App.GetConfig] but panics on error. It suits
// composition-root code where a missing or invalid required key should abort
// startup.
func (app *App) MustGetConfig[T any](key string) T {
	v, err := app.GetConfig[T](key)
	if err != nil {
		panic(err)
	}
	return v
}

// WithLogger sets the application-level logger. Each service receives a
// scoped copy with a "service" attribute. If not set, the framework default
// logger (a text handler writing to stderr) is used, so access and request
// logging are on by default without any configuration.
func WithLogger(l *slog.Logger) Option {
	return func(o *appOptions) { o.logger = l }
}

// WithRedirectTrailingSlash controls whether the router automatically redirects
// requests whose trailing slash variant matches a registered route. GET/HEAD
// requests receive 301; other methods receive 308 (preserving the method).
// Defaults to true when not set.
func WithRedirectTrailingSlash(enabled bool) Option {
	return func(o *appOptions) { o.redirectTrailingSlash.set(enabled) }
}

// WithTrustedProxies configures the CIDR ranges from which forwarded headers
// such as X-Forwarded-For, X-Forwarded-Proto, and X-Real-IP are trusted.
// Requests whose immediate peer is outside this list ignore forwarded headers.
//
// Pass no entries (the default) to disable proxy-header trust entirely.
// Invalid CIDR entries cause [New] to return an error.
func WithTrustedProxies(cidrs ...string) Option {
	return func(o *appOptions) { o.trustedProxies.set(slices.Clone(cidrs)) }
}

// WithoutRecover disables the built-in panic recovery that wraps the entire
// handler chain. By default, Credo recovers from panics in all middleware and
// handlers, logs the stack trace, and returns 500 Internal Server Error.
//
// Disable this if you provide your own recovery mechanism or need panics
// to propagate (e.g., in tests).
func WithoutRecover() Option {
	return func(o *appOptions) { o.disableRecover = true }
}

// WithoutRequestID disables the built-in request ID middleware. By default,
// every request gets a unique ID (set on context and X-Request-Id header),
// and the request-scoped logger is enriched with the request_id attribute.
//
// Disable this if you use [middleware.RequestID] with custom configuration
// (e.g., different header name, custom generator). Note that the built-in
// access logger will still work but request_id will not appear in logs
// unless the custom middleware also enriches ctx.Logger().
func WithoutRequestID() Option {
	return func(o *appOptions) { o.disableRequestID = true }
}

// WithoutAccessLog disables the built-in access logger. By default,
// every request is logged with method, path, status, bytes, duration,
// remote_addr (from Request.RealIP), and user_agent attributes.
//
// The built-in remains the preferred surface for a dedicated sink, minimum
// level, request skipper, or result filter because it observes final response
// state. Disable it when [middleware.AccessLog] supplies a global replacement;
// keeping both is allowed and produces two intentional log entries.
func WithoutAccessLog() Option {
	return func(o *appOptions) { o.disableAccessLog = true }
}

// WithAccessLogLogger sets a dedicated logger for built-in access-log records.
// A nil logger keeps the default request-scoped logger. A dedicated logger
// does not inherit attributes added through [Context.AddLogAttrs] or
// [Context.SetLogger]; the framework still adds the standard access-log fields
// and the request ID explicitly.
//
// Prefer this option over replacing the built-in logger with
// [middleware.AccessLog] when only a separate sink is needed: the built-in
// layer observes final error-renderer status, bytes, and duration. This option
// has no effect when [WithoutAccessLog] is active.
func WithAccessLogLogger(logger *slog.Logger) Option {
	return func(o *appOptions) { o.accessLogLogger = logger }
}

// WithAccessLogMinLevel sets the minimum status-derived level the built-in
// access logger submits. Status still determines the record's actual level:
// 1xx/2xx/3xx are Info, 4xx are Warn, and 5xx+ are Error. A nil level defaults
// to Info. A typed-nil Leveler is rejected by [New].
//
// The Leveler may be consulted concurrently and must be concurrency-safe;
// [slog.LevelVar] supports runtime threshold changes. The level is read once
// per eligible request, before [WithAccessLogResultFilter]. No request-time
// work occurs when [WithoutAccessLog] is active, but typed-nil configuration is
// still rejected during [New].
func WithAccessLogMinLevel(level slog.Leveler) Option {
	return func(o *appOptions) { o.accessLogMinLevel = level }
}

// WithAccessLogSkipper installs a predicate consulted by the built-in access
// logger; when it returns true the request is not logged. Use it to silence
// noisy paths (metrics scrape, static assets) without disabling the logger
// entirely. For per-route or per-group silencing prefer the [MetaAccessLog]
// route meta, and note that health probes are already silenced by default
// (see [HealthConfig.LogRequests]).
//
// The predicate runs BEFORE routing, so only request-level data is reliable
// (method, path, and headers via ctx.Request()); ctx.Route(), route params,
// and the response status are not yet set. For route-based decisions use
// MetaAccessLog. For post-response decisions use [WithAccessLogMinLevel] or
// [WithAccessLogResultFilter].
//
// This has no effect when the built-in access logger is disabled via
// [WithoutAccessLog]; the configurable [middleware.AccessLog] has its own
// Skipper field.
func WithAccessLogSkipper(skip func(*Context) bool) Option {
	return func(o *appOptions) { o.accessLogSkipper = skip }
}

// WithAccessLogResultFilter installs a post-response predicate for the
// built-in access logger. The filter runs only after route-meta silencing and
// the minimum-level check; true emits the entry and false skips it. It cannot
// restore an entry rejected by [WithAccessLogMinLevel]. A nil filter accepts
// every entry that reaches it.
//
// The Context is pooled and valid only for the synchronous callback. The same
// filter may run concurrently for multiple requests and must be concurrency-
// safe. Use the AccessLogEntry fields, not ctx.Response(), for status, bytes,
// and duration. A panic occurs outside built-in recovery. This option has no
// effect when [WithoutAccessLog] is active.
func WithAccessLogResultFilter(filter AccessLogResultFilter) Option {
	return func(o *appOptions) { o.accessLogFilter = filter }
}

// WithDebug enables development-mode warnings. When active, the framework
// logs warnings for common mistakes such as binding a struct that does not
// implement [validation.Validatable]. Can also be enabled via the
// server.debug config key.
func WithDebug() Option {
	return func(o *appOptions) { o.debug = true }
}

// WithStrictBodies makes [Request.BindBody] reject JSON payloads that carry
// object members not mapping to any field of the target, returning a 400
// [BindError] with reason [BindReasonUnknownField]. The default is lenient:
// unknown members are ignored, which is the right posture for public APIs
// whose clients must tolerate server-side additions. Enable strict bodies
// when client and server ship together and a misspelled member should
// surface as an error rather than silently drop. Can also be enabled via
// the server.strict_bodies config key; the option wins over the key.
//
// Only JSON decoding is affected; XML and form binding are unchanged.
func WithStrictBodies() Option {
	return func(o *appOptions) { o.strictBodies = true }
}

// WithJSONOptions overrides Credo's JSON response encoding profile. The
// given options are applied after the framework defaults, so each one
// overrides that axis and leaves the rest intact:
//
//	// keep encoding/json v1's null for nil slices
//	credo.WithJSONOptions(jsonv2.FormatNilSliceAsNull(true))
//
//	// re-enable HTML escaping of < > &
//	credo.WithJSONOptions(jsontext.EscapeForHTML(true))
//
//	// full legacy mode, byte-identical to encoding/json v1 except for the
//	// trailing newline, which v1's Encoder added and json/v2 never writes
//	credo.WithJSONOptions(jsonv1.DefaultOptionsV1())
//
// The profile applies to [Response.JSON] (and therefore to [Context.Render]'s
// fallback) and to framework-owned default error bodies, except that error
// bodies always sort map keys — they are a framework contract.
// Decoding is not affected: request-body policy is [WithStrictBodies].
//
// Repeated calls accumulate in order. Construction-time only; there is no
// config-file key, since options are Go values.
func WithJSONOptions(opts ...jsonv2.Options) Option {
	return func(o *appOptions) { o.jsonOptions = append(o.jsonOptions, opts...) }
}

// WithAddr sets the listen address directly (for testing or programmatic use).
func WithAddr(host string, port int) Option {
	return func(o *appOptions) { o.addr.set(listenAddr{host: host, port: port}) }
}

// WithTLSFiles configures HTTPS by loading the certificate and private key from
// the given PEM file paths. When set, Run and RunContext serve TLS; the key pair
// is loaded and validated at startup (before the server accepts connections), so
// a missing file or mismatched pair fails fast. The same paths may instead come
// from the server.tls.cert_file / server.tls.key_file config keys — WithTLSFiles
// takes precedence over those. It is in turn shadowed by WithTLSConfig.
//
// Calling it with an empty cert or key path is a configuration error caught at
// startup: an explicit but empty WithTLSFiles does not silently fall back to the
// config keys or to plaintext. For conditional TLS, omit the option entirely
// rather than passing empty strings.
//
// The pair is served through GetCertificate backed by an atomic pointer, so
// every [App.Reload] (SIGHUP under Run) re-reads the same two paths and swaps
// the pair in for new handshakes; a failed re-read keeps the current pair and
// is reported through the reload error. Open connections are never affected.
//
// This option performs no I/O; the files are read when the server starts.
func WithTLSFiles(certFile, keyFile string) Option {
	return func(o *appOptions) { o.tlsFiles.set(serverTLS{CertFile: certFile, KeyFile: keyFile}) }
}

// WithTLSConfig configures HTTPS from a fully-formed *tls.Config, exposing the
// complete crypto/tls surface: mutual TLS, SNI via GetCertificate, custom
// minimum version and cipher suites, ALPN, and hot certificate reload. It has
// the highest TLS precedence — when set, WithTLSFiles and the server.tls.* keys
// are ignored.
//
// The config must carry a certificate source (Certificates, GetCertificate, or
// GetConfigForClient), validated at startup. It is cloned before use, so the
// framework never mutates the caller's value and later caller mutations do not
// affect the running server. Passing a nil config is a configuration error
// caught at startup: an explicit WithTLSConfig(nil) does not silently fall back
// to WithTLSFiles, the config keys, or plaintext.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *appOptions) { o.tlsConfig.set(cfg) }
}

// WithHTTPRedirect runs a second, plaintext listener on addr (for example
// ":80") whose only job is to permanently redirect every request to its HTTPS
// equivalent. GET and HEAD receive 301; other methods receive 308 so the method
// (and body) are preserved — matching the framework's trailing-slash redirect
// convention. The redirect target reuses the request host with the TLS server's
// port (omitted when it is 443).
//
// TLS must be configured (via WithTLSFiles, WithTLSConfig, or server.tls.*);
// otherwise startup fails fast at preflight, since redirecting to an HTTPS
// server that does not exist makes no sense. The redirect listener starts and
// drains with the main server, and a runtime failure of the redirect listener
// tears the whole app down — the same as a failure of the main listener — so a
// requested redirect can never silently die while the app reports healthy. It
// does not apply to ServeContext, which serves the caller's listener as-is.
func WithHTTPRedirect(addr string) Option {
	return func(o *appOptions) { o.httpRedirectAddr = addr }
}

// WithHTTPServer registers a callback that receives the [http.Server] the
// framework built, so the whole standard-library surface stays reachable
// without Credo growing an option per field. It runs once, at construction
// time, after every framework-set field — timeouts, MaxHeaderBytes,
// MaxHeaderValueCount, and the ErrorLog bridge — so the callback has the last
// word on all of them, config keys included:
//
//	credo.WithHTTPServer(func(s *http.Server) {
//		s.Protocols = new(http.Protocols)
//		s.Protocols.SetHTTP1(true)
//		s.Protocols.SetUnencryptedHTTP2(true) // H2C
//		s.ConnState = metrics.TrackConnState
//	})
//
// Three fields are framework-owned and re-imposed after the callback returns,
// because the lifecycle depends on them: Handler (always the App), Addr (the
// listener is bound from it), and TLSConfig. TLS is configured through
// [WithTLSConfig] or [WithTLSFiles], which resolve later at startup; a
// TLSConfig set here is either overwritten by those or, with no Credo TLS
// source configured, ignored — it never silently upgrades a plaintext server.
//
// The server's lifecycle methods (Serve, ServeTLS, Shutdown, Close,
// RegisterOnShutdown) belong to the framework: the callback must not call them
// or retain the pointer past its return. The [WithHTTPRedirect] listener is a
// separate, fixed-function server and is deliberately not passed to the
// callback. A nil callback is a no-op.
func WithHTTPServer(fn func(*http.Server)) Option {
	return func(o *appOptions) { o.configureServer = fn }
}

// WithMaxBodyBytes sets the maximum number of bytes read from any request body.
// Requests whose body exceeds the limit receive 413 Request Entity Too Large.
// A negative value disables the limit; zero (the default) applies a 4 MiB cap.
func WithMaxBodyBytes(n int64) Option {
	return func(o *appOptions) { o.maxBodyBytes.set(n) }
}

// WithShutdownTimeout sets the graceful-shutdown drain budget used by the
// signal-aware Run and by context-cancellation-triggered RunContext. The
// parallel HTTP/OnDrain phase, DI singleton cleanup, and OnShutdown hooks must
// complete within this single absolute budget. Zero (the default) applies a
// 30s budget. An explicit Shutdown(ctx) call ignores this and honours the
// caller's context deadline instead. Can also be set via the
// server.shutdown_timeout config key.
func WithShutdownTimeout(d time.Duration) Option {
	return func(o *appOptions) { o.shutdownTimeout.set(d) }
}

// WithoutReloadSignals disables signal-triggered reloads under [App.Run]:
// SIGHUP no longer triggers [App.Reload]. The signal is still captured — and
// ignored with an Info log line — so a stray SIGHUP (logrotate postrotate, a
// forgotten systemd reload directive, a closing terminal) can never terminate
// the process; raw Unix signal disposition, where an unhandled SIGHUP kills
// the process, remains available via [App.RunContext] or [App.ServeContext],
// which install no signal handlers at all. Programmatic [App.Reload] calls are
// unaffected. On platforms without reload signals (Windows) this option is a
// no-op. SIGINT/SIGTERM shutdown handling under Run is unchanged.
func WithoutReloadSignals() Option {
	return func(o *appOptions) { o.disableReloadSignals = true }
}

// WithReloadTimeout sets the context budget for reloads triggered by SIGHUP
// under Run: re-reading configuration, notifying [App.OnConfigChange]
// subscribers, and running [App.OnReload] hooks must complete within it. Zero
// (the default) applies a 30s budget. A programmatic [App.Reload] call ignores
// this and honours the caller's context instead. Can also be set via the
// server.reload_timeout config key.
func WithReloadTimeout(d time.Duration) Option {
	return func(o *appOptions) { o.reloadTimeout.set(d) }
}

// buildServer creates an *http.Server from serverConfig. logger receives the
// server's own diagnostics through the [newServerErrorLog] bridge; a nil
// logger falls back to the framework default.
//
// configure is the [WithHTTPServer] callback. It runs last, after every
// framework-set field, so it can override any of them; the fields the
// lifecycle owns (Handler, Addr, TLSConfig) are re-imposed by the caller.
func buildServer(cfg serverConfig, handler http.Handler, logger *slog.Logger, configure func(*http.Server)) *http.Server {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	srv := &http.Server{
		Addr:                addr,
		Handler:             handler,
		ReadTimeout:         cfg.ReadTimeout,
		WriteTimeout:        cfg.WriteTimeout,
		IdleTimeout:         cfg.IdleTimeout,
		ReadHeaderTimeout:   cfg.ReadHeaderTimeout,
		MaxHeaderBytes:      cfg.MaxHeaderBytes,
		MaxHeaderValueCount: cfg.MaxHeaderValueCount,
		ErrorLog:            newServerErrorLog(logger),
	}
	if configure != nil {
		configure(srv)
		// Re-impose the fields the lifecycle depends on. TLSConfig is not
		// restored here but overwritten later, at preflight, by the TLS
		// precedence chain; when no Credo TLS source is configured the
		// server is served with Serve, which ignores TLSConfig entirely.
		srv.Handler = handler
		srv.Addr = addr
	}
	return srv
}

// newServerErrorLog bridges net/http's own diagnostics into the application
// logger at Error level, tagged component=net/http. Without it the standard
// library writes them to the log package's default output (stderr,
// unstructured), where structured-logging consumers never see them.
//
// What this makes visible: TLS handshake failures, listener accept errors,
// panics that escape the framework recovery, superfluous WriteHeader and
// hijacked-connection writes, and malformed Content-Length or
// Transfer-Encoding responses. The same logger is handed to the HTTP/2
// server.
//
// What it does not cover: header-limit rejections (431, from MaxHeaderBytes
// or the Go 1.27 MaxHeaderValueCount) and unsupported transfer encodings are
// written straight to the connection by net/http and never reach ErrorLog.
//
// The stdlib message text is preserved verbatim ("http: TLS handshake error
// from ...") so existing greps keep working. A nil logger falls back to the
// framework default logger.
func newServerErrorLog(logger *slog.Logger) *log.Logger {
	if logger == nil {
		logger = defaultLogger
	}
	return slog.NewLogLogger(logger.With("component", "net/http").Handler(), slog.LevelError)
}

// defaultReadHeaderTimeout is applied when the server config does not specify
// read_header_timeout, mitigating Slowloris-style attacks that hold connections
// open by trickling request headers one byte at a time.
const defaultReadHeaderTimeout = 10 * time.Second

// defaultMaxBodyBytes is the request body size limit applied when the server
// config does not specify max_body_bytes, mitigating memory-exhaustion DoS.
const defaultMaxBodyBytes = 4 << 20 // 4 MiB

// defaultShutdownTimeout bounds graceful shutdown when none is configured,
// matching the conventional 30s container stop-grace period.
const defaultShutdownTimeout = 30 * time.Second

// defaultReloadTimeout bounds a SIGHUP-triggered reload when none is
// configured.
const defaultReloadTimeout = 30 * time.Second

// applyServerDefaults fills in safe defaults for server settings left at their
// zero value (which would otherwise mean "no limit").
func applyServerDefaults(c *serverConfig) {
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	if c.ReloadTimeout == 0 {
		c.ReloadTimeout = defaultReloadTimeout
	}
}

// validateServerConfig returns an error if serverConfig contains invalid values.
func validateServerConfig(c *serverConfig) error {
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("credo: invalid port %d: must be 0-65535", c.Port)
	}
	if c.ReadTimeout < 0 {
		return fmt.Errorf("credo: invalid ReadTimeout %v: must not be negative", c.ReadTimeout)
	}
	if c.WriteTimeout < 0 {
		return fmt.Errorf("credo: invalid WriteTimeout %v: must not be negative", c.WriteTimeout)
	}
	if c.IdleTimeout < 0 {
		return fmt.Errorf("credo: invalid IdleTimeout %v: must not be negative", c.IdleTimeout)
	}
	if c.ReadHeaderTimeout < 0 {
		return fmt.Errorf("credo: invalid ReadHeaderTimeout %v: must not be negative", c.ReadHeaderTimeout)
	}
	if c.ShutdownTimeout < 0 {
		return fmt.Errorf("credo: invalid ShutdownTimeout %v: must not be negative", c.ShutdownTimeout)
	}
	if c.ReloadTimeout < 0 {
		return fmt.Errorf("credo: invalid ReloadTimeout %v: must not be negative", c.ReloadTimeout)
	}
	if c.MaxHeaderBytes < 0 {
		return fmt.Errorf("credo: invalid MaxHeaderBytes %d: must not be negative", c.MaxHeaderBytes)
	}
	// Unlike MaxBodyBytes, a header-line limit has no "disabled" state in
	// net/http: any value below 1 means the default. Rejecting negatives keeps
	// a typo from reading as a deliberate opt-out that does not exist.
	if c.MaxHeaderValueCount < 0 {
		return fmt.Errorf("credo: invalid MaxHeaderValueCount %d: must not be negative", c.MaxHeaderValueCount)
	}
	return nil
}

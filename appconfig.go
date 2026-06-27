package credo

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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

	// MaxHeaderBytes controls the maximum number of bytes the
	// server will read parsing the request header's keys and values.
	MaxHeaderBytes int `credo:"max_header_bytes"`

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

// appOptions collects all App construction options.
type appOptions struct {
	rawConfig         RawConfig
	serverCfg         serverConfig
	logger            *slog.Logger
	disableRecover    bool
	disableRequestID  bool
	disableAccessLog  bool
	accessLogSkipper  func(*Context) bool
	debug             bool
	trustedProxies    []string
	trustedProxiesSet bool
	tlsConfig         *tls.Config
	tlsConfigSet      bool
	tlsCertFile       string
	tlsKeyFile        string
	tlsFilesSet       bool
	httpRedirectAddr  string
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
	return func(o *appOptions) { o.serverCfg.RedirectTrailingSlash = &enabled }
}

// WithTrustedProxies configures the CIDR ranges from which forwarded headers
// such as X-Forwarded-For, X-Forwarded-Proto, and X-Real-IP are trusted.
// Requests whose immediate peer is outside this list ignore forwarded headers.
//
// Pass no entries (the default) to disable proxy-header trust entirely.
// Invalid CIDR entries cause [New] to return an error.
func WithTrustedProxies(cidrs ...string) Option {
	return func(o *appOptions) {
		o.trustedProxies = append([]string(nil), cidrs...)
		o.trustedProxiesSet = true
	}
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
// Disable this if you use [middleware.AccessLog] with custom configuration
// (e.g., a Skipper function). Using both the built-in and middleware
// loggers produces duplicate log entries.
func WithoutAccessLog() Option {
	return func(o *appOptions) { o.disableAccessLog = true }
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
// MetaAccessLog; status-based filtering is a separate, deferred concern.
//
// This has no effect when the built-in access logger is disabled via
// [WithoutAccessLog]; the configurable [middleware.AccessLog] has its own
// Skipper field.
func WithAccessLogSkipper(skip func(*Context) bool) Option {
	return func(o *appOptions) { o.accessLogSkipper = skip }
}

// WithDebug enables development-mode warnings. When active, the framework
// logs warnings for common mistakes such as binding a struct that does not
// implement [validation.Validatable]. Can also be enabled via the
// server.debug config key.
func WithDebug() Option {
	return func(o *appOptions) { o.debug = true }
}

// WithAddr sets the listen address directly (for testing or programmatic use).
func WithAddr(host string, port int) Option {
	return func(o *appOptions) {
		o.serverCfg.Host = host
		o.serverCfg.Port = port
	}
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
// This option performs no I/O; the files are read when the server starts.
func WithTLSFiles(certFile, keyFile string) Option {
	return func(o *appOptions) {
		o.tlsCertFile = certFile
		o.tlsKeyFile = keyFile
		o.tlsFilesSet = true
	}
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
	return func(o *appOptions) {
		o.tlsConfig = cfg
		o.tlsConfigSet = true
	}
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

// WithMaxBodyBytes sets the maximum number of bytes read from any request body.
// Requests whose body exceeds the limit receive 413 Request Entity Too Large.
// A negative value disables the limit; zero (the default) applies a 4 MiB cap.
func WithMaxBodyBytes(n int64) Option {
	return func(o *appOptions) { o.serverCfg.MaxBodyBytes = n }
}

// WithShutdownTimeout sets the graceful-shutdown drain budget used by the
// signal-aware Run and by context-cancellation-triggered RunContext. The
// drain (HTTP in-flight requests, DI singleton
// cleanup, OnShutdown hooks) must complete within this duration. Zero (the
// default) applies a 30s budget. An explicit Shutdown(ctx) call ignores this
// and honours the caller's context deadline instead. Can also be set via the
// server.shutdown_timeout config key.
func WithShutdownTimeout(d time.Duration) Option {
	return func(o *appOptions) { o.serverCfg.ShutdownTimeout = d }
}

// buildServer creates an *http.Server from serverConfig.
func buildServer(cfg serverConfig, handler http.Handler) *http.Server {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
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
	if c.MaxHeaderBytes < 0 {
		return fmt.Errorf("credo: invalid MaxHeaderBytes %d: must not be negative", c.MaxHeaderBytes)
	}
	return nil
}

package middleware

import (
	"net/http"

	"github.com/credo-go/credo"
)

// CSRFConfig defines configuration for the CSRF middleware.
type CSRFConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// TrustedOrigins lists origins whose cross-origin requests are allowed.
	// Each entry must be of the form "scheme://host[:port]" and is matched
	// exactly against the request's Origin header.
	//
	// Subdomains are cross-origin: a form on app.example.com posting to
	// api.example.com is rejected unless "https://app.example.com" is listed
	// here (browsers send Sec-Fetch-Site: same-site for that case, which is
	// not trusted by default).
	TrustedOrigins []string

	// InsecureBypassPatterns lists path patterns exempt from cross-origin
	// checks, using [http.ServeMux] pattern syntax (e.g. "/webhooks/",
	// "POST /payments/callback"). Matching requests skip CSRF protection
	// entirely — reserve this for endpoints that authenticate by other
	// means (signed webhooks, mTLS) and keep the list as narrow as possible.
	InsecureBypassPatterns []string

	// ErrorHandler maps a rejected request to the error returned by the
	// middleware. The rejection reason from the detector is passed as err.
	// Default: 403 *credo.HTTPError with the reason attached as internal
	// error (logged, never exposed to the client).
	ErrorHandler func(ctx *credo.Context, err error) error
}

// DefaultCSRFConfig returns the default CSRF middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultCSRFConfig() CSRFConfig {
	return CSRFConfig{
		Skipper: DefaultSkipper,
	}
}

// CSRF returns middleware that rejects non-safe cross-origin browser
// requests, protecting state-changing endpoints against Cross-Site Request
// Forgery. It wraps the standard library's [http.CrossOriginProtection],
// which detects cross-origin requests via the Sec-Fetch-Site header — sent
// by all modern browsers — with an Origin/Host comparison fallback for
// older ones. No tokens, cookies, or session state are
// involved, so there is no per-request overhead beyond a header check.
//
// What passes, in detector order:
//   - GET, HEAD, and OPTIONS — safe methods are always allowed; never
//     perform state changes in them.
//   - Sec-Fetch-Site: same-origin or none — same-origin browser requests.
//   - Requests with neither Sec-Fetch-Site nor Origin headers — non-browser
//     clients (curl, server-to-server, mobile SDKs) are unaffected.
//   - Origin matching the Host header (older browsers without
//     Sec-Fetch-Site).
//   - Origins listed in [CSRFConfig.TrustedOrigins] and paths matching
//     [CSRFConfig.InsecureBypassPatterns].
//
// Everything else — in particular Sec-Fetch-Site: cross-site and same-site
// (subdomains!) — is rejected through [CSRFConfig.ErrorHandler] (default:
// 403 Problem Details via the framework error pipeline; the stdlib deny
// handler is not used).
//
// CSRF and [CORS] are complementary, not interchangeable: CORS governs
// whether a browser may *read* a cross-origin response, while CSRF
// protection stops state-changing cross-origin requests from being
// *processed*. APIs serving browser frontends on other origins typically
// need both, with the frontend origins in both allow lists.
//
// Register it globally or per group:
//
//	app.GlobalMiddleware(middleware.CSRF())
//
//	app.GlobalMiddleware(middleware.CSRF(middleware.CSRFConfig{
//	    TrustedOrigins:         []string{"https://app.example.com"},
//	    InsecureBypassPatterns: []string{"/webhooks/"},
//	}))
//
// Panics if a TrustedOrigins entry is not a valid "scheme://host[:port]"
// origin, or if an InsecureBypassPatterns entry is syntactically invalid or
// conflicts with another — middleware construction is startup configuration
// (see the credo package's "Panics and Errors" section).
func CSRF(cfg ...CSRFConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultCSRFConfig(), normalizeCSRFConfig)

	protection := http.NewCrossOriginProtection()
	for _, origin := range config.TrustedOrigins {
		if err := protection.AddTrustedOrigin(origin); err != nil {
			panic("middleware: CSRF: " + err.Error())
		}
	}
	for _, pattern := range config.InsecureBypassPatterns {
		// Panics on invalid or conflicting patterns.
		protection.AddInsecureBypassPattern(pattern)
	}

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}

			if err := protection.Check(ctx.Request().Request); err != nil {
				return config.ErrorHandler(ctx, err)
			}
			return next(ctx)
		}
	}
}

func normalizeCSRFConfig(config CSRFConfig) CSRFConfig {
	if config.Skipper == nil {
		config.Skipper = DefaultSkipper
	}
	if config.ErrorHandler == nil {
		config.ErrorHandler = func(_ *credo.Context, err error) error {
			return credo.NewHTTPError(http.StatusForbidden).WithInternal(err)
		}
	}
	return config
}

package auth

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

var (
	// ErrJWTMissing is returned when no JWT token can be extracted.
	ErrJWTMissing = errors.New("auth: jwt token is missing")

	// ErrJWTInvalid is returned when a JWT token fails validation.
	ErrJWTInvalid = errors.New("auth: jwt token is invalid")
)

// JWTConfig defines configuration for JWTAuthenticator.
//
// The config has two tiers. The simple tier covers common setups with
// Credo-typed fields only: token extraction, key material, registered-claim
// validation (Issuer, Audience, Leeway, RequireExpiry), and user extraction
// via [JWTConfig.ParseClaims]. The [JWTConfig.Advanced] tier exposes the
// underlying golang-jwt primitives for setups the simple tier cannot
// express (custom claims structs, JWKS key resolution, extra parser
// options).
type JWTConfig[T any] struct {
	// Header is the header name used to read the token.
	// Default: "Authorization".
	Header string

	// Prefix is the token scheme prefix in Header.
	// Default: "Bearer".
	Prefix string

	// Query is an optional query parameter name for token fallback.
	// Example: "token".
	Query string

	// Cookie is an optional cookie name for token fallback.
	// Example: "jwt".
	Cookie string

	// SigningMethod is the expected token alg (e.g. HS256, RS256).
	// Default: HS256.
	SigningMethod string

	// SigningKey is the default key used to validate signatures.
	// It is also used as a fallback when SigningKeys is configured but
	// an incoming token does not include a kid header.
	SigningKey any

	// SigningKeys is an optional key set selected by token header kid.
	// If token kid is present, it must exist in this map.
	SigningKeys map[string]any

	// Issuer, when set, requires the token's iss claim to match exactly.
	Issuer string

	// Audience, when set, requires the token's aud claim to contain AT
	// LEAST ONE of the listed values. This any-of rule is the standard
	// RFC 7519 validator practice: a token may be addressed to several
	// audiences, and the recipient accepts it if it identifies itself
	// with any of them.
	Audience []string

	// Leeway widens time-based claim validation (exp, nbf, iat) by the
	// given duration in both directions, absorbing clock skew between
	// the token issuer and this server.
	Leeway time.Duration

	// RequireExpiry rejects tokens that carry no exp claim. By default
	// (matching golang-jwt v5), exp is validated only when present —
	// a token without exp never expires. Set this when tokens are
	// expected to be short-lived.
	RequireExpiry bool

	// ParseClaims converts the validated token's claims into the user
	// value T. This is the simple-tier counterpart of
	// [JWTAdvanced.ParseToken]; configuring both is a constructor error.
	// When neither is set, the raw claims value is type-asserted to T.
	ParseClaims func(claims JWTClaims) (T, error)

	// Advanced exposes golang-jwt primitives directly. Most applications
	// never need it; see [JWTAdvanced].
	Advanced JWTAdvanced[T]
}

// JWTAdvanced is the escape hatch tier of [JWTConfig]: direct golang-jwt
// integration for setups the Credo-typed fields cannot express. Fields
// here trade framework guarantees for full library access — for example,
// ParserOptions can override the option the framework derives from
// SigningMethod, Issuer, Leeway, or RequireExpiry.
type JWTAdvanced[T any] struct {
	// KeyFunc overrides SigningKey/SigningKeys selection entirely
	// (e.g. JWKS lookup).
	KeyFunc jwt.Keyfunc

	// NewClaims creates the claims value tokens are parsed into.
	// Default: a JSON object map (jwt.MapClaims).
	NewClaims func() jwt.Claims

	// ParseToken converts a validated *jwt.Token into the user value T,
	// with access to the raw token (header, signature, typed claims).
	// Configuring both ParseToken and JWTConfig.ParseClaims is a
	// constructor error.
	ParseToken func(token *jwt.Token) (T, error)

	// ParserOptions are appended after the options the framework derives
	// from the simple tier, so on conflict the last (advanced) option
	// wins.
	ParserOptions []jwt.ParserOption
}

// JWTClaims is a read-only view over a validated token's claims, passed to
// [JWTConfig.ParseClaims]. Registered claims are exposed with proper Go
// types — timestamps as time.Time rather than the raw float64 that JSON
// decoding produces — and read as zero values when absent or malformed
// (signature and temporal validity are already enforced before ParseClaims
// runs).
type JWTClaims struct {
	claims jwt.Claims
}

// Subject returns the sub claim, or "" when absent.
func (c JWTClaims) Subject() string {
	if c.claims == nil {
		return ""
	}
	s, _ := c.claims.GetSubject()
	return s
}

// Issuer returns the iss claim, or "" when absent.
func (c JWTClaims) Issuer() string {
	if c.claims == nil {
		return ""
	}
	s, _ := c.claims.GetIssuer()
	return s
}

// Audience returns the aud claim values, or nil when absent.
func (c JWTClaims) Audience() []string {
	if c.claims == nil {
		return nil
	}
	aud, _ := c.claims.GetAudience()
	return aud
}

// ExpiresAt returns the exp claim as a time.Time, or the zero time when
// absent.
func (c JWTClaims) ExpiresAt() time.Time {
	return c.numericDate(jwt.Claims.GetExpirationTime)
}

// IssuedAt returns the iat claim as a time.Time, or the zero time when
// absent.
func (c JWTClaims) IssuedAt() time.Time {
	return c.numericDate(jwt.Claims.GetIssuedAt)
}

// NotBefore returns the nbf claim as a time.Time, or the zero time when
// absent.
func (c JWTClaims) NotBefore() time.Time {
	return c.numericDate(jwt.Claims.GetNotBefore)
}

func (c JWTClaims) numericDate(get func(jwt.Claims) (*jwt.NumericDate, error)) time.Time {
	if c.claims == nil {
		return time.Time{}
	}
	date, err := get(c.claims)
	if err != nil || date == nil {
		return time.Time{}
	}
	return date.Time
}

// Get returns the named custom claim when the default claims
// representation (a JSON object map) is in use, or nil when the claim is
// absent or a custom [JWTAdvanced.NewClaims] type is configured. Typed
// custom claims structs should be read via [JWTAdvanced.ParseToken]
// instead.
func (c JWTClaims) Get(name string) any {
	if m, ok := c.claims.(jwt.MapClaims); ok {
		return m[name]
	}
	return nil
}

// GetString returns the named custom claim as a string, or "" when the
// claim is absent or not a string. See [JWTClaims.Get] for the lookup
// rules.
func (c JWTClaims) GetString(name string) string {
	s, _ := c.Get(name).(string)
	return s
}

// JWTAuthenticator validates JWT credentials from an HTTP request.
type JWTAuthenticator[T any] struct {
	cfg    JWTConfig[T]
	parser *jwt.Parser
}

// NewJWTAuthenticator creates a new JWT Authenticator.
func NewJWTAuthenticator[T any](cfg JWTConfig[T]) (*JWTAuthenticator[T], error) {
	if cfg.Header == "" && cfg.Query == "" && cfg.Cookie == "" {
		cfg.Header = http.CanonicalHeaderKey("Authorization")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "Bearer"
	}
	if cfg.SigningMethod == "" {
		cfg.SigningMethod = jwt.SigningMethodHS256.Alg()
	}
	if cfg.ParseClaims != nil && cfg.Advanced.ParseToken != nil {
		return nil, errors.New("auth: jwt: configure either ParseClaims or Advanced.ParseToken, not both")
	}
	if cfg.Advanced.NewClaims == nil {
		cfg.Advanced.NewClaims = func() jwt.Claims {
			return jwt.MapClaims{}
		}
	}

	if cfg.Advanced.KeyFunc == nil {
		if cfg.SigningKey == nil && len(cfg.SigningKeys) == 0 {
			return nil, errors.New("auth: jwt signing key is required")
		}
		cfg.Advanced.KeyFunc = jwtKeyFunc(cfg.SigningMethod, cfg.SigningKey, cfg.SigningKeys)
	}

	parserOptions := make([]jwt.ParserOption, 0, len(cfg.Advanced.ParserOptions)+4)
	if cfg.SigningMethod != "" {
		parserOptions = append(parserOptions, jwt.WithValidMethods([]string{cfg.SigningMethod}))
	}
	if cfg.Issuer != "" {
		parserOptions = append(parserOptions, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Leeway > 0 {
		parserOptions = append(parserOptions, jwt.WithLeeway(cfg.Leeway))
	}
	if cfg.RequireExpiry {
		parserOptions = append(parserOptions, jwt.WithExpirationRequired())
	}
	parserOptions = append(parserOptions, cfg.Advanced.ParserOptions...)

	return &JWTAuthenticator[T]{
		cfg:    cfg,
		parser: jwt.NewParser(parserOptions...),
	}, nil
}

// Authenticate validates a JWT token and returns the authenticated user value.
func (a *JWTAuthenticator[T]) Authenticate(r *http.Request) (T, error) {
	tokenString, err := a.extractToken(r)
	if err != nil {
		var zero T
		return zero, err
	}

	claims := a.cfg.Advanced.NewClaims()
	token, err := a.parser.ParseWithClaims(tokenString, claims, a.cfg.Advanced.KeyFunc)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %w", ErrJWTInvalid, err)
	}
	if !token.Valid {
		var zero T
		return zero, ErrJWTInvalid
	}

	if err := a.validateAudience(token.Claims); err != nil {
		var zero T
		return zero, err
	}

	if a.cfg.Advanced.ParseToken != nil {
		user, err := a.cfg.Advanced.ParseToken(token)
		if err != nil {
			var zero T
			return zero, fmt.Errorf("%w: parse user: %w", ErrJWTInvalid, err)
		}
		return user, nil
	}

	if a.cfg.ParseClaims != nil {
		user, err := a.cfg.ParseClaims(JWTClaims{claims: token.Claims})
		if err != nil {
			var zero T
			return zero, fmt.Errorf("%w: parse claims: %w", ErrJWTInvalid, err)
		}
		return user, nil
	}

	user, ok := token.Claims.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("%w: claims type %T is not assignable", ErrJWTInvalid, token.Claims)
	}

	return user, nil
}

// validateAudience enforces the any-of audience rule from JWTConfig.Audience.
// golang-jwt's WithAudience option matches a single expected value, so the
// framework validates the list itself after parsing.
func (a *JWTAuthenticator[T]) validateAudience(claims jwt.Claims) error {
	if len(a.cfg.Audience) == 0 {
		return nil
	}
	tokenAud, err := claims.GetAudience()
	if err != nil {
		return fmt.Errorf("%w: read audience: %w", ErrJWTInvalid, err)
	}
	for _, want := range a.cfg.Audience {
		if slices.Contains(tokenAud, want) {
			return nil
		}
	}
	return fmt.Errorf("%w: audience mismatch", ErrJWTInvalid)
}

func (a *JWTAuthenticator[T]) extractToken(r *http.Request) (string, error) {
	if token, ok := extractHeaderCredential(r, a.cfg.Header, a.cfg.Prefix); ok {
		return token, nil
	}

	if token, ok := extractQueryCredential(r, a.cfg.Query); ok {
		return token, nil
	}

	if token, ok := extractCookieCredential(r, a.cfg.Cookie); ok {
		return token, nil
	}

	return "", ErrJWTMissing
}

func jwtKeyFunc(signingMethod string, signingKey any, signingKeys map[string]any) jwt.Keyfunc {
	// Defensive copy — later mutation of the caller's map must not affect
	// key resolution.
	keys := maps.Clone(signingKeys)

	return func(token *jwt.Token) (any, error) {
		// Deliberate overlap with the parser-level WithValidMethods option
		// (set in NewJWTAuthenticator): defense in depth against
		// algorithm-confusion attacks, and the only check when a caller
		// supplies ParserOptions that omit method validation.
		if signingMethod != "" && token.Method.Alg() != signingMethod {
			return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
		}

		if len(keys) == 0 {
			return signingKey, nil
		}

		kid, ok := token.Header["kid"].(string)
		if ok && kid != "" {
			key, found := keys[kid]
			if !found {
				return nil, fmt.Errorf("unknown key id %q", kid)
			}
			return key, nil
		}

		if signingKey != nil {
			return signingKey, nil
		}

		return nil, errors.New("missing key id (kid)")
	}
}

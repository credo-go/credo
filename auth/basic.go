package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/credo-go/credo"
)

var (
	// ErrBasicCredentialsMissing is returned when Basic credentials are absent.
	ErrBasicCredentialsMissing = errors.New("auth: basic credentials are missing")

	// ErrBasicCredentialsInvalid is returned when Basic credentials are rejected.
	ErrBasicCredentialsInvalid = errors.New("auth: basic credentials are invalid")
)

// BasicValidator validates username/password credentials and returns user data.
// ok=false means credentials were parsed but rejected.
//
// When comparing the password against a known plaintext secret, use
// [SecureCompare] instead of == to avoid leaking timing information; stored
// passwords should be verified with a dedicated password hash (bcrypt,
// argon2id) instead.
type BasicValidator[T any] func(username, password string, r *http.Request) (user T, ok bool, err error)

// BasicConfig defines configuration for BasicAuthenticator.
type BasicConfig[T any] struct {
	// Validator checks whether username/password are valid.
	Validator BasicValidator[T]

	// Realm is used by BasicErrorHandler for WWW-Authenticate.
	// Default: "Restricted".
	Realm string
}

// BasicAuthenticator validates HTTP Basic credentials.
type BasicAuthenticator[T any] struct {
	validator BasicValidator[T]
	realm     string
}

// NewBasicAuthenticator creates a new Basic Authenticator.
func NewBasicAuthenticator[T any](cfg BasicConfig[T]) (*BasicAuthenticator[T], error) {
	if cfg.Validator == nil {
		return nil, errors.New("auth: basic validator is required")
	}
	if cfg.Realm == "" {
		cfg.Realm = "Restricted"
	}

	return &BasicAuthenticator[T]{
		validator: cfg.Validator,
		realm:     cfg.Realm,
	}, nil
}

// Authenticate validates request Basic credentials.
func (a *BasicAuthenticator[T]) Authenticate(r *http.Request) (T, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		var zero T
		return zero, ErrBasicCredentialsMissing
	}

	user, valid, err := a.validator(username, password, r)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %w", ErrBasicCredentialsInvalid, err)
	}
	if !valid {
		var zero T
		return zero, ErrBasicCredentialsInvalid
	}

	return user, nil
}

// Realm returns the configured Basic auth realm.
func (a *BasicAuthenticator[T]) Realm() string {
	return a.realm
}

// BasicChallenge returns a valid WWW-Authenticate header value for Basic auth.
func BasicChallenge(realm string) string {
	if realm == "" {
		realm = "Restricted"
	}
	return "Basic realm=" + strconv.Quote(realm)
}

// BasicErrorHandler returns an ErrorFunc that adds a Basic challenge header
// before returning 401 Unauthorized.
func BasicErrorHandler(realm string) ErrorFunc {
	challenge := BasicChallenge(realm)

	return func(err error, ctx *credo.Context) error {
		ctx.Response().Header().Set("WWW-Authenticate", challenge)
		return credo.ErrUnauthorized.WithInternal(err)
	}
}

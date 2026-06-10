package auth

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	// ErrAPIKeyMissing is returned when no API key can be extracted.
	ErrAPIKeyMissing = errors.New("auth: api key is missing")

	// ErrAPIKeyInvalid is returned when an API key is rejected.
	ErrAPIKeyInvalid = errors.New("auth: api key is invalid")
)

// APIKeyValidator validates an API key and returns an authenticated user.
// ok=false means credentials were recognized but rejected.
//
// When comparing the key against a known secret, use [SecureCompare]
// instead of == to avoid leaking timing information.
type APIKeyValidator[T any] func(key string, r *http.Request) (user T, ok bool, err error)

// APIKeyConfig defines configuration for APIKeyAuthenticator.
type APIKeyConfig[T any] struct {
	// Header is the header name used for key extraction.
	// Default: "X-API-Key" when Header and Query are both empty.
	Header string

	// Prefix trims an optional scheme prefix from Header value.
	// Example: "ApiKey" for "Authorization: ApiKey <key>".
	Prefix string

	// Query is an optional query parameter fallback.
	Query string

	// Validator checks whether the extracted key is valid.
	Validator APIKeyValidator[T]
}

// APIKeyAuthenticator validates API keys extracted from HTTP requests.
type APIKeyAuthenticator[T any] struct {
	cfg APIKeyConfig[T]
}

// NewAPIKeyAuthenticator creates a new API key Authenticator.
func NewAPIKeyAuthenticator[T any](cfg APIKeyConfig[T]) (*APIKeyAuthenticator[T], error) {
	if cfg.Validator == nil {
		return nil, errors.New("auth: api key validator is required")
	}

	if cfg.Header == "" && cfg.Query == "" {
		cfg.Header = http.CanonicalHeaderKey("X-API-Key")
	}

	return &APIKeyAuthenticator[T]{cfg: cfg}, nil
}

// Authenticate validates request API key credentials.
func (a *APIKeyAuthenticator[T]) Authenticate(r *http.Request) (T, error) {
	key, err := a.extractKey(r)
	if err != nil {
		var zero T
		return zero, err
	}

	user, ok, err := a.cfg.Validator(key, r)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %w", ErrAPIKeyInvalid, err)
	}
	if !ok {
		var zero T
		return zero, ErrAPIKeyInvalid
	}

	return user, nil
}

func (a *APIKeyAuthenticator[T]) extractKey(r *http.Request) (string, error) {
	if key, ok := extractHeaderCredential(r, a.cfg.Header, a.cfg.Prefix); ok {
		return key, nil
	}

	if key, ok := extractQueryCredential(r, a.cfg.Query); ok {
		return key, nil
	}

	return "", ErrAPIKeyMissing
}

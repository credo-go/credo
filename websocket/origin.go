package websocket

import (
	"errors"
	"fmt"
	"strings"

	internalorigin "github.com/credo-go/credo/internal/origin"
)

// originPolicy is the compiled AllowedOrigins/InsecureSkipOriginCheck
// decision. Origin grammar and matching live in internal/origin so the
// adapter and CORS share one definition.
type originPolicy struct {
	allowed  []internalorigin.Pattern
	insecure bool
}

func compileOriginPolicy(values []string, insecure bool) (originPolicy, error) {
	if insecure && len(values) > 0 {
		return originPolicy{}, errors.New(
			"InsecureSkipOriginCheck cannot be combined with AllowedOrigins",
		)
	}
	policy := originPolicy{insecure: insecure}
	for i, value := range values {
		pattern, err := internalorigin.ParsePattern(value)
		if err != nil {
			return originPolicy{}, fmt.Errorf("AllowedOrigins[%d]: %w", i, err)
		}
		policy.allowed = append(policy.allowed, pattern)
	}
	return policy, nil
}

func (policy originPolicy) authorize(
	originValues []string,
	requestScheme string,
	requestHost string,
) error {
	if policy.insecure {
		return nil
	}
	if len(originValues) == 0 {
		return nil
	}
	if len(originValues) != 1 {
		return errors.New("multiple Origin header values are not allowed")
	}
	origin, err := internalorigin.Parse(originValues[0])
	if err != nil {
		return fmt.Errorf("invalid Origin header: %w", err)
	}
	requestOrigin, err := internalorigin.Parse(strings.ToLower(requestScheme) + "://" + requestHost)
	if err != nil {
		return fmt.Errorf("invalid request origin: %w", err)
	}
	if origin == requestOrigin {
		return nil
	}
	for _, allowed := range policy.allowed {
		if allowed.Matches(origin) {
			return nil
		}
	}
	return errors.New("origin is not authorized")
}

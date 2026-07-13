package websocket

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const maxOriginLength = 2048

type canonicalOrigin struct {
	scheme string
	host   string
	port   uint16
}

type originPattern struct {
	origin   canonicalOrigin
	wildcard bool
}

type originPolicy struct {
	allowed  []originPattern
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
		pattern, err := parseOriginPattern(value)
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
	origin, err := parseCanonicalOrigin(originValues[0], false)
	if err != nil {
		return fmt.Errorf("invalid Origin header: %w", err)
	}
	requestOrigin, err := parseCanonicalOrigin(
		strings.ToLower(requestScheme)+"://"+requestHost,
		false,
	)
	if err != nil {
		return fmt.Errorf("invalid request origin: %w", err)
	}
	if origin == requestOrigin {
		return nil
	}
	for _, allowed := range policy.allowed {
		if allowed.matches(origin) {
			return nil
		}
	}
	return errors.New("origin is not authorized")
}

func (pattern originPattern) matches(origin canonicalOrigin) bool {
	if pattern.origin.scheme != origin.scheme || pattern.origin.port != origin.port {
		return false
	}
	if !pattern.wildcard {
		return pattern.origin.host == origin.host
	}
	suffix := "." + pattern.origin.host
	if !strings.HasSuffix(origin.host, suffix) {
		return false
	}
	leftLabel := strings.TrimSuffix(origin.host, suffix)
	return leftLabel != "" && !strings.Contains(leftLabel, ".")
}

func parseOriginPattern(raw string) (originPattern, error) {
	origin, wildcard, err := parseOrigin(raw, true)
	if err != nil {
		return originPattern{}, err
	}
	return originPattern{origin: origin, wildcard: wildcard}, nil
}

func parseCanonicalOrigin(raw string, allowWildcard bool) (canonicalOrigin, error) {
	origin, _, err := parseOrigin(raw, allowWildcard)
	return origin, err
}

func parseOrigin(raw string, allowWildcard bool) (canonicalOrigin, bool, error) {
	if raw == "" || raw == "null" {
		return canonicalOrigin{}, false, errors.New("origin must not be empty or null")
	}
	if len(raw) > maxOriginLength {
		return canonicalOrigin{}, false, fmt.Errorf("origin exceeds %d bytes", maxOriginLength)
	}
	if strings.TrimSpace(raw) != raw {
		return canonicalOrigin{}, false, errors.New("origin must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return canonicalOrigin{}, false, fmt.Errorf("parse origin: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return canonicalOrigin{}, false, errors.New("origin scheme must be http or https")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return canonicalOrigin{}, false, errors.New("origin must be an absolute hierarchical URL")
	}
	if parsed.User != nil {
		return canonicalOrigin{}, false, errors.New("origin must not contain userinfo")
	}
	if parsed.Path != "" || parsed.RawPath != "" {
		return canonicalOrigin{}, false, errors.New("origin must not contain a path")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return canonicalOrigin{}, false, errors.New("origin must not contain a query")
	}
	if strings.Contains(raw, "#") {
		return canonicalOrigin{}, false, errors.New("origin must not contain a fragment")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.Contains(host, "%") || !isASCII(host) {
		return canonicalOrigin{}, false, errors.New("origin hostname must be non-empty ASCII without a zone")
	}
	wildcard := strings.Contains(host, "*")
	if wildcard {
		if !allowWildcard || strings.Count(host, "*") != 1 || !strings.HasPrefix(host, "*.") {
			return canonicalOrigin{}, false, errors.New("wildcard must be one complete left-most DNS label")
		}
		host = strings.TrimPrefix(host, "*.")
		if net.ParseIP(host) != nil || strings.Count(host, ".") < 1 {
			return canonicalOrigin{}, false, errors.New("wildcard suffix must be a multi-label DNS hostname")
		}
		if validationErr := validateDNSName(host); validationErr != nil {
			return canonicalOrigin{}, false, validationErr
		}
	} else if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else if validationErr := validateDNSName(host); validationErr != nil {
		return canonicalOrigin{}, false, validationErr
	}

	portText := parsed.Port()
	if portText == "" && (strings.HasSuffix(parsed.Host, ":") || strings.HasSuffix(parsed.Host, "]:")) {
		return canonicalOrigin{}, false, errors.New("origin port must not be empty")
	}
	if portText == "" {
		if scheme == "http" {
			portText = "80"
		} else {
			portText = "443"
		}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return canonicalOrigin{}, false, fmt.Errorf("invalid origin port %q", portText)
	}
	return canonicalOrigin{scheme: scheme, host: host, port: uint16(port)}, wildcard, nil
}

func validateDNSName(host string) error {
	if len(host) > 253 {
		return errors.New("origin hostname exceeds 253 bytes")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return errors.New("origin hostname has an empty or oversized label")
		}
		if !asciiLetterOrDigit(label[0]) || !asciiLetterOrDigit(label[len(label)-1]) {
			return errors.New("origin hostname labels must start and end with a letter or digit")
		}
		for i := 1; i < len(label)-1; i++ {
			if !asciiLetterOrDigit(label[i]) && label[i] != '-' {
				return errors.New("origin hostname contains an invalid DNS character")
			}
		}
	}
	return nil
}

func asciiLetterOrDigit(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isASCII(value string) bool {
	for i := range len(value) {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

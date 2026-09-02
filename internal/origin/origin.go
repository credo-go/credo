// Package origin parses and matches web origins (scheme://host[:port]) with
// one strict grammar shared by every Credo surface that accepts an allow-list
// of origins (the WebSocket adapter, CORS). An origin is canonicalized to a
// lowercase scheme and host and an explicit port; an allow-list pattern may
// additionally carry one wildcard that stands for exactly one left-most DNS
// label ("https://*.example.com" matches "https://app.example.com" but not
// "https://example.com" or "https://a.b.example.com").
package origin

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MaxLength bounds the raw text accepted by Parse and ParsePattern.
const MaxLength = 2048

// Origin is a canonical web origin: lowercase scheme and host, explicit port.
// IPv6 hosts are stored unbracketed in their canonical textual form.
type Origin struct {
	Scheme string
	Host   string
	Port   uint16
}

// String renders the origin as scheme://host[:port], omitting the scheme's
// default port and bracketing IPv6 hosts.
func (o Origin) String() string {
	host := o.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (o.Scheme == "http" && o.Port == 80) || (o.Scheme == "https" && o.Port == 443) {
		return o.Scheme + "://" + host
	}
	return o.Scheme + "://" + host + ":" + strconv.Itoa(int(o.Port))
}

// Pattern is one allow-list entry: an exact origin, or a wildcard that
// matches exactly one left-most DNS label in front of Origin.Host.
type Pattern struct {
	Origin   Origin
	Wildcard bool
}

// Matches reports whether o is allowed by the pattern. Scheme and port must
// match exactly; the host matches exactly or, for a wildcard pattern, as
// "<label>." + pattern host where label is one non-empty DNS label.
func (p Pattern) Matches(o Origin) bool {
	if p.Origin.Scheme != o.Scheme || p.Origin.Port != o.Port {
		return false
	}
	if !p.Wildcard {
		return p.Origin.Host == o.Host
	}
	suffix := "." + p.Origin.Host
	if !strings.HasSuffix(o.Host, suffix) {
		return false
	}
	leftLabel := strings.TrimSuffix(o.Host, suffix)
	return leftLabel != "" && !strings.Contains(leftLabel, ".")
}

// Parse canonicalizes a concrete origin such as an Origin header value.
// Wildcards are rejected.
func Parse(raw string) (Origin, error) {
	o, _, err := parse(raw, false)
	return o, err
}

// ParsePattern parses an allow-list entry, accepting one left-most wildcard
// label.
func ParsePattern(raw string) (Pattern, error) {
	o, wildcard, err := parse(raw, true)
	if err != nil {
		return Pattern{}, err
	}
	return Pattern{Origin: o, Wildcard: wildcard}, nil
}

func parse(raw string, allowWildcard bool) (Origin, bool, error) {
	if raw == "" || raw == "null" {
		return Origin{}, false, errors.New("origin must not be empty or null")
	}
	if len(raw) > MaxLength {
		return Origin{}, false, fmt.Errorf("origin exceeds %d bytes", MaxLength)
	}
	if strings.TrimSpace(raw) != raw {
		return Origin{}, false, errors.New("origin must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return Origin{}, false, fmt.Errorf("parse origin: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return Origin{}, false, errors.New("origin scheme must be http or https")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return Origin{}, false, errors.New("origin must be an absolute hierarchical URL")
	}
	if parsed.User != nil {
		return Origin{}, false, errors.New("origin must not contain userinfo")
	}
	if parsed.Path != "" || parsed.RawPath != "" {
		return Origin{}, false, errors.New("origin must not contain a path")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return Origin{}, false, errors.New("origin must not contain a query")
	}
	if strings.Contains(raw, "#") {
		return Origin{}, false, errors.New("origin must not contain a fragment")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.Contains(host, "%") || !isASCII(host) {
		return Origin{}, false, errors.New("origin hostname must be non-empty ASCII without a zone")
	}
	wildcard := strings.Contains(host, "*")
	if wildcard {
		if !allowWildcard || strings.Count(host, "*") != 1 || !strings.HasPrefix(host, "*.") {
			return Origin{}, false, errors.New("wildcard must be one complete left-most DNS label")
		}
		host = strings.TrimPrefix(host, "*.")
		if net.ParseIP(host) != nil || strings.Count(host, ".") < 1 {
			return Origin{}, false, errors.New("wildcard suffix must be a multi-label DNS hostname")
		}
		if validationErr := validateDNSName(host); validationErr != nil {
			return Origin{}, false, validationErr
		}
	} else if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else if validationErr := validateDNSName(host); validationErr != nil {
		return Origin{}, false, validationErr
	}

	portText := parsed.Port()
	if portText == "" && (strings.HasSuffix(parsed.Host, ":") || strings.HasSuffix(parsed.Host, "]:")) {
		return Origin{}, false, errors.New("origin port must not be empty")
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
		return Origin{}, false, fmt.Errorf("invalid origin port %q", portText)
	}
	return Origin{Scheme: scheme, Host: host, Port: uint16(port)}, wildcard, nil
}

func validateDNSName(host string) error {
	if len(host) > 253 {
		return errors.New("origin hostname exceeds 253 bytes")
	}
	for label := range strings.SplitSeq(host, ".") {
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

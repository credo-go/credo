package websocket

import (
	"fmt"
	"strings"
)

func validateSubprotocols(values []string, required bool) ([]string, error) {
	if required && len(values) == 0 {
		return nil, fmt.Errorf("RequireSubprotocol needs at least one Subprotocol")
	}
	result := make([]string, len(values))
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if !validSubprotocolToken(value) {
			return nil, fmt.Errorf("Subprotocols[%d] is not a valid HTTP token: %q", i, value)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("Subprotocols contains duplicate token %q", value)
		}
		seen[value] = struct{}{}
		result[i] = value
	}
	return result, nil
}

func validSubprotocolToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func parseClientSubprotocols(headerValues []string) ([]string, error) {
	var protocols []string
	for _, headerValue := range headerValues {
		for token := range strings.SplitSeq(headerValue, ",") {
			token = strings.TrimSpace(token)
			if !validSubprotocolToken(token) {
				return nil, fmt.Errorf("invalid client subprotocol token %q", token)
			}
			protocols = append(protocols, token)
		}
	}
	return protocols, nil
}

func selectSubprotocol(client, server []string, required bool) (string, error) {
	if len(client) == 0 {
		if required {
			return "", fmt.Errorf("client did not offer a required subprotocol")
		}
		return "", nil
	}
	for _, serverProtocol := range server {
		for _, clientProtocol := range client {
			if serverProtocol == clientProtocol {
				return serverProtocol, nil
			}
		}
	}
	return "", fmt.Errorf("client and server have no common subprotocol")
}

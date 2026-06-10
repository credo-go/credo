package config

import (
	"fmt"
	"strings"
)

// parseDotenv parses .env file content into key-value pairs.
//
// Supported syntax:
//   - KEY=VALUE (basic assignment)
//   - KEY="quoted value" (double-quoted, supports \n \" \\)
//   - KEY='literal value' (single-quoted, no escapes)
//   - export KEY=VALUE (export prefix stripped)
//   - # comment lines (must be first non-whitespace character)
//   - Empty lines (ignored)
//   - Whitespace around = is trimmed
//
// Limitations:
//   - No multiline values (even in quotes, each line is a separate entry)
//   - No variable expansion ($VAR / ${VAR})
//   - No YAML-style KEY: VALUE syntax
func parseDotenv(data []byte) (map[string]string, error) {
	result := make(map[string]string)
	lines := strings.Split(string(data), "\n")

	for i, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments.
		if line == "" || line[0] == '#' {
			continue
		}

		// Strip optional "export " prefix.
		line = strings.TrimPrefix(line, "export ")

		// Split on the first "=" separator.
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: missing '=' in %q", i+1, line)
		}

		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", i+1)
		}

		// Handle quoted values.
		val = unquoteDotenvValue(val)

		result[key] = val
	}

	return result, nil
}

// unquoteDotenvValue handles double-quoted, single-quoted, and unquoted values.
func unquoteDotenvValue(val string) string {
	if len(val) < 2 {
		return val
	}

	switch val[0] {
	case '"':
		// Double-quoted: process escape sequences in a single pass.
		if val[len(val)-1] == '"' {
			return unescapeDoubleQuoted(val[1 : len(val)-1])
		}
	case '\'':
		// Single-quoted: literal value, no escape processing.
		if val[len(val)-1] == '\'' {
			return val[1 : len(val)-1]
		}
	}

	return val
}

// unescapeDoubleQuoted processes escape sequences in a single pass,
// ensuring that \\ is handled correctly before other escapes.
func unescapeDoubleQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		// Process escape sequence.
		next := s[i+1]
		switch next {
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		default:
			// Unknown escape: keep as-is.
			b.WriteByte('\\')
			b.WriteByte(next)
		}
		i++ // skip the next character
	}

	return b.String()
}

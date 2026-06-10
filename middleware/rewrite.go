package middleware

import (
	"regexp"
	"strings"

	"github.com/credo-go/credo"
	internalhost "github.com/credo-go/credo/internal/host"
	"github.com/credo-go/credo/internal/radix"
)

// RewriteRule defines a URL rewrite rule for the [Rewrite] middleware.
type RewriteRule struct {
	// Host restricts the rule to requests matching this exact host (case-insensitive).
	// Empty matches all hosts.
	Host string

	// From is the path pattern to match. Supports Credo route syntax:
	//   - {name}      — matches a single path segment
	//   - {name...}   — matches the rest of the path
	//   - {name:regex} — matches with regex constraint
	// Ignored when Regexp is set.
	From string

	// To is the replacement path. Named placeholders ({name}) are expanded
	// with captured values.
	To string

	// Regexp is an optional pre-compiled regex. When set, From is ignored.
	// Named capture groups (?P<name>...) are used for placeholder expansion.
	Regexp *regexp.Regexp

	// PreserveQuery appends the original query string to the rewritten URL
	// when To does not contain a query string.
	PreserveQuery bool
}

// compiledRule is the internal representation of a RewriteRule after compilation.
type compiledRule struct {
	hostExact    string
	regexp       *regexp.Regexp
	captureNames []string
	to           string
	preserveQ    bool
}

// RewriteConfig defines configuration for the [Rewrite] middleware.
type RewriteConfig struct {
	// Skipper skips rewriting for matching requests.
	Skipper Skipper

	// Rules are evaluated in order; the first match wins.
	Rules []RewriteRule
}

// Rewrite returns middleware that rewrites URL paths before dispatch.
// Rules are evaluated in order; the first match wins. The rewrite is
// transparent to the client (no redirect).
//
// Rewrite is the rule-list shortcut; use [RewriteWithConfig] to set a
// Skipper alongside the rules.
//
// Panics if rules is empty.
func Rewrite(rules ...RewriteRule) credo.Middleware {
	return RewriteWithConfig(RewriteConfig{Rules: rules})
}

// RewriteWithConfig is the config-struct variant of [Rewrite].
//
// Panics if cfg.Rules is empty.
func RewriteWithConfig(cfg RewriteConfig) credo.Middleware {
	if len(cfg.Rules) == 0 {
		panic("credo: middleware.Rewrite requires at least one rule")
	}
	if cfg.Skipper == nil {
		cfg.Skipper = DefaultSkipper
	}
	compiled := compileRules(cfg.Rules)

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if cfg.Skipper(ctx) {
				return next(ctx)
			}
			rewritePath(ctx, compiled)
			return next(ctx)
		}
	}
}

// compileRules converts user-facing RewriteRules into compiledRules.
func compileRules(rules []RewriteRule) []compiledRule {
	out := make([]compiledRule, len(rules))
	for i, r := range rules {
		cr := compiledRule{
			hostExact: internalhost.NormalizeRequest(r.Host),
			to:        r.To,
			preserveQ: r.PreserveQuery,
		}
		if r.Regexp != nil {
			cr.regexp = r.Regexp
			cr.captureNames = r.Regexp.SubexpNames()
		} else {
			re, names := patternToRegexp(r.From)
			cr.regexp = re
			cr.captureNames = names
		}
		out[i] = cr
	}
	return out
}

// patternToRegexp converts a Credo route pattern to a regexp.
// Returns the compiled regexp and ordered capture names.
//
//	{name}       → ([^/]+)
//	{name...}    → (.*)
//	{name:regex} → (regex)
func patternToRegexp(from string) (*regexp.Regexp, []string) {
	var b strings.Builder
	names := []string{""} // index 0 = full match (aligns with FindStringSubmatch)
	b.WriteString("^")

	i := 0
	for i < len(from) {
		if from[i] == '{' {
			end := radix.FindMatchingBrace(from, i)
			if end < 0 {
				// No closing brace — treat as literal.
				b.WriteString(regexp.QuoteMeta(string(from[i])))
				i++
				continue
			}
			inner := from[i+1 : end]

			if strings.HasSuffix(inner, "...") {
				// Catch-all: {name...}
				name := inner[:len(inner)-3]
				names = append(names, name)
				b.WriteString("(.*)")
			} else if name, reStr, ok := strings.Cut(inner, ":"); ok {
				// Regex constraint: {name:regex}
				names = append(names, name)
				b.WriteString("(" + reStr + ")")
			} else {
				// Plain param: {name}
				names = append(names, inner)
				b.WriteString("([^/]+)")
			}
			i = end + 1
		} else {
			b.WriteString(regexp.QuoteMeta(string(from[i])))
			i++
		}
	}
	b.WriteString("$")

	return regexp.MustCompile(b.String()), names
}

// rewritePath applies the first matching rule to the context's URL path.
func rewritePath(ctx *credo.Context, rules []compiledRule) {
	r := ctx.Request()
	path := r.URL.Path
	host := internalhost.NormalizeRequest(r.Host)

	for _, rule := range rules {
		// Host filter.
		if rule.hostExact != "" && rule.hostExact != host {
			continue
		}

		matches := rule.regexp.FindStringSubmatch(path)
		if matches == nil {
			continue
		}

		// Build replacement values map.
		values := make(map[string]string, len(rule.captureNames))
		for i, name := range rule.captureNames {
			if name != "" && i < len(matches) {
				values[name] = matches[i]
			}
		}

		newPath := expandNamedPlaceholders(rule.to, values)

		// Handle query string in To.
		if qi := strings.IndexByte(newPath, '?'); qi >= 0 {
			r.URL.RawQuery = newPath[qi+1:]
			newPath = newPath[:qi]
		} else if rule.preserveQ {
			// Keep original query string.
		} else {
			r.URL.RawQuery = ""
		}

		r.URL.Path = newPath
		r.URL.RawPath = ""
		return // first match wins
	}
}

// expandNamedPlaceholders replaces {name} placeholders in template with
// values from the map.
func expandNamedPlaceholders(template string, values map[string]string) string {
	var b strings.Builder
	i := 0
	for i < len(template) {
		if template[i] == '{' {
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				b.WriteByte(template[i])
				i++
				continue
			}
			name := template[i+1 : i+end]
			if v, ok := values[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString(template[i : i+end+1])
			}
			i += end + 1
		} else {
			b.WriteByte(template[i])
			i++
		}
	}
	return b.String()
}

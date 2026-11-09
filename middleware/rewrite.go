package middleware

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/credo-go/credo"
	internalhost "github.com/credo-go/credo/internal/host"
	internalpattern "github.com/credo-go/credo/internal/pattern"
	"github.com/credo-go/credo/internal/wirepath"
)

// RewriteRule defines a URL rewrite rule for the [Rewrite] middleware.
//
// A rule matches the request path in the canonical wire form the router
// matches, not the decoded URL.Path: an escaped slash ("%2F") is data inside
// a segment and never a separator, and an escaped reserved character
// ("%3B") is not its literal spelling (";"). The target is a wire-form path,
// and each placeholder is replaced with its capture in wire form, so the
// router sees after the rewrite the segments the client sent and decodes a
// captured value once, as it decodes a route parameter.
type RewriteRule struct {
	// Host restricts the rule to requests matching this exact host (case-insensitive).
	// Empty matches all hosts.
	Host string

	// From is the path pattern to match. Supports Credo route syntax and
	// matches as the router matches a route:
	//   - {name}       — matches one segment, up to the byte that follows
	//     the parameter in the pattern or the next slash
	//   - {name:regex} — matches one segment as {name} does, whose whole
	//     decoded value matches regex
	//   - {name...}    — matches the rest of the path, an empty rest
	//     included; it ends the pattern
	// Literal text matches only its literal spelling, as in a route pattern.
	// Ignored when Regexp is set.
	From string

	// To is the replacement path in wire form, optionally followed by a
	// query string: an escape such as "%2F" stays data, and a literal "%" is
	// written "%25". Named placeholders ({name}) are expanded with captured
	// values in wire form. A malformed escape in the path panics.
	To string

	// Regexp is an optional pre-compiled regex, matched against the
	// canonical wire path (escapes of reserved characters and of "%" kept in
	// upper-case hexadecimal, every other escape decoded); unlike a From
	// parameter, a group may span segments. When set, From is ignored. Named capture groups (?P<name>...) are used for placeholder
	// expansion; a group should capture each "%XX" escape whole.
	Regexp *regexp.Regexp

	// PreserveQuery appends the original query string to the rewritten URL
	// when To does not contain a query string.
	PreserveQuery bool
}

// compiledRule is the internal representation of a RewriteRule after
// compilation: a From pattern becomes a matcher that matches as the router
// does, while a Regexp rule keeps the caller's regexp.
type compiledRule struct {
	hostExact    string
	matcher      *internalpattern.Matcher
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

// DefaultRewriteConfig returns the default Rewrite middleware config: no
// Skipper and no rules (a rule list is required). Each call returns a fresh
// value, so callers cannot mutate the package-wide defaults.
func DefaultRewriteConfig() RewriteConfig {
	return RewriteConfig{Skipper: DefaultSkipper}
}

// Rewrite returns middleware that rewrites URL paths before dispatch.
// Rules are evaluated in order; the first match wins. The rewrite is
// transparent to the client (no redirect).
//
// Rewrite follows the package-wide config-struct convention: pass one
// [RewriteConfig] carrying the Rules and an optional Skipper.
//
//	middleware.Rewrite(middleware.RewriteConfig{
//	    Rules: []middleware.RewriteRule{
//	        {From: "/v1/{path...}", To: "/api/v1/{path}"},
//	    },
//	})
//
// Panics if cfg.Rules is empty, a From pattern is malformed or has text
// after its catch-all, or the path of a To target has a malformed escape. A rewrite whose expanded target has a
// malformed escape, which only a Regexp group that splits an escape
// produces, returns an error.
func Rewrite(cfg ...RewriteConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultRewriteConfig(), normalizeRewriteConfig)
	compiled := compileRules(config.Rules)

	return func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			if config.Skipper(c) {
				return next(c)
			}
			if err := rewritePath(c, compiled); err != nil {
				return err
			}
			return next(c)
		}
	}
}

func normalizeRewriteConfig(cfg RewriteConfig) RewriteConfig {
	if len(cfg.Rules) == 0 {
		panic("credo: middleware.Rewrite requires at least one rule")
	}
	if cfg.Skipper == nil {
		cfg.Skipper = DefaultSkipper
	}
	return cfg
}

// compileRules converts user-facing RewriteRules into compiledRules.
func compileRules(rules []RewriteRule) []compiledRule {
	out := make([]compiledRule, len(rules))
	for i, r := range rules {
		target, _, _ := strings.Cut(r.To, "?")
		if at := wirepath.MalformedEscape(target); at >= 0 {
			panic(fmt.Sprintf("credo: middleware.Rewrite rule %d: To %q has a malformed escape at byte %d; "+
				"To is a wire-form path, so write each escape as %%XX with two hex digits and a literal %% as %%25", i, r.To, at))
		}
		cr := compiledRule{
			hostExact: internalhost.NormalizeRequest(r.Host),
			to:        r.To,
			preserveQ: r.PreserveQuery,
		}
		if r.Regexp != nil {
			cr.regexp = r.Regexp
			cr.captureNames = r.Regexp.SubexpNames()
		} else {
			m, err := internalpattern.NewMatcher(r.From)
			if err != nil {
				panic(fmt.Sprintf("credo: middleware.Rewrite rule %d: %v", i, err))
			}
			cr.matcher = m
			cr.captureNames = m.Names()
		}
		out[i] = cr
	}
	return out
}

// rewritePath applies the first matching rule to the request's path. The
// rules match the canonical wire path, and the target is stored in wire form,
// so Path and RawPath stay consistent and an escaped slash stays data.
func rewritePath(c *credo.Context, rules []compiledRule) error {
	r := c.Request()
	path := wirepath.Canonical(r.URL.EscapedPath())
	host := internalhost.NormalizeRequest(r.Host)

	for i, rule := range rules {
		// Host filter.
		if rule.hostExact != "" && rule.hostExact != host {
			continue
		}

		captures, ok := rule.match(path)
		if !ok {
			continue
		}

		// Build replacement values map: each capture is canonical wire text.
		values := make(map[string]string, len(rule.captureNames))
		for j, name := range rule.captureNames {
			if name != "" && j < len(captures) {
				values[name] = captures[j]
			}
		}

		target, query, hasQuery := strings.Cut(expandNamedPlaceholders(rule.to, values), "?")
		if err := wirepath.SetPath(r.URL, target); err != nil {
			return fmt.Errorf("credo: middleware.Rewrite rule %d: the rewritten path %q has a malformed escape (%w); "+
				"a capture group splits a percent-escape, so make each group capture whole %%XX escapes", i, target, err)
		}
		switch {
		case hasQuery:
			r.URL.RawQuery = query
		case !rule.preserveQ:
			r.URL.RawQuery = ""
		}
		return nil // first match wins
	}
	return nil
}

// match matches the canonical wire path, returning the captures aligned with
// captureNames: the pattern's parameters, or a Regexp rule's groups after the
// whole match.
func (rule *compiledRule) match(path string) ([]string, bool) {
	if rule.matcher != nil {
		return rule.matcher.Match(path)
	}
	matches := rule.regexp.FindStringSubmatch(path)
	return matches, matches != nil
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

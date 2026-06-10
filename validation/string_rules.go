package validation

import (
	"fmt"
	"net/url"
	"regexp"
	"unicode/utf8"
)

// Compiled regexes for string validation rules.
var (
	// emailRegex is a simplified RFC 5322 pattern.
	emailRegex = regexp.MustCompile(`^[a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

	// uuidHyphenRegex matches UUIDs with hyphens.
	uuidHyphenRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

	// uuidPlainRegex matches UUIDs without hyphens.
	uuidPlainRegex = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// Required creates a [Rule] that fails if the value is the zero value for
// type T. Works with any comparable type: strings, ints, bools, etc.
func Required[T comparable]() Rule[T] {
	return &requiredRule[T]{}
}

type requiredRule[T comparable] struct{}

func (r *requiredRule[T]) Validate(value T) error {
	var zero T
	if value == zero {
		return newRuleError("required", "is required", nil)
	}
	return nil
}

// Email creates a [Rule] that validates email format.
// Empty strings pass validation — use [Required] to enforce non-empty.
func Email() Rule[string] {
	return &emailRule{}
}

type emailRule struct{}

func (r *emailRule) Validate(value string) error {
	if value == "" {
		return nil
	}
	if !emailRegex.MatchString(value) {
		return newRuleError("email", "must be a valid email address", nil)
	}
	return nil
}

// URL creates a [Rule] that validates URL format. The URL must have both
// a scheme and a host.
// Empty strings pass validation — use [Required] to enforce non-empty.
func URL() Rule[string] {
	return &urlRule{}
}

type urlRule struct{}

func (r *urlRule) Validate(value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return newRuleError("url", "must be a valid URL", nil)
	}
	return nil
}

// UUID creates a [Rule] that validates UUID format.
// Accepts both hyphenated (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx) and
// plain (xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx) formats.
// Empty strings pass validation — use [Required] to enforce non-empty.
func UUID() Rule[string] {
	return &uuidRule{}
}

type uuidRule struct{}

func (r *uuidRule) Validate(value string) error {
	if value == "" {
		return nil
	}
	if !uuidHyphenRegex.MatchString(value) && !uuidPlainRegex.MatchString(value) {
		return newRuleError("uuid", "must be a valid UUID", nil)
	}
	return nil
}

// Regex creates a [Rule] that validates the value matches the given pattern.
// The pattern must be pre-compiled via [regexp.MustCompile] or [regexp.Compile].
// Empty strings pass validation — use [Required] to enforce non-empty.
// Panics if pattern is nil — a nil pattern is a programming error, never a
// runtime condition.
func Regex(pattern *regexp.Regexp) Rule[string] {
	if pattern == nil {
		panic("validation: Regex requires a non-nil pattern")
	}
	return &regexRule{pattern: pattern}
}

type regexRule struct {
	pattern *regexp.Regexp
}

func (r *regexRule) Validate(value string) error {
	if value == "" {
		return nil
	}
	if !r.pattern.MatchString(value) {
		return newRuleError("regex", "must match the required pattern", nil)
	}
	return nil
}

// Length creates a [Rule] that validates string length (rune count) is
// between min and max (inclusive). Uses [utf8.RuneCountInString] for
// correct Unicode handling.
// Empty strings pass validation — use [Required] to enforce non-empty.
func Length(min, max int) Rule[string] {
	return &lengthRule{min: min, max: max}
}

type lengthRule struct {
	min, max int
}

func (r *lengthRule) Validate(value string) error {
	if value == "" {
		return nil
	}
	l := utf8.RuneCountInString(value)
	if l < r.min || l > r.max {
		return newRuleError("length",
			fmt.Sprintf("must be between %d and %d characters", r.min, r.max),
			map[string]any{"min": r.min, "max": r.max},
		)
	}
	return nil
}

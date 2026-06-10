// Copyright (c) 2014 Nick Snyder https://github.com/nicksnyder/go-i18n
// License: MIT (https://github.com/nicksnyder/go-i18n/blob/main/LICENSE)
//
// Adapted for the Credo framework (github.com/credo-go/credo).
// The operand decomposition derives from go-i18n's plural.NewOperands;
// form selection is delegated to golang.org/x/text CLDR data.

package i18n

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

// pluralForm returns the CLDR cardinal plural form of count for the given
// language tag, using golang.org/x/text/feature/plural CLDR data.
//
// count may be any signed integer kind, an integral float (3.0 — common
// with JSON-decoded numbers), or a decimal string. A string makes the
// visible fraction digits explicit ("1.50" → two visible digits), which
// matters for languages whose rules inspect them. Non-integral floats must
// be pre-formatted into a string, because a float64 carries no information
// about how many fraction digits are visible.
func pluralForm(tag language.Tag, count any) (plural.Form, error) {
	i, v, w, f, t, err := operands(count)
	if err != nil {
		return plural.Other, err
	}
	return plural.Cardinal.MatchPlural(tag, i, v, w, f, t), nil
}

// operands decomposes count into the CLDR plural operands expected by
// x/text's MatchPlural: i (integer digits), v/w (visible fraction digit
// counts with/without trailing zeros), f/t (visible fraction digit values
// with/without trailing zeros).
func operands(count any) (i, v, w, f, t int, err error) {
	switch n := count.(type) {
	case int:
		return intOperands(int64(n))
	case int8:
		return intOperands(int64(n))
	case int16:
		return intOperands(int64(n))
	case int32:
		return intOperands(int64(n))
	case int64:
		return intOperands(n)
	case float32:
		return floatOperands(float64(n))
	case float64:
		return floatOperands(n)
	case string:
		return stringOperands(n)
	default:
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid count type %T; expected integer, integral float, or decimal string", count)
	}
}

func intOperands(n int64) (i, v, w, f, t int, err error) {
	if n < 0 {
		n = -n
	}
	return int(n), 0, 0, 0, 0, nil
}

// floatOperands accepts only integral floats: a float64 carries no
// visible-fraction-digit information, so 1.5 must be passed as "1.5".
func floatOperands(n float64) (i, v, w, f, t int, err error) {
	if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
		return 0, 0, 0, 0, 0, fmt.Errorf("non-integral float count %v; format it into a string (e.g. %q)", n, strconv.FormatFloat(n, 'f', -1, 64))
	}
	return intOperands(int64(n))
}

// stringOperands parses a plain decimal string ("12", "-3", "1.50").
// Exponent notation is not supported.
func stringOperands(s string) (i, v, w, f, t int, err error) {
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return 0, 0, 0, 0, 0, fmt.Errorf("empty count string")
	}

	intPart, fraction, _ := strings.Cut(s, ".")
	iv, err := strconv.Atoi(intPart)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid count string %q: %w", s, err)
	}
	i = iv

	if fraction == "" {
		return i, 0, 0, 0, 0, nil
	}

	v = len(fraction)
	trimmed := strings.TrimRight(fraction, "0")
	w = len(trimmed)

	if f, err = strconv.Atoi(fraction); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid count string %q: %w", s, err)
	}
	if w > 0 {
		if t, err = strconv.Atoi(trimmed); err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid count string %q: %w", s, err)
		}
	}
	return i, v, w, f, t, nil
}

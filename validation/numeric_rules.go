package validation

import (
	"cmp"
	"fmt"
)

// Min creates a [Rule] that validates the value is >= threshold.
func Min[T cmp.Ordered](threshold T) Rule[T] {
	return funcRule[T](func(value T) error {
		if value < threshold {
			return newRuleError("min",
				fmt.Sprintf("must be at least %v", threshold),
				map[string]any{"min": threshold},
			)
		}
		return nil
	})
}

// Max creates a [Rule] that validates the value is <= threshold.
func Max[T cmp.Ordered](threshold T) Rule[T] {
	return funcRule[T](func(value T) error {
		if value > threshold {
			return newRuleError("max",
				fmt.Sprintf("must be at most %v", threshold),
				map[string]any{"max": threshold},
			)
		}
		return nil
	})
}

// Between creates a [Rule] that validates the value is between min and max
// (inclusive).
func Between[T cmp.Ordered](min, max T) Rule[T] {
	return funcRule[T](func(value T) error {
		if value < min || value > max {
			return newRuleError("between",
				fmt.Sprintf("must be between %v and %v", min, max),
				map[string]any{"min": min, "max": max},
			)
		}
		return nil
	})
}

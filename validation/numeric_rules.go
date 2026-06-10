package validation

import (
	"cmp"
	"fmt"
)

// Min creates a [Rule] that validates the value is >= threshold.
func Min[T cmp.Ordered](threshold T) Rule[T] {
	return &minRule[T]{threshold: threshold}
}

type minRule[T cmp.Ordered] struct {
	threshold T
}

func (r *minRule[T]) Validate(value T) error {
	if value < r.threshold {
		return newRuleError("min",
			fmt.Sprintf("must be at least %v", r.threshold),
			map[string]any{"min": r.threshold},
		)
	}
	return nil
}

// Max creates a [Rule] that validates the value is <= threshold.
func Max[T cmp.Ordered](threshold T) Rule[T] {
	return &maxRule[T]{threshold: threshold}
}

type maxRule[T cmp.Ordered] struct {
	threshold T
}

func (r *maxRule[T]) Validate(value T) error {
	if value > r.threshold {
		return newRuleError("max",
			fmt.Sprintf("must be at most %v", r.threshold),
			map[string]any{"max": r.threshold},
		)
	}
	return nil
}

// Between creates a [Rule] that validates the value is between min and max
// (inclusive).
func Between[T cmp.Ordered](min, max T) Rule[T] {
	return &betweenRule[T]{min: min, max: max}
}

type betweenRule[T cmp.Ordered] struct {
	min, max T
}

func (r *betweenRule[T]) Validate(value T) error {
	if value < r.min || value > r.max {
		return newRuleError("between",
			fmt.Sprintf("must be between %v and %v", r.min, r.max),
			map[string]any{"min": r.min, "max": r.max},
		)
	}
	return nil
}

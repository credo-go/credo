package validation

import (
	"fmt"
	"time"
)

// DateBefore creates a [Rule] that validates the time is before the given
// threshold. Zero time passes validation — use [Required] to enforce non-zero.
func DateBefore(threshold time.Time) Rule[time.Time] {
	return funcRule[time.Time](func(value time.Time) error {
		if value.IsZero() {
			return nil
		}
		if !value.Before(threshold) {
			return newRuleError("date_before",
				fmt.Sprintf("must be before %s", threshold.Format(time.RFC3339)),
				map[string]any{"threshold": threshold},
			)
		}
		return nil
	})
}

// DateAfter creates a [Rule] that validates the time is after the given
// threshold. Zero time passes validation — use [Required] to enforce non-zero.
func DateAfter(threshold time.Time) Rule[time.Time] {
	return funcRule[time.Time](func(value time.Time) error {
		if value.IsZero() {
			return nil
		}
		if !value.After(threshold) {
			return newRuleError("date_after",
				fmt.Sprintf("must be after %s", threshold.Format(time.RFC3339)),
				map[string]any{"threshold": threshold},
			)
		}
		return nil
	})
}

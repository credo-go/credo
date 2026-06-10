package validation

import (
	"fmt"
	"time"
)

// DateBefore creates a [Rule] that validates the time is before the given
// threshold. Zero time passes validation — use [Required] to enforce non-zero.
func DateBefore(threshold time.Time) Rule[time.Time] {
	return &dateBeforeRule{threshold: threshold}
}

type dateBeforeRule struct {
	threshold time.Time
}

func (r *dateBeforeRule) Validate(value time.Time) error {
	if value.IsZero() {
		return nil
	}
	if !value.Before(r.threshold) {
		return newRuleError("date_before",
			fmt.Sprintf("must be before %s", r.threshold.Format(time.RFC3339)),
			map[string]any{"threshold": r.threshold},
		)
	}
	return nil
}

// DateAfter creates a [Rule] that validates the time is after the given
// threshold. Zero time passes validation — use [Required] to enforce non-zero.
func DateAfter(threshold time.Time) Rule[time.Time] {
	return &dateAfterRule{threshold: threshold}
}

type dateAfterRule struct {
	threshold time.Time
}

func (r *dateAfterRule) Validate(value time.Time) error {
	if value.IsZero() {
		return nil
	}
	if !value.After(r.threshold) {
		return newRuleError("date_after",
			fmt.Sprintf("must be after %s", r.threshold.Format(time.RFC3339)),
			map[string]any{"threshold": r.threshold},
		)
	}
	return nil
}

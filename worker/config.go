package worker

import (
	"cmp"
	"errors"
	"fmt"
	"time"

	"github.com/credo-go/credo"
)

// DefaultMinRestartDelay is the default first and shortest wait before a
// continuous worker restarts ([Restart.MinDelay]).
const DefaultMinRestartDelay = 3 * time.Second

// DefaultMaxRestartDelay is the default cap on the restart delay of a
// continuous worker ([Restart.MaxDelay]).
const DefaultMaxRestartDelay = time.Minute

// ErrRunTimeout is the cancellation cause of a run that exceeded its
// [ScheduledConfig.RunTimeout] budget. Inside Run,
//
//	errors.Is(context.Cause(ctx), worker.ErrRunTimeout)
//
// tells the budget running out from the worker being stopped. A run cut short
// by the timeout is recorded as a failure wrapping ErrRunTimeout, even when
// Run returns nil.
var ErrRunTimeout = errors.New("worker: run timed out")

// ContinuousConfig configures a continuous worker. Zero means the default in
// every field.
type ContinuousConfig struct {
	// Tier is the drain tier of the worker's component; zero is
	// credo.TierInternal: the worker stops after the HTTP drain, before the
	// components it depends on. A worker that consumes an external queue
	// declares credo.TierIngress, so it stops taking work with the listener.
	Tier credo.Tier `json:"tier"`
	// Restart is the restart policy after a failed run.
	Restart Restart `json:"restart"`
	// UnreadyWhenFailed reports the instance unready once the worker is
	// failed.
	UnreadyWhenFailed bool `json:"unready_when_failed"`
}

// ScheduledConfig configures a scheduled worker. Zero means the default in
// every field.
type ScheduledConfig struct {
	// Tier is the drain tier of the worker's component; zero is
	// credo.TierIngress: the worker stops with the listener, so no run starts
	// during the drain.
	Tier credo.Tier `json:"tier"`
	// RunOnStart adds one run as soon as the worker starts, before the first
	// computed activation.
	RunOnStart bool `json:"run_on_start"`
	// RunTimeout bounds every run, the RunOnStart run included: the run
	// context is cancelled with the cause [ErrRunTimeout], and the run is a
	// failure whatever Run returns. The timeout is cooperative; it never
	// abandons the goroutine. Zero means no timeout.
	RunTimeout time.Duration `json:"run_timeout"`
	// MaxConsecutiveFailures makes the worker failed after that many failed
	// runs in a row; zero means unlimited.
	MaxConsecutiveFailures int `json:"max_consecutive_failures"`
	// UnreadyWhenFailed reports the instance unready once the worker is
	// failed.
	UnreadyWhenFailed bool `json:"unready_when_failed"`
	// UnreadyUntilFirstSuccess keeps the instance unready until the worker's
	// first run succeeds; once met it stays met. Pair it with RunOnStart
	// unless waiting for the first activation is intended.
	UnreadyUntilFirstSuccess bool `json:"unready_until_first_success"`
	// UnreadyAfterSuccessAge reports the instance unready when the last
	// success is older than this; zero turns the check off. It is not applied
	// before the first success.
	UnreadyAfterSuccessAge time.Duration `json:"unready_after_success_age"`
}

// Restart is the restart policy of a continuous worker. After a failed run
// the worker waits and runs again; the wait starts at MinDelay and backs off,
// with jitter, up to MaxDelay while failures repeat:
//
//	ceiling = min(MinDelay × 2^(k−1), MaxDelay)
//	delay   = uniform in [max(MinDelay, ceiling/2), ceiling]
//
// where k counts the failures of the current sequence, so the first delay is
// exactly MinDelay. A run that lasted at least MaxDelay starts a new
// sequence. Equal delays give a fixed delay.
type Restart struct {
	// Disabled makes the first failed run terminal. It cannot be combined
	// with any other field.
	Disabled bool `json:"disabled"`
	// Limit allows the first run plus at most Limit restarts; zero means
	// unlimited.
	Limit int `json:"limit"`
	// MinDelay is the first and shortest wait; zero is
	// [DefaultMinRestartDelay].
	MinDelay time.Duration `json:"min_delay"`
	// MaxDelay is the longest wait; zero is the larger of
	// [DefaultMaxRestartDelay] and MinDelay. A positive MaxDelay below
	// MinDelay is rejected.
	MaxDelay time.Duration `json:"max_delay"`
}

// oneConfig returns the single configuration of a registration, or the zero
// value; it panics, naming call, when more than one is given.
func oneConfig[C any](call string, cfgs []C) C {
	var zero C
	switch len(cfgs) {
	case 0:
		return zero
	case 1:
		return cfgs[0]
	}
	panic(fmt.Sprintf("worker: %s: %d configurations given; pass at most one %T", call, len(cfgs), zero))
}

// resolveTier returns tier, or def when tier is zero; it panics on a value
// that is neither.
func resolveTier(call string, tier, def credo.Tier) credo.Tier {
	switch tier {
	case 0:
		return def
	case credo.TierIngress, credo.TierInternal:
		return tier
	}
	panic(fmt.Sprintf("worker: %s: %s is not a tier; use credo.TierIngress, credo.TierInternal, "+
		"or zero for the default", call, tier))
}

// resolveContinuous validates cfg and resolves every zero field to its
// default; it panics, naming call, on a rejected value.
func resolveContinuous(call string, cfg ContinuousConfig) ContinuousConfig {
	cfg.Tier = resolveTier(call, cfg.Tier, credo.TierInternal)
	cfg.Restart = resolveRestart(call, cfg.Restart)
	return cfg
}

// resolveRestart resolves MinDelay first, then raises a zero MaxDelay to it.
// A positive MaxDelay below the resolved MinDelay is rejected rather than
// lowering an omitted floor, which would tighten the retry loop.
func resolveRestart(call string, r Restart) Restart {
	switch {
	case r.Disabled && (r.Limit != 0 || r.MinDelay != 0 || r.MaxDelay != 0):
		panic(fmt.Sprintf("worker: %s: Restart.Disabled beside Restart.Limit, MinDelay or MaxDelay; "+
			"a worker without restarts has no limit or delays, so set Disabled alone", call))
	case r.Limit < 0:
		panic(fmt.Sprintf("worker: %s: Restart.Limit %d is negative; use 0 for unlimited restarts "+
			"or a positive limit", call, r.Limit))
	case r.MinDelay < 0:
		panic(fmt.Sprintf("worker: %s: Restart.MinDelay %s is negative; use 0 for the default %s "+
			"or a positive delay", call, r.MinDelay, DefaultMinRestartDelay))
	case r.MaxDelay < 0:
		panic(fmt.Sprintf("worker: %s: Restart.MaxDelay %s is negative; use 0 for the default "+
			"or a positive cap", call, r.MaxDelay))
	case r.Disabled:
		return Restart{Disabled: true}
	}
	minDelay := cmp.Or(r.MinDelay, DefaultMinRestartDelay)
	switch {
	case r.MaxDelay == 0:
		r.MaxDelay = max(DefaultMaxRestartDelay, minDelay)
	case r.MaxDelay < minDelay && r.MinDelay == 0:
		panic(fmt.Sprintf("worker: %s: Restart.MaxDelay %s is below MinDelay %s (the default); "+
			"set MinDelay to at most %s, or raise MaxDelay", call, r.MaxDelay, minDelay, r.MaxDelay))
	case r.MaxDelay < minDelay:
		panic(fmt.Sprintf("worker: %s: Restart.MaxDelay %s is below MinDelay %s; "+
			"lower MinDelay to at most %s, or raise MaxDelay", call, r.MaxDelay, minDelay, r.MaxDelay))
	}
	r.MinDelay = minDelay
	return r
}

// resolveScheduled validates cfg and resolves its tier; it panics, naming
// call, on a rejected value.
func resolveScheduled(call string, cfg ScheduledConfig) ScheduledConfig {
	cfg.Tier = resolveTier(call, cfg.Tier, credo.TierIngress)
	switch {
	case cfg.RunTimeout < 0:
		panic(fmt.Sprintf("worker: %s: ScheduledConfig.RunTimeout %s is negative; use 0 for no timeout "+
			"or a positive budget", call, cfg.RunTimeout))
	case cfg.MaxConsecutiveFailures < 0:
		panic(fmt.Sprintf("worker: %s: ScheduledConfig.MaxConsecutiveFailures %d is negative; use 0 for "+
			"unlimited failures or a positive limit", call, cfg.MaxConsecutiveFailures))
	case cfg.UnreadyAfterSuccessAge < 0:
		panic(fmt.Sprintf("worker: %s: ScheduledConfig.UnreadyAfterSuccessAge %s is negative; use 0 to "+
			"turn the check off or a positive age", call, cfg.UnreadyAfterSuccessAge))
	}
	return cfg
}

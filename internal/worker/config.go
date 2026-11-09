package worker

import (
	"cmp"
	"errors"
	"fmt"
	"time"
)

// DefaultMinRestartDelay is the default first and shortest wait before a
// continuous worker restarts; credo.DefaultMinRestartDelay has the same
// value.
const DefaultMinRestartDelay = 3 * time.Second

// DefaultMaxRestartDelay is the default cap on the restart delay of a
// continuous worker; credo.DefaultMaxRestartDelay has the same value.
const DefaultMaxRestartDelay = time.Minute

// ErrRunTimeout is the cancellation cause of a run that exceeded its
// ScheduledConfig.RunTimeout budget; the root package exports it as
// credo.ErrWorkerRunTimeout. A run cut short by the timeout is recorded as a
// failure wrapping ErrRunTimeout, even when Run returns nil.
var ErrRunTimeout = errors.New("worker: run timed out")

// ContinuousConfig is the engine's half of credo.ContinuousConfig: its tier
// is the root package's to resolve. Zero means the default in every field.
type ContinuousConfig struct {
	// Restart is the restart policy after a failed run.
	Restart Restart
	// UnreadyWhenFailed reports the instance unready once the worker is
	// failed.
	UnreadyWhenFailed bool
}

// ScheduledConfig is the engine's half of credo.ScheduledConfig, whose
// fields document these.
type ScheduledConfig struct {
	RunOnStart               bool
	RunTimeout               time.Duration
	MaxConsecutiveFailures   int
	UnreadyWhenFailed        bool
	UnreadyUntilFirstSuccess bool
	UnreadyAfterSuccessAge   time.Duration
}

// Restart is the engine's credo.RestartPolicy, whose fields document these.
type Restart struct {
	Disabled bool
	Limit    int
	MinDelay time.Duration
	MaxDelay time.Duration
}

// resolveContinuous validates cfg and resolves every zero field to its
// default; it panics, naming call, on a rejected value.
func resolveContinuous(call string, cfg ContinuousConfig) ContinuousConfig {
	cfg.Restart = resolveRestart(call, cfg.Restart)
	return cfg
}

// resolveRestart resolves MinDelay first, then raises a zero MaxDelay to it.
// A positive MaxDelay below the resolved MinDelay is rejected rather than
// lowering an omitted floor, which would tighten the retry loop.
func resolveRestart(call string, r Restart) Restart {
	switch {
	case r.Disabled && (r.Limit != 0 || r.MinDelay != 0 || r.MaxDelay != 0):
		panic(fmt.Sprintf("credo: %s: Restart.Disabled beside Restart.Limit, MinDelay or MaxDelay; "+
			"a worker without restarts has no limit or delays, so set Disabled alone", call))
	case r.Limit < 0:
		panic(fmt.Sprintf("credo: %s: Restart.Limit %d is negative; use 0 for unlimited restarts "+
			"or a positive limit", call, r.Limit))
	case r.MinDelay < 0:
		panic(fmt.Sprintf("credo: %s: Restart.MinDelay %s is negative; use 0 for the default %s "+
			"or a positive delay", call, r.MinDelay, DefaultMinRestartDelay))
	case r.MaxDelay < 0:
		panic(fmt.Sprintf("credo: %s: Restart.MaxDelay %s is negative; use 0 for the default "+
			"or a positive cap", call, r.MaxDelay))
	case r.Disabled:
		return Restart{Disabled: true}
	}
	minDelay := cmp.Or(r.MinDelay, DefaultMinRestartDelay)
	switch {
	case r.MaxDelay == 0:
		r.MaxDelay = max(DefaultMaxRestartDelay, minDelay)
	case r.MaxDelay < minDelay && r.MinDelay == 0:
		panic(fmt.Sprintf("credo: %s: Restart.MaxDelay %s is below MinDelay %s (the default); "+
			"set MinDelay to at most %s, or raise MaxDelay", call, r.MaxDelay, minDelay, r.MaxDelay))
	case r.MaxDelay < minDelay:
		panic(fmt.Sprintf("credo: %s: Restart.MaxDelay %s is below MinDelay %s; "+
			"lower MinDelay to at most %s, or raise MaxDelay", call, r.MaxDelay, minDelay, r.MaxDelay))
	}
	r.MinDelay = minDelay
	return r
}

// resolveScheduled validates cfg; it panics, naming call, on a rejected
// value.
func resolveScheduled(call string, cfg ScheduledConfig) ScheduledConfig {
	switch {
	case cfg.RunTimeout < 0:
		panic(fmt.Sprintf("credo: %s: ScheduledConfig.RunTimeout %s is negative; use 0 for no timeout "+
			"or a positive budget", call, cfg.RunTimeout))
	case cfg.MaxConsecutiveFailures < 0:
		panic(fmt.Sprintf("credo: %s: ScheduledConfig.MaxConsecutiveFailures %d is negative; use 0 for "+
			"unlimited failures or a positive limit", call, cfg.MaxConsecutiveFailures))
	case cfg.UnreadyAfterSuccessAge < 0:
		panic(fmt.Sprintf("credo: %s: ScheduledConfig.UnreadyAfterSuccessAge %s is negative; use 0 to "+
			"turn the check off or a positive age", call, cfg.UnreadyAfterSuccessAge))
	}
	return cfg
}

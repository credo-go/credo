package credo

import (
	"context"
	"time"

	internalworker "github.com/credo-go/credo/internal/worker"
)

// Worker is a background task run by the App. The name that identifies it is
// given at registration ([Workers.Continuous], [Workers.Scheduled]), not by
// the worker itself. A func(context.Context) error is registered as it is,
// and a function whose other parameters the container supplies, or a
// constructor of a worker, is registered in its own form.
//
// A continuous worker's Run must stay active until ctx is cancelled; a
// scheduled worker's Run performs one activation and returns.
type Worker interface {
	// Run executes the worker's logic.
	Run(ctx context.Context) error
}

// WorkerKind reports whether a worker runs continuously or on a schedule.
type WorkerKind string

const (
	// WorkerKindContinuous is a worker whose Run lives until its component
	// stops.
	WorkerKindContinuous WorkerKind = "continuous"
	// WorkerKindScheduled is a worker that runs once per cron activation.
	WorkerKindScheduled WorkerKind = "scheduled"
)

// WorkerStatus is a worker's lifecycle state.
type WorkerStatus string

const (
	// WorkerStatusPending means the worker's component has not started, or a
	// continuous worker has not yet been admitted to its first run.
	WorkerStatusPending WorkerStatus = "pending"
	// WorkerStatusRunning means a run has been admitted and is executing.
	WorkerStatusRunning WorkerStatus = "running"
	// WorkerStatusBackoff means a continuous worker is waiting to restart
	// after a failure: unhealthy, recovering.
	WorkerStatusBackoff WorkerStatus = "backoff"
	// WorkerStatusWaiting means a scheduled worker is waiting for its next
	// activation, whatever its last run did.
	WorkerStatusWaiting WorkerStatus = "waiting"
	// WorkerStatusStopped means the loop ended because the worker's component
	// was shut down; the worker will not run again.
	WorkerStatusStopped WorkerStatus = "stopped"
	// WorkerStatusFailed means a positive failure limit was exhausted,
	// restarts are disabled and a run failed, or the schedule has no future
	// activation. It is permanent until the application restarts.
	WorkerStatusFailed WorkerStatus = "failed"
)

// WorkerInfo is a point-in-time snapshot of a worker: its identity, its
// resolved configuration and its live state.
//
// WorkerInfo is shaped for direct JSON encoding, for example from an admin
// endpoint: field names are snake_case, durations encode as integer
// nanoseconds under Credo's response profile, and schedule, continuous,
// scheduled, last_started_at, last_succeeded_at and last_error are omitted
// while zero.
type WorkerInfo struct {
	Name string     `json:"name"`
	Kind WorkerKind `json:"kind"`
	// Schedule is the expression of a scheduled worker, as registered; empty
	// for a continuous worker.
	Schedule string `json:"schedule,omitzero"`
	// Continuous is the resolved configuration of a continuous worker; nil
	// for a scheduled one. Each snapshot holds its own copy.
	Continuous *ContinuousConfig `json:"continuous,omitzero"`
	// Scheduled is the resolved configuration of a scheduled worker; nil for
	// a continuous one. Each snapshot holds its own copy.
	Scheduled *ScheduledConfig `json:"scheduled,omitzero"`

	Status WorkerStatus `json:"status"`
	// Restarts counts the restarts of a continuous worker that started.
	Restarts int64 `json:"restarts"`
	// ConsecutiveFailures counts the failed runs of a scheduled worker since
	// its last success.
	ConsecutiveFailures int64 `json:"consecutive_failures"`
	// LastStartedAt is the start time of the most recently admitted run.
	LastStartedAt time.Time `json:"last_started_at,omitzero"`
	// LastSucceededAt is the completion time of the last successful run of a
	// scheduled worker; always zero for a continuous worker.
	LastSucceededAt time.Time `json:"last_succeeded_at,omitzero"`
	// LastError is the error of the most recent failed run, without any stack
	// trace; a successful scheduled run clears it, a graceful stop does not.
	LastError string `json:"last_error,omitzero"`
}

// WorkerRun is the execution metadata of one run, carried by the context
// passed to Run.
type WorkerRun struct {
	// Worker is the registration name.
	Worker string
	// ID is the run's identifier, equal to the run_id of the App's log lines
	// for that run.
	ID string
	// ScheduledAt is the intended activation of a scheduled run; zero for
	// continuous runs and for the RunOnStart run.
	ScheduledAt time.Time
}

// CurrentWorkerRun returns the metadata of the run that ctx belongs to, and
// false with a zero WorkerRun for a context that is not a run context.
func CurrentWorkerRun(ctx context.Context) (WorkerRun, bool) {
	run, ok := internalworker.CurrentRun(ctx)
	if !ok {
		return WorkerRun{}, false
	}
	return WorkerRun{Worker: run.Worker, ID: run.ID, ScheduledAt: run.ScheduledAt}, true
}

// ErrWorkerRunTimeout is the cancellation cause of a run that exceeded its
// [ScheduledConfig.RunTimeout] budget. Inside Run,
//
//	errors.Is(context.Cause(ctx), credo.ErrWorkerRunTimeout)
//
// tells the budget running out from the worker being stopped. A run cut short
// by the timeout is recorded as a failure wrapping ErrWorkerRunTimeout, even
// when Run returns nil.
var ErrWorkerRunTimeout = internalworker.ErrRunTimeout

// DefaultMinRestartDelay is the default first and shortest wait before a
// continuous worker restarts ([RestartPolicy.MinDelay]).
const DefaultMinRestartDelay = 3 * time.Second

// DefaultMaxRestartDelay is the default cap on the restart delay of a
// continuous worker ([RestartPolicy.MaxDelay]).
const DefaultMaxRestartDelay = time.Minute

// ContinuousConfig configures a continuous worker. Zero means the default in
// every field.
type ContinuousConfig struct {
	// Tier is the drain tier of the worker's component; zero is
	// TierInternal: the worker stops after the HTTP drain, before the
	// components it depends on. A worker that consumes an external queue
	// declares TierIngress, so it stops taking work with the listener.
	Tier Tier `json:"tier"`
	// Restart is the restart policy after a failed run.
	Restart RestartPolicy `json:"restart"`
	// UnreadyWhenFailed reports the instance unready once the worker is
	// failed.
	UnreadyWhenFailed bool `json:"unready_when_failed"`
}

// ScheduledConfig configures a scheduled worker. Zero means the default in
// every field.
type ScheduledConfig struct {
	// Tier is the drain tier of the worker's component; zero is TierIngress:
	// the worker stops with the listener, so no run starts during the drain.
	Tier Tier `json:"tier"`
	// RunOnStart adds one run as soon as the worker starts, before the first
	// computed activation.
	RunOnStart bool `json:"run_on_start"`
	// RunTimeout bounds every run, the RunOnStart run included: the run
	// context is cancelled with the cause [ErrWorkerRunTimeout], and the run
	// is a failure whatever Run returns. The timeout is cooperative; it never
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

// RestartPolicy is the restart policy of a continuous worker. After a failed
// run the worker waits and runs again; the wait starts at MinDelay and backs
// off, with jitter, up to MaxDelay while failures repeat:
//
//	ceiling = min(MinDelay × 2^(k−1), MaxDelay)
//	delay   = uniform in [max(MinDelay, ceiling/2), ceiling]
//
// where k counts the failures of the current sequence, so the first delay is
// exactly MinDelay. A run that lasted at least MaxDelay starts a new
// sequence. Equal delays give a fixed delay.
type RestartPolicy struct {
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

// Schedule is a compiled cron schedule.
type Schedule struct {
	s *internalworker.Schedule
}

// Next returns the next fire time after now, or the zero time when the
// schedule has none.
func (s *Schedule) Next(now time.Time) time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.s.Next(now)
}

// String returns the expression the schedule was parsed from.
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	return s.s.String()
}

// ParseSchedule parses a cron expression into a compiled schedule.
//
// The supported syntax is standard 5-field Vixie cron — minute, hour,
// day-of-month, month, day-of-week — with lists ("1,15"), ranges ("1-5"),
// steps ("*/10", "8-18/2"), month and weekday names ("jan", "sat"), "?" as
// an alias for "*", and 7 accepted as Sunday. The descriptors @hourly,
// @daily (alias @midnight), @weekly, @monthly, and "@every <duration>" are
// also accepted. Schedules are evaluated in the server's local time zone
// and fire at second 0 of the matching minute; for sub-minute periods use
// "@every <duration>", whose duration must be a positive whole number of
// seconds ("@every 90s", "@every 1h30m"; "@every 1500ms" is rejected).
//
// As in crontab(5), when both the day-of-month and day-of-week fields are
// restricted (neither is "*"), the schedule fires when EITHER matches:
// "0 0 13 * fri" runs on the 13th of the month AND on every Friday. Note
// that a step applied to "*" (e.g. "*/2" in day-of-month) counts as
// restricted for this rule.
func ParseSchedule(expr string) (*Schedule, error) {
	s, err := internalworker.ParseSchedule(expr)
	if err != nil {
		return nil, err
	}
	return &Schedule{s: s}, nil
}

// The conversions at the boundary with the engine.

func (cfg ContinuousConfig) engine() internalworker.ContinuousConfig {
	return internalworker.ContinuousConfig{Restart: cfg.Restart.engine(), UnreadyWhenFailed: cfg.UnreadyWhenFailed}
}

func (r RestartPolicy) engine() internalworker.Restart {
	return internalworker.Restart{Disabled: r.Disabled, Limit: r.Limit, MinDelay: r.MinDelay, MaxDelay: r.MaxDelay}
}

func (cfg ScheduledConfig) engine() internalworker.ScheduledConfig {
	return internalworker.ScheduledConfig{
		RunOnStart:               cfg.RunOnStart,
		RunTimeout:               cfg.RunTimeout,
		MaxConsecutiveFailures:   cfg.MaxConsecutiveFailures,
		UnreadyWhenFailed:        cfg.UnreadyWhenFailed,
		UnreadyUntilFirstSuccess: cfg.UnreadyUntilFirstSuccess,
		UnreadyAfterSuccessAge:   cfg.UnreadyAfterSuccessAge,
	}
}

// workerInfo converts an engine snapshot, adding the tier the registry
// resolved.
func workerInfo(in internalworker.Info, tier Tier) WorkerInfo {
	info := WorkerInfo{
		Name:                in.Name,
		Kind:                WorkerKind(in.Kind),
		Schedule:            in.Schedule,
		Status:              WorkerStatus(in.Status),
		Restarts:            in.Restarts,
		ConsecutiveFailures: in.ConsecutiveFailures,
		LastStartedAt:       in.LastStartedAt,
		LastSucceededAt:     in.LastSucceededAt,
		LastError:           in.LastError,
	}
	if c := in.Continuous; c != nil {
		info.Continuous = &ContinuousConfig{
			Tier: tier,
			Restart: RestartPolicy{
				Disabled: c.Restart.Disabled,
				Limit:    c.Restart.Limit,
				MinDelay: c.Restart.MinDelay,
				MaxDelay: c.Restart.MaxDelay,
			},
			UnreadyWhenFailed: c.UnreadyWhenFailed,
		}
	}
	if s := in.Scheduled; s != nil {
		info.Scheduled = &ScheduledConfig{
			Tier:                     tier,
			RunOnStart:               s.RunOnStart,
			RunTimeout:               s.RunTimeout,
			MaxConsecutiveFailures:   s.MaxConsecutiveFailures,
			UnreadyWhenFailed:        s.UnreadyWhenFailed,
			UnreadyUntilFirstSuccess: s.UnreadyUntilFirstSuccess,
			UnreadyAfterSuccessAge:   s.UnreadyAfterSuccessAge,
		}
	}
	return info
}

package worker

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Definition is the immutable registration of a worker: its identity, its
// kind and its resolved configuration.
type Definition struct {
	name     string
	kind     Kind
	schedule *Schedule // scheduled workers only

	continuous ContinuousConfig // resolved; continuous workers only
	scheduled  ScheduledConfig  // resolved; scheduled workers only
}

// NewContinuous validates name and cfg and returns the definition of a
// continuous worker; it panics, naming call (such as
// `Workers.Continuous("outbox-relay")`), on misuse.
func NewContinuous(call, name string, cfg ContinuousConfig) *Definition {
	ValidateName(call, name)
	return &Definition{name: name, kind: KindContinuous, continuous: resolveContinuous(call, cfg)}
}

// NewScheduled validates name, expr and cfg and returns the definition of a
// scheduled worker; it panics, naming call, on misuse.
func NewScheduled(call, name, expr string, cfg ScheduledConfig) *Definition {
	ValidateName(call, name)
	return &Definition{
		name:      name,
		kind:      KindScheduled,
		scheduled: resolveScheduled(call, cfg),
		schedule:  mustSchedule(call, expr),
	}
}

// Name returns the registration name.
func (d *Definition) Name() string { return d.name }

// Kind returns the worker's kind.
func (d *Definition) Kind() Kind { return d.kind }

// Continuous returns the resolved configuration of a continuous worker.
func (d *Definition) Continuous() ContinuousConfig { return d.continuous }

// Scheduled returns the resolved configuration of a scheduled worker.
func (d *Definition) Scheduled() ScheduledConfig { return d.scheduled }

// Readies reports whether the registration sets a readiness condition, so
// that its component answers Ready.
func (d *Definition) Readies() bool {
	return d.unreadyWhenFailed() ||
		d.kind == KindScheduled && (d.scheduled.UnreadyUntilFirstSuccess || d.scheduled.UnreadyAfterSuccessAge > 0)
}

func (d *Definition) scheduleExpr() string {
	if d.schedule == nil {
		return ""
	}
	return d.schedule.String()
}

// runTimeout is the budget of each run; zero for none.
func (d *Definition) runTimeout() time.Duration {
	if d.kind == KindScheduled {
		return d.scheduled.RunTimeout
	}
	return 0
}

// unreadyWhenFailed reports the readiness condition both kinds share.
func (d *Definition) unreadyWhenFailed() bool {
	if d.kind == KindScheduled {
		return d.scheduled.UnreadyWhenFailed
	}
	return d.continuous.UnreadyWhenFailed
}

// info is the single builder of Info: every snapshot — before the worker's
// component has started and from a running runner alike — goes through it.
// The configuration is copied, so a caller mutating a snapshot never reaches
// the definition.
func (d *Definition) info(state runState) Info {
	info := Info{
		Name:                d.name,
		Kind:                d.kind,
		Schedule:            d.scheduleExpr(),
		Status:              state.status,
		Restarts:            state.restarts,
		ConsecutiveFailures: state.consecutiveFailures,
		LastStartedAt:       state.lastStartedAt,
		LastSucceededAt:     state.lastSucceededAt,
		LastError:           state.lastError,
	}
	if d.kind == KindScheduled {
		cfg := d.scheduled
		info.Scheduled = &cfg
	} else {
		cfg := d.continuous
		info.Continuous = &cfg
	}
	return info
}

// ValidateName applies the worker name rules, panicking under call; the
// registry calls it before it reads the configuration, so a bad name is
// reported first. Names are never normalized, so
// the registered name is exactly the one every log line and snapshot reports.
// No prefix is reserved: health's reserved "credo." prefix applies to the
// full component name, which always starts with "worker:".
func ValidateName(call, name string) {
	switch {
	case name == "":
		panic(fmt.Sprintf("credo: %s: the name is empty; name the worker", call))
	case strings.TrimSpace(name) != name:
		panic(fmt.Sprintf("credo: %s: the name has leading or trailing whitespace; names are never "+
			"trimmed, so remove it", call))
	case strings.ContainsFunc(name, unicode.IsControl):
		panic(fmt.Sprintf("credo: %s: the name contains control characters; remove them", call))
	}
}

// mustSchedule parses expr, panicking with the parse error.
func mustSchedule(call, expr string) *Schedule {
	schedule, err := ParseSchedule(expr)
	if err != nil {
		panic(fmt.Sprintf("credo: %s: %s; validate a schedule from configuration with "+
			"credo.ParseSchedule before registering it", call, strings.TrimPrefix(err.Error(), "credo: ")))
	}
	return schedule
}

package worker

import "time"

// definition is the immutable registration of a worker: its identity, its
// kind and its resolved configuration.
type definition struct {
	name     string
	kind     Kind
	schedule *Schedule // scheduled workers only

	continuous ContinuousConfig // resolved; continuous workers only
	scheduled  ScheduledConfig  // resolved; scheduled workers only
}

func (d *definition) scheduleExpr() string {
	if d.schedule == nil {
		return ""
	}
	return d.schedule.String()
}

// runTimeout is the budget of each run; zero for none.
func (d *definition) runTimeout() time.Duration {
	if d.kind == KindScheduled {
		return d.scheduled.RunTimeout
	}
	return 0
}

// unreadyWhenFailed reports the readiness condition both kinds share.
func (d *definition) unreadyWhenFailed() bool {
	if d.kind == KindScheduled {
		return d.scheduled.UnreadyWhenFailed
	}
	return d.continuous.UnreadyWhenFailed
}

// readies reports whether the registration sets a readiness condition, so
// that its component answers Ready.
func (d *definition) readies() bool {
	return d.unreadyWhenFailed() ||
		d.kind == KindScheduled && (d.scheduled.UnreadyUntilFirstSuccess || d.scheduled.UnreadyAfterSuccessAge > 0)
}

// info is the single builder of Info: every snapshot — before the worker's
// component has started and from a running runner alike — goes through it.
// The configuration is copied, so a caller mutating a snapshot never reaches
// the definition.
func (d *definition) info(state runState) Info {
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

package credo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/credo-go/credo/config"
)

// reloadState holds everything the App-level reload (ADR-020) needs: the
// registered hooks and subscribers, the framework-internal participants, and
// the slot that serializes Reload calls against each other and against
// shutdown.
type reloadState struct {
	// slot is a capacity-one channel: holding its token is the right to run a
	// reload. A channel rather than a mutex so that waiting is context-aware —
	// a queued caller gives up when its own ctx or the lifecycle ctx ends —
	// and so that the drain can take the token and keep it, which blocks every
	// later reload before it touches infrastructure. A caller that waits and
	// then acquires performs its own full reload, so after Reload returns the
	// snapshot is at least as new as when it was called.
	slot     chan struct{}
	slotOnce sync.Once

	// onReload holds the generic hooks, run FIFO at the end of every reload.
	onReload []func(ctx context.Context) error

	// subs holds the typed per-section subscribers in registration order.
	subs []configSubscription

	// participants are framework-internal reload steps (file-based TLS
	// rotation) that run before user subscribers, on every reload, with the
	// change set. Their prefixes count as covered when reporting unsubscribed
	// changes.
	participants []reloadParticipant
}

// configSubscription is one OnConfigChange[T] registration with T erased:
// decode produces the typed value from a RawConfig (the staged candidate or the
// live snapshot) and apply hands it to the user's callback.
type configSubscription struct {
	key    string
	decode func(RawConfig) (any, error)
	apply  func(ctx context.Context, v any) error
}

// reloadParticipant is a framework-internal reload step.
type reloadParticipant struct {
	// prefixes are the config key prefixes this participant owns; changes under
	// them are not reported as restart-required.
	prefixes []string
	// active, when set, reports whether the participant currently owns its
	// prefixes; an inactive participant's prefixes are not treated as covered.
	active func() bool
	run    func(ctx context.Context, changes config.Changes) error
}

// token returns the slot channel, allocating it on first use so a zero-value
// App still works.
func (r *reloadState) token() chan struct{} {
	r.slotOnce.Do(func() { r.slot = make(chan struct{}, 1) })
	return r.slot
}

// acquire takes the reload slot. It returns ctx's error if the caller's context
// ends first and errReloadNotRunning if the lifecycle context ends first (the
// session is shutting down, so a queued reload must not start).
func (r *reloadState) acquire(ctx context.Context, lifecycleDone <-chan struct{}) error {
	select {
	case r.token() <- struct{}{}:
		return nil
	default:
	}
	select {
	case r.token() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("credo: Reload: waiting for the in-flight reload: %w", ctx.Err())
	case <-lifecycleDone:
		return errReloadNotRunning
	}
}

// release returns the reload slot.
func (r *reloadState) release() { <-r.token() }

// errReloadNotRunning is returned when Reload is called (or a queued Reload
// wakes up) outside the running state.
var errReloadNotRunning = errors.New("credo: Reload: server not running")

// covers reports whether key equals prefix or lies under it, segment-wise.
func covers(prefix, key string) bool {
	return key == prefix || strings.HasPrefix(key, prefix+".")
}

// OnReload registers a reload hook. Hooks run in FIFO order at the end of
// every [App.Reload] — after the new configuration snapshot is visible and the
// typed [App.OnConfigChange] subscribers have applied their sections — with
// the reload context ([WithReloadTimeout] for the SIGHUP path, the caller's
// for a programmatic Reload). An error, or a recovered panic, is joined into
// the Reload result and does not skip later hooks; it never stops the process.
//
// Typical uses: re-open a log file after rotation, refresh an allowlist, drive
// certificate rotation for a [WithTLSConfig] source through its own
// GetCertificate.
//
// Must be called before Run; panics for a nil hook or after the App is frozen.
func (app *App) OnReload(fn func(ctx context.Context) error) {
	app.registerHook("App.OnReload", fn, &app.reload.onReload)
}

// OnConfigChange registers a typed subscriber for one configuration section.
// When a reload changes any leaf under key (or key itself), T is decoded from
// the new snapshot and fn receives it; subscribers whose section did not change
// are not invoked. Several subscriptions may share a key, and nested keys are
// independent: "databases" and "databases.primary" both fire when
// databases.primary.dsn changes.
//
// When the App's [RawConfig] implements config.Stager (as *config.Config
// does), every affected subscriber is decoded — and validated, if T has a
// Validate() error method — against the candidate snapshot before it is
// published; any failure aborts the reload and the previous snapshot stays
// current. With a store that only implements config.Reloader the snapshot is
// published first and a decode failure is reported through the Reload error.
//
// The subscriber owns atomic application in its own domain — store the value
// behind an atomic.Pointer, call slog.LevelVar.Set, swap a limiter. The
// framework never rebuilds DI singletons (ADR-004): a value already injected
// through a constructor does not change.
//
// Must be called before Run. Panics for a nil fn, after the App is frozen, or
// when the App's RawConfig implements neither config.Stager nor
// config.Reloader — a subscription that can never fire is startup misuse.
func (app *App) OnConfigChange[T any](key string, fn func(ctx context.Context, next T) error) {
	app.checkHookRegistration("App.OnConfigChange", fn == nil)
	if !app.configReloadable() {
		panic("credo: OnConfigChange: the App's RawConfig implements neither config.Stager nor config.Reloader, so the subscription could never fire")
	}
	app.reload.subs = append(app.reload.subs, configSubscription{
		key: key,
		decode: func(rc RawConfig) (any, error) {
			var next T
			if err := rc.Unmarshal(key, &next); err != nil {
				return nil, err
			}
			if v, ok := any(&next).(interface{ Validate() error }); ok {
				if err := v.Validate(); err != nil {
					return nil, fmt.Errorf("validation: %w", err)
				}
			}
			return next, nil
		},
		apply: func(ctx context.Context, v any) error {
			return fn(ctx, v.(T))
		},
	})
}

// configReloadable reports whether the App's RawConfig can take part in reload.
func (app *App) configReloadable() bool {
	switch app.rawConfig.(type) {
	case config.Stager, config.Reloader:
		return true
	}
	return false
}

// addReloadParticipant registers a framework-internal reload step.
func (app *App) addReloadParticipant(p reloadParticipant) {
	app.reload.participants = append(app.reload.participants, p)
}

// Reload performs a trigger-driven partial reload (ADR-020). It succeeds only
// while the server is running. The sequence is:
//
//  1. If the RawConfig implements config.Stager, re-read every source into a
//     candidate snapshot (a load error aborts with the old snapshot untouched).
//  2. Decode — and validate — every [App.OnConfigChange] subscriber whose
//     section the candidate changes; any failure aborts before anything is
//     published.
//  3. Publish the snapshot atomically, run framework participants (file-based
//     TLS rotation), the affected subscribers in registration order, then the
//     [App.OnReload] hooks FIFO. Errors and recovered panics are collected and
//     the sequence continues; there is no rollback.
//  4. Return the joined step-3 errors (nil on full success), log one Info
//     summary, and log a Warn naming every changed key that no subscriber or
//     participant covers — those sections are restart-only.
//
// A RawConfig that implements only config.Reloader skips the pre-publish
// validation: the snapshot is published first and decode failures become
// step-3 errors. A RawConfig that implements neither leaves the configuration
// untouched and runs only the participants and OnReload hooks.
//
// Concurrent calls are serialized: a caller waits for the in-flight reload
// unless its ctx ends first, and a caller still queued when shutdown begins
// returns the not-running error instead of reloading a stopping server. Every
// participant, subscriber, and hook receives a context that is cancelled when
// either ctx or the application lifecycle ends, so a reload overlapping a
// shutdown is told to stop; shutdown then waits for it to return before
// tearing down DI infrastructure (see [App.Shutdown]).
//
// Under [App.Run] a SIGHUP (Unix) calls Reload with a context bounded by
// [WithReloadTimeout]; [App.RunContext] and [App.ServeContext] install no
// signal handler, so their callers invoke Reload directly. A reload never
// stops the process, whatever it reports.
func (app *App) Reload(ctx context.Context) error {
	if ctx == nil {
		return errors.New("credo: Reload: nil context")
	}
	lm := app.lifecycle
	notRunning := func() error {
		return fmt.Errorf("credo: Reload: server in state %q, expected %q", lm.currentState(), stateRunning)
	}
	if lm.currentState() != stateRunning {
		return notRunning()
	}
	lifecycleCtx := lm.lifecycleContext()
	if lifecycleCtx == nil { // lost a race with the session teardown
		return notRunning()
	}

	r := &app.reload
	if err := r.acquire(ctx, lifecycleCtx.Done()); err != nil {
		if errors.Is(err, errReloadNotRunning) {
			return notRunning()
		}
		return err
	}
	defer r.release()
	// Re-check under the slot: a caller queued behind a long reload may have
	// outlived the session.
	if lm.currentState() != stateRunning {
		return notRunning()
	}

	// Callbacks observe the caller's budget and the lifecycle: shutdown cancels
	// this context (drain step 2) before waiting for the reload to return.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(lifecycleCtx, cancel)()

	start := time.Now()
	var (
		changes   config.Changes
		reloaded  bool            // the configuration snapshot was re-read
		staged    config.Staged   // non-nil while validating a candidate
		decoded   = map[int]any{} // subscriber index → value decoded in step 2
		stepErrs  []error
		notified  int
		failedLog = func(what string, i int, err error) {
			app.logger.LogAttrs(ctx, slog.LevelError, "credo: reload step failed",
				slog.String("step", what), slog.Int("index", i), slog.Any("error", err))
		}
	)

	// Steps 1–2: re-read and, when staging is available, validate before publish.
	switch rc := app.rawConfig.(type) {
	case config.Stager:
		s, err := rc.Stage()
		if err != nil {
			return fmt.Errorf("credo: Reload: %w", err)
		}
		staged = s
		changes = s.Changes()
		var verr []error
		for i, sub := range r.subs {
			if !changes.Affects(sub.key) {
				continue
			}
			v, err := sub.decode(s)
			if err != nil {
				verr = append(verr, fmt.Errorf("credo: Reload: OnConfigChange[%d] %q: %w", i, sub.key, err))
				continue
			}
			decoded[i] = v
		}
		if len(verr) > 0 {
			// Nothing has been published: the previous snapshot stays current.
			app.logger.LogAttrs(ctx, slog.LevelError, "credo: reload aborted before publish",
				slog.Int("errors", len(verr)), slog.Any("changed_keys", changes.Keys()))
			return errors.Join(verr...)
		}
		staged.Commit()
		reloaded = true
	case config.Reloader:
		c, err := rc.Reload()
		if err != nil {
			return fmt.Errorf("credo: Reload: %w", err)
		}
		changes = c
		reloaded = true
	}

	// Step 3a: framework participants, on every reload.
	for i, p := range r.participants {
		if err := safeHook(func() error { return p.run(ctx, changes) }); err != nil {
			stepErrs = append(stepErrs, fmt.Errorf("credo: Reload: participant[%d]: %w", i, err))
			failedLog("participant", i, err)
		}
	}

	// Step 3b: affected typed subscribers, registration order.
	if reloaded {
		for i, sub := range r.subs {
			if !changes.Affects(sub.key) {
				continue
			}
			v, ok := decoded[i]
			if !ok { // Reloader-only store: decode from the live snapshot.
				dv, err := sub.decode(app.rawConfig)
				if err != nil {
					stepErrs = append(stepErrs, fmt.Errorf("credo: Reload: OnConfigChange[%d] %q: %w", i, sub.key, err))
					failedLog("config_change", i, err)
					continue
				}
				v = dv
			}
			notified++
			if err := safeHook(func() error { return sub.apply(ctx, v) }); err != nil {
				stepErrs = append(stepErrs, fmt.Errorf("credo: Reload: OnConfigChange[%d] %q: %w", i, sub.key, err))
				failedLog("config_change", i, err)
			}
		}
	}

	// Step 3c: generic hooks, FIFO.
	for i, fn := range r.onReload {
		if err := safeHook(func() error { return fn(ctx) }); err != nil {
			stepErrs = append(stepErrs, fmt.Errorf("credo: Reload: OnReload[%d]: %w", i, err))
			failedLog("on_reload", i, err)
		}
	}

	// Step 4: report. Unsubscribed changes are restart-only — say so.
	if restartOnly := r.uncovered(changes); len(restartOnly) > 0 {
		app.logger.LogAttrs(ctx, slog.LevelWarn,
			"credo: config changed but no reloadable consumer is registered; restart required",
			slog.Any("keys", restartOnly))
	}
	app.logger.LogAttrs(ctx, slog.LevelInfo, "credo: reload complete",
		slog.Duration("duration", time.Since(start)),
		slog.Bool("config_reloaded", reloaded),
		slog.Int("changed_keys", len(changes.Keys())),
		slog.Int("subscribers_notified", notified),
		slog.Int("errors", len(stepErrs)))
	return errors.Join(stepErrs...)
}

// uncovered returns the changed keys that no subscription or participant
// covers.
func (r *reloadState) uncovered(changes config.Changes) []string {
	var out []string
	for _, key := range changes.Keys() {
		covered := false
		for _, sub := range r.subs {
			if sub.key == "" || covers(sub.key, key) {
				covered = true
				break
			}
		}
		if !covered {
			for _, p := range r.participants {
				if p.active != nil && !p.active() {
					continue
				}
				for _, prefix := range p.prefixes {
					if covers(prefix, key) {
						covered = true
						break
					}
				}
				if covered {
					break
				}
			}
		}
		if !covered {
			out = append(out, key)
		}
	}
	return out
}

// safeHook runs fn, converting a panic into an error so one misbehaving hook
// cannot take the reload — or the process — down with it.
func safeHook(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return fn()
}

// reloadTimeout returns the budget for signal-triggered reloads.
func (app *App) reloadTimeout() time.Duration {
	if app.serverCfg.ReloadTimeout > 0 {
		return app.serverCfg.ReloadTimeout
	}
	return defaultReloadTimeout
}

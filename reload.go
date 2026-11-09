package credo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/credo-go/credo/config"
)

// reloadState holds the slot that serializes Reload calls against each other
// and against shutdown (ADR-020).
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

// Reload re-reads the configuration and applies it to its live sections
// (ADR-020) through [config.Config.Reload], and returns that reload's result:
// the keys it changed, the live sections it replaced, the keys whose change
// takes a restart and the file keys a later source keeps. A section bound
// through [config.Section.Decode] is a snapshot, and its changed keys are
// reported as needing a restart, as is every changed key outside a live
// section; a reload never rebuilds a DI singleton (ADR-004). Reload logs the
// result, so an endpoint or an orchestrator hook that calls it can also
// return it to the operator.
//
// A live section that fails to decode or validate aborts the reload: nothing
// is published, the result says so and why, and the error names the section.
// An [config.Live.OnChange] callback's error or panic is joined into the
// returned error after the publish; nothing rolls back, and the process never
// terminates.
//
// Reload succeeds only while the server is running, and only over a
// configuration built by config.Load or config.LoadBytes: any other
// [RawConfig] cannot reload, and Reload returns an error naming its type.
// Concurrent calls are serialized: a caller waits for the in-flight reload
// unless its ctx ends first, and a caller still queued when shutdown begins
// returns the not-running error instead of reloading a stopping server. The
// callbacks receive a context that is cancelled when either ctx or the
// application lifecycle ends, so a reload overlapping a shutdown is told to
// stop; shutdown then waits for it to return before tearing down the internal
// tier (see [App.Shutdown]).
//
// Under [App.Run] a SIGHUP (Unix) calls Reload with a context bounded by
// [WithReloadTimeout]; [App.RunContext] and [App.ServeContext] install no
// signal handler, so their callers invoke Reload directly. Configuration files
// are not watched.
func (app *App) Reload(ctx context.Context) (config.ReloadResult, error) {
	if ctx == nil {
		return config.ReloadResult{}, errors.New("credo: Reload: nil context")
	}
	cfg, ok := app.rawConfig.(*config.Config)
	if !ok {
		return config.ReloadResult{}, fmt.Errorf(
			"credo: Reload: the configuration is a %s, which cannot reload; load it with config.Load (or config.LoadBytes) and pass it to credo.WithRawConfig",
			reflect.TypeOf(app.rawConfig))
	}
	lm := app.lifecycle
	notRunning := func() error {
		return fmt.Errorf("credo: Reload: server in state %q, expected %q", lm.currentState(), stateRunning)
	}
	if lm.currentState() != stateRunning {
		return config.ReloadResult{}, notRunning()
	}
	lifecycleCtx := lm.lifecycleContext()
	if lifecycleCtx == nil { // lost a race with the session teardown
		return config.ReloadResult{}, notRunning()
	}

	r := &app.reload
	if err := r.acquire(ctx, lifecycleCtx.Done()); err != nil {
		if errors.Is(err, errReloadNotRunning) {
			return config.ReloadResult{}, notRunning()
		}
		return config.ReloadResult{}, err
	}
	defer r.release()
	// Re-check under the slot: a caller queued behind a long reload may have
	// outlived the session.
	if lm.currentState() != stateRunning {
		return config.ReloadResult{}, notRunning()
	}

	// Callbacks observe the caller's budget and the lifecycle: shutdown cancels
	// this context (drain step 2) before waiting for the reload to return.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(lifecycleCtx, cancel)()
	// The slot's fast path does not consult ctx: a caller that arrives with an
	// already-cancelled context, or whose context ended while it waited, must
	// not load a candidate or run a single callback.
	if err := ctx.Err(); err != nil {
		return config.ReloadResult{}, fmt.Errorf("credo: Reload: %w", err)
	}

	start := time.Now()
	res, err := cfg.Reload(ctx)
	app.logReload(ctx, res, err, time.Since(start))
	return res, err
}

// logReload logs a reload's result: an abort at Error, the keys that need a
// restart and the overridden file keys at Warn, callback failures at Error,
// and a summary at Info.
func (app *App) logReload(ctx context.Context, res config.ReloadResult, err error, took time.Duration) {
	log := app.logger
	if !res.Published {
		log.LogAttrs(ctx, slog.LevelError, "credo: reload aborted; nothing was published",
			slog.String("reason", res.Aborted), slog.Any("changed_keys", res.Changed))
		return
	}
	if len(res.RestartRequired) > 0 {
		log.LogAttrs(ctx, slog.LevelWarn,
			"credo: configuration differs from the start outside every live section; restart to apply it",
			slog.Any("keys", res.RestartRequired))
	}
	for _, o := range res.Overridden {
		log.LogAttrs(ctx, slog.LevelWarn, "credo: configuration file value changed, but a later source keeps the key",
			slog.String("key", o.Key), slog.String("source", o.Source))
	}
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "credo: reload callback failed", slog.Any("error", err))
	}
	log.LogAttrs(ctx, slog.LevelInfo, "credo: reload complete",
		slog.Duration("duration", took),
		slog.Int("changed_keys", len(res.Changed)),
		slog.Any("replaced", res.Replaced),
		slog.Int("restart_required", len(res.RestartRequired)))
}

// reloadTimeout returns the budget for signal-triggered reloads.
func (app *App) reloadTimeout() time.Duration {
	if app.serverCfg.ReloadTimeout > 0 {
		return app.serverCfg.ReloadTimeout
	}
	return defaultReloadTimeout
}

package credo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	internalobserve "github.com/credo-go/credo/internal/observe"
)

const drainHookStackSize = 8192

const (
	preDrainHookPhase          = "OnPreDrain"
	drainHookPhase             = "OnDrain"
	preDrainBoundaryLogMessage = "credo: OnPreDrain still waiting after shutdown boundary"
)

type drainHook struct {
	index  int
	source string
	fn     func(context.Context) error
}

func newDrainHook(index int, fn func(context.Context) error) drainHook {
	return drainHook{index: index, source: drainHookSource(3), fn: fn}
}

func drainHookSource(skip int) string {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	name := "unknown"
	if function := runtime.FuncForPC(pc); function != nil {
		name = function.Name()
	}
	return fmt.Sprintf("%s (%s:%d)", name, filepath.Base(file), line)
}

type drainWork struct {
	key    int
	label  string
	index  int
	source string
}

type drainResult struct {
	key         int
	err         error
	completedAt time.Time
	contextErr  error
}

type drainHookPanicError struct {
	index  int
	source string
	cause  error
	stack  string
}

func (e *drainHookPanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.cause)
}

func (e *drainHookPanicError) Unwrap() error {
	return e.cause
}

type drainIncompleteError struct {
	cause   error
	pending []drainWork
}

func (e *drainIncompleteError) Error() string {
	labels := make([]string, 0, len(e.pending))
	for _, work := range e.pending {
		labels = append(labels, work.label)
	}
	return fmt.Sprintf(
		"credo: drain incomplete with %d task(s) pending (%s): %v",
		len(labels),
		strings.Join(labels, ", "),
		e.cause,
	)
}

func (e *drainIncompleteError) Unwrap() error {
	return e.cause
}

func (lm *lifecycleManager) drainBeforeInfrastructure(
	ctx context.Context,
	redirectSrv *http.Server,
	srv *http.Server,
) error {
	return lm.runDrainPhase(ctx, func(ctx context.Context) error {
		return drainHTTPServers(ctx, redirectSrv, srv)
	})
}

func (lm *lifecycleManager) runPreDrainPhase(ctx context.Context) error {
	hooks := append([]drainHook(nil), lm.onPreDrain...)
	results := make(chan drainResult, len(hooks))
	pending := make(map[int]drainWork, len(hooks))
	lm.launchDrainHooks(ctx, preDrainHookPhase, hooks, results, pending)
	return lm.collectPreDrainResults(ctx, results, pending)
}

// collectPreDrainResults treats context completion as a diagnostic boundary,
// not permission to abandon a hook. It emits one provisional warning when that
// boundary is reached, then waits for every hook. Each hook records its
// framework completion time and the context state observed immediately after.
// The absolute deadline is authoritative even if its timer has not delivered
// Done/Err yet, while ctx.Err still records manual cancellation. Final
// incomplete identities are reported exactly once, after their results arrive.
// This is the safety property that distinguishes OnPreDrain from the ordinary
// drain phase: lifecycle cancellation and infrastructure teardown may not race
// work that explicitly requires those dependencies to remain live.
func (lm *lifecycleManager) collectPreDrainResults(
	ctx context.Context,
	results <-chan drainResult,
	pending map[int]drainWork,
) error {
	var errs []error
	consume := func(result drainResult) {
		work, ok := pending[result.key]
		delete(pending, result.key)
		if ok {
			if completionErr := preDrainCompletionError(ctx, result); completionErr != nil {
				errs = append(errs, lm.newDrainIncompleteError(
					completionErr,
					map[int]drainWork{result.key: work},
				))
			}
		}
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}

	ctxDone := ctx.Done()
	var deadlineTimer *time.Timer
	var deadlineDone <-chan time.Time
	if deadline, ok := ctx.Deadline(); ok {
		delay := time.Until(deadline)
		if delay < 0 {
			delay = 0
		}
		deadlineTimer = time.NewTimer(delay)
		deadlineDone = deadlineTimer.C
		defer func() {
			if !deadlineTimer.Stop() {
				select {
				case <-deadlineTimer.C:
				default:
				}
			}
		}()
	}

	for len(pending) > 0 {
		// Prefer already-buffered results so the provisional boundary warning does
		// not name work that the collector can prove has already completed.
		select {
		case result := <-results:
			consume(result)
			continue
		default:
		}

		select {
		case result := <-results:
			consume(result)
		case <-ctxDone:
			cause := ctx.Err()
			if cause == nil {
				cause = context.Canceled
			}
			lm.logPreDrainBoundary(cause, pending)
			ctxDone = nil
			deadlineDone = nil
			if deadlineTimer != nil {
				deadlineTimer.Stop()
			}
		case <-deadlineDone:
			lm.logPreDrainBoundary(context.DeadlineExceeded, pending)
			ctxDone = nil
			deadlineDone = nil
		}
	}
	return errors.Join(errs...)
}

func preDrainCompletionError(ctx context.Context, result drainResult) error {
	completionErr := result.contextErr
	if deadline, ok := ctx.Deadline(); ok && !result.completedAt.IsZero() {
		switch {
		case !result.completedAt.Before(deadline):
			completionErr = context.DeadlineExceeded
		case errors.Is(completionErr, context.DeadlineExceeded):
			// The hook completed before the absolute deadline, but the deadline
			// became observable between recording completedAt and reading ctx.Err.
			completionErr = nil
		}
	}
	return completionErr
}

func (lm *lifecycleManager) runDrainPhase(
	ctx context.Context,
	httpDrain func(context.Context) error,
) error {
	const httpWorkKey = -1

	hooks := append([]drainHook(nil), lm.onDrain...)
	results := make(chan drainResult, len(hooks)+1)
	pending := make(map[int]drainWork, len(hooks)+1)
	pending[httpWorkKey] = drainWork{key: httpWorkKey, label: "HTTP drain", index: -1, source: "net/http"}

	go func() {
		results <- drainResult{key: httpWorkKey, err: httpDrain(ctx)}
	}()

	lm.launchDrainHooks(ctx, drainHookPhase, hooks, results, pending)
	return lm.collectDrainResults(ctx, results, pending)
}

func (lm *lifecycleManager) launchDrainHooks(
	ctx context.Context,
	phase string,
	hooks []drainHook,
	results chan<- drainResult,
	pending map[int]drainWork,
) {
	for _, hook := range hooks {
		label := fmt.Sprintf("%s hook [%d] (%s)", phase, hook.index, hook.source)
		pending[hook.index] = drainWork{
			key: hook.index, label: label, index: hook.index, source: hook.source,
		}
		go func() {
			err := lm.runDrainHook(ctx, phase, hook)
			completedAt := time.Now()
			contextErr := ctx.Err()
			if err != nil {
				err = fmt.Errorf("credo: %s hook [%d] (%s): %w", phase, hook.index, hook.source, err)
			}
			results <- drainResult{
				key: hook.index, err: err, completedAt: completedAt, contextErr: contextErr,
			}
		}()
	}
}

func (lm *lifecycleManager) collectDrainResults(
	ctx context.Context,
	results <-chan drainResult,
	pending map[int]drainWork,
) error {
	var errs []error
	consume := func(result drainResult) {
		delete(pending, result.key)
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}

	for len(pending) > 0 {
		select {
		case result := <-results:
			consume(result)
		case <-ctx.Done():
			for {
				select {
				case result := <-results:
					consume(result)
				default:
					if len(pending) == 0 {
						return errors.Join(errs...)
					}
					incomplete := lm.newDrainIncompleteError(ctx.Err(), pending)
					errs = append(errs, incomplete)
					return errors.Join(errs...)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func drainHTTPServers(ctx context.Context, redirectSrv, srv *http.Server) error {
	var errs []error
	// Preserve redirect-before-main ordering so clients are not redirected to a
	// main listener that has already stopped accepting connections.
	if redirectSrv != nil {
		if err := redirectSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("credo: HTTP redirect drain: %w", err))
		}
	}
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("credo: server drain: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (lm *lifecycleManager) runDrainHook(ctx context.Context, phase string, hook drainHook) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := &drainHookPanicError{
				index:  hook.index,
				source: hook.source,
				cause:  internalobserve.PanicError(recovered),
				stack:  internalobserve.StackTrace(drainHookStackSize),
			}
			lm.app.logger.LogAttrs(
				context.WithoutCancel(ctx),
				slog.LevelError,
				"credo: "+phase+" hook panic",
				slog.Int("hook_index", hook.index),
				slog.String("hook_source", hook.source),
				slog.Any("panic", recovered),
				slog.String("stack", panicErr.stack),
			)
			err = panicErr
		}
	}()
	return hook.fn(ctx)
}

func (lm *lifecycleManager) newDrainIncompleteError(
	cause error,
	pending map[int]drainWork,
) *drainIncompleteError {
	work := sortedDrainWork(pending)

	logCtx := context.Background()
	for _, item := range work {
		attrs := []slog.Attr{
			slog.String("task", item.label),
			slog.Any("error", cause),
		}
		if item.index >= 0 {
			attrs = append(attrs,
				slog.Int("hook_index", item.index),
				slog.String("hook_source", item.source),
			)
		}
		lm.app.logger.LogAttrs(logCtx, slog.LevelError, "credo: drain task incomplete", attrs...)
	}
	return &drainIncompleteError{cause: cause, pending: work}
}

func (lm *lifecycleManager) logPreDrainBoundary(cause error, pending map[int]drainWork) {
	work := sortedDrainWork(pending)
	labels := make([]string, 0, len(work))
	for _, item := range work {
		labels = append(labels, item.label)
	}
	lm.app.logger.LogAttrs(
		context.Background(),
		slog.LevelWarn,
		preDrainBoundaryLogMessage,
		slog.Any("error", cause),
		slog.Int("pending_count", len(work)),
		slog.Any("pending_tasks", labels),
	)
}

func sortedDrainWork(pending map[int]drainWork) []drainWork {
	work := make([]drainWork, 0, len(pending))
	for _, item := range pending {
		work = append(work, item)
	}
	sort.Slice(work, func(i, j int) bool { return work[i].key < work[j].key })
	return work
}

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

	internalobserve "github.com/credo-go/credo/internal/observe"
)

const drainHookStackSize = 8192

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
	key int
	err error
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

	for _, hook := range hooks {
		label := fmt.Sprintf("OnDrain hook [%d] (%s)", hook.index, hook.source)
		pending[hook.index] = drainWork{
			key: hook.index, label: label, index: hook.index, source: hook.source,
		}
		go func() {
			err := lm.runDrainHook(ctx, hook)
			if err != nil {
				err = fmt.Errorf("credo: OnDrain hook [%d] (%s): %w", hook.index, hook.source, err)
			}
			results <- drainResult{key: hook.index, err: err}
		}()
	}

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

func (lm *lifecycleManager) runDrainHook(ctx context.Context, hook drainHook) (err error) {
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
				"credo: OnDrain hook panic",
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
	work := make([]drainWork, 0, len(pending))
	for _, item := range pending {
		work = append(work, item)
	}
	sort.Slice(work, func(i, j int) bool { return work[i].key < work[j].key })

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

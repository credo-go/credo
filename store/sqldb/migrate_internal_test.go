package sqldb

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo/store"
)

func TestRunMigrationUnlock_DetachesCanceledParent(t *testing.T) {
	type contextKey struct{}

	parent, cancel := context.WithCancel(context.WithValue(t.Context(), contextKey{}, "value"))
	cancel()

	unlockErr := errors.New("unlock failed")
	var called bool
	var gotContextErr error
	var gotValue any
	var gotDeadline bool

	err := runMigrationUnlock(parent, defaultMigrationUnlockTimeout, func(ctx context.Context) error {
		called = true
		gotContextErr = ctx.Err()
		gotValue = ctx.Value(contextKey{})
		_, gotDeadline = ctx.Deadline()
		return unlockErr
	})
	if !called {
		t.Fatal("Unlock was not called")
	}
	if gotContextErr != nil {
		t.Fatalf("Unlock context error = %v, want nil", gotContextErr)
	}
	if gotValue != "value" {
		t.Fatalf("Unlock context value = %v, want value", gotValue)
	}
	if !gotDeadline {
		t.Fatal("Unlock context has no cleanup deadline")
	}
	if !errors.Is(err, unlockErr) {
		t.Fatalf("runMigrationUnlock() = %v, want unlock error", err)
	}
}

func TestRunMigrationUnlock_BoundsContextIgnoringOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 5 * time.Second
		release := make(chan struct{})
		started := make(chan context.Context, 1)

		start := time.Now()
		err := runMigrationUnlock(t.Context(), timeout, func(ctx context.Context) error {
			started <- ctx
			<-release
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("runMigrationUnlock() = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != timeout {
			t.Fatalf("unlock wait = %s, want %s", elapsed, timeout)
		}

		cleanupCtx := <-started
		if !errors.Is(cleanupCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("cleanup context error = %v, want context.DeadlineExceeded", cleanupCtx.Err())
		}

		close(release)
		synctest.Wait()
	})
}

func TestFormatMigrationUnlockResult_PreservesDeadlineAndDriverCause(t *testing.T) {
	cleanupCtx, cancel := context.WithCancelCause(t.Context())
	cancel(context.DeadlineExceeded)

	driverErr := context.Canceled
	err := formatMigrationUnlockResult(
		cleanupCtx,
		defaultMigrationUnlockTimeout,
		driverErr,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("formatMigrationUnlockResult() = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, driverErr) {
		t.Fatalf("formatMigrationUnlockResult() = %v, want driver cause", err)
	}

	mapped := (*DB)(nil).mapMigrationUnlockError(t.Context(), err)
	if got, ok := store.KindOf(mapped); !ok || got != store.KindTimeout {
		t.Fatalf("mapped kind = %q, %t, want %q, true", got, ok, store.KindTimeout)
	}
	if !errors.Is(mapped, context.DeadlineExceeded) || !errors.Is(mapped, driverErr) {
		t.Fatalf("mapped error = %v, want deadline and driver causes", mapped)
	}
}

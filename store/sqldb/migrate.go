package sqldb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun/migrate"

	"github.com/credo-go/credo/store"
)

const defaultMigrationUnlockTimeout = 5 * time.Second

// RegisterMigrations registers the migration set that [DB.Migrate] runs.
// The set is a plain [migrate.Migrations] from Bun: populate it with
// (*migrate.Migrations).Discover for SQL files (works with embed.FS) or
// MustRegister for Go migrations, then hand it to the DB once at wiring
// time. Optional opts are passed through to the underlying
// [migrate.NewMigrator] (table names, hooks, template data, ...).
//
// By default the wrapper applies [migrate.WithMarkAppliedOnSuccess](true), so
// a migration is recorded only after its Up function returns nil. This is an
// at-least-once bookkeeping policy, not an atomicity guarantee: partial
// non-transactional effects or a marker-write failure can make a later
// [DB.Migrate] repeat work. Migrations must therefore be transactional where
// the database supports it or explicitly idempotent/reconcilable. Pass
// WithMarkAppliedOnSuccess(false) to restore Bun's record-before-running
// policy when its different recovery tradeoff is intentional.
//
// Panics if m is nil or if migrations were already registered — both are
// wiring-time programming errors, never runtime conditions.
func (db *DB) RegisterMigrations(m *migrate.Migrations, opts ...migrate.MigratorOption) {
	if m == nil {
		panic("sqldb: RegisterMigrations called with nil migrations")
	}
	if db.migrations != nil {
		panic("sqldb: migrations already registered")
	}
	db.migrations = m
	db.migratorOpts = opts
}

// Migrate runs all pending registered migrations. In multi-replica production,
// call it from one deadline-bounded pre-deploy job before rolling out the
// application. Its signature also matches credo's App.OnStart hook; this
// opt-in form is convenient for development, tests, and deliberate
// single-replica deployments:
//
//	db.RegisterMigrations(migrations)
//	app.OnStart(db.Migrate)
//
// Migrate creates the Bun migration bookkeeping tables if needed, takes
// Bun's table-based advisory lock, applies unapplied migrations in order,
// and releases the lock. If another instance holds the lock, Migrate fails
// immediately rather than waiting; it does not retry. Running with no pending
// migrations executes no Up body, although Init, Lock, and Unlock still run.
//
// Unlock ignores parent cancellation but gets a fresh five-second budget.
// Caller wait remains bounded even if a driver ignores context. A timeout
// means the unlock outcome is uncertain: the driver goroutine or connection
// may remain active and the lock row may remain or be deleted later. Do not
// retry or delete a stale row until the old runner is known to have stopped.
//
// Returns an error if no migration set was registered, or if an operation
// surfaced by Bun fails; errors are mapped to store.Err* sentinels where
// applicable. Bun v1.2.18 does not surface SQL-migration transaction-finalizer
// errors reliably, so use a Go migration with an explicit transaction when
// the commit result must gate the applied marker. Direct Bun migrators do not
// inherit RegisterMigrations options. Callers must repeat the options relevant
// to status or generation; DB-mutating apply/rollback paths must additionally
// own Init, Lock, and bounded cancellation-detached Unlock themselves.
func (db *DB) Migrate(ctx context.Context) (err error) {
	if db.migrations == nil {
		return fmt.Errorf("sqldb: no migrations registered (call RegisterMigrations first)")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	opts := append([]migrate.MigratorOption{migrate.WithMarkAppliedOnSuccess(true)}, db.migratorOpts...)
	migrator := migrate.NewMigrator(db.db, db.migrations, opts...)

	if initErr := migrator.Init(ctx); initErr != nil {
		return db.mapError(ctx, fmt.Errorf("sqldb: migrate init: %w", initErr))
	}
	if lockErr := migrator.Lock(ctx); lockErr != nil {
		return db.mapError(ctx, fmt.Errorf("sqldb: migrate lock: %w", lockErr))
	}
	defer func() {
		if unlockErr := runMigrationUnlock(
			ctx,
			defaultMigrationUnlockTimeout,
			migrator.Unlock,
		); unlockErr != nil {
			err = errors.Join(err, db.mapMigrationUnlockError(ctx, unlockErr))
		}
	}()

	if _, migrateErr := migrator.Migrate(ctx); migrateErr != nil {
		return db.mapError(ctx, fmt.Errorf("sqldb: migrate: %w", migrateErr))
	}
	return nil
}

func runMigrationUnlock(
	parent context.Context,
	timeout time.Duration,
	unlock func(context.Context) error,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- unlock(cleanupCtx)
	}()

	select {
	case unlockErr := <-result:
		return formatMigrationUnlockResult(cleanupCtx, timeout, unlockErr)
	case <-cleanupCtx.Done():
		// Prefer a completed Unlock result when it raced with the deadline.
		select {
		case unlockErr := <-result:
			return formatMigrationUnlockResult(cleanupCtx, timeout, unlockErr)
		default:
		}
		return migrationUnlockTimeoutError(timeout)
	}
}

func formatMigrationUnlockResult(
	cleanupCtx context.Context,
	timeout time.Duration,
	unlockErr error,
) error {
	if unlockErr == nil {
		return nil
	}

	wrapped := fmt.Errorf("sqldb: migrate unlock: %w", unlockErr)
	if errors.Is(context.Cause(cleanupCtx), context.DeadlineExceeded) {
		return errors.Join(migrationUnlockTimeoutError(timeout), wrapped)
	}
	return wrapped
}

func migrationUnlockTimeoutError(timeout time.Duration) error {
	return fmt.Errorf(
		"sqldb: migrate unlock exceeded cleanup timeout %s: %w",
		timeout,
		context.DeadlineExceeded,
	)
}

func (db *DB) mapMigrationUnlockError(parent context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return wrapMappedError(
			errorClassification{kind: store.KindTimeout, transient: true},
			"",
			err,
		)
	}
	return db.mapError(context.WithoutCancel(parent), err)
}

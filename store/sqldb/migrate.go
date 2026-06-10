package sqldb

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun/migrate"
)

// RegisterMigrations registers the migration set that [DB.Migrate] runs.
// The set is a plain [migrate.Migrations] from Bun: populate it with
// (*migrate.Migrations).Discover for SQL files (works with embed.FS) or
// MustRegister for Go migrations, then hand it to the DB once at wiring
// time. Optional opts are passed through to the underlying
// [migrate.NewMigrator] (table names, hooks, template data, ...).
//
// By default the wrapper applies [migrate.WithMarkAppliedOnSuccess](true),
// so a migration is recorded as applied only after it succeeds and a failed
// migration is retried on the next [DB.Migrate] run. (Bun's bare default
// records a migration before running it, which would silently skip a failed
// migration on restart.) Pass WithMarkAppliedOnSuccess(false) explicitly to
// restore Bun's behavior.
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

// Migrate runs all pending registered migrations. Its signature matches the
// hook accepted by credo's App.OnStart, so opt-in auto-run on application
// start is a one-liner:
//
//	db.RegisterMigrations(migrations)
//	app.OnStart(db.Migrate)
//
// Migrate creates the Bun migration bookkeeping tables if needed, takes
// Bun's table-based advisory lock, applies unapplied migrations in order,
// and releases the lock. If another instance holds the lock (for example, a
// second replica starting concurrently), Migrate fails immediately rather
// than waiting — the failed instance can simply be restarted. Running with
// no pending migrations is a no-op.
//
// Returns an error if no migration set was registered, or if any step fails;
// errors are mapped to store.Err* sentinels where applicable. For rollback,
// status inspection, or migration file generation, use Bun's migrator
// directly: migrate.NewMigrator(db.Client(), migrations).
func (db *DB) Migrate(ctx context.Context) (err error) {
	if db.migrations == nil {
		return fmt.Errorf("sqldb: no migrations registered (call RegisterMigrations first)")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	opts := append([]migrate.MigratorOption{migrate.WithMarkAppliedOnSuccess(true)}, db.migratorOpts...)
	migrator := migrate.NewMigrator(db.db, db.migrations, opts...)

	if err := migrator.Init(ctx); err != nil {
		return mapError(fmt.Errorf("sqldb: migrate init: %w", err))
	}
	if err := migrator.Lock(ctx); err != nil {
		return mapError(fmt.Errorf("sqldb: migrate lock: %w", err))
	}
	defer func() {
		// Release the lock even when ctx is already cancelled — a leaked
		// lock row would make every subsequent Migrate fail at Lock.
		if uerr := migrator.Unlock(context.WithoutCancel(ctx)); uerr != nil {
			err = errors.Join(err, mapError(fmt.Errorf("sqldb: migrate unlock: %w", uerr)))
		}
	}()

	if _, err := migrator.Migrate(ctx); err != nil {
		return mapError(fmt.Errorf("sqldb: migrate: %w", err))
	}
	return nil
}

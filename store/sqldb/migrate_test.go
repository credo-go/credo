package sqldb_test

import (
	"context"
	"embed"
	"errors"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"

	"github.com/credo-go/credo/store/sqldb"
)

//go:embed testdata/migrations
var migrationFS embed.FS

// Compile-time proof that DB.Migrate satisfies credo's App.OnStart hook
// signature — the whole lifecycle integration is app.OnStart(db.Migrate).
var _ func(context.Context) error = (*sqldb.DB)(nil).Migrate

// newGoMigrations builds a single-entry migration set programmatically via
// Add. (Migrations.Register derives the migration name from the calling
// file's name, which a _test.go file cannot satisfy.)
func newGoMigrations(name string, up func(ctx context.Context, db *bun.DB) error) *migrate.Migrations {
	ms := migrate.NewMigrations()
	ms.Add(migrate.Migration{
		Name: name,
		Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
			return up(ctx, m.DB())
		},
	})
	return ms
}

func createNotesTable(ctx context.Context, db *bun.DB) error {
	_, err := db.NewRaw(`
		CREATE TABLE notes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL
		)
	`).Exec(ctx)
	return err
}

func countNotes(t *testing.T, db *sqldb.DB) int {
	t.Helper()
	var n int
	if err := db.Client().NewRaw(`SELECT count(*) FROM notes`).Scan(context.Background(), &n); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	return n
}

// --- Migration wrapper tests ---

func TestMigrate_RunsPendingMigrations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	db.RegisterMigrations(newGoMigrations("1", createNotesTable))

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() = %v", err)
	}

	// The migrated table exists and is usable.
	if _, err := db.Client().NewRaw(`INSERT INTO notes (title) VALUES ('x')`).Exec(ctx); err != nil {
		t.Fatalf("insert into migrated table: %v", err)
	}
}

func TestMigrate_RerunIsNoOp(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var runs int
	db.RegisterMigrations(newGoMigrations("1", func(ctx context.Context, bdb *bun.DB) error {
		runs++
		return createNotesTable(ctx, bdb)
	}))

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate() = %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() = %v", err)
	}
	if runs != 1 {
		t.Errorf("migration ran %d times, want 1", runs)
	}
}

func TestMigrate_FailedMigrationRetriedOnNextRun(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// First attempt fails; the wrapper's WithMarkAppliedOnSuccess default
	// must leave it unapplied so the next Migrate retries it (Bun's bare
	// default would record it as applied and silently skip it).
	var attempts int
	db.RegisterMigrations(newGoMigrations("1", func(ctx context.Context, bdb *bun.DB) error {
		attempts++
		if attempts == 1 {
			return errors.New("transient failure")
		}
		return createNotesTable(ctx, bdb)
	}))

	if err := db.Migrate(ctx); err == nil {
		t.Fatal("first Migrate() = nil, want error")
	}
	// Also proves the advisory lock was released after the failure.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() = %v, want retry to succeed", err)
	}
	if attempts != 2 {
		t.Errorf("migration attempted %d times, want 2", attempts)
	}
	if got := countNotes(t, db); got != 0 {
		t.Errorf("notes count = %d, want 0", got)
	}
}

func TestMigrate_DiscoverEmbedFS(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// SQL migrations bundled into the binary via embed.FS; the seed row
	// comes from a plain migration file (2_seed_notes.up.sql).
	ms := migrate.NewMigrations()
	if err := ms.Discover(migrationFS); err != nil {
		t.Fatalf("Discover() = %v", err)
	}
	db.RegisterMigrations(ms)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() = %v", err)
	}

	var title string
	err := db.Client().NewRaw(`SELECT title FROM notes WHERE title = 'welcome'`).Scan(ctx, &title)
	if err != nil {
		t.Fatalf("seeded row not found: %v", err)
	}
}

func TestMigrate_NoRegistration(t *testing.T) {
	db := openTestDB(t)

	err := db.Migrate(context.Background())
	if err == nil {
		t.Fatal("Migrate() = nil, want error")
	}
	if !strings.Contains(err.Error(), "no migrations registered") {
		t.Errorf("Migrate() = %v, want mention of missing registration", err)
	}
}

func TestRegisterMigrations_NilPanics(t *testing.T) {
	db := openTestDB(t)

	defer func() {
		if recover() == nil {
			t.Fatal("RegisterMigrations(nil) did not panic")
		}
	}()
	db.RegisterMigrations(nil)
}

func TestRegisterMigrations_TwicePanics(t *testing.T) {
	db := openTestDB(t)
	db.RegisterMigrations(migrate.NewMigrations())

	defer func() {
		if recover() == nil {
			t.Fatal("second RegisterMigrations did not panic")
		}
	}()
	db.RegisterMigrations(migrate.NewMigrations())
}

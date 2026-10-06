package sqldb_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/uptrace/bun/migrate"

	"github.com/credo-go/credo/store/sqldb"
)

// txSQLMigrationFS builds a two-file migration set: a plain file that creates
// a parent table and a child table with a deferred foreign key, and a
// transactional file whose INSERT violates that key. The INSERT succeeds and
// the violation is raised by COMMIT, which is the finalizer whose error the
// migrator must report.
func txSQLMigrationFS(parentDDL, childDDL string) fstest.MapFS {
	return fstest.MapFS{
		"1_schema.up.sql": {Data: []byte(parentDDL + "\n--bun:split\n" + childDDL + "\n")},
		"2_orphan.tx.up.sql": {Data: []byte(
			"INSERT INTO txsql_children (id, parent_id) VALUES (1, 42);\n",
		)},
	}
}

// assertTxSQLCommitErrorReachesCaller runs the set against db and checks the
// contract: Migrate returns the COMMIT error, the transactional migration is
// not marked applied, and its row is not visible afterwards.
func assertTxSQLCommitErrorReachesCaller(
	t *testing.T,
	ctx context.Context,
	db *sqldb.DB,
	fsys fstest.MapFS,
	wantErr string,
	opts ...migrate.MigratorOption,
) {
	t.Helper()
	ms := migrate.NewMigrations()
	if err := ms.Discover(fsys); err != nil {
		t.Fatalf("Discover() = %v", err)
	}
	db.RegisterMigrations(ms, opts...)

	err := db.Migrate(ctx)
	if err == nil {
		t.Fatal("Migrate() = nil, want the COMMIT error of the transactional migration")
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("Migrate() = %v, want an error containing %q", err, wantErr)
	}
	assertNoUnexpectedMigrationCleanupError(t, err)

	status, statusErr := migrate.NewMigrator(db.Client(), ms, opts...).MigrationsWithStatus(ctx)
	if statusErr != nil {
		t.Fatalf("MigrationsWithStatus() = %v", statusErr)
	}
	applied := map[string]bool{}
	for _, m := range status {
		applied[m.Name] = m.IsApplied()
	}
	if !applied["1"] || applied["2"] {
		t.Fatalf("applied = %v, want migration 1 applied and migration 2 not applied", applied)
	}

	var children int
	if scanErr := db.Client().NewRaw(`SELECT count(*) FROM txsql_children`).Scan(ctx, &children); scanErr != nil {
		t.Fatalf("count children: %v", scanErr)
	}
	if children != 0 {
		t.Fatalf("children = %d, want 0: the failed transaction leaked its row", children)
	}
}

func TestMigrate_TxSQLCommitErrorReachesCaller(t *testing.T) {
	maxIdle := 4
	db, err := sqldb.Open(&sqldb.Config{
		Driver: "sqlite",
		DSN: fmt.Sprintf(
			"file:credo-txsql-%d?mode=memory&cache=shared&_pragma=foreign_keys(1)",
			sqliteMemorySequence.Add(1),
		),
		MaxOpen: 4,
		MaxIdle: &maxIdle,
	})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { db.Shutdown(context.Background()) })

	assertTxSQLCommitErrorReachesCaller(t, t.Context(), db, txSQLMigrationFS(
		`CREATE TABLE txsql_parents (id INTEGER PRIMARY KEY);`,
		`CREATE TABLE txsql_children (
	id INTEGER PRIMARY KEY,
	parent_id INTEGER NOT NULL REFERENCES txsql_parents(id) DEFERRABLE INITIALLY DEFERRED
);`,
	), "FOREIGN KEY constraint failed")
}

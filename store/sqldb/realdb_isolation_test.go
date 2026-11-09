package sqldb_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/credo-go/credo/store/sqldb"
)

// TestRealDBSubtests_LeaveTheItemsTable runs, on a SQLite file in WAL mode, the
// real-database contract subtests that need no server-specific behaviour, in
// TestRealDB_Contracts's order on one shared items table. Each subtest must
// leave the table as it found it, so a later subtest's fixed IDs and unique
// names never collide with an earlier one's rows; this catches a collision
// locally that only the PostgreSQL and MySQL jobs would otherwise reveal.
func TestRealDBSubtests_LeaveTheItemsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "realdb-items.db")
	db, err := sqldb.Open(&sqldb.Config{Driver: "sqlite", DSN: path, MaxOpen: 4, MaxIdle: new(4)})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := db.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Errorf("Shutdown() = %v", shutdownErr)
		}
	})
	ctx := t.Context()
	// WAL lets the pool read the committed rows while a transaction writes.
	if _, walErr := db.Exec(ctx, "PRAGMA journal_mode=WAL"); walErr != nil {
		t.Fatalf("enable WAL: %v", walErr)
	}
	createRealDBItemsTable(t, ctx, db)

	countRows := func(t *testing.T) int {
		t.Helper()
		n, countErr := db.Select((*realDBItem)(nil)).Count(ctx)
		if countErr != nil {
			t.Fatalf("count items: %v", countErr)
		}
		return n
	}

	t.Run("count runs on the query's own connection", func(t *testing.T) {
		testRealDBCountConnection(t, ctx, db)
	})
	if n := countRows(t); n != 0 {
		t.Fatalf("items left by the count-connection subtest = %d, want 0", n)
	}
	t.Run("commit and rollback", func(t *testing.T) {
		testRealDBTransactions(t, ctx, db)
	})
}

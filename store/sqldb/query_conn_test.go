package sqldb_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/pagination"
	"github.com/credo-go/credo/store/sqldb"
)

// TestQueries_ConnFromWrapperOnly pins, on all four query types, that the
// connection chosen through the wrapper's Conn is kept and that a Conn set
// inside Apply is overridden by transaction injection. Two SQLite files tell
// the connections apart: every query is built on defaultDB, and explicitDB's
// client is the explicit connection.
func TestQueries_ConnFromWrapperOnly(t *testing.T) {
	dir := t.TempDir()
	defaultDB := openSelectStateFileDB(t, filepath.Join(dir, "default.db"))
	explicitDB := openSelectStateFileDB(t, filepath.Join(dir, "explicit.db"))
	createUsersTable(t, defaultDB)
	createUsersTable(t, explicitDB)
	ctx := t.Context()
	explicit := explicitDB.Client()

	countUsers := func(t *testing.T, db *sqldb.DB, name string) int {
		t.Helper()
		n, err := db.Select((*User)(nil)).Where("name = ?", name).Count(ctx)
		if err != nil {
			t.Fatalf("count %q: %v", name, err)
		}
		return n
	}
	assertOn := func(t *testing.T, name string, wantExplicit bool) {
		t.Helper()
		inDefault, inExplicit := countUsers(t, defaultDB, name), countUsers(t, explicitDB, name)
		if wantExplicit && (inExplicit != 1 || inDefault != 0) {
			t.Fatalf("%q: default %d / explicit %d rows, want the explicit connection only", name, inDefault, inExplicit)
		}
		if !wantExplicit && (inDefault != 1 || inExplicit != 0) {
			t.Fatalf("%q: default %d / explicit %d rows, want the injected connection only", name, inDefault, inExplicit)
		}
	}
	seedBoth := func(t *testing.T, name string) {
		t.Helper()
		for _, db := range []*sqldb.DB{defaultDB, explicitDB} {
			if _, err := db.Insert(&User{Name: name, Email: name + "@example.com"}).Exec(ctx); err != nil {
				t.Fatalf("seed %q: %v", name, err)
			}
		}
	}

	t.Run("Insert", func(t *testing.T) {
		if _, err := defaultDB.Insert(&User{Name: "insert-conn", Email: "i@example.com"}).
			Conn(explicit).Exec(ctx); err != nil {
			t.Fatalf("Insert with Conn = %v", err)
		}
		assertOn(t, "insert-conn", true)

		if _, err := defaultDB.Insert(&User{Name: "insert-apply", Email: "a@example.com"}).
			Apply(func(q *bun.InsertQuery) *bun.InsertQuery { return q.Conn(explicit) }).
			Exec(ctx); err != nil {
			t.Fatalf("Insert with Apply Conn = %v", err)
		}
		assertOn(t, "insert-apply", false)
	})

	t.Run("Update", func(t *testing.T) {
		seedBoth(t, "update-conn")
		if _, err := defaultDB.Update((*User)(nil)).Set("name = ?", "updated-conn").
			Where("name = ?", "update-conn").Conn(explicit).Exec(ctx); err != nil {
			t.Fatalf("Update with Conn = %v", err)
		}
		assertOn(t, "updated-conn", true)

		seedBoth(t, "update-apply")
		if _, err := defaultDB.Update((*User)(nil)).Set("name = ?", "updated-apply").
			Where("name = ?", "update-apply").
			Apply(func(q *bun.UpdateQuery) *bun.UpdateQuery { return q.Conn(explicit) }).
			Exec(ctx); err != nil {
			t.Fatalf("Update with Apply Conn = %v", err)
		}
		assertOn(t, "updated-apply", false)
	})

	t.Run("Delete", func(t *testing.T) {
		seedBoth(t, "delete-conn")
		if _, err := defaultDB.Delete((*User)(nil)).Where("name = ?", "delete-conn").
			Conn(explicit).Exec(ctx); err != nil {
			t.Fatalf("Delete with Conn = %v", err)
		}
		assertOn(t, "delete-conn", false) // the explicit copy is gone

		seedBoth(t, "delete-apply")
		if _, err := defaultDB.Delete((*User)(nil)).Where("name = ?", "delete-apply").
			Apply(func(q *bun.DeleteQuery) *bun.DeleteQuery { return q.Conn(explicit) }).
			Exec(ctx); err != nil {
			t.Fatalf("Delete with Apply Conn = %v", err)
		}
		assertOn(t, "delete-apply", true) // the default copy is gone
	})

	t.Run("Select", func(t *testing.T) {
		if _, err := explicitDB.Insert(&User{Name: "select-only-explicit", Email: "s@example.com"}).Exec(ctx); err != nil {
			t.Fatalf("seed explicit: %v", err)
		}
		n, err := defaultDB.Select((*User)(nil)).Where("name = ?", "select-only-explicit").
			Conn(explicit).Count(ctx)
		if err != nil || n != 1 {
			t.Fatalf("Count with Conn = %d, %v; want 1 from the explicit connection", n, err)
		}
		page, err := defaultDB.Select().Where("name = ?", "select-only-explicit").Conn(explicit).
			Page[User](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
		if err != nil || page.Total != 1 || len(page.Records) != 1 {
			t.Fatalf("Page with Conn = %+v, %v; want one row from the explicit connection", page, err)
		}
		all, err := defaultDB.Select().Where("name = ?", "select-only-explicit").
			Apply(func(q *bun.SelectQuery) *bun.SelectQuery { return q.Conn(explicit) }).
			All[User](ctx)
		if err != nil || len(all) != 0 {
			t.Fatalf("All with Apply Conn = %+v, %v; want no row from the injected connection", all, err)
		}
	})
}

// TestSelectQuery_CountRunsOnTheInjectedTransaction pins that Count and Page
// count on the query's own connection: inside a transaction they see its
// uncommitted row.
func TestSelectQuery_CountRunsOnTheInjectedTransaction(t *testing.T) {
	db := openSelectStateFileDB(t, filepath.Join(t.TempDir(), "tx.db"))
	createUsersTable(t, db)
	ctx := t.Context()
	rollback := errors.New("roll back")
	err := db.InTx(ctx, func(txCtx context.Context) error {
		if _, err := db.Insert(&User{Name: "uncommitted", Email: "u@example.com"}).Exec(txCtx); err != nil {
			return err
		}
		n, err := db.Select((*User)(nil)).Count(txCtx)
		if err != nil || n != 1 {
			t.Fatalf("Count in the transaction = %d, %v; want its uncommitted row", n, err)
		}
		page, err := db.Select().Page[User](txCtx, &pagination.PageRequest{Page: 1, PerPage: 10})
		if err != nil || page.Total != 1 || len(page.Records) != 1 {
			t.Fatalf("Page in the transaction = %+v, %v; want its uncommitted row", page, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("InTx = %v", err)
	}
}

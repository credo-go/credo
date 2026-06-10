package sqldb_test

import (
	"context"
	"errors"
	"testing"
)

// These tests pin the shallow-copy contract of the proxy terminal methods
// (Insert/Update/Delete Exec): prepareQuery copies the builder before
// injecting the context connection, so
//
//  1. the same builder can be executed more than once (bun reads — never
//     mutates — the builder while generating SQL), and
//  2. an execution inside a transaction must not leak the (by then
//     finished) TX connection into a later execution of the same builder.
//
// If the copy were removed, the post-rollback Exec in each test would fail
// with "transaction has already been committed or rolled back".

func TestInsertExec_BuilderReusableAfterTxRollback(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	q := db.Insert(&User{Name: "pin-insert", Email: "p@i.n"})

	sentinel := errors.New("rollback")
	err := db.InTx(ctx, func(txCtx context.Context) error {
		if _, execErr := q.Exec(txCtx); execErr != nil {
			return execErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx = %v, want rollback sentinel", err)
	}

	// The rolled-back TX is finished. If Exec had stored the injected TX
	// connection on the caller's builder, this second run would fail.
	if _, err := q.Exec(ctx); err != nil {
		t.Fatalf("Exec after rollback = %v (TX connection leaked into the builder?)", err)
	}

	count, err := db.Select((*User)(nil)).Where("name = ?", "pin-insert").Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (rolled-back insert must not persist; the later one must)", count)
	}
}

func TestUpdateExec_BuilderReusableAfterTxRollback(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	if _, err := db.Insert(&User{Name: "pin-update", Email: "old"}).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	q := db.Update((*User)(nil)).Set("email = ?", "new").Where("name = ?", "pin-update")

	sentinel := errors.New("rollback")
	err := db.InTx(ctx, func(txCtx context.Context) error {
		if _, execErr := q.Exec(txCtx); execErr != nil {
			return execErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx = %v, want rollback sentinel", err)
	}

	res, err := q.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec after rollback = %v (TX connection leaked into the builder?)", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("RowsAffected = %d, want 1", n)
	}

	var u User
	if err := db.Select(&u).Where("name = ?", "pin-update").Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if u.Email != "new" {
		t.Errorf("email = %q, want %q (rolled-back update must not persist; the later one must)", u.Email, "new")
	}
}

func TestDeleteExec_BuilderReusableAfterTxRollback(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	if _, err := db.Insert(&User{Name: "pin-delete", Email: "d@e.l"}).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	q := db.Delete((*User)(nil)).Where("name = ?", "pin-delete")

	sentinel := errors.New("rollback")
	err := db.InTx(ctx, func(txCtx context.Context) error {
		if _, execErr := q.Exec(txCtx); execErr != nil {
			return execErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx = %v, want rollback sentinel", err)
	}

	// Row must still exist: the in-TX delete was rolled back.
	count, err := db.Select((*User)(nil)).Where("name = ?", "pin-delete").Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("count after rollback = %d, want 1", count)
	}

	res, err := q.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec after rollback = %v (TX connection leaked into the builder?)", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("RowsAffected = %d, want 1", n)
	}

	count, err = db.Select((*User)(nil)).Where("name = ?", "pin-delete").Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("count after delete = %d, want 0", count)
	}
}

func TestUpdateExec_SameBuilderTwice(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	if _, err := db.Insert(&User{Name: "pin-twice", Email: "old"}).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	q := db.Update((*User)(nil)).Set("email = ?", "x").Where("name = ?", "pin-twice")

	for i := 1; i <= 2; i++ {
		res, err := q.Exec(ctx)
		if err != nil {
			t.Fatalf("Exec #%d = %v (builder must stay executable — bun reads, never mutates, it)", i, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("Exec #%d RowsAffected = %d, want 1", i, n)
		}
	}
}

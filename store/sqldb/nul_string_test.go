package sqldb_test

import (
	"testing"

	"github.com/credo-go/credo/store"
)

// nulStringValue is a string argument carrying a NUL byte. Bun v1.3.0's base
// dialect refuses to render it (the statement fails at the database, nothing
// is persisted) instead of dropping the byte, which stored a value different
// from the one the application validated.
const nulStringValue = "nul\x00x"

// assertNULStringFailsClosed runs insert, which must try to persist
// nulStringValue, and checks the contract shared by SQLite and PostgreSQL:
// the statement fails, the error carries no store.Kind, and count sees no
// row. The error text is Bun's and the database's and is not asserted.
func assertNULStringFailsClosed(
	t *testing.T,
	insert func() error,
	count func() (int, error),
) {
	t.Helper()
	err := insert()
	if err == nil {
		t.Fatal("insert with a NUL byte succeeded, want the statement to fail")
	}
	if kind, ok := store.KindOf(err); ok {
		t.Fatalf("insert with a NUL byte = %v, mapped to store kind %v, want no kind", err, kind)
	}
	n, countErr := count()
	if countErr != nil {
		t.Fatalf("count rows: %v", countErr)
	}
	if n != 0 {
		t.Fatalf("rows = %d, want 0: a NUL-bearing value was persisted", n)
	}
}

func TestNULString_FailsClosedOnSQLite(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()
	countUsers := func() (int, error) {
		var n int
		err := db.Client().NewRaw(`SELECT count(*) FROM users`).Scan(ctx, &n)
		return n, err
	}

	t.Run("model insert", func(t *testing.T) {
		assertNULStringFailsClosed(t, func() error {
			_, err := db.Insert(&User{Name: nulStringValue, Email: "nul@example.com"}).Exec(ctx)
			return err
		}, countUsers)
	})
	t.Run("raw argument", func(t *testing.T) {
		assertNULStringFailsClosed(t, func() error {
			_, err := db.Exec(ctx, `INSERT INTO users (name, email) VALUES (?, ?)`, nulStringValue, "nul@example.com")
			return err
		}, countUsers)
	})
	t.Run("where argument", func(t *testing.T) {
		if _, err := db.Insert(&User{Name: "plain", Email: "plain@example.com"}).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
		var users []User
		err := db.Select().Model(&users).Where("name = ?", nulStringValue).Scan(ctx)
		if err == nil {
			t.Fatal("select with a NUL byte succeeded, want the statement to fail")
		}
		if kind, ok := store.KindOf(err); ok {
			t.Fatalf("select with a NUL byte = %v, mapped to store kind %v, want no kind", err, kind)
		}
	})
}

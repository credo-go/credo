package sqldb_test

import (
	"context"
	"testing"

	"github.com/uptrace/bun"
)

// TestQueryProxyBuilders exercises the builder-method proxies on each query
// type. These methods only append to the underlying bun query and return the
// wrapper for chaining, so building the query (and Unwrap) covers them without
// depending on dialect-specific execution semantics. A final round-trip
// Insert/Select proves the proxies drive real SQL end to end.
func TestQueryProxyBuilders(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	t.Run("insert builders", func(t *testing.T) {
		q := db.Insert(&User{Name: "z", Email: "z@z"}).
			Column("name", "email").
			Value("email", "?", "v@v").
			On("CONFLICT (name) DO UPDATE").
			Set("email = EXCLUDED.email").
			Returning("id").
			Conn(db.Client()).
			Apply(func(iq *bun.InsertQuery) *bun.InsertQuery { return iq })
		if q.Unwrap() == nil {
			t.Fatal("Insert Unwrap returned nil")
		}
	})

	t.Run("update builders", func(t *testing.T) {
		q := db.Update().
			Model(&User{ID: 1, Email: "new@z"}).
			Column("email").
			WherePK().
			OmitZero().
			Returning("id").
			Conn(db.Client()).
			ApplyQueryBuilder(nil). // nil fn is a no-op
			ApplyQueryBuilder(func(qb bun.QueryBuilder) bun.QueryBuilder { return qb }).
			Apply(func(uq *bun.UpdateQuery) *bun.UpdateQuery { return uq })
		if q.Unwrap() == nil {
			t.Fatal("Update Unwrap returned nil")
		}
	})

	t.Run("delete builders", func(t *testing.T) {
		q := db.Delete().
			Model(&User{ID: 1}).
			WherePK().
			Returning("id").
			Conn(db.Client()).
			ApplyQueryBuilder(func(qb bun.QueryBuilder) bun.QueryBuilder { return qb }).
			Apply(func(dq *bun.DeleteQuery) *bun.DeleteQuery { return dq })
		if q.Unwrap() == nil {
			t.Fatal("Delete Unwrap returned nil")
		}
	})

	t.Run("select builders", func(t *testing.T) {
		var users []User
		q := db.Select(&users).
			Column("id", "name").
			Distinct().
			WhereOr("id = ?", 1).
			GroupExpr("name").
			Having("COUNT(*) > ?", 0).
			ApplyQueryBuilder(func(qb bun.QueryBuilder) bun.QueryBuilder { return qb }).
			Apply(func(sq *bun.SelectQuery) *bun.SelectQuery { return sq })
		if q.Unwrap() == nil {
			t.Fatal("Select Unwrap returned nil")
		}
	})

	t.Run("round trip via proxies", func(t *testing.T) {
		if _, err := db.Insert(&User{Name: "rt", Email: "rt@z"}).Exec(ctx); err != nil {
			t.Fatalf("insert exec: %v", err)
		}
		var got User
		if err := db.Select(&got).Column("id", "name", "email").Where("name = ?", "rt").Scan(ctx); err != nil {
			t.Fatalf("select scan: %v", err)
		}
		if got.Email != "rt@z" {
			t.Errorf("round trip email = %q, want rt@z", got.Email)
		}
	})
}

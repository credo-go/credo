package sqldb

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	_ "modernc.org/sqlite"
)

func TestCloneBunSelectQuery_Nil(t *testing.T) {
	if got := cloneBunSelectQuery(nil); got != nil {
		t.Fatalf("cloneBunSelectQuery(nil) = %p, want nil", got)
	}
}

func TestCloneBunSelectQuery_PreservesCTEMaterialization(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	db := bun.NewDB(sqlDB, pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	tests := []struct {
		name string
		with *bun.WithQuery
		want string
	}{
		{
			name: "materialized",
			with: bun.NewWithQuery("cte", db.NewSelect().ColumnExpr("1 AS n")).Materialized(),
			want: "AS MATERIALIZED",
		},
		{
			name: "not materialized",
			with: bun.NewWithQuery("cte", db.NewSelect().ColumnExpr("1 AS n")).NotMaterialized(),
			want: "AS NOT MATERIALIZED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := db.NewSelect().WithQuery(tt.with).TableExpr("cte")
			got := cloneBunSelectQuery(query).String()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("cloned query = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectQuery_PrepareUsesActualRawConnectionState(t *testing.T) {
	primary, err := Open(&Config{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open primary DB: %v", err)
	}
	t.Cleanup(func() { _ = primary.Shutdown(t.Context()) })

	ambient, err := Open(&Config{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open ambient DB: %v", err)
	}
	t.Cleanup(func() { _ = ambient.Shutdown(t.Context()) })

	ctx := primary.txScope.WithTx(t.Context(), ambient.Client())
	tests := []struct {
		name  string
		apply func(*bun.SelectQuery) *bun.SelectQuery
	}{
		{
			name: "Apply clears wrapper Conn",
			apply: func(query *bun.SelectQuery) *bun.SelectQuery {
				return query.Conn(nil)
			},
		},
		{
			name: "Apply replaces raw query",
			apply: func(*bun.SelectQuery) *bun.SelectQuery {
				return primary.Client().NewSelect()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := primary.Select().Conn(primary.Client()).Apply(tt.apply)
			prepared := query.prepareTerminal(ctx)
			conn, err := bunSelectQueryConn(prepared)
			if err != nil {
				t.Fatalf("prepared connection: %v", err)
			}
			if conn != ambient.Client().DB {
				t.Fatalf("prepared connection = %T, want the ambient *sql.DB", conn)
			}
		})
	}
}

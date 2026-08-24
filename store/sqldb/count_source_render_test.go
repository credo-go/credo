package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

type mysqlCountGuardDriver struct{}

func (*mysqlCountGuardDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("mysql count guard unexpectedly opened a connection")
}

type mysqlCountGuardConnector struct {
	connects atomic.Int32
}

func (c *mysqlCountGuardConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects.Add(1)
	return nil, errors.New("mysql count guard unexpectedly connected")
}

func (*mysqlCountGuardConnector) Driver() driver.Driver {
	return new(mysqlCountGuardDriver)
}

type mysqlCountGuardDialect struct {
	*mysqldialect.Dialect
}

func (*mysqlCountGuardDialect) Init(*sql.DB) {}

func newMySQLCountGuardDialect() *mysqlCountGuardDialect {
	return &mysqlCountGuardDialect{Dialect: mysqldialect.New()}
}

type mysqlCountGuardParent struct {
	bun.BaseModel `bun:"table:parents,alias:mcgp"`
	ID            int                   `bun:"id,pk"`
	Child         *mysqlCountGuardChild `bun:"rel:has-one,join:id=parent_id"`
}

type mysqlCountGuardChild struct {
	bun.BaseModel `bun:"table:children,alias:mcgc"`
	ID            int `bun:"id,pk"`
	ParentID      int `bun:"parent_id"`
}

func TestSelectQuery_MySQLCountRendersRelationApplyOnce(t *testing.T) {
	connector := new(mysqlCountGuardConnector)
	db, err := Open(
		&Config{},
		WithConnector(connector),
		WithDialect(newMySQLCountGuardDialect()),
	)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })

	var applies atomic.Int32
	_, err = db.Select((*mysqlCountGuardParent)(nil)).
		Relation("Child", func(query *bun.SelectQuery) *bun.SelectQuery {
			applies.Add(1)
			return query
		}).
		Count(t.Context())
	if err == nil {
		t.Fatal("Count() succeeded with a connector that rejects every connection")
	}
	if errors.Is(err, ErrUnsupportedCountQuery) {
		t.Fatalf("Count() = %v, relation projection should reach database execution", err)
	}
	if got := applies.Load(); got != 1 {
		t.Fatalf("relation apply calls = %d, want exactly 1 render", got)
	}
}

func TestSelectQuery_CountRejectsLateRelationWindowMutation(t *testing.T) {
	connector := new(mysqlCountGuardConnector)
	db, err := Open(
		&Config{},
		WithConnector(connector),
		WithDialect(newMySQLCountGuardDialect()),
	)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })

	var applies atomic.Int32
	_, err = db.Select((*mysqlCountGuardParent)(nil)).
		Relation("Child", func(query *bun.SelectQuery) *bun.SelectQuery {
			applies.Add(1)
			return query.Limit(1)
		}).
		Count(t.Context())
	if !errors.Is(err, ErrUnsupportedCountQuery) {
		t.Fatalf("Count() = %v, want ErrUnsupportedCountQuery", err)
	}
	if got := applies.Load(); got != 1 {
		t.Fatalf("relation apply calls = %d, want 1", got)
	}
	if got := connector.connects.Load(); got != 0 {
		t.Fatalf("Connect calls = %d, want 0", got)
	}
}

func TestSelectQuery_CountRejectsLateRelationShapeMutation(t *testing.T) {
	connector := new(mysqlCountGuardConnector)
	db, err := Open(
		&Config{},
		WithConnector(connector),
		WithDialect(newMySQLCountGuardDialect()),
	)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(context.WithoutCancel(t.Context())) })

	tests := []struct {
		name  string
		apply func(*bun.SelectQuery) *bun.SelectQuery
	}{
		{
			name: "non-table model replacement",
			apply: func(query *bun.SelectQuery) *bun.SelectQuery {
				replacement := map[string]any{}
				return query.Model(&replacement)
			},
		},
		{
			name: "standalone Having",
			apply: func(query *bun.SelectQuery) *bun.SelectQuery {
				return query.Having("COUNT(*) > 0")
			},
		},
		{
			name: "compound root",
			apply: func(query *bun.SelectQuery) *bun.SelectQuery {
				other := db.db.NewSelect().Model((*mysqlCountGuardParent)(nil)).Column("id")
				return query.Union(other)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := connector.connects.Load()
			_, err := db.Select((*mysqlCountGuardParent)(nil)).
				Relation("Child", tt.apply).
				Count(t.Context())
			if !errors.Is(err, ErrUnsupportedCountQuery) {
				t.Fatalf("Count() = %v, want ErrUnsupportedCountQuery", err)
			}
			if got := connector.connects.Load(); got != before {
				t.Fatalf("Connect calls = %d, want %d", got, before)
			}
		})
	}
}

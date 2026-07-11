package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"

	"github.com/credo-go/credo/pagination"
)

func TestMySQLMainSelectExpressions(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "plain",
			query: "SELECT `u`.`id`, `u`.`name` FROM `users` AS `u`",
			want:  []string{"`u`.`id`", "`u`.`name`"},
		},
		{
			name: "CTE and nested commas",
			query: "WITH `x` AS (SELECT 1 AS `n`) " +
				"SELECT DISTINCT COALESCE(`u`.`name`, 'a,b') AS `name`, " +
				"CAST(`u`.`id` AS CHAR) AS `id` FROM `users` AS `u`",
			want: []string{
				"DISTINCT COALESCE(`u`.`name`, 'a,b') AS `name`",
				"CAST(`u`.`id` AS CHAR) AS `id`",
			},
		},
		{
			name:  "no FROM",
			query: "SELECT 1 AS `one`, CONCAT('x,y', 'z') AS `two`",
			want:  []string{"1 AS `one`", "CONCAT('x,y', 'z') AS `two`"},
		},
		{
			name:  "executable marker inside value is data",
			query: "SELECT `id` FROM `users` WHERE `name` = 'customer /*! note /*+ hint /*M! data'",
			want:  []string{"`id`"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mysqlMainSelectExpressions(tt.query)
			if err != nil {
				t.Fatalf("mysqlMainSelectExpressions() = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("expressions = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMySQLMainSelectExpressionsRejectsUnsafeSyntax(t *testing.T) {
	for _, query := range []string{
		"SELECT /*! 1 AS injected, */ id FROM users",
		"SELECT /*+ SET_VAR(sort_buffer_size=16M) */ id FROM users",
		"SELECT id /*M!100100 , id AS ID */ FROM users",
		"SELECT id /*M! , id */ AS id FROM users",
		"SELECT COALESCE(id, 1 AS value FROM users",
	} {
		t.Run(query, func(t *testing.T) {
			if _, err := mysqlMainSelectExpressions(query); err == nil {
				t.Fatalf("mysqlMainSelectExpressions(%q) succeeded, want fail-loud error", query)
			}
		})
	}
}

func TestValidateMySQLCountSourceRejectsQuoteModeAmbiguity(t *testing.T) {
	query := "SELECT 'x\\' AS id /*! , id + */ ' AS guard FROM ignored ' AS id"
	err := validateMySQLCountSource(query)
	if !errors.Is(err, ErrUnsupportedCountQuery) {
		t.Fatalf("validateMySQLCountSource() = %v, want ErrUnsupportedCountQuery", err)
	}
}

func TestMySQLProjectionOutputName(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       string
		wantErr    bool
	}{
		{name: "qualified identifier", expression: "`u`.`id`", want: "id"},
		{name: "explicit alias", expression: "COUNT(*) AS `total`", want: "total"},
		{name: "cast alias", expression: "CAST(`id` AS CHAR) AS `text_id`", want: "text_id"},
		{name: "SQL-mode-dependent quoted alias", expression: "1 AS 'one'", wantErr: true},
		{name: "wildcard", expression: "`u`.*", wantErr: true},
		{name: "raw expression needs alias", expression: "COUNT(*)", wantErr: true},
		{name: "implicit alias needs AS", expression: "`u`.`id` user_id", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mysqlProjectionOutputName(tt.expression)
			if (err != nil) != tt.wantErr {
				t.Fatalf("mysqlProjectionOutputName() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

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

type mysqlCountGuardUser struct {
	bun.BaseModel `bun:"table:users,alias:mcgu"`
	ID            int    `bun:"id,pk"`
	Name          string `bun:"name"`
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

type mysqlCountGuardHookKey struct{}

type mysqlCountGuardHookUser struct {
	bun.BaseModel `bun:"table:users,alias:mcghu"`
	ID            int `bun:"id,pk"`
}

func (*mysqlCountGuardHookUser) BeforeSelect(ctx context.Context, query *bun.SelectQuery) error {
	if calls, ok := ctx.Value(mysqlCountGuardHookKey{}).(*atomic.Int32); ok {
		calls.Add(1)
	}
	query.Column("id").ColumnExpr("?TableAlias.id AS ID")
	return nil
}

func TestSelectQuery_MySQLCountProjectionFailsBeforeConnect(t *testing.T) {
	connector := new(mysqlCountGuardConnector)
	db, err := Open(
		&Config{},
		WithConnector(connector),
		WithDialect(newMySQLCountGuardDialect()),
	)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "Count duplicate output name",
			run: func() error {
				_, err := db.Select((*mysqlCountGuardUser)(nil)).
					Column("id").
					ColumnExpr("?TableAlias.id AS id").
					Count(t.Context())
				return err
			},
		},
		{
			name: "Page duplicate output name",
			run: func() error {
				_, err := db.Select().
					Column("id").
					ColumnExpr("?TableAlias.id AS id").
					Page[mysqlCountGuardUser](
					t.Context(),
					&pagination.PageRequest{Page: 1, PerPage: 10},
				)
				return err
			},
		},
		{
			name: "raw expression without alias",
			run: func() error {
				_, err := db.Select((*mysqlCountGuardUser)(nil)).
					ColumnExpr("COUNT(*)").
					Count(t.Context())
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := connector.connects.Load()
			err := tt.run()
			if !errors.Is(err, ErrUnsupportedCountQuery) {
				t.Fatalf("error = %v, want ErrUnsupportedCountQuery", err)
			}
			if got := connector.connects.Load(); got != before {
				t.Fatalf("Connect calls = %d, want %d (no database I/O)", got, before)
			}
		})
	}
}

func TestValidateMySQLCountSourceProjection(t *testing.T) {
	db, err := Open(
		&Config{},
		WithConnector(new(mysqlCountGuardConnector)),
		WithDialect(newMySQLCountGuardDialect()),
	)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

	tests := []struct {
		name      string
		query     *bun.SelectQuery
		wantErr   bool
		forbidden string
	}{
		{
			name:  "default model columns are unique",
			query: db.db.NewSelect().Model((*mysqlCountGuardUser)(nil)),
		},
		{
			name: "raw aggregate has explicit alias",
			query: db.db.NewSelect().
				Model((*mysqlCountGuardUser)(nil)).
				ColumnExpr("COUNT(*) AS total"),
		},
		{
			name: "distinct qualified column",
			query: db.db.NewSelect().
				Model((*mysqlCountGuardUser)(nil)).
				Column("name").
				Distinct(),
		},
		{
			name: "group and having qualified column",
			query: db.db.NewSelect().
				Model((*mysqlCountGuardUser)(nil)).
				Column("name").
				Group("name").
				Having("COUNT(*) > 0"),
		},
		{
			name: "top-level comma expressions are checked separately",
			query: db.db.NewSelect().
				Model((*mysqlCountGuardUser)(nil)).
				ColumnExpr("?TableAlias.id, ?TableAlias.id"),
			wantErr: true,
		},
		{
			name: "case-insensitive aliases collide",
			query: db.db.NewSelect().
				Model((*mysqlCountGuardUser)(nil)).
				ColumnExpr("?TableAlias.id AS value").
				ColumnExpr("?TableAlias.id AS VALUE"),
			wantErr: true,
		},
		{
			name: "validation error does not expose bound values",
			query: db.db.NewSelect().
				Model((*mysqlCountGuardUser)(nil)).
				ColumnExpr("CASE WHEN name = ? THEN id END", "top-secret"),
			wantErr:   true,
			forbidden: "top-secret",
		},
		{
			name: "control character starts dash comment",
			query: db.db.NewSelect().
				Model((*mysqlCountGuardUser)(nil)).
				ColumnExpr("id --\x01 AS ignored FROM ignored\n, id AS ID"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered, err := renderBunSelectCountSource(db.db.QueryGen(), tt.query)
			if err == nil {
				err = validateMySQLCountSource(string(rendered))
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("validation error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrUnsupportedCountQuery) {
				t.Fatalf("validation error = %v, want ErrUnsupportedCountQuery", err)
			}
			if err != nil && tt.forbidden != "" && strings.Contains(err.Error(), tt.forbidden) {
				t.Fatalf("validation error %q exposes bound value %q", err, tt.forbidden)
			}
		})
	}
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
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

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
		t.Fatalf("Count() = %v, relation aliases should have a provably unique projection", err)
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
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

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
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

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

func TestSelectQuery_MySQLCountValidatesHookProjectionBeforeConnect(t *testing.T) {
	connector := new(mysqlCountGuardConnector)
	db, err := Open(
		&Config{},
		WithConnector(connector),
		WithDialect(newMySQLCountGuardDialect()),
	)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

	var hookCalls atomic.Int32
	ctx := context.WithValue(t.Context(), mysqlCountGuardHookKey{}, &hookCalls)
	_, err = db.Select((*mysqlCountGuardHookUser)(nil)).Count(ctx)
	if !errors.Is(err, ErrUnsupportedCountQuery) {
		t.Fatalf("Count() = %v, want ErrUnsupportedCountQuery", err)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("BeforeSelect calls = %d, want 1", got)
	}
	if got := connector.connects.Load(); got != 0 {
		t.Fatalf("Connect calls = %d, want 0", got)
	}
}

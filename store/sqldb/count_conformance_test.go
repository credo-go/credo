package sqldb_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"

	"github.com/credo-go/credo/pagination"
	"github.com/credo-go/credo/store/sqldb"
)

// The count conformance suite pins the contract of Count, Page, PageWithCount
// and ApplyFilter against the pinned Bun on every database the module
// supports: TestCountConformance_SQLite runs it locally, and TestRealDB_Contracts
// runs it on PostgreSQL and MySQL in CI. A Bun upgrade passes when it passes.

const countItemsTable = "credo_count_items"

type countItem struct {
	bun.BaseModel `bun:"table:credo_count_items,alias:ci"`
	ID            int64  `bun:"id,pk"`
	Tenant        string `bun:"tenant"`
	Grp           string `bun:"grp"`
	Name          string `bun:"name"`
}

// countHookItem implements bun.BeforeSelectHook, which Bun runs for the data
// query and not for its count, so Count and Page cannot prove its total.
type countHookItem struct {
	bun.BaseModel `bun:"table:credo_count_items,alias:ci"`
	ID            int64  `bun:"id,pk"`
	Tenant        string `bun:"tenant"`
	Grp           string `bun:"grp"`
	Name          string `bun:"name"`
}

func (*countHookItem) BeforeSelect(_ context.Context, query *bun.SelectQuery) error {
	query.Where("?TableAlias.tenant = ?", "a")
	return nil
}

type countAppendCalls struct{}

// countAppendItem implements only bun.BeforeAppendModelHook, the hook a model
// uses to set timestamps; it keeps its automatic total.
type countAppendItem struct {
	bun.BaseModel `bun:"table:credo_count_items,alias:ci"`
	ID            int64  `bun:"id,pk"`
	Tenant        string `bun:"tenant"`
	Grp           string `bun:"grp"`
	Name          string `bun:"name"`
}

func (*countAppendItem) BeforeAppendModel(ctx context.Context, _ schema.Query) error {
	if calls, ok := ctx.Value(countAppendCalls{}).(*atomic.Int64); ok {
		calls.Add(1)
	}
	return nil
}

// countRelItem has a relation, so a Relation callback can be applied to it.
type countRelItem struct {
	bun.BaseModel `bun:"table:credo_count_items,alias:ci"`
	ID            int64      `bun:"id,pk"`
	Tenant        string     `bun:"tenant"`
	Self          *countItem `bun:"rel:belongs-to,join:id=id"`
}

// countRecorder records the SQL of every statement, for the no-I/O and
// clause assertions.
type countRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (r *countRecorder) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (r *countRecorder) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	r.mu.Lock()
	r.queries = append(r.queries, event.Query)
	r.mu.Unlock()
}

func (r *countRecorder) Reset() {
	r.mu.Lock()
	r.queries = nil
	r.mu.Unlock()
}

func (r *countRecorder) Queries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.queries)
}

var countFixture = []countItem{
	{ID: 1, Tenant: "a", Grp: "x", Name: "n1"},
	{ID: 2, Tenant: "a", Grp: "x", Name: "n2"},
	{ID: 3, Tenant: "a", Grp: "y", Name: "n3"},
	{ID: 4, Tenant: "b", Grp: "y", Name: "n4"},
	{ID: 5, Tenant: "b", Grp: "z", Name: "n5"},
	{ID: 6, Tenant: "b", Grp: "z", Name: "n6"},
	{ID: 7, Tenant: "a", Grp: "z", Name: "n7"},
}

func tenantScope(tenant string) func(*sqldb.Filter) {
	return func(f *sqldb.Filter) { f.Where("tenant = ?", tenant) }
}

func TestCountConformance_SQLite(t *testing.T) {
	db := openTestDB(t)
	runCountConformance(t, t.Context(), db)
}

// runCountConformance runs the suite on db. It owns countItemsTable: it
// creates it, reseeds it per section, and drops it when it ends.
func runCountConformance(t *testing.T, ctx context.Context, db *sqldb.DB) {
	t.Helper()
	if _, err := db.Exec(ctx, "DROP TABLE IF EXISTS "+countItemsTable); err != nil {
		t.Fatalf("drop %s: %v", countItemsTable, err)
	}
	if _, err := db.Exec(ctx, `
		CREATE TABLE credo_count_items (
			id BIGINT PRIMARY KEY,
			tenant VARCHAR(64) NOT NULL,
			grp VARCHAR(64) NOT NULL,
			name VARCHAR(191) NOT NULL
		)
	`); err != nil {
		t.Fatalf("create %s: %v", countItemsTable, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+countItemsTable); err != nil {
			t.Errorf("drop %s: %v", countItemsTable, err)
		}
	})
	recorder := &countRecorder{}
	db.Client().AddQueryHook(recorder)

	seedCountItems(t, ctx, db)
	t.Run("provable totals match the derived-table oracle", func(t *testing.T) {
		testCountProvableTotals(t, ctx, db, recorder)
	})
	t.Run("counts drop ORDER BY LIMIT OFFSET and FOR", func(t *testing.T) {
		testCountDropsSortWindowAndLock(t, ctx, db, recorder)
	})
	t.Run("unknown queries fail before I/O", func(t *testing.T) {
		testCountUnknownQueries(t, ctx, db, recorder)
	})
	t.Run("BeforeAppendModelHook keeps the total", func(t *testing.T) {
		testCountBeforeAppendModelHook(t, ctx, db, recorder)
	})
	t.Run("PageWithCount", func(t *testing.T) {
		testPageWithCount(t, ctx, db, recorder)
	})
	t.Run("ApplyFilter scopes select update and delete", func(t *testing.T) {
		testApplyFilterScopes(t, ctx, db)
	})
}

func seedCountItems(t *testing.T, ctx context.Context, db *sqldb.DB) {
	t.Helper()
	if _, err := db.Exec(ctx, "DELETE FROM "+countItemsTable); err != nil {
		t.Fatalf("clear %s: %v", countItemsTable, err)
	}
	items := slices.Clone(countFixture)
	if _, err := db.Insert(&items).Exec(ctx); err != nil {
		t.Fatalf("seed %s: %v", countItemsTable, err)
	}
}

type countShape struct {
	name      string
	build     func(*sqldb.SelectQuery) *sqldb.SelectQuery
	want      int
	countOnly bool // a model-less source, such as TableExpr, that Page's T cannot share
}

func provableCountShapes() []countShape {
	return []countShape{
		{name: "all rows", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery { return q }, want: 7},
		{name: "where", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Where("ci.tenant = ?", "a")
		}, want: 4},
		{name: "where or", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Where("ci.grp = ?", "x").WhereOr("ci.grp = ?", "z")
		}, want: 5},
		{name: "sorted", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.OrderExpr("ci.name DESC")
		}, want: 7},
		{name: "filter scope", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.ApplyFilter(tenantScope("a"))
		}, want: 4},
		{name: "filter group", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.ApplyFilter(func(f *sqldb.Filter) {
				f.Where("tenant = ?", "b").WhereGroup(" AND ", func(g *sqldb.Filter) {
					g.Where("grp = ?", "y").WhereOr("grp = ?", "z")
				})
			})
		}, want: 3},
		{name: "join", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Join("JOIN credo_count_items AS peer ON peer.grp = ci.grp AND peer.id <> ci.id")
		}, want: 10},
		{name: "distinct column", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Column("grp").Distinct()
		}, want: 3},
		{name: "distinct raw expression", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Distinct().ColumnExpr("ci.tenant AS tenant")
		}, want: 2},
		{name: "group", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Column("grp").GroupExpr("ci.grp")
		}, want: 3},
		{name: "group with aggregate expression", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.ColumnExpr("ci.grp AS grp").ColumnExpr("COUNT(*) AS id").GroupExpr("ci.grp")
		}, want: 3},
		{name: "group and having", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Column("grp").GroupExpr("ci.grp").Having("COUNT(*) >= ?", 3)
		}, want: 1},
		{name: "distinct aggregate with having", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Distinct().ColumnExpr("COUNT(*) AS id").Having("COUNT(*) > ?", 3)
		}, want: 1},
		{name: "table expression", countOnly: true, build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.TableExpr("credo_count_items AS ci").Where("ci.tenant = ?", "b")
		}, want: 3},
	}
}

// countOracle is the count source Credo ran before Bun's native count: the
// complete data query, wrapped as a derived table and counted.
func countOracle(t *testing.T, ctx context.Context, db *sqldb.DB, q *sqldb.SelectQuery, model any) int {
	t.Helper()
	raw := q.Clone().Unwrap()
	if model != nil {
		raw = raw.Model(model)
	}
	var n int
	if err := db.QueryRow(ctx, &n, "SELECT count(*) FROM (?) AS _credo_count_source", bun.Safe(raw.String())); err != nil {
		t.Fatalf("oracle count: %v", err)
	}
	return n
}

func testCountProvableTotals(t *testing.T, ctx context.Context, db *sqldb.DB, recorder *countRecorder) {
	for _, shape := range provableCountShapes() {
		t.Run(shape.name, func(t *testing.T) {
			var oracle int
			if shape.countOnly {
				oracle = countOracle(t, ctx, db, shape.build(db.Select()), nil)
			} else {
				oracle = countOracle(t, ctx, db, shape.build(db.Select()), &[]countItem{})
			}
			if oracle != shape.want {
				t.Fatalf("oracle = %d, want %d", oracle, shape.want)
			}

			countQuery := db.Select()
			if !shape.countOnly {
				countQuery = db.Select((*countItem)(nil))
			}
			recorder.Reset()
			total, err := shape.build(countQuery).Count(ctx)
			if err != nil {
				t.Fatalf("Count() = %v", err)
			}
			if total != oracle {
				t.Fatalf("Count() = %d, want oracle %d", total, oracle)
			}
			if queries := recorder.Queries(); len(queries) != 1 || !strings.Contains(strings.ToLower(queries[0]), "count(*)") {
				t.Fatalf("Count statements = %q, want one Bun count", queries)
			}
			if shape.countOnly {
				return
			}

			all, err := shape.build(db.Select()).All[countItem](ctx)
			if err != nil {
				t.Fatalf("All() = %v", err)
			}
			if len(all) != oracle {
				t.Fatalf("All() returned %d rows, want oracle %d", len(all), oracle)
			}

			req := &pagination.PageRequest{Page: 2, PerPage: 2}
			page, err := shape.build(db.Select()).Page[countItem](ctx, req)
			if err != nil {
				t.Fatalf("Page() = %v", err)
			}
			wantRecords := min(max(oracle-2, 0), 2)
			if page.Total != int64(oracle) || len(page.Records) != wantRecords {
				t.Fatalf("Page() = total %d / %d records, want %d / %d",
					page.Total, len(page.Records), oracle, wantRecords)
			}
		})
	}
}

func assertCountSQLDropsClauses(t *testing.T, statement string) {
	t.Helper()
	upper := strings.ToUpper(statement)
	for _, clause := range []string{"ORDER BY", "LIMIT", "OFFSET", "FOR UPDATE"} {
		if strings.Contains(upper, clause) {
			t.Fatalf("count SQL %q contains %s", statement, clause)
		}
	}
}

func testCountDropsSortWindowAndLock(t *testing.T, ctx context.Context, db *sqldb.DB, recorder *countRecorder) {
	for _, tt := range []struct {
		name  string
		build func(*sqldb.SelectQuery) *sqldb.SelectQuery
		want  int
	}{
		{name: "plain", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery { return q }, want: 7},
		{name: "distinct", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Column("grp").Distinct()
		}, want: 3},
		{name: "group", build: func(q *sqldb.SelectQuery) *sqldb.SelectQuery {
			return q.Column("grp").GroupExpr("ci.grp")
		}, want: 3},
	} {
		t.Run("Count "+tt.name, func(t *testing.T) {
			recorder.Reset()
			query := tt.build(db.Select((*countItem)(nil))).OrderExpr("ci.grp DESC").Limit(1).Offset(1)
			total, err := query.Count(ctx)
			if err != nil {
				t.Fatalf("Count() = %v", err)
			}
			if total != tt.want {
				t.Fatalf("Count() = %d, want %d rows before sort and window", total, tt.want)
			}
			queries := recorder.Queries()
			if len(queries) != 1 {
				t.Fatalf("Count statements = %q, want one", queries)
			}
			assertCountSQLDropsClauses(t, queries[0])
		})

		t.Run("Page "+tt.name, func(t *testing.T) {
			recorder.Reset()
			page, err := tt.build(db.Select()).OrderExpr("ci.grp DESC").
				Page[countItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 2})
			if err != nil {
				t.Fatalf("Page() = %v", err)
			}
			if page.Total != int64(tt.want) || len(page.Records) != 2 {
				t.Fatalf("Page() = total %d / %d records, want %d / 2", page.Total, len(page.Records), tt.want)
			}
			queries := recorder.Queries()
			if len(queries) != 2 {
				t.Fatalf("Page statements = %q, want count + data", queries)
			}
			assertCountSQLDropsClauses(t, queries[0])
			if !strings.Contains(strings.ToUpper(queries[1]), "LIMIT") {
				t.Fatalf("data SQL %q lacks the page window", queries[1])
			}
		})

		// Bun conformance: its native count drops a lock clause too. FOR is
		// reachable only through an escape hatch, so the wrapper never counts
		// a locked query; this pins the Bun behaviour the rule relies on.
		t.Run("Bun count "+tt.name+" drops FOR", func(t *testing.T) {
			recorder.Reset()
			raw := tt.build(db.Select((*countItem)(nil))).Clone().Unwrap().
				OrderExpr("ci.grp DESC").Limit(1).Offset(1).For("UPDATE")
			total, err := raw.Count(ctx)
			if err != nil {
				t.Fatalf("bun Count() = %v", err)
			}
			if int(total) != tt.want {
				t.Fatalf("bun Count() = %d, want %d", total, tt.want)
			}
			queries := recorder.Queries()
			if len(queries) != 1 {
				t.Fatalf("bun Count statements = %q, want one", queries)
			}
			assertCountSQLDropsClauses(t, queries[0])
		})
	}
}

func testCountUnknownQueries(t *testing.T, ctx context.Context, db *sqldb.DB, recorder *countRecorder) {
	req := &pagination.PageRequest{Page: 1, PerPage: 10}
	identity := func(q *bun.SelectQuery) *bun.SelectQuery { return q }
	for _, tt := range []struct {
		name  string
		count func() (int, error)
		page  func() error
		want  []string // the rule's culprit and words
	}{
		{
			name:  "Apply",
			count: func() (int, error) { return db.Select((*countItem)(nil)).Apply(identity).Count(ctx) },
			page: func() error {
				_, err := db.Select().Apply(identity).Page[countItem](ctx, req)
				return err
			},
			want: []string{"built with Apply", "whose changes the wrapper cannot see"},
		},
		{
			name: "Unwrap",
			count: func() (int, error) {
				q := db.Select((*countItem)(nil))
				q.Unwrap().Where("ci.tenant = ?", "a")
				return q.Count(ctx)
			},
			page: func() error {
				q := db.Select()
				q.Unwrap().Where("ci.tenant = ?", "a")
				_, err := q.Page[countItem](ctx, req)
				return err
			},
			want: []string{"built with Unwrap", "whose changes the wrapper cannot see"},
		},
		{
			name: "Relation callback",
			count: func() (int, error) {
				return db.Select((*countRelItem)(nil)).Relation("Self", identity).Count(ctx)
			},
			want: []string{`built with a Relation("Self") callback`, "whose changes the wrapper cannot see"},
		},
		{
			name: "compound through Apply",
			count: func() (int, error) {
				other := db.Client().NewSelect().Model((*countItem)(nil)).Column("id")
				return db.Select((*countItem)(nil)).Column("id").
					Apply(func(q *bun.SelectQuery) *bun.SelectQuery { return q.Union(other) }).
					Count(ctx)
			},
			want: []string{"built with Apply"},
		},
		{
			name: "raw ColumnExpr",
			count: func() (int, error) {
				return db.Select((*countItem)(nil)).ColumnExpr("COUNT(*) AS id").Count(ctx)
			},
			page: func() error {
				_, err := db.Select().ColumnExpr("COUNT(*) AS id").Page[countItem](ctx, req)
				return err
			},
			want: []string{`raw ColumnExpr("COUNT(*) AS id") without GroupExpr or Distinct`, "Bun's count replaces the projection"},
		},
		{
			name: "Having without GroupExpr",
			count: func() (int, error) {
				return db.Select((*countItem)(nil)).Having("COUNT(*) > ?", 0).Count(ctx)
			},
			page: func() error {
				_, err := db.Select().Having("COUNT(*) > ?", 0).Page[countItem](ctx, req)
				return err
			},
			want: []string{`Having("COUNT(*) > ?") without GroupExpr or Distinct`},
		},
		{
			name: "BeforeSelect hook",
			count: func() (int, error) {
				return db.Select((*countHookItem)(nil)).Count(ctx)
			},
			page: func() error {
				_, err := db.Select().Page[countHookItem](ctx, req)
				return err
			},
			want: []string{"model type *sqldb_test.countHookItem implements BeforeSelectHook", "not for its count"},
		},
		{
			name: "escape hatch named first",
			count: func() (int, error) {
				return db.Select((*countHookItem)(nil)).ColumnExpr("ci.id").Apply(identity).Count(ctx)
			},
			want: []string{"built with Apply"},
		},
	} {
		assertUnknown := func(t *testing.T, terminal string, err error) {
			t.Helper()
			if !errors.Is(err, sqldb.ErrUnsupportedCountQuery) {
				t.Fatalf("error = %v, want ErrUnsupportedCountQuery", err)
			}
			msg := err.Error()
			for _, part := range append([]string{
				terminal + " cannot prove the total of this query",
				"count it with a query of your own and page it with PageWithCount",
			}, tt.want...) {
				if !strings.Contains(msg, part) {
					t.Fatalf("error %q does not contain %q", msg, part)
				}
			}
			if queries := recorder.Queries(); len(queries) != 0 {
				t.Fatalf("unknown query ran statements %q, want none", queries)
			}
		}
		t.Run("Count "+tt.name, func(t *testing.T) {
			recorder.Reset()
			_, err := tt.count()
			assertUnknown(t, "Count", err)
		})
		if tt.page != nil {
			t.Run("Page "+tt.name, func(t *testing.T) {
				recorder.Reset()
				assertUnknown(t, "Page", tt.page())
			})
		}
	}
}

func testCountBeforeAppendModelHook(t *testing.T, ctx context.Context, db *sqldb.DB, recorder *countRecorder) {
	calls := &atomic.Int64{}
	hookCtx := context.WithValue(ctx, countAppendCalls{}, calls)

	recorder.Reset()
	total, err := db.Select((*countAppendItem)(nil)).Count(hookCtx)
	if err != nil || total != 7 {
		t.Fatalf("Count() = %d, %v; want 7", total, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("Count ran BeforeAppendModel %d times, want 0", got)
	}

	page, err := db.Select().OrderExpr("ci.id").
		Page[countAppendItem](hookCtx, &pagination.PageRequest{Page: 1, PerPage: 3})
	if err != nil {
		t.Fatalf("Page() = %v", err)
	}
	if page.Total != 7 || len(page.Records) != 3 {
		t.Fatalf("Page() = total %d / %d records, want 7 / 3", page.Total, len(page.Records))
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("Page ran BeforeAppendModel %d times, want 0: it scans into an empty slice", got)
	}

	// Control: on a SELECT, Bun runs the hook for the values already in the
	// destination, so the counter above would have seen a call.
	dest := []countAppendItem{{}}
	if err := db.Select(&dest).OrderExpr("ci.id").Limit(1).Scan(hookCtx); err != nil {
		t.Fatalf("Scan(prefilled) = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Scan into a prefilled slice ran BeforeAppendModel %d times, want 1", got)
	}
}

func testPageWithCount(t *testing.T, ctx context.Context, db *sqldb.DB, recorder *countRecorder) {
	t.Run("pages a BeforeSelect model with the caller's count", func(t *testing.T) {
		recorder.Reset()
		count := db.Select((*countItem)(nil)).ApplyFilter(tenantScope("a")).Count
		page, err := db.Select().OrderExpr("ci.id").
			PageWithCount[countHookItem](ctx, &pagination.PageRequest{Page: 2, PerPage: 3}, count)
		if err != nil {
			t.Fatalf("PageWithCount() = %v", err)
		}
		if page.Total != 4 || page.TotalPages != 2 || page.Page != 2 || page.PerPage != 3 {
			t.Fatalf("PageWithCount metadata = %+v, want total 4, 2 pages, page 2 of 3", page)
		}
		if len(page.Records) != 1 || page.Records[0].ID != 7 {
			t.Fatalf("PageWithCount records = %+v, want the hook-filtered row 7", page.Records)
		}
		queries := recorder.Queries()
		if len(queries) != 2 {
			t.Fatalf("PageWithCount statements = %q, want count + data", queries)
		}
		data := strings.ToUpper(queries[1])
		if !strings.Contains(data, "LIMIT 3") || !strings.Contains(data, "OFFSET 3") {
			t.Fatalf("data SQL %q, want the page's window LIMIT 3 OFFSET 3", queries[1])
		}
	})

	t.Run("returns the given count as Total", func(t *testing.T) {
		page, err := db.Select().Apply(func(q *bun.SelectQuery) *bun.SelectQuery { return q }).
			PageWithCount[countItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 2},
			func(context.Context) (int, error) { return 42, nil })
		if err != nil {
			t.Fatalf("PageWithCount() = %v", err)
		}
		if page.Total != 42 || page.TotalPages != 21 || len(page.Records) != 2 {
			t.Fatalf("PageWithCount() = %+v, want total 42 and two records", page)
		}
	})

	t.Run("returns the count's error", func(t *testing.T) {
		countErr := errors.New("count failed")
		recorder.Reset()
		_, err := db.Select().PageWithCount[countItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 2},
			func(context.Context) (int, error) { return 0, countErr })
		if !errors.Is(err, countErr) {
			t.Fatalf("PageWithCount() = %v, want the count's error", err)
		}
		if queries := recorder.Queries(); len(queries) != 0 {
			t.Fatalf("PageWithCount ran %q after a failed count, want nothing", queries)
		}
	})

	t.Run("validates the request before the count", func(t *testing.T) {
		for _, req := range []*pagination.PageRequest{nil, {Page: 0, PerPage: 10}, {Page: 1, PerPage: 0}} {
			called := false
			recorder.Reset()
			_, err := db.Select().PageWithCount[countItem](ctx, req, func(context.Context) (int, error) {
				called = true
				return 1, nil
			})
			if !errors.Is(err, pagination.ErrInvalidPageRequest) {
				t.Fatalf("PageWithCount(%+v) = %v, want ErrInvalidPageRequest", req, err)
			}
			if called || len(recorder.Queries()) != 0 {
				t.Fatalf("PageWithCount(%+v) ran the count or a statement before validation", req)
			}
		}
	})

	t.Run("refuses a bound model and a nil count before the count", func(t *testing.T) {
		called := false
		_, err := db.Select((*countItem)(nil)).PageWithCount[countItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 2},
			func(context.Context) (int, error) {
				called = true
				return 1, nil
			})
		if !errors.Is(err, sqldb.ErrTypedTerminalModel) || !strings.Contains(err.Error(), "PageWithCount cannot override") || called {
			t.Fatalf("PageWithCount(bound model) = %v (count called %v), want ErrTypedTerminalModel first", err, called)
		}
		_, err = db.Select().PageWithCount[countItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 2}, nil)
		if err == nil || !strings.Contains(err.Error(), "PageWithCount: count must not be nil") ||
			!strings.Contains(err.Error(), "another query's Count") {
			t.Fatalf("PageWithCount(nil count) = %v, want the nil-count error with its remedy", err)
		}
	})
}

func testApplyFilterScopes(t *testing.T, ctx context.Context, db *sqldb.DB) {
	seedCountItems(t, ctx, db)
	scope := tenantScope("a")

	page, err := db.Select().ApplyFilter(scope).OrderExpr("ci.id").
		Page[countItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("Page() = %v", err)
	}
	if page.Total != 4 || len(page.Records) != 4 {
		t.Fatalf("scoped Page() = total %d / %d records, want 4 / 4", page.Total, len(page.Records))
	}
	for _, item := range page.Records {
		if item.Tenant != "a" {
			t.Fatalf("scoped Page() returned %+v outside the scope", item)
		}
	}

	res, err := db.Update((*countItem)(nil)).Set("name = ?", "scoped").ApplyFilter(scope).Exec(ctx)
	if err != nil {
		t.Fatalf("scoped Update = %v", err)
	}
	if n, _ := res.RowsAffected(); n != 4 {
		t.Fatalf("scoped Update affected %d rows, want 4", n)
	}
	renamed, err := db.Select((*countItem)(nil)).Where("name = ?", "scoped").Count(ctx)
	if err != nil || renamed != 4 {
		t.Fatalf("renamed rows = %d, %v; want 4", renamed, err)
	}

	res, err = db.Delete((*countItem)(nil)).ApplyFilter(scope).Exec(ctx)
	if err != nil {
		t.Fatalf("scoped Delete = %v", err)
	}
	if n, _ := res.RowsAffected(); n != 4 {
		t.Fatalf("scoped Delete affected %d rows, want 4", n)
	}
	remaining, err := db.Select((*countItem)(nil)).Count(ctx)
	if err != nil || remaining != 3 {
		t.Fatalf("remaining rows = %d, %v; want 3", remaining, err)
	}
	seedCountItems(t, ctx, db)
}

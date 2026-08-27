package sqldb_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"

	"github.com/credo-go/credo/pagination"
	"github.com/credo-go/credo/store/sqldb"
)

type paginationQueryRecorder struct {
	mu      sync.Mutex
	queries []string
	models  []bun.Model
}

func (*paginationQueryRecorder) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (r *paginationQueryRecorder) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if event.Operation() != "SELECT" {
		return
	}
	r.mu.Lock()
	r.queries = append(r.queries, event.Query)
	r.models = append(r.models, event.Model)
	r.mu.Unlock()
}

func (r *paginationQueryRecorder) Reset() {
	r.mu.Lock()
	r.queries = nil
	r.models = nil
	r.mu.Unlock()
}

func (r *paginationQueryRecorder) Queries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.queries)
}

func (r *paginationQueryRecorder) Models() []bun.Model {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.models)
}

func TestSelectQuery_PageLogicalCountConformance(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()
	for _, user := range []User{
		{Name: "a1", Email: "a@example.com"},
		{Name: "a2", Email: "a@example.com"},
		{Name: "b1", Email: "b@example.com"},
		{Name: "c1", Email: "c@example.com"},
		{Name: "c2", Email: "c@example.com"},
		{Name: "c3", Email: "c@example.com"},
	} {
		if _, err := db.Insert(&user).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", user.Name, err)
		}
	}

	recorder := &paginationQueryRecorder{}
	db.Client().AddQueryHook(recorder)
	tests := []struct {
		name      string
		build     func(*sqldb.SelectQuery) *sqldb.SelectQuery
		wantTotal int64
		wantEmail string
	}{
		{
			name: "distinct projection",
			build: func(query *sqldb.SelectQuery) *sqldb.SelectQuery {
				return query.Column("email").Distinct().OrderExpr("email ASC")
			},
			wantTotal: 3,
			wantEmail: "b@example.com",
		},
		{
			name: "group",
			build: func(query *sqldb.SelectQuery) *sqldb.SelectQuery {
				return query.Column("email").GroupExpr("email").OrderExpr("email ASC")
			},
			wantTotal: 3,
			wantEmail: "b@example.com",
		},
		{
			name: "group and having",
			build: func(query *sqldb.SelectQuery) *sqldb.SelectQuery {
				return query.
					Column("email").
					GroupExpr("email").
					Having("COUNT(*) >= ?", 2).
					OrderExpr("email ASC")
			},
			wantTotal: 2,
			wantEmail: "c@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder.Reset()
			req := &pagination.PageRequest{Page: 2, PerPage: 1}
			page, err := tt.build(db.Select()).Page[User](ctx, req)
			if err != nil {
				t.Fatalf("Page() = %v", err)
			}
			if page.Total != tt.wantTotal || page.TotalPages != tt.wantTotal {
				t.Fatalf("Page metadata = total %d / pages %d, want %d / %d",
					page.Total, page.TotalPages, tt.wantTotal, tt.wantTotal)
			}
			if len(page.Records) != 1 || page.Records[0].Email != tt.wantEmail {
				t.Fatalf("Page records = %+v, want email %q", page.Records, tt.wantEmail)
			}

			queries := recorder.Queries()
			if len(queries) != 2 {
				t.Fatalf("Page queries = %d, want COUNT + SELECT", len(queries))
			}
			if !strings.Contains(queries[0], "AS _credo_count_source") {
				t.Fatalf("Count SQL = %q, want Credo logical-count source", queries[0])
			}

			recorder.Reset()
			total, err := tt.build(db.Select((*User)(nil))).Count(ctx)
			if err != nil {
				t.Fatalf("Count() = %v", err)
			}
			if int64(total) != tt.wantTotal {
				t.Fatalf("Count() = %d, want %d", total, tt.wantTotal)
			}
			queries = recorder.Queries()
			if len(queries) != 1 || !strings.Contains(queries[0], "AS _credo_count_source") {
				t.Fatalf("Count queries = %q, want one Credo logical-count query", queries)
			}
		})
	}

	t.Run("group and having empty result", func(t *testing.T) {
		recorder.Reset()
		page, err := db.Select().
			Column("email").
			GroupExpr("email").
			Having("COUNT(*) > ?", 99).
			Page[User](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
		if err != nil {
			t.Fatalf("Page() = %v", err)
		}
		if page.Total != 0 || page.TotalPages != 0 || page.Records == nil || len(page.Records) != 0 {
			t.Fatalf("Page() = %+v, want zero metadata and non-nil empty records", page)
		}
		queries := recorder.Queries()
		if len(queries) != 1 || !strings.Contains(queries[0], "AS _credo_count_source") {
			t.Fatalf("empty Page queries = %q, want one wrapped COUNT", queries)
		}
	})

	t.Run("ungrouped aggregate projection", func(t *testing.T) {
		recorder.Reset()
		page, err := db.Select().
			ColumnExpr("COUNT(*) AS id").
			Page[User](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
		if err != nil {
			t.Fatalf("Page() = %v", err)
		}
		if page.Total != 1 || page.TotalPages != 1 || len(page.Records) != 1 {
			t.Fatalf("Page() = %+v, want one logical aggregate row", page)
		}
		if page.Records[0].ID != 6 {
			t.Fatalf("aggregate record ID = %d, want COUNT(*) 6", page.Records[0].ID)
		}
		queries := recorder.Queries()
		if len(queries) != 2 || !strings.Contains(queries[0], "AS _credo_count_source") {
			t.Fatalf("aggregate Page queries = %q, want wrapped COUNT + SELECT", queries)
		}

		recorder.Reset()
		total, err := db.Select((*User)(nil)).ColumnExpr("COUNT(*) AS id").Count(ctx)
		if err != nil {
			t.Fatalf("aggregate Count() = %v", err)
		}
		if total != 1 {
			t.Fatalf("aggregate Count() = %d, want one logical result row", total)
		}
		total, err = db.Select((*User)(nil)).ColumnExpr("COUNT(*)").Count(ctx)
		if err != nil {
			t.Fatalf("SQLite unaliased aggregate Count() = %v", err)
		}
		if total != 1 {
			t.Fatalf("SQLite unaliased aggregate Count() = %d, want 1", total)
		}

		emptyPage, err := db.Select().
			ColumnExpr("COUNT(*) AS id").
			Where("1 = 0").
			Page[User](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
		if err != nil {
			t.Fatalf("empty-input aggregate Page() = %v", err)
		}
		if emptyPage.Total != 1 || len(emptyPage.Records) != 1 || emptyPage.Records[0].ID != 0 {
			t.Fatalf("empty-input aggregate Page() = %+v, want one logical row with COUNT(*) 0", emptyPage)
		}
	})
}

func TestSelectQuery_LogicalCountStripsNonCardinalityClauses(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()
	for i := range 3 {
		user := &User{Name: fmt.Sprintf("user-%d", i), Email: fmt.Sprintf("%d@example.com", i)}
		if _, err := db.Insert(user).Exec(ctx); err != nil {
			t.Fatalf("insert user %d: %v", i, err)
		}
	}

	recorder := &paginationQueryRecorder{}
	db.Client().AddQueryHook(recorder)
	query := db.Select((*User)(nil)).
		OrderExpr("id DESC").
		Limit(1).
		Offset(1).
		Apply(func(raw *bun.SelectQuery) *bun.SelectQuery {
			return raw.For("UPDATE")
		})
	wantBuilderSQL := query.Unwrap().String()

	total, err := query.Count(ctx)
	if err != nil {
		t.Fatalf("Count() = %v", err)
	}
	if total != 3 {
		t.Fatalf("Count() = %d, want 3 rows before order/window/lock", total)
	}
	queries := recorder.Queries()
	if len(queries) != 1 {
		t.Fatalf("Count queries = %q, want one query", queries)
	}
	countSQL := strings.ToUpper(queries[0])
	for _, clause := range []string{"ORDER BY", "LIMIT", "OFFSET", "FOR UPDATE"} {
		if strings.Contains(countSQL, clause) {
			t.Fatalf("Count SQL = %q, unexpectedly contains %s", queries[0], clause)
		}
	}
	if got := query.Unwrap().String(); got != wantBuilderSQL {
		t.Fatalf("Count mutated reusable builder:\n got: %s\nwant: %s", got, wantBuilderSQL)
	}
}

type logicalCountHookContextKey struct{}

type logicalCountHookState struct {
	before      int
	after       int
	unsupported bool
	afterLens   []int
}

type logicalCountHookUser struct {
	bun.BaseModel `bun:"table:users,alias:lchu"`
	ID            int    `bun:"id,pk,autoincrement"`
	Name          string `bun:"name"`
	Email         string `bun:"email"`
}

func (*logicalCountHookUser) BeforeSelect(ctx context.Context, query *bun.SelectQuery) error {
	if state, ok := ctx.Value(logicalCountHookContextKey{}).(*logicalCountHookState); ok {
		state.before++
		if state.unsupported {
			query.Having("COUNT(*) > 0")
			return nil
		}
	}
	query.Where("?TableAlias.email LIKE ?", "keep-%")
	return nil
}

func (*logicalCountHookUser) AfterSelect(ctx context.Context, query *bun.SelectQuery) error {
	if state, ok := ctx.Value(logicalCountHookContextKey{}).(*logicalCountHookState); ok {
		state.after++
		if model := query.GetModel(); model != nil {
			if records, ok := model.Value().(*[]logicalCountHookUser); ok {
				state.afterLens = append(state.afterLens, len(*records))
			}
		}
	}
	return nil
}

func TestSelectQuery_LogicalCountRunsModelHooks(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	for _, user := range []*User{
		{Name: "keep-one", Email: "keep-one@example.com"},
		{Name: "drop", Email: "drop@example.com"},
		{Name: "keep-two", Email: "keep-two@example.com"},
	} {
		if _, err := db.Insert(user).Exec(t.Context()); err != nil {
			t.Fatalf("insert %q: %v", user.Name, err)
		}
	}

	recorder := &paginationQueryRecorder{}
	db.Client().AddQueryHook(recorder)
	pageHooks := &logicalCountHookState{}
	pageCtx := context.WithValue(t.Context(), logicalCountHookContextKey{}, pageHooks)
	page, err := db.Select().
		OrderExpr("id ASC").
		Page[logicalCountHookUser](pageCtx, &pagination.PageRequest{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatalf("Page() = %v", err)
	}
	if page.Total != 2 || len(page.Records) != 2 {
		t.Fatalf("Page() = %+v, want two hook-filtered rows", page)
	}
	if pageHooks.before != 2 || pageHooks.after != 2 {
		t.Fatalf("Page hooks = before %d / after %d, want 2 / 2", pageHooks.before, pageHooks.after)
	}
	if !slices.Equal(pageHooks.afterLens, []int{0, 2}) {
		t.Fatalf("Page AfterSelect model lengths = %v, want [0 2]", pageHooks.afterLens)
	}
	for i, model := range recorder.Models() {
		if model == nil {
			t.Fatalf("Page query %d QueryEvent.Model is nil", i)
		}
	}

	recorder.Reset()
	countHooks := &logicalCountHookState{}
	countCtx := context.WithValue(t.Context(), logicalCountHookContextKey{}, countHooks)
	total, err := db.Select((*logicalCountHookUser)(nil)).Count(countCtx)
	if err != nil {
		t.Fatalf("Count() = %v", err)
	}
	if total != 2 {
		t.Fatalf("Count() = %d, want two hook-filtered rows", total)
	}
	if countHooks.before != 1 || countHooks.after != 1 {
		t.Fatalf("Count hooks = before %d / after %d, want 1 / 1", countHooks.before, countHooks.after)
	}
	if len(countHooks.afterLens) != 0 {
		t.Fatalf("scalar Count AfterSelect model lengths = %v, want none", countHooks.afterLens)
	}
	models := recorder.Models()
	if len(models) != 1 || models[0] == nil {
		t.Fatalf("Count QueryEvent models = %#v, want one non-nil model", models)
	}

	recorder.Reset()
	boundHooks := &logicalCountHookState{}
	boundCtx := context.WithValue(t.Context(), logicalCountHookContextKey{}, boundHooks)
	bound := []logicalCountHookUser{{Name: "pre-existing"}}
	total, err = db.Select(&bound).Count(boundCtx)
	if err != nil {
		t.Fatalf("bound Count() = %v", err)
	}
	if total != 2 || len(bound) != 1 || bound[0].Name != "pre-existing" {
		t.Fatalf("bound Count() = total %d / model %+v, want total 2 and unchanged model", total, bound)
	}
	if !slices.Equal(boundHooks.afterLens, []int{1}) {
		t.Fatalf("bound Count AfterSelect model lengths = %v, want unchanged [1]", boundHooks.afterLens)
	}

	recorder.Reset()
	unsupportedHooks := &logicalCountHookState{unsupported: true}
	unsupportedCtx := context.WithValue(t.Context(), logicalCountHookContextKey{}, unsupportedHooks)
	_, err = db.Select().Page[logicalCountHookUser](
		unsupportedCtx,
		&pagination.PageRequest{Page: 1, PerPage: 10},
	)
	if !errors.Is(err, sqldb.ErrUnsupportedCountQuery) {
		t.Fatalf("hook-mutated Page error = %v, want ErrUnsupportedCountQuery", err)
	}
	if queries := recorder.Queries(); len(queries) != 0 {
		t.Fatalf("hook-mutated Page queries = %q, want no database operation", queries)
	}
	if unsupportedHooks.before != 1 || unsupportedHooks.after != 0 {
		t.Fatalf(
			"rejected Page hooks = before %d / after %d, want 1 / 0",
			unsupportedHooks.before,
			unsupportedHooks.after,
		)
	}
}

func TestSelectQuery_LogicalCountPreservesNonTableQueryEventModel(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	if _, err := db.Insert(&User{Name: "one", Email: "one@example.com"}).Exec(t.Context()); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	recorder := &paginationQueryRecorder{}
	db.Client().AddQueryHook(recorder)
	model := map[string]any{}
	total, err := db.Select(&model).
		TableExpr("users").
		Column("id").
		Count(t.Context())
	if err != nil {
		t.Fatalf("Count() = %v", err)
	}
	if total != 1 {
		t.Fatalf("Count() = %d, want 1", total)
	}
	models := recorder.Models()
	if len(models) != 1 || models[0] == nil {
		t.Fatalf("Count QueryEvent models = %#v, want one non-nil map model", models)
	}
}

type logicalCountModelReplacementKey struct{}

type logicalCountModelReplacementState struct {
	replacement   *logicalCountModelReplacementB
	aBefore       int
	aAfter        int
	bBeforeAppend int
	bAfter        int
}

type logicalCountModelReplacementA struct {
	bun.BaseModel `bun:"table:logical_count_replacement_a,alias:lcra"`
	ID            int `bun:"id,pk"`
}

func (*logicalCountModelReplacementA) BeforeSelect(
	ctx context.Context,
	query *bun.SelectQuery,
) error {
	state, _ := ctx.Value(logicalCountModelReplacementKey{}).(*logicalCountModelReplacementState)
	state.aBefore++
	query.Model(state.replacement)
	return nil
}

func (*logicalCountModelReplacementA) AfterSelect(ctx context.Context, _ *bun.SelectQuery) error {
	state, _ := ctx.Value(logicalCountModelReplacementKey{}).(*logicalCountModelReplacementState)
	state.aAfter++
	return nil
}

type logicalCountModelReplacementB struct {
	bun.BaseModel `bun:"table:logical_count_replacement_b,alias:lcrb"`
	ID            int       `bun:"id,pk"`
	DeletedAt     time.Time `bun:"deleted_at,soft_delete,nullzero"`
}

func (*logicalCountModelReplacementB) BeforeAppendModel(
	ctx context.Context,
	_ schema.Query,
) error {
	state, _ := ctx.Value(logicalCountModelReplacementKey{}).(*logicalCountModelReplacementState)
	state.bBeforeAppend++
	return nil
}

func (*logicalCountModelReplacementB) AfterSelect(ctx context.Context, _ *bun.SelectQuery) error {
	state, _ := ctx.Value(logicalCountModelReplacementKey{}).(*logicalCountModelReplacementState)
	state.bAfter++
	return nil
}

func TestSelectQuery_LogicalCountUsesPostHookModelState(t *testing.T) {
	db := openTestDB(t)
	_, err := db.Client().NewRaw(`
		CREATE TABLE logical_count_replacement_b (
			id INTEGER PRIMARY KEY,
			deleted_at TIMESTAMP NULL
		);
		INSERT INTO logical_count_replacement_b (id, deleted_at)
		VALUES (1, NULL), (2, CURRENT_TIMESTAMP)
	`).Exec(t.Context())
	if err != nil {
		t.Fatalf("create replacement fixture: %v", err)
	}

	state := &logicalCountModelReplacementState{
		replacement: new(logicalCountModelReplacementB),
	}
	ctx := context.WithValue(t.Context(), logicalCountModelReplacementKey{}, state)
	total, err := db.Select(new(logicalCountModelReplacementA)).Count(ctx)
	if err != nil {
		t.Fatalf("Count() = %v", err)
	}
	if total != 1 {
		t.Fatalf("Count() = %d, want one active replacement-model row", total)
	}
	if state.aBefore != 1 || state.aAfter != 0 ||
		state.bBeforeAppend != 1 || state.bAfter != 1 {
		t.Fatalf(
			"replacement hooks = A before/after %d/%d, B beforeAppend/after %d/%d; "+
				"want 1/0 and 1/1",
			state.aBefore,
			state.aAfter,
			state.bBeforeAppend,
			state.bAfter,
		)
	}
}

func TestSelectQuery_CountOuterDerivedSource(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	for _, user := range []User{
		{Name: "a1", Email: "a@example.com"},
		{Name: "a2", Email: "a@example.com"},
		{Name: "b1", Email: "b@example.com"},
	} {
		if _, err := db.Insert(&user).Exec(t.Context()); err != nil {
			t.Fatalf("insert %q: %v", user.Name, err)
		}
	}

	source := db.Client().NewSelect().
		Model((*User)(nil)).
		Column("email").
		Distinct()
	total, err := db.Select().
		TableExpr("(?) AS page_source", source).
		Count(t.Context())
	if err != nil {
		t.Fatalf("Count(derived source) = %v", err)
	}
	if total != 2 {
		t.Fatalf("Count(derived source) = %d, want 2", total)
	}
}

func TestSelectQuery_UnsupportedCountShapesFailBeforeQuery(t *testing.T) {
	db := openTestDB(t)
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)
	req := &pagination.PageRequest{Page: 1, PerPage: 10}

	assertRejected := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, sqldb.ErrUnsupportedCountQuery) {
			t.Fatalf("error = %v, want ErrUnsupportedCountQuery", err)
		}
		if got := hook.Total(); got != 0 {
			t.Fatalf("query executed %d DB operations, want 0", got)
		}
	}

	t.Run("Count rejects standalone Having", func(t *testing.T) {
		hook.Reset()
		_, err := db.Select((*User)(nil)).Having("COUNT(*) > 0").Count(t.Context())
		assertRejected(t, err)
	})

	t.Run("Page rejects standalone Having", func(t *testing.T) {
		hook.Reset()
		_, err := db.Select().Having("COUNT(*) > 0").Page[User](t.Context(), req)
		assertRejected(t, err)
	})

	compoundOperators := []struct {
		name  string
		apply func(*bun.SelectQuery, *bun.SelectQuery) *bun.SelectQuery
	}{
		{name: "UNION", apply: func(query, other *bun.SelectQuery) *bun.SelectQuery {
			return query.Union(other)
		}},
		{name: "UNION ALL", apply: func(query, other *bun.SelectQuery) *bun.SelectQuery {
			return query.UnionAll(other)
		}},
		{name: "INTERSECT", apply: func(query, other *bun.SelectQuery) *bun.SelectQuery {
			return query.Intersect(other)
		}},
		{name: "EXCEPT", apply: func(query, other *bun.SelectQuery) *bun.SelectQuery {
			return query.Except(other)
		}},
	}
	for _, operator := range compoundOperators {
		t.Run("Count rejects direct "+operator.name, func(t *testing.T) {
			hook.Reset()
			other := db.Client().NewSelect().Model((*User)(nil)).Column("id")
			_, err := db.Select((*User)(nil)).
				Column("id").
				Apply(func(query *bun.SelectQuery) *bun.SelectQuery {
					return operator.apply(query, other)
				}).
				Count(t.Context())
			assertRejected(t, err)
		})

		t.Run("Page rejects direct "+operator.name, func(t *testing.T) {
			hook.Reset()
			other := db.Client().NewSelect().Model((*User)(nil)).Column("id")
			_, err := db.Select().
				Column("id").
				Apply(func(query *bun.SelectQuery) *bun.SelectQuery {
					return operator.apply(query, other)
				}).
				Page[User](t.Context(), req)
			assertRejected(t, err)
		})
	}
}

type pauseAfterPageCount struct {
	ready   chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (*pauseAfterPageCount) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *pauseAfterPageCount) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	query := strings.ToUpper(event.Query)
	if event.Err != nil || !strings.Contains(query, "COUNT(*)") || !strings.Contains(query, "USERS") {
		return
	}
	h.once.Do(func() {
		close(h.ready)
		<-h.release
	})
}

type snapshotPageResult struct {
	page *pagination.Page[User]
	err  error
}

func openSnapshotSQLitePair(t *testing.T) (*sqldb.DB, *sqldb.DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "credo-page-snapshot-*")
	if err != nil {
		t.Fatalf("create snapshot temp dir: %v", err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove snapshot temp dir %q: %v", dir, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	dsn := filepath.Join(dir, "page-snapshot.db")
	var reader *sqldb.DB
	var writer *sqldb.DB
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if writer != nil {
				_, _ = writer.Exec(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
				_ = writer.Shutdown(ctx)
			}
			if reader != nil {
				_ = reader.Shutdown(ctx)
			}
		})
	}
	t.Cleanup(cleanup)
	open := func() *sqldb.DB {
		db, err := sqldb.Open(&sqldb.Config{
			Driver:  "sqlite",
			DSN:     dsn,
			MaxOpen: 1,
			MaxIdle: new(1),
		})
		if err != nil {
			t.Fatalf("Open(%q) = %v", dsn, err)
		}
		return db
	}

	reader = open()
	if _, err := reader.Exec(t.Context(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	createUsersTable(t, reader)
	for _, user := range []User{
		{Name: "before-a", Email: "a@example.com"},
		{Name: "before-b", Email: "b@example.com"},
	} {
		if _, err := reader.Insert(&user).Exec(t.Context()); err != nil {
			t.Fatalf("seed %q: %v", user.Name, err)
		}
	}

	writer = open()
	if _, err := writer.Exec(t.Context(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("confirm WAL: %v", err)
	}
	return reader, writer, cleanup
}

func runPageAcrossConcurrentInsert(t *testing.T, inTx bool) *pagination.Page[User] {
	t.Helper()
	reader, writer, cleanup := openSnapshotSQLitePair(t)
	defer cleanup()
	ready := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	reader.Client().AddQueryHook(&pauseAfterPageCount{ready: ready, release: release})

	result := make(chan snapshotPageResult, 1)
	runPage := func(ctx context.Context) (*pagination.Page[User], error) {
		return reader.Select().OrderExpr("name ASC").
			Page[User](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
	}
	go func() {
		if !inTx {
			page, err := runPage(t.Context())
			result <- snapshotPageResult{page: page, err: err}
			return
		}
		var page *pagination.Page[User]
		err := reader.InTx(t.Context(), func(txCtx context.Context) error {
			var err error
			page, err = runPage(txCtx)
			return err
		})
		result <- snapshotPageResult{page: page, err: err}
	}()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("Page COUNT did not reach the synchronization hook")
	}
	if _, err := writer.Insert(&User{Name: "after-count", Email: "late@example.com"}).Exec(t.Context()); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}
	releaseOnce()

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Page() = %v", got.err)
		}
		return got.page
	case <-time.After(5 * time.Second):
		t.Fatal("Page did not finish after releasing COUNT hook")
		return nil
	}
}

func TestSelectQuery_PageSQLiteSnapshotBoundary(t *testing.T) {
	t.Run("without transaction COUNT and SELECT can drift", func(t *testing.T) {
		page := runPageAcrossConcurrentInsert(t, false)
		if page.Total != 2 || len(page.Records) != 3 {
			t.Fatalf("Page = total %d / %d records, want observable drift 2 / 3",
				page.Total, len(page.Records))
		}
	})

	t.Run("explicit transaction keeps the first read snapshot", func(t *testing.T) {
		page := runPageAcrossConcurrentInsert(t, true)
		if page.Total != 2 || len(page.Records) != 2 {
			t.Fatalf("Page = total %d / %d records, want stable SQLite snapshot 2 / 2",
				page.Total, len(page.Records))
		}
	})
}

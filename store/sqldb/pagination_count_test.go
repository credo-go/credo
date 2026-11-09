package sqldb_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

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

func TestSelectQuery_CountPreservesNonTableQueryEventModel(t *testing.T) {
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

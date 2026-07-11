package sqldb_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"

	"github.com/credo-go/credo/pagination"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/store/sqldb"
)

type selectQueryCounter struct {
	count atomic.Int64
	total atomic.Int64
}

func (h *selectQueryCounter) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	h.total.Add(1)
	if event.Operation() == "SELECT" {
		h.count.Add(1)
	}
	return ctx
}

func (*selectQueryCounter) AfterQuery(context.Context, *bun.QueryEvent) {}

func (h *selectQueryCounter) Reset() {
	h.count.Store(0)
	h.total.Store(0)
}

func (h *selectQueryCounter) Count() int64 {
	return h.count.Load()
}

func (h *selectQueryCounter) Total() int64 {
	return h.total.Load()
}

func TestDB_QueryBuildersRejectMultipleModels(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()
	first := &User{Name: "first", Email: "first@example.com"}
	second := &User{Name: "second", Email: "second@example.com"}
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "Select",
			run: func() error {
				return db.Select(first, second).Scan(ctx)
			},
		},
		{
			name: "Insert",
			run: func() error {
				_, err := db.Insert(first, second).Exec(ctx)
				return err
			},
		},
		{
			name: "Update",
			run: func() error {
				_, err := db.Update(first, second).Where("id = ?", 1).Exec(ctx)
				return err
			},
		},
		{
			name: "Delete",
			run: func() error {
				_, err := db.Delete(first, second).Where("id = ?", 1).Exec(ctx)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook.Reset()
			err := tt.run()
			if err == nil {
				t.Fatal("multiple models error = nil")
			}
			if !strings.Contains(err.Error(), "accepts at most one model, got 2") {
				t.Fatalf("multiple models error = %q", err)
			}
			if count := hook.Total(); count != 0 {
				t.Fatalf("multiple models executed %d queries, want 0", count)
			}
		})
	}
}

func TestSelectQuery_WherePKStatePreserved(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()

	first := &User{Name: "where-pk-first", Email: "first@example.com"}
	second := &User{Name: "where-pk-second", Email: "second@example.com"}
	for _, user := range []*User{first, second} {
		if _, err := db.Insert(user).Exec(ctx); err != nil {
			t.Fatalf("seed %q: %v", user.Name, err)
		}
	}

	t.Run("Scan", func(t *testing.T) {
		// A missing PK in a non-empty table makes an accidentally unfiltered
		// query observable without relying on database row order.
		probe := User{ID: second.ID + 1_000_000}
		err := db.Select(&probe).WherePK().Scan(ctx)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("WherePK Scan error = %v, want store.ErrNotFound", err)
		}
	})

	t.Run("Count", func(t *testing.T) {
		probe := User{ID: second.ID}
		count, err := db.Select(&probe).WherePK().Count(ctx)
		if err != nil {
			t.Fatalf("WherePK Count = %v", err)
		}
		if count != 1 {
			t.Fatalf("WherePK Count = %d, want 1", count)
		}
	})

	t.Run("Exists", func(t *testing.T) {
		probe := User{ID: second.ID + 1_000_000}
		exists, err := db.Select(&probe).WherePK().Exists(ctx)
		if err != nil {
			t.Fatalf("WherePK Exists = %v", err)
		}
		if exists {
			t.Fatal("WherePK Exists = true for a missing primary key")
		}
	})
}

func TestSelectQuery_ClonePreservesStateAndBuilderIsolation(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()

	first := &User{Name: "clone-first", Email: "first@example.com"}
	second := &User{Name: "clone-second", Email: "second@example.com"}
	for _, user := range []*User{first, second} {
		if _, err := db.Insert(user).Exec(ctx); err != nil {
			t.Fatalf("seed %q: %v", user.Name, err)
		}
	}

	probe := User{ID: second.ID}
	count, err := db.Select(&probe).WherePK().Clone().Count(ctx)
	if err != nil {
		t.Fatalf("cloned WherePK Count = %v", err)
	}
	if count != 1 {
		t.Fatalf("cloned WherePK Count = %d, want 1", count)
	}

	base := db.Select((*User)(nil)).Where("id > ?", 0)
	filtered := base.Clone().Where("id = ?", second.ID)
	filteredCount, err := filtered.Count(ctx)
	if err != nil {
		t.Fatalf("filtered clone Count = %v", err)
	}
	if filteredCount != 1 {
		t.Fatalf("filtered clone Count = %d, want 1", filteredCount)
	}

	baseCount, err := base.Count(ctx)
	if err != nil {
		t.Fatalf("base Count after clone mutation = %v", err)
	}
	if baseCount != 2 {
		t.Fatalf("base Count after clone mutation = %d, want 2", baseCount)
	}
}

func TestSelectQuery_CommentPreservedAcrossCloneAndTypedTerminal(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()

	if _, err := db.Insert(&User{Name: "commented", Email: "commented@example.com"}).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	recorder := &paginationQueryRecorder{}
	db.Client().AddQueryHook(recorder)

	const comment = "credo-clone-comment"
	got, err := db.Select().
		Apply(func(query *bun.SelectQuery) *bun.SelectQuery {
			return query.Comment(comment)
		}).
		Clone().
		One[User](ctx)
	if err != nil {
		t.Fatalf("commented cloned One = %v", err)
	}
	if got.Name != "commented" {
		t.Fatalf("commented cloned One Name = %q, want commented", got.Name)
	}

	queries := recorder.Queries()
	if len(queries) != 1 {
		t.Fatalf("commented cloned One queries = %d, want 1", len(queries))
	}
	if want := "/* " + comment + " */"; !strings.HasPrefix(strings.TrimSpace(queries[0]), want) {
		t.Fatalf("commented cloned One query = %q, want prefix %q", queries[0], want)
	}
}

func TestSelectQuery_BuilderErrorsPreservedWithoutQuery(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()

	if _, err := db.Insert(&User{Name: "builder-error", Email: "builder@example.com"}).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	t.Run("invalid Relation", func(t *testing.T) {
		hook.Reset()
		var got User
		err := db.Select(&got).Relation("DoesNotExist").Scan(ctx)
		if err == nil {
			t.Fatal("invalid Relation Scan error = nil")
		}
		if !strings.Contains(err.Error(), "DoesNotExist") {
			t.Fatalf("invalid Relation Scan error = %q, want relation name", err)
		}
		if count := hook.Count(); count != 0 {
			t.Fatalf("invalid Relation executed %d SELECT queries, want 0", count)
		}
	})

	t.Run("JoinOn without Join", func(t *testing.T) {
		hook.Reset()
		var got User
		err := db.Select(&got).JoinOn("users.id = users.id").Scan(ctx)
		if err == nil {
			t.Fatal("JoinOn without Join error = nil")
		}
		if count := hook.Count(); count != 0 {
			t.Fatalf("invalid JoinOn executed %d SELECT queries, want 0", count)
		}
	})

	t.Run("Apply Err", func(t *testing.T) {
		hook.Reset()
		sentinel := errors.New("select builder sentinel")
		var got User
		err := db.Select(&got).
			Apply(func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Err(sentinel)
			}).
			Scan(ctx)
		if err != sentinel {
			t.Fatalf("Apply Err Scan error = %v, want exact sentinel", err)
		}
		if count := hook.Count(); count != 0 {
			t.Fatalf("Apply Err executed %d SELECT queries, want 0", count)
		}
	})

	t.Run("public Clone preserves Apply Err", func(t *testing.T) {
		hook.Reset()
		sentinel := errors.New("cloned select builder sentinel")
		var got User
		err := db.Select(&got).
			Apply(func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Err(sentinel)
			}).
			Clone().
			Scan(ctx)
		if err != sentinel {
			t.Fatalf("cloned Apply Err Scan error = %v, want exact sentinel", err)
		}
		if count := hook.Count(); count != 0 {
			t.Fatalf("cloned Apply Err executed %d SELECT queries, want 0", count)
		}
	})

	t.Run("model-less Relation", func(t *testing.T) {
		hook.Reset()
		_, err := db.Select().
			Relation("Orders").
			All[selectStateRelationUser](ctx)
		if err == nil {
			t.Fatal("model-less Relation All error = nil")
		}
		if errors.Is(err, sqldb.ErrTypedTerminalModel) {
			t.Fatalf("model-less Relation error = %v, want the preserved Bun builder error", err)
		}
		if count := hook.Count(); count != 0 {
			t.Fatalf("model-less Relation executed %d SELECT queries, want 0", count)
		}
	})

	t.Run("Model nil builder error", func(t *testing.T) {
		hook.Reset()
		_, err := db.Select().Model(nil).All[User](ctx)
		if err == nil {
			t.Fatal("Model(nil) All error = nil")
		}
		if errors.Is(err, sqldb.ErrTypedTerminalModel) {
			t.Fatalf("Model(nil) error = %v, want the preserved Bun builder error", err)
		}
		if count := hook.Count(); count != 0 {
			t.Fatalf("Model(nil) executed %d SELECT queries, want 0", count)
		}
	})
}

type selectStateSoftUser struct {
	bun.BaseModel `bun:"table:select_state_soft_users,alias:ssu"`
	ID            int       `bun:"id,pk,autoincrement"`
	Name          string    `bun:"name"`
	DeletedAt     time.Time `bun:"deleted_at,soft_delete,nullzero"`
}

func createSelectStateSoftUsersTable(t *testing.T, db *sqldb.DB) {
	t.Helper()
	_, err := db.Client().NewRaw(`
		CREATE TABLE select_state_soft_users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			deleted_at TIMESTAMP NULL
		)
	`).Exec(t.Context())
	if err != nil {
		t.Fatalf("create soft-delete table: %v", err)
	}
}

func TestSelectQuery_SoftDeleteFlagsPreserved(t *testing.T) {
	db := openTestDB(t)
	createSelectStateSoftUsersTable(t, db)
	ctx := t.Context()

	active := &selectStateSoftUser{Name: "active"}
	deleted := &selectStateSoftUser{Name: "deleted", DeletedAt: time.Now().UTC().Truncate(time.Second)}
	for _, user := range []*selectStateSoftUser{active, deleted} {
		if _, err := db.Insert(user).Exec(ctx); err != nil {
			t.Fatalf("seed %q: %v", user.Name, err)
		}
	}

	t.Run("WhereDeleted", func(t *testing.T) {
		count, err := db.Select((*selectStateSoftUser)(nil)).
			Where("name = ?", deleted.Name).
			ApplyQueryBuilder(func(q bun.QueryBuilder) bun.QueryBuilder {
				return q.WhereDeleted()
			}).
			Count(ctx)
		if err != nil {
			t.Fatalf("WhereDeleted Count = %v", err)
		}
		if count != 1 {
			t.Fatalf("WhereDeleted Count = %d, want 1", count)
		}
	})

	t.Run("WhereAllWithDeleted", func(t *testing.T) {
		count, err := db.Select((*selectStateSoftUser)(nil)).
			ApplyQueryBuilder(func(q bun.QueryBuilder) bun.QueryBuilder {
				return q.WhereAllWithDeleted()
			}).
			Count(ctx)
		if err != nil {
			t.Fatalf("WhereAllWithDeleted Count = %v", err)
		}
		if count != 2 {
			t.Fatalf("WhereAllWithDeleted Count = %d, want 2", count)
		}
	})
}

func openSelectStateFileDB(t *testing.T, path string) *sqldb.DB {
	t.Helper()
	db, err := sqldb.Open(&sqldb.Config{
		Driver:  "sqlite",
		DSN:     path,
		MaxOpen: 1,
	})
	if err != nil {
		t.Fatalf("open SQLite DB %q: %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown SQLite DB %q: %v", path, err)
		}
	})
	return db
}

func TestSelectQuery_ExplicitConnPreserved(t *testing.T) {
	dir := t.TempDir()
	defaultDB := openSelectStateFileDB(t, filepath.Join(dir, "default.db"))
	explicitDB := openSelectStateFileDB(t, filepath.Join(dir, "explicit.db"))
	createUsersTable(t, defaultDB)
	createUsersTable(t, explicitDB)
	ctx := t.Context()

	defaultUser := &User{Name: "from-default", Email: "default@example.com"}
	explicitUser := &User{Name: "from-explicit", Email: "explicit@example.com"}
	if _, err := defaultDB.Insert(defaultUser).Exec(ctx); err != nil {
		t.Fatalf("seed default DB: %v", err)
	}
	if _, err := explicitDB.Insert(explicitUser).Exec(ctx); err != nil {
		t.Fatalf("seed explicit DB: %v", err)
	}
	if defaultUser.ID != explicitUser.ID {
		t.Fatalf("fixture IDs differ: default=%d explicit=%d", defaultUser.ID, explicitUser.ID)
	}

	for _, tt := range []struct {
		name      string
		clone     bool
		applyConn bool
	}{
		{name: "terminal snapshot"},
		{name: "public Clone", clone: true},
		{name: "Apply Conn", applyConn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got User
			query := defaultDB.Select(&got).Where("id = ?", explicitUser.ID)
			if tt.applyConn {
				query = query.Apply(func(raw *bun.SelectQuery) *bun.SelectQuery {
					return raw.Conn(explicitDB.Client())
				})
			} else {
				query = query.Conn(explicitDB.Client())
			}
			if tt.clone {
				query = query.Clone()
			}
			if err := query.Scan(ctx); err != nil {
				t.Fatalf("explicit Conn Scan = %v", err)
			}
			if got.Name != explicitUser.Name {
				t.Fatalf("explicit Conn returned %q, want %q", got.Name, explicitUser.Name)
			}
		})
	}

	t.Run("Page count and select", func(t *testing.T) {
		page, err := defaultDB.Select().
			Conn(explicitDB.Client()).
			OrderExpr("id ASC").
			Page[User](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
		if err != nil {
			t.Fatalf("explicit Conn Page = %v", err)
		}
		if page.Total != 1 || len(page.Records) != 1 || page.Records[0].Name != explicitUser.Name {
			t.Fatalf("explicit Conn Page = %+v, want only %q", page, explicitUser.Name)
		}
	})
}

func TestSelectQuery_ReceiverReusableAfterTxRollback(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := t.Context()

	query := db.Select((*User)(nil)).Where("name = ?", "tx-only")
	sentinel := errors.New("rollback select receiver reuse")
	err := db.InTx(ctx, func(txCtx context.Context) error {
		if _, err := db.Insert(&User{Name: "tx-only", Email: "tx@example.com"}).Exec(txCtx); err != nil {
			return err
		}
		count, err := query.Count(txCtx)
		if err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("Count inside TX = %d, want 1", count)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx error = %v, want rollback sentinel", err)
	}

	count, err := query.Count(ctx)
	if err != nil {
		t.Fatalf("Count after rollback = %v (finished TX leaked into receiver?)", err)
	}
	if count != 0 {
		t.Fatalf("Count after rollback = %d, want 0", count)
	}
}

type selectStateRelationUser struct {
	bun.BaseModel `bun:"table:select_state_relation_users,alias:sru"`
	ID            int                         `bun:"id,pk,autoincrement"`
	Name          string                      `bun:"name"`
	Profile       *selectStateRelationProfile `bun:"rel:has-one,join:id=user_id"`
	Orders        []selectStateRelationOrder  `bun:"rel:has-many,join:id=user_id"`
}

type selectStateRelationProfile struct {
	bun.BaseModel `bun:"table:select_state_relation_profiles,alias:srp"`
	ID            int    `bun:"id,pk,autoincrement"`
	UserID        int    `bun:"user_id"`
	Label         string `bun:"label"`
}

type selectStateRelationOrder struct {
	bun.BaseModel `bun:"table:select_state_relation_orders,alias:sro"`
	ID            int                       `bun:"id,pk,autoincrement"`
	UserID        int                       `bun:"user_id"`
	Total         int                       `bun:"total"`
	Items         []selectStateRelationItem `bun:"rel:has-many,join:id=order_id"`
}

type selectStateRelationItem struct {
	bun.BaseModel `bun:"table:select_state_relation_items,alias:sri"`
	ID            int    `bun:"id,pk,autoincrement"`
	OrderID       int    `bun:"order_id"`
	SKU           string `bun:"sku"`
}

func createSelectStateRelationTables(t *testing.T, db *sqldb.DB) {
	t.Helper()
	for _, statement := range []string{
		`CREATE TABLE select_state_relation_users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL
		)`,
		`CREATE TABLE select_state_relation_profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			label TEXT NOT NULL
		)`,
		`CREATE TABLE select_state_relation_orders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			total INTEGER NOT NULL
		)`,
		`CREATE TABLE select_state_relation_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id INTEGER NOT NULL,
			sku TEXT NOT NULL
		)`,
	} {
		if _, err := db.Client().NewRaw(statement).Exec(t.Context()); err != nil {
			t.Fatalf("create relation table: %v", err)
		}
	}
}

func TestSelectQuery_RelationCallbacksAndReceiverReuse(t *testing.T) {
	db := openTestDB(t)
	createSelectStateRelationTables(t, db)
	ctx := t.Context()

	user := &selectStateRelationUser{Name: "relation-user"}
	if _, err := db.Insert(user).Exec(ctx); err != nil {
		t.Fatalf("seed relation user: %v", err)
	}
	profile := &selectStateRelationProfile{UserID: user.ID, Label: "primary"}
	if _, err := db.Insert(profile).Exec(ctx); err != nil {
		t.Fatalf("seed relation profile: %v", err)
	}
	for _, total := range []int{50, 150, 250} {
		order := &selectStateRelationOrder{UserID: user.ID, Total: total}
		if _, err := db.Insert(order).Exec(ctx); err != nil {
			t.Fatalf("seed relation order %d: %v", total, err)
		}
	}

	var got selectStateRelationUser
	query := db.Select(&got).
		Relation("Profile", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Column("id", "user_id", "label")
		}).
		Relation("Orders", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("total >= ?", 100).OrderExpr("total ASC")
		}).
		Where("?TableAlias.id = ?", user.ID)

	assertResult := func(t *testing.T) {
		t.Helper()
		if got.Profile == nil || got.Profile.Label != profile.Label {
			t.Fatalf("Profile = %+v, want label %q", got.Profile, profile.Label)
		}
		if len(got.Orders) != 2 {
			t.Fatalf("Orders = %+v, want two filtered orders", got.Orders)
		}
		if got.Orders[0].Total != 150 || got.Orders[1].Total != 250 {
			t.Fatalf("order totals = [%d %d], want [150 250]", got.Orders[0].Total, got.Orders[1].Total)
		}
	}

	if err := query.Scan(ctx); err != nil {
		t.Fatalf("first relation Scan = %v", err)
	}
	assertResult(t)

	// Reset the destination, but reuse the exact query receiver. Relation
	// callback state and table-model scan state must not leak or disappear.
	got = selectStateRelationUser{}
	if err := query.Scan(ctx); err != nil {
		t.Fatalf("second relation Scan = %v", err)
	}
	assertResult(t)
}

func TestSelectQuery_ClonePreservesNestedRelation(t *testing.T) {
	db := openTestDB(t)
	createSelectStateRelationTables(t, db)
	ctx := t.Context()

	user := &selectStateRelationUser{Name: "nested-relation-user"}
	if _, err := db.Insert(user).Exec(ctx); err != nil {
		t.Fatalf("seed relation user: %v", err)
	}
	order := &selectStateRelationOrder{UserID: user.ID, Total: 100}
	if _, err := db.Insert(order).Exec(ctx); err != nil {
		t.Fatalf("seed relation order: %v", err)
	}
	for _, sku := range []string{"sku-b", "sku-a"} {
		if _, err := db.Insert(&selectStateRelationItem{OrderID: order.ID, SKU: sku}).Exec(ctx); err != nil {
			t.Fatalf("seed relation item %q: %v", sku, err)
		}
	}

	var got selectStateRelationUser
	err := db.Select(&got).
		Relation("Orders", func(query *bun.SelectQuery) *bun.SelectQuery {
			return query.OrderExpr("total ASC")
		}).
		Relation("Orders.Items", func(query *bun.SelectQuery) *bun.SelectQuery {
			return query.OrderExpr("sku ASC")
		}).
		Where("?TableAlias.id = ?", user.ID).
		Clone().
		Scan(ctx)
	if err != nil {
		t.Fatalf("cloned nested relation Scan = %v", err)
	}
	if len(got.Orders) != 1 {
		t.Fatalf("cloned nested relation Orders = %+v, want one order", got.Orders)
	}
	items := got.Orders[0].Items
	if len(items) != 2 || items[0].SKU != "sku-a" || items[1].SKU != "sku-b" {
		t.Fatalf("cloned nested relation Items = %+v, want sku-a then sku-b", items)
	}
}

func TestSelectQuery_ExplicitConnIncludesRelationQueries(t *testing.T) {
	dir := t.TempDir()
	defaultDB := openSelectStateFileDB(t, filepath.Join(dir, "relation-default.db"))
	explicitDB := openSelectStateFileDB(t, filepath.Join(dir, "relation-explicit.db"))
	createSelectStateRelationTables(t, defaultDB)
	createSelectStateRelationTables(t, explicitDB)
	ctx := t.Context()

	seed := func(t *testing.T, db *sqldb.DB, label string, total int) *selectStateRelationUser {
		t.Helper()
		user := &selectStateRelationUser{Name: label + "-user"}
		if _, err := db.Insert(user).Exec(ctx); err != nil {
			t.Fatalf("seed %s user: %v", label, err)
		}
		if _, err := db.Insert(&selectStateRelationProfile{
			UserID: user.ID,
			Label:  label + "-profile",
		}).Exec(ctx); err != nil {
			t.Fatalf("seed %s profile: %v", label, err)
		}
		if _, err := db.Insert(&selectStateRelationOrder{
			UserID: user.ID,
			Total:  total,
		}).Exec(ctx); err != nil {
			t.Fatalf("seed %s order: %v", label, err)
		}
		return user
	}

	defaultUser := seed(t, defaultDB, "default", 10)
	explicitUser := seed(t, explicitDB, "explicit", 20)
	if defaultUser.ID != explicitUser.ID {
		t.Fatalf("fixture IDs differ: default=%d explicit=%d", defaultUser.ID, explicitUser.ID)
	}

	var got selectStateRelationUser
	err := defaultDB.Select(&got).
		Conn(explicitDB.Client()).
		Relation("Profile").
		Relation("Orders").
		Where("?TableAlias.id = ?", explicitUser.ID).
		Scan(ctx)
	if err != nil {
		t.Fatalf("explicit Conn relation Scan = %v", err)
	}
	if got.Name != "explicit-user" {
		t.Fatalf("relation root = %q, want explicit-user", got.Name)
	}
	if got.Profile == nil || got.Profile.Label != "explicit-profile" {
		t.Fatalf("relation profile = %+v, want explicit-profile", got.Profile)
	}
	if len(got.Orders) != 1 || got.Orders[0].Total != 20 {
		t.Fatalf("relation orders = %+v, want explicit total 20", got.Orders)
	}
}

type selectStateHookUser struct {
	bun.BaseModel `bun:"table:select_state_hook_users,alias:shu"`
	ID            int    `bun:"id,pk,autoincrement"`
	Name          string `bun:"name"`
	Email         string `bun:"email"`
}

type selectStateBeforeAppendKey struct{}

type selectStateBeforeAppendState struct {
	calls int
}

type selectStateBeforeAppendUser struct {
	bun.BaseModel `bun:"table:select_state_before_append_users,alias:sbau"`
	ID            int    `bun:"id,pk,autoincrement"`
	Name          string `bun:"name"`
}

func (*selectStateBeforeAppendUser) BeforeAppendModel(
	ctx context.Context,
	query schema.Query,
) error {
	state, ok := ctx.Value(selectStateBeforeAppendKey{}).(*selectStateBeforeAppendState)
	if !ok {
		return errors.New("missing BeforeAppendModel test state")
	}
	selectQuery, ok := query.(*bun.SelectQuery)
	if !ok {
		return errors.New("BeforeAppendModel received a non-select query")
	}
	state.calls++
	selectQuery.Where("?TableAlias.name = ?", "included")
	return nil
}

func TestSelectQuery_ClonePreservesBeforeAppendModel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.WithValue(
		t.Context(),
		selectStateBeforeAppendKey{},
		new(selectStateBeforeAppendState),
	)
	_, err := db.Client().NewRaw(`
		CREATE TABLE select_state_before_append_users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL
		);
		INSERT INTO select_state_before_append_users (name)
		VALUES ('included'), ('excluded')
	`).Exec(ctx)
	if err != nil {
		t.Fatalf("create BeforeAppendModel fixture: %v", err)
	}

	var got selectStateBeforeAppendUser
	source := db.Select(&got).OrderExpr("id ASC")
	if err := source.Clone().Scan(ctx); err != nil {
		t.Fatalf("cloned BeforeAppendModel Scan = %v", err)
	}
	if got.Name != "included" {
		t.Fatalf("cloned BeforeAppendModel rows = %+v, want only included", got)
	}
	state := ctx.Value(selectStateBeforeAppendKey{}).(*selectStateBeforeAppendState)
	if state.calls != 1 {
		t.Fatalf("BeforeAppendModel calls = %d, want 1", state.calls)
	}
	if query := source.Unwrap().String(); strings.Contains(query, "included") {
		t.Fatalf("BeforeAppendModel mutated reusable source query: %q", query)
	}
}

func (*selectStateHookUser) BeforeSelect(_ context.Context, query *bun.SelectQuery) error {
	// Excluding the middle element mutates the columns slice in place. A
	// top-level shallow query copy would therefore corrupt the reusable source
	// builder's backing array and make the second execution fail.
	query.ExcludeColumn("name")
	return nil
}

func createSelectStateHookUsersTable(t *testing.T, db *sqldb.DB) {
	t.Helper()
	_, err := db.Client().NewRaw(`
		CREATE TABLE select_state_hook_users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			email TEXT NOT NULL
		)
	`).Exec(t.Context())
	if err != nil {
		t.Fatalf("create hook table: %v", err)
	}
}

func TestSelectQuery_ModelHookDoesNotMutateReusableBuilder(t *testing.T) {
	db := openTestDB(t)
	createSelectStateHookUsersTable(t, db)
	ctx := t.Context()

	for _, user := range []*selectStateHookUser{
		{Name: "hook-first", Email: "first@example.com"},
		{Name: "hook-second", Email: "second@example.com"},
	} {
		if _, err := db.Insert(user).Exec(ctx); err != nil {
			t.Fatalf("seed %q: %v", user.Name, err)
		}
	}

	var got []selectStateHookUser
	query := db.Select(&got).
		Column("id", "name", "email").
		OrderExpr("id ASC")

	for run := 1; run <= 2; run++ {
		got = nil
		if err := query.Scan(ctx); err != nil {
			t.Fatalf("run %d Scan = %v", run, err)
		}
		if len(got) != 2 {
			t.Fatalf("run %d len = %d, want 2", run, len(got))
		}
		for i, user := range got {
			if user.Name != "" {
				t.Errorf("run %d row %d Name = %q, want excluded zero value", run, i, user.Name)
			}
			if user.Email == "" {
				t.Errorf("run %d row %d Email is empty, want selected email", run, i)
			}
		}
		sourceSQL := query.Unwrap().String()
		if count := strings.Count(sourceSQL, `"name"`); count != 1 {
			t.Fatalf("run %d source SQL name count = %d in %q, want 1", run, count, sourceSQL)
		}
		if count := strings.Count(sourceSQL, `"email"`); count != 1 {
			t.Fatalf("run %d source SQL email count = %d in %q, want 1", run, count, sourceSQL)
		}
	}
}

func TestSelectQuery_TypedTerminalsRejectPreboundModelWithoutQuery(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	assertRejected := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, sqldb.ErrTypedTerminalModel) {
			t.Errorf("typed terminal error = %v, want sqldb.ErrTypedTerminalModel", err)
		}
		if err != nil && !strings.Contains(err.Error(), "Scan") {
			t.Errorf("typed terminal error = %q, want safe Scan guidance", err)
		}
		if count := hook.Count(); count != 0 {
			t.Fatalf("typed terminal executed %d SELECT queries, want 0", count)
		}
	}

	t.Run("One rejects Select model with relation", func(t *testing.T) {
		hook.Reset()
		var bound selectStateRelationUser
		got, err := db.Select(&bound).
			Relation("Orders").
			One[selectStateRelationUser](ctx)
		assertRejected(t, err)
		if got.ID != 0 || got.Name != "" || got.Profile != nil || got.Orders != nil {
			t.Errorf("One rejected value = %+v, want zero value", got)
		}
	})

	t.Run("All rejects Model method", func(t *testing.T) {
		hook.Reset()
		bound := []selectStateRelationUser{}
		got, err := db.Select().
			Model(&bound).
			All[selectStateRelationUser](ctx)
		assertRejected(t, err)
		if got == nil || len(got) != 0 {
			t.Errorf("All rejected value = %#v, want non-nil empty slice", got)
		}
	})

	t.Run("Page rejects model bound through Apply", func(t *testing.T) {
		hook.Reset()
		bound := []selectStateRelationUser{}
		got, err := db.Select().
			Apply(func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Model(&bound)
			}).
			Page[selectStateRelationUser](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
		assertRejected(t, err)
		if got != nil {
			t.Errorf("Page rejected value = %+v, want nil", got)
		}
	})

	t.Run("earlier builder error wins over model guard", func(t *testing.T) {
		hook.Reset()
		sentinel := errors.New("pre-bound builder sentinel")
		var bound selectStateRelationUser
		got, err := db.Select(&bound).
			Apply(func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Err(sentinel)
			}).
			One[selectStateRelationUser](ctx)
		if err != sentinel {
			t.Fatalf("typed terminal error = %v, want exact earlier sentinel", err)
		}
		if got.ID != 0 || got.Name != "" || got.Profile != nil || got.Orders != nil {
			t.Errorf("One builder-error value = %+v, want zero value", got)
		}
		if count := hook.Total(); count != 0 {
			t.Fatalf("builder-error typed terminal executed %d queries, want 0", count)
		}
	})
}

func TestSelectQuery_PageRejectsInvalidRequestWithoutQuery(t *testing.T) {
	db := openTestDB(t)
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	const maxBunValue = int(1<<31 - 1)
	tests := []struct {
		name string
		req  *pagination.PageRequest
	}{
		{"nil request", nil},
		{"zero page", &pagination.PageRequest{Page: 0, PerPage: 10}},
		{"negative page", &pagination.PageRequest{Page: -1, PerPage: 10}},
		{"zero per page", &pagination.PageRequest{Page: 1, PerPage: 0}},
		{"negative per page", &pagination.PageRequest{Page: 1, PerPage: -1}},
		{
			"Bun int32 offset overflow",
			&pagination.PageRequest{Page: 1_073_741_825, PerPage: 2},
		},
	}
	if strconv.IntSize > 32 {
		aboveMax := int64(maxBunValue) + 1
		tests = append(tests, struct {
			name string
			req  *pagination.PageRequest
		}{"Bun int32 limit overflow", &pagination.PageRequest{Page: 1, PerPage: int(aboveMax)}})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook.Reset()
			var before pagination.PageRequest
			if tt.req != nil {
				before = *tt.req
			}

			page, err := db.Select().Page[User](t.Context(), tt.req)
			if page != nil {
				t.Fatalf("Page() result = %+v, want nil", page)
			}
			if !errors.Is(err, pagination.ErrInvalidPageRequest) {
				t.Fatalf("Page() error = %v, want pagination.ErrInvalidPageRequest", err)
			}
			if got := hook.Total(); got != 0 {
				t.Fatalf("Page() executed %d DB operations, want 0", got)
			}
			if tt.req != nil && *tt.req != before {
				t.Fatalf("Page() mutated request: got %+v, want %+v", *tt.req, before)
			}
		})
	}
}

func TestSelectQuery_PageBunInt32Boundary(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	if _, err := db.Insert(&User{Name: "boundary", Email: "boundary@example.com"}).Exec(t.Context()); err != nil {
		t.Fatalf("insert boundary user: %v", err)
	}
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	tests := []struct {
		name string
		req  pagination.PageRequest
	}{
		{
			name: "maximum offset minus one",
			req:  pagination.PageRequest{Page: 1_073_741_824, PerPage: 2},
		},
		{
			name: "maximum limit and offset",
			req:  pagination.PageRequest{Page: 2, PerPage: int(1<<31 - 1)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook.Reset()
			req := tt.req
			page, err := db.Select().Page[User](t.Context(), &req)
			if err != nil {
				t.Fatalf("Page() = %v", err)
			}
			if page == nil || page.Page != req.Page || page.PerPage != req.PerPage {
				t.Fatalf("Page() = %+v, want request metadata %+v", page, req)
			}
			if page.Total != 1 || len(page.Records) != 0 {
				t.Fatalf("Page() = %+v, want one total row beyond the requested offset", page)
			}
			if got := hook.Total(); got != 2 {
				t.Fatalf("Page() executed %d DB operations, want COUNT and bounded SELECT", got)
			}
			if req != tt.req {
				t.Fatalf("Page() mutated request: got %+v, want %+v", req, tt.req)
			}
		})
	}
}

func TestSelectQuery_BunInt32LimitOffsetConformance(t *testing.T) {
	db := openTestDB(t)
	const maxBunValue = int(1<<31 - 1)
	selectType := reflect.TypeFor[bun.SelectQuery]()
	orderLimitOffset, ok := selectType.FieldByName("orderLimitOffsetQuery")
	if !ok {
		t.Fatal("Bun SelectQuery no longer embeds orderLimitOffsetQuery; re-evaluate Page bounds")
	}
	for _, name := range []string{"limit", "offset"} {
		field, ok := orderLimitOffset.Type.FieldByName(name)
		if !ok || field.Type != reflect.TypeFor[int32]() {
			t.Fatalf("Bun %s field = (%v, %v), want int32; re-evaluate Page bounds", name, field.Type, ok)
		}
	}

	sql := db.Select().
		TableExpr("users").
		Limit(maxBunValue).
		Offset(maxBunValue).
		Unwrap().String()
	if !strings.Contains(sql, "LIMIT 2147483647") || !strings.Contains(sql, "OFFSET 2147483647") {
		t.Fatalf("Bun max LIMIT/OFFSET SQL = %q, want both int32 maxima", sql)
	}
}

func TestSelectQuery_LimitOffsetRejectBunNarrowingWithoutQuery(t *testing.T) {
	if strconv.IntSize <= 32 {
		t.Skip("int cannot represent values outside Bun's int32 range")
	}
	db := openTestDB(t)
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	const (
		minBunValue = int(-1 << 31)
		maxBunValue = int(1<<31 - 1)
	)
	above64 := int64(maxBunValue)
	above64++
	aboveMax := int(above64)
	below64 := int64(minBunValue)
	below64--
	belowMin := int(below64)
	tests := []struct {
		name  string
		build func() *sqldb.SelectQuery
	}{
		{"limit above max", func() *sqldb.SelectQuery { return db.Select().Limit(aboveMax) }},
		{"limit below min", func() *sqldb.SelectQuery { return db.Select().Limit(belowMin) }},
		{"offset above max", func() *sqldb.SelectQuery { return db.Select().Offset(aboveMax) }},
		{"offset below min", func() *sqldb.SelectQuery { return db.Select().Offset(belowMin) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook.Reset()
			_, err := tt.build().All[User](t.Context())
			if !errors.Is(err, sqldb.ErrInvalidLimitOffset) {
				t.Fatalf("All() error = %v, want ErrInvalidLimitOffset", err)
			}
			if got := hook.Total(); got != 0 {
				t.Fatalf("All() executed %d DB operations, want 0", got)
			}
		})
	}
}

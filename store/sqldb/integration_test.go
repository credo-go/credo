package sqldb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/pagination"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/store/sqldb"

	_ "modernc.org/sqlite"
)

// openTestDB creates an in-memory SQLite DB for testing.
func openTestDB(t *testing.T) *sqldb.DB {
	t.Helper()
	db, err := sqldb.Open(&sqldb.Config{
		Driver: "sqlite",
		DSN:    ":memory:",
	})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { db.Shutdown(context.Background()) })
	return db
}

// createUsersTable creates a test table.
func createUsersTable(t *testing.T, db *sqldb.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := db.Client().NewRaw(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			email TEXT NOT NULL
		)
	`).Exec(ctx)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
}

type User struct {
	bun.BaseModel `bun:"table:users"`
	ID            int    `bun:"id,pk,autoincrement"`
	Name          string `bun:"name"`
	Email         string `bun:"email"`
}

type blockingSavepointHook struct {
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (h *blockingSavepointHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(event.Query)), "SAVEPOINT ") {
		h.once.Do(func() { close(h.entered) })
		<-h.release
	}
	return ctx
}

func (*blockingSavepointHook) AfterQuery(context.Context, *bun.QueryEvent) {}

// --- Lifecycle tests ---

func TestDB_Ping(t *testing.T) {
	db := openTestDB(t)
	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() = %v", err)
	}
}

func TestDB_Health_Up(t *testing.T) {
	db := openTestDB(t)
	h := db.Health(context.Background())
	if h.Status != store.StatusUp {
		t.Errorf("Health().Status = %q, want %q", h.Status, store.StatusUp)
	}
	if h.Latency < 0 {
		t.Errorf("Health().Latency = %v, want >= 0", h.Latency)
	}
	assertPoolHealthDetails(t, h, db.Stats())
}

func TestDB_Stats(t *testing.T) {
	db, err := sqldb.Open(&sqldb.Config{
		Driver:  "sqlite",
		DSN:     ":memory:",
		MaxOpen: 3,
		MaxIdle: new(0),
	})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

	stats := db.Stats()
	if stats.MaxOpenConnections != 3 {
		t.Fatalf("Stats().MaxOpenConnections = %d, want 3", stats.MaxOpenConnections)
	}
	if stats.OpenConnections != 0 || stats.InUse != 0 || stats.Idle != 0 {
		t.Fatalf("Stats() before first use = %+v, want empty pool", stats)
	}
}

func TestDB_MaxIdlePresenceControlsRuntimePool(t *testing.T) {
	tests := []struct {
		name              string
		maxIdle           *int
		wantIdle          int
		wantMaxIdleClosed bool
	}{
		{name: "omitted keeps database sql default", wantIdle: 1},
		{
			name:              "explicit zero disables idle connections",
			maxIdle:           new(0),
			wantMaxIdleClosed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := sqldb.Open(&sqldb.Config{
				Driver:  "sqlite",
				DSN:     ":memory:",
				MaxIdle: tt.maxIdle,
			})
			if err != nil {
				t.Fatalf("Open() = %v", err)
			}
			t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

			if err := db.Ping(t.Context()); err != nil {
				t.Fatalf("Ping() = %v", err)
			}
			stats := db.Stats()
			if stats.Idle != tt.wantIdle {
				t.Fatalf("Stats().Idle = %d, want %d", stats.Idle, tt.wantIdle)
			}
			if tt.wantMaxIdleClosed && stats.MaxIdleClosed < 1 {
				t.Fatalf("Stats().MaxIdleClosed = %d, want >= 1", stats.MaxIdleClosed)
			}
		})
	}
}

func TestDB_Stats_WaitCounters(t *testing.T) {
	db, err := sqldb.Open(&sqldb.Config{
		Driver:  "sqlite",
		DSN:     ":memory:",
		MaxOpen: 1,
		MaxIdle: new(1),
	})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

	held, err := db.Client().DB.Conn(t.Context())
	if err != nil {
		t.Fatalf("acquire held connection: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	before := db.Stats()
	waitCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waitResult := make(chan error, 1)
	go func() {
		second, secondErr := db.Client().DB.Conn(waitCtx)
		if second != nil {
			_ = second.Close()
		}
		waitResult <- secondErr
	}()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for db.Stats().WaitCount <= before.WaitCount {
		select {
		case err := <-waitResult:
			_ = held.Close()
			t.Fatalf("second Conn() returned before a pool wait was observed: %v", err)
		case <-ticker.C:
		case <-deadline.C:
			cancel()
			_ = held.Close()
			err := <-waitResult
			t.Fatalf("pool wait was not observed before deadline; second Conn() = %v", err)
		}
	}
	// WaitCount increments when the request joins the queue. Keep it queued for
	// one clock tick so WaitDuration also has a measurable positive delta.
	durationTick := time.NewTimer(time.Millisecond)
	defer durationTick.Stop()
	select {
	case err := <-waitResult:
		_ = held.Close()
		t.Fatalf("second Conn() returned while the only connection was held: %v", err)
	case <-durationTick.C:
	case <-deadline.C:
		cancel()
		_ = held.Close()
		err := <-waitResult
		t.Fatalf("pool wait duration was not observable before deadline; second Conn() = %v", err)
	}
	cancel()
	if err := <-waitResult; !errors.Is(err, context.Canceled) {
		_ = held.Close()
		t.Fatalf("second Conn() error = %v, want context canceled", err)
	}
	if err := held.Close(); err != nil {
		t.Fatalf("close held connection: %v", err)
	}

	stats := db.Stats()
	if stats.WaitCount <= before.WaitCount {
		t.Fatalf("Stats().WaitCount = %d, want > %d", stats.WaitCount, before.WaitCount)
	}
	if stats.WaitDuration <= before.WaitDuration {
		t.Fatalf("Stats().WaitDuration = %s, want > %s", stats.WaitDuration, before.WaitDuration)
	}

	h := db.Health(t.Context())
	if h.Status != store.StatusUp {
		t.Fatalf("Health().Status = %q after historical pool waits, want %q", h.Status, store.StatusUp)
	}
	if got := h.Details["wait_count"]; got != stats.WaitCount {
		t.Fatalf("Health().Details[wait_count] = %v, want %d", got, stats.WaitCount)
	}
	if got := h.Details["wait_duration"]; got != stats.WaitDuration {
		t.Fatalf("Health().Details[wait_duration] = %v, want %s", got, stats.WaitDuration)
	}
}

func assertPoolHealthDetails(t *testing.T, h store.Health, stats sql.DBStats) {
	t.Helper()
	want := map[string]any{
		"max_open":             stats.MaxOpenConnections,
		"open_connections":     stats.OpenConnections,
		"in_use":               stats.InUse,
		"idle":                 stats.Idle,
		"wait_count":           stats.WaitCount,
		"wait_duration":        stats.WaitDuration,
		"max_idle_closed":      stats.MaxIdleClosed,
		"max_idle_time_closed": stats.MaxIdleTimeClosed,
		"max_lifetime_closed":  stats.MaxLifetimeClosed,
	}
	if !reflect.DeepEqual(h.Details, want) {
		t.Fatalf("Health().Details = %#v, want %#v", h.Details, want)
	}
}

func TestDB_Client(t *testing.T) {
	db := openTestDB(t)
	client := db.Client()
	if client == nil {
		t.Fatal("Client() returned nil")
	}
}

// --- Query proxy tests ---

func TestSelectQuery_Scan(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	// Insert via Client escape hatch.
	_, err := db.Client().NewInsert().Model(&User{Name: "alice", Email: "a@b.c"}).Exec(ctx)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var user User
	err = db.Select(&user).Where("name = ?", "alice").Scan(ctx)
	if err != nil {
		t.Fatalf("Select().Scan() = %v", err)
	}
	if user.Name != "alice" {
		t.Errorf("user.Name = %q, want %q", user.Name, "alice")
	}
}

func TestSelectQuery_Scan_NotFound(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)

	var user User
	err := db.Select(&user).Where("name = ?", "nonexistent").Scan(context.Background())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Select non-existent: err = %v, want store.ErrNotFound", err)
	}
}

func TestSelectQuery_Count(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Client().NewInsert().Model(&User{Name: "a", Email: "a@b"}).Exec(ctx)
	db.Client().NewInsert().Model(&User{Name: "b", Email: "b@b"}).Exec(ctx)

	n, err := db.Select((*User)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("Count() = %v", err)
	}
	if n != 2 {
		t.Errorf("Count() = %d, want 2", n)
	}
}

func TestSelectQuery_Exists(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Client().NewInsert().Model(&User{Name: "a", Email: "a@b"}).Exec(ctx)

	ok, err := db.Select((*User)(nil)).Where("name = ?", "a").Exists(ctx)
	if err != nil {
		t.Fatalf("Exists() = %v", err)
	}
	if !ok {
		t.Error("Exists() = false, want true")
	}

	ok, err = db.Select((*User)(nil)).Where("name = ?", "zzz").Exists(ctx)
	if err != nil {
		t.Fatalf("Exists() = %v", err)
	}
	if ok {
		t.Error("Exists() = true for non-existent, want false")
	}
}

// --- JOIN / projection builder tests ---

type Order struct {
	bun.BaseModel `bun:"table:orders"`
	ID            int `bun:"id,pk,autoincrement"`
	UserID        int `bun:"user_id"`
	Total         int `bun:"total"`
}

func createOrdersTable(t *testing.T, db *sqldb.DB) {
	t.Helper()
	_, err := db.Client().NewRaw(`
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			total INTEGER NOT NULL
		)
	`).Exec(context.Background())
	if err != nil {
		t.Fatalf("create orders table: %v", err)
	}
}

func TestSelectQuery_Join(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	createOrdersTable(t, db)
	ctx := context.Background()

	u := &User{Name: "joiner", Email: "j@b"}
	db.Insert(u).Exec(ctx)
	db.Insert(&Order{UserID: u.ID, Total: 100}).Exec(ctx)
	db.Insert(&Order{UserID: u.ID, Total: 200}).Exec(ctx)

	var users []User
	err := db.Select(&users).
		Join("JOIN orders AS o ON o.user_id = ?TableAlias.id").
		Where("o.total > ?", 150).
		Scan(ctx)
	if err != nil {
		t.Fatalf("Select().Join().Scan() = %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("len(users) = %d, want 1", len(users))
	}
	if users[0].Name != "joiner" {
		t.Errorf("users[0].Name = %q, want joiner", users[0].Name)
	}
}

func TestSelectQuery_JoinOn(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	createOrdersTable(t, db)
	ctx := context.Background()

	u := &User{Name: "joinon", Email: "jo@b"}
	db.Insert(u).Exec(ctx)
	db.Insert(&Order{UserID: u.ID, Total: 50}).Exec(ctx)

	n, err := db.Select((*User)(nil)).
		Join("JOIN orders AS o").
		JoinOn("o.user_id = ?TableAlias.id").
		JoinOn("o.total >= ?", 50).
		Count(ctx)
	if err != nil {
		t.Fatalf("Select().Join().JoinOn().Count() = %v", err)
	}
	if n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
}

func TestSelectQuery_JoinOnOr(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	createOrdersTable(t, db)
	ctx := context.Background()

	u := &User{Name: "joinor", Email: "jr@b"}
	db.Insert(u).Exec(ctx)
	db.Insert(&Order{UserID: u.ID, Total: 1}).Exec(ctx)
	db.Insert(&Order{UserID: 99999, Total: 2}).Exec(ctx)

	n, err := db.Select((*Order)(nil)).
		Join("LEFT JOIN users AS u").
		JoinOn("u.id = ?TableAlias.user_id").
		JoinOnOr("?TableAlias.total = ?", 2).
		Count(ctx)
	if err != nil {
		t.Fatalf("Select().Join().JoinOnOr().Count() = %v", err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestSelectQuery_TableExpr_ColumnExpr(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "u1", Email: "1@b"}).Exec(ctx)
	db.Insert(&User{Name: "u2", Email: "2@b"}).Exec(ctx)

	var n int
	err := db.Select().
		ColumnExpr("count(*)").
		TableExpr("users").
		Scan(ctx, &n)
	if err != nil {
		t.Fatalf("Select().ColumnExpr().TableExpr().Scan() = %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestSelectQuery_ExcludeColumn(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "exc", Email: "secret@b"}).Exec(ctx)

	var user User
	err := db.Select(&user).
		ExcludeColumn("email").
		Where("name = ?", "exc").
		Scan(ctx)
	if err != nil {
		t.Fatalf("Select().ExcludeColumn().Scan() = %v", err)
	}
	if user.Name != "exc" {
		t.Errorf("Name = %q, want exc", user.Name)
	}
	if user.Email != "" {
		t.Errorf("Email = %q, want empty (excluded)", user.Email)
	}
}

func TestSelectQuery_Join_PreservesInterceptors(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	createOrdersTable(t, db)
	ctx := context.Background()

	// Outside TX: nothing inserted yet.
	err := sqldb.RunInTx(ctx, db, func(txCtx context.Context) error {
		u := &User{Name: "tx-join", Email: "t@b"}
		if _, err := db.Insert(u).Exec(txCtx); err != nil {
			return err
		}
		if _, err := db.Insert(&Order{UserID: u.ID, Total: 10}).Exec(txCtx); err != nil {
			return err
		}

		// JOIN query inside TX must see uncommitted rows — proves
		// TX injection still works through the new JOIN proxy methods.
		n, err := db.Select((*User)(nil)).
			Join("JOIN orders AS o").
			JoinOn("o.user_id = ?TableAlias.id").
			Count(txCtx)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("in-tx JOIN Count = %d, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunInTx() = %v", err)
	}
}

func TestSelectQuery_Join_NotFound(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	createOrdersTable(t, db)

	// JOIN against empty orders table — error mapping must still apply.
	var user User
	err := db.Select(&user).
		Join("JOIN orders AS o ON o.user_id = ?TableAlias.id").
		Scan(context.Background())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("empty JOIN Scan: err = %v, want store.ErrNotFound", err)
	}
}

func TestInsertQuery_Exec(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	res, err := db.Insert(&User{Name: "bob", Email: "bob@b"}).Exec(ctx)
	if err != nil {
		t.Fatalf("Insert().Exec() = %v", err)
	}
	rows, _ := res.RowsAffected()
	if rows != 1 {
		t.Errorf("RowsAffected = %d, want 1", rows)
	}
}

func TestInsertQuery_Exec_Duplicate(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "dup", Email: "a@b"}).Exec(ctx)

	_, err := db.Insert(&User{Name: "dup", Email: "b@b"}).Exec(ctx)
	if !errors.Is(err, store.ErrAlreadyExists) || !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("duplicate insert: err = %v, want AlreadyExists/ErrDuplicate", err)
	}
	if kind, ok := store.KindOf(err); !ok || kind != store.KindAlreadyExists {
		t.Errorf("duplicate KindOf = (%q, %v), want (%q, true)", kind, ok, store.KindAlreadyExists)
	}
	if store.IsTransient(err) {
		t.Error("duplicate constraint must not be transient")
	}
}

func TestRaw_ErrorMapping_SQLiteConstraint(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	if _, err := db.Exec(ctx, `
		CREATE TABLE constrained_items (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			quantity INTEGER CHECK (quantity > 0)
		)
	`); err != nil {
		t.Fatalf("create constrained table: %v", err)
	}

	tests := []struct {
		name string
		sql  string
	}{
		{"not null", `INSERT INTO constrained_items (id, name, quantity) VALUES (1, NULL, 1)`},
		{"check", `INSERT INTO constrained_items (id, name, quantity) VALUES (2, 'bad', 0)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(ctx, tt.sql)
			if !errors.Is(err, store.ErrConstraint) || !errors.Is(err, store.ErrConflict) {
				t.Fatalf("Exec = %v, want exact constraint + legacy conflict", err)
			}
			if kind, ok := store.KindOf(err); !ok || kind != store.KindConstraint {
				t.Fatalf("KindOf = (%q, %v), want (%q, true)", kind, ok, store.KindConstraint)
			}
			if store.IsTransient(err) {
				t.Fatal("persistent SQLite constraint must not be transient")
			}
			structured, ok := errors.AsType[*store.Error](err)
			if !ok || structured.Code == "" {
				t.Fatalf("structured driver code missing: %#v", structured)
			}
		})
	}
}

func TestRaw_ErrorMapping_SQLiteContention(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "contention.db")
	open := func() *sqldb.DB {
		db, err := sqldb.Open(&sqldb.Config{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatalf("Open(%q) = %v", dsn, err)
		}
		t.Cleanup(func() { _ = db.Shutdown(t.Context()) })
		return db
	}

	holder := open()
	contender := open()
	ctx := t.Context()
	if _, err := holder.Exec(ctx, `CREATE TABLE locks (id INTEGER PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create locks table: %v", err)
	}

	rollbackHolder := errors.New("rollback lock holder")
	err := holder.InTx(ctx, func(txCtx context.Context) error {
		if _, err := holder.Exec(txCtx, `INSERT INTO locks (id, value) VALUES (1, 'held')`); err != nil {
			return fmt.Errorf("holder insert: %w", err)
		}

		_, contentionErr := contender.Exec(ctx, `INSERT INTO locks (id, value) VALUES (2, 'blocked')`)
		if !errors.Is(contentionErr, store.ErrContention) ||
			!errors.Is(contentionErr, store.ErrConflict) {
			t.Fatalf("contender insert = %v, want contention + legacy conflict", contentionErr)
		}
		if errors.Is(contentionErr, store.ErrTimeout) {
			t.Fatal("SQLite busy/locked must not become a request timeout")
		}
		if kind, ok := store.KindOf(contentionErr); !ok || kind != store.KindContention {
			t.Fatalf("KindOf = (%q, %v), want (%q, true)", kind, ok, store.KindContention)
		}
		if !store.IsTransient(contentionErr) {
			t.Fatal("SQLite contention must be transient metadata")
		}
		return rollbackHolder
	})
	if err != rollbackHolder {
		t.Fatalf("holder InTx = %v, want exact rollback sentinel", err)
	}
}

func TestUpdateQuery_Exec(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "charlie", Email: "old@b"}).Exec(ctx)

	res, err := db.Update((*User)(nil)).
		Set("email = ?", "new@b").
		Where("name = ?", "charlie").
		Exec(ctx)
	if err != nil {
		t.Fatalf("Update().Exec() = %v", err)
	}
	rows, _ := res.RowsAffected()
	if rows != 1 {
		t.Errorf("RowsAffected = %d, want 1", rows)
	}
}

func TestDeleteQuery_Exec(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "dave", Email: "d@b"}).Exec(ctx)

	res, err := db.Delete((*User)(nil)).Where("name = ?", "dave").Exec(ctx)
	if err != nil {
		t.Fatalf("Delete().Exec() = %v", err)
	}
	rows, _ := res.RowsAffected()
	if rows != 1 {
		t.Errorf("RowsAffected = %d, want 1", rows)
	}
}

// --- TX injection tests ---

func TestRunInTx_CommitOnNil(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	err := sqldb.RunInTx(ctx, db, func(ctx context.Context) error {
		_, err := db.Insert(&User{Name: "tx-user", Email: "tx@b"}).Exec(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx() = %v", err)
	}

	// Verify committed.
	var user User
	err = db.Select(&user).Where("name = ?", "tx-user").Scan(ctx)
	if err != nil {
		t.Fatalf("after commit: Scan() = %v", err)
	}
}

func TestRunInTxWith_CommitOnNil(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	err := sqldb.RunInTxWith(ctx, db, &sql.TxOptions{}, func(ctx context.Context) error {
		_, err := db.Insert(&User{Name: "tx-with-user", Email: "with@b"}).Exec(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("RunInTxWith() = %v", err)
	}

	var user User
	if err := db.Select(&user).Where("name = ?", "tx-with-user").Scan(ctx); err != nil {
		t.Fatalf("after RunInTxWith commit: Scan() = %v", err)
	}
}

func TestInTx_CommitOnNil(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	err := db.InTx(ctx, func(ctx context.Context) error {
		_, err := db.Insert(&User{Name: "intx-user", Email: "intx@b"}).Exec(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("InTx() = %v", err)
	}

	var user User
	if err := db.Select(&user).Where("name = ?", "intx-user").Scan(ctx); err != nil {
		t.Fatalf("after InTx commit: Scan() = %v", err)
	}
}

func TestInTx_RollbackOnError(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	err := db.InTx(ctx, func(ctx context.Context) error {
		db.Insert(&User{Name: "intx-rollback", Email: "rb@b"}).Exec(ctx)
		return errors.New("forced error")
	})
	if err == nil {
		t.Fatal("InTx should return error")
	}

	var user User
	scanErr := db.Select(&user).Where("name = ?", "intx-rollback").Scan(ctx)
	if !errors.Is(scanErr, store.ErrNotFound) {
		t.Errorf("after InTx rollback: Scan() = %v, want ErrNotFound", scanErr)
	}
}

func TestInTxWith_CommitOnNil(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	err := db.InTxWith(ctx, &sql.TxOptions{}, func(ctx context.Context) error {
		_, err := db.Insert(&User{Name: "intxwith-user", Email: "iw@b"}).Exec(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("InTxWith() = %v", err)
	}

	var user User
	if err := db.Select(&user).Where("name = ?", "intxwith-user").Scan(ctx); err != nil {
		t.Fatalf("after InTxWith commit: Scan() = %v", err)
	}
}

func TestRunInTx_RollbackOnError(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()
	callbackErr := errors.New("domain duplicate key validation")

	err := sqldb.RunInTx(ctx, db, func(ctx context.Context) error {
		db.Insert(&User{Name: "rollback-user", Email: "rb@b"}).Exec(ctx)
		return callbackErr
	})
	if err != callbackErr {
		t.Fatalf("RunInTx = %v (%T), want exact callback error %p", err, err, callbackErr)
	}
	if errors.Is(err, store.ErrDuplicate) {
		t.Fatal("domain callback error was incorrectly classified as ErrDuplicate")
	}

	// Verify rolled back.
	var user User
	scanErr := db.Select(&user).Where("name = ?", "rollback-user").Scan(ctx)
	if !errors.Is(scanErr, store.ErrNotFound) {
		t.Errorf("after rollback: Scan() = %v, want ErrNotFound", scanErr)
	}
}

func TestRunInTx_RollbackOnPanic(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()
	panicValue := &struct{ message string }{message: "test panic"}

	defer func() {
		r := recover()
		if r != panicValue {
			t.Fatalf("recovered panic = %v (%T), want exact value %p", r, r, panicValue)
		}

		// Verify rolled back.
		var user User
		scanErr := db.Select(&user).Where("name = ?", "panic-user").Scan(ctx)
		if !errors.Is(scanErr, store.ErrNotFound) {
			t.Errorf("after panic rollback: Scan() = %v, want ErrNotFound", scanErr)
		}
	}()

	sqldb.RunInTx(ctx, db, func(ctx context.Context) error {
		db.Insert(&User{Name: "panic-user", Email: "p@b"}).Exec(ctx)
		panic(panicValue)
	})
}

func TestRunInTx_NilCallbackFailsBeforeBegin(t *testing.T) {
	db := openTestDB(t)
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	err := db.InTx(t.Context(), nil)
	if !errors.Is(err, sqldb.ErrNilTxCallback) {
		t.Fatalf("InTx(nil) = %v, want ErrNilTxCallback", err)
	}
	if got := hook.Total(); got != 0 {
		t.Fatalf("InTx(nil) executed %d DB operations, want 0", got)
	}
}

func TestRunInTx_TXInjection_ViaProxy(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	// Insert outside TX.
	db.Insert(&User{Name: "pre-tx", Email: "pre@b"}).Exec(ctx)

	err := sqldb.RunInTx(ctx, db, func(txCtx context.Context) error {
		// Insert via proxy — should use the TX from context.
		_, err := db.Insert(&User{Name: "in-tx", Email: "in@b"}).Exec(txCtx)
		if err != nil {
			return err
		}

		// Verify visible within TX.
		var user User
		err = db.Select(&user).Where("name = ?", "in-tx").Scan(txCtx)
		if err != nil {
			t.Errorf("in-tx query should find row: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunInTx() = %v", err)
	}
}

func TestRunInTx_Nested_Savepoint(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	err := sqldb.RunInTx(ctx, db, func(outerCtx context.Context) error {
		_, err := db.Insert(&User{Name: "outer", Email: "o@b"}).Exec(outerCtx)
		if err != nil {
			return err
		}

		// Nested TX — inner error should rollback only the inner savepoint.
		innerErr := sqldb.RunInTx(outerCtx, db, func(innerCtx context.Context) error {
			db.Insert(&User{Name: "inner", Email: "i@b"}).Exec(innerCtx)
			return errors.New("inner rollback")
		})
		if innerErr == nil {
			t.Error("inner RunInTx should return error")
		}

		// "outer" should still be visible, "inner" should be rolled back.
		var user User
		if err := db.Select(&user).Where("name = ?", "outer").Scan(outerCtx); err != nil {
			t.Errorf("outer row should be visible: %v", err)
		}
		innerScanErr := db.Select(&user).Where("name = ?", "inner").Scan(outerCtx)
		if !errors.Is(innerScanErr, store.ErrNotFound) {
			t.Errorf("inner row should be rolled back, got: %v", innerScanErr)
		}

		return nil // commit outer
	})
	if err != nil {
		t.Fatalf("outer RunInTx() = %v", err)
	}

	// After outer commit, "outer" is visible, "inner" is not.
	var user User
	if err := db.Select(&user).Where("name = ?", "outer").Scan(ctx); err != nil {
		t.Errorf("outer row should be committed: %v", err)
	}
	innerErr := db.Select(&user).Where("name = ?", "inner").Scan(ctx)
	if !errors.Is(innerErr, store.ErrNotFound) {
		t.Errorf("inner row should not exist after rollback: %v", innerErr)
	}
}

func TestRunInTx_NestedNonDefaultOptionsFailBeforeSavepoint(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	hook := &selectQueryCounter{}
	db.Client().AddQueryHook(hook)

	err := db.InTx(t.Context(), func(outerCtx context.Context) error {
		before := hook.Total()
		called := false
		innerErr := db.InTxWith(outerCtx, &sql.TxOptions{ReadOnly: true}, func(context.Context) error {
			called = true
			return nil
		})
		if !errors.Is(innerErr, sqldb.ErrNestedTxOptions) {
			t.Fatalf("nested InTxWith = %v, want ErrNestedTxOptions", innerErr)
		}
		if called {
			t.Fatal("nested callback ran despite unsupported options")
		}
		if got := hook.Total(); got != before {
			t.Fatalf("nested option rejection executed %d DB operations, want 0", got-before)
		}

		// Zero options are valid for a savepoint and preserve callback identity.
		innerCallbackErr := errors.New("rollback zero-option savepoint")
		innerErr = db.InTxWith(outerCtx, &sql.TxOptions{}, func(context.Context) error {
			return innerCallbackErr
		})
		if innerErr != innerCallbackErr {
			t.Fatalf("zero-option nested InTxWith = %v, want exact callback error", innerErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("outer InTx = %v", err)
	}
}

func TestRunInTx_NestedSavepointCreationTimeoutFailsClosed(t *testing.T) {
	const cleanupTimeout = 25 * time.Millisecond
	db, err := sqldb.Open(
		&sqldb.Config{Driver: "sqlite", DSN: ":memory:"},
		sqldb.WithTxCleanupTimeout(cleanupTimeout),
	)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

	entered := make(chan struct{})
	release := make(chan struct{})
	db.Client().AddQueryHook(&blockingSavepointHook{entered: entered, release: release})
	callbackCalled := false
	var innerErr error
	var elapsed time.Duration
	secondCallbackCalled := false

	outerErr := db.InTx(t.Context(), func(outerCtx context.Context) error {
		defer close(release)
		start := time.Now()
		innerErr = db.InTx(outerCtx, func(context.Context) error {
			callbackCalled = true
			return nil
		})
		elapsed = time.Since(start)
		select {
		case <-entered:
		default:
			t.Fatal("SAVEPOINT query never reached the blocking hook")
		}
		secondErr := db.InTx(outerCtx, func(context.Context) error {
			secondCallbackCalled = true
			return nil
		})
		if !errors.Is(secondErr, sqldb.ErrTxRollbackOnly) {
			t.Fatalf("second nested InTx = %v, want immediate ErrTxRollbackOnly", secondErr)
		}
		return nil
	})

	if !errors.Is(innerErr, store.ErrTimeout) || !errors.Is(innerErr, context.DeadlineExceeded) {
		t.Fatalf("nested InTx = %v, want ErrTimeout preserving DeadlineExceeded", innerErr)
	}
	if callbackCalled {
		t.Fatal("nested callback ran after savepoint creation timed out")
	}
	if secondCallbackCalled {
		t.Fatal("nested callback ran after transaction became rollback-only")
	}
	if elapsed < cleanupTimeout || elapsed > time.Second {
		t.Fatalf("nested begin elapsed = %s, want bounded near %s", elapsed, cleanupTimeout)
	}
	if !errors.Is(outerErr, sqldb.ErrTxRollbackOnly) {
		t.Fatalf("outer InTx = %v, want ErrTxRollbackOnly after fail-closed abort", outerErr)
	}
}

func TestRunInTx_NestedSavepointCreationHonorsCallbackCancellation(t *testing.T) {
	db := openTestDB(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	db.Client().AddQueryHook(&blockingSavepointHook{entered: entered, release: release})
	callbackCalled := false
	var innerErr error

	outerErr := db.InTx(t.Context(), func(outerCtx context.Context) error {
		innerCtx, cancel := context.WithCancel(outerCtx)
		defer cancel()
		canceled := make(chan struct{})
		go func() {
			<-entered
			cancel()
			close(canceled)
		}()

		innerErr = db.InTx(innerCtx, func(context.Context) error {
			callbackCalled = true
			return nil
		})
		<-canceled
		close(release)
		return nil
	})

	if !errors.Is(innerErr, context.Canceled) {
		t.Fatalf("nested InTx = %v, want context.Canceled", innerErr)
	}
	if callbackCalled {
		t.Fatal("nested callback ran after cancellation during savepoint creation")
	}
	if !errors.Is(outerErr, sqldb.ErrTxRollbackOnly) {
		t.Fatalf("outer InTx = %v, want ErrTxRollbackOnly after canceled begin", outerErr)
	}
}

func TestRunInTx_TxCleanupTimeoutExcludesCallbackDuration(t *testing.T) {
	const cleanupTimeout = 100 * time.Millisecond
	db, err := sqldb.Open(
		&sqldb.Config{Driver: "sqlite", DSN: ":memory:"},
		sqldb.WithTxCleanupTimeout(cleanupTimeout),
	)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() { _ = db.Shutdown(t.Context()) })

	err = db.InTx(t.Context(), func(outerCtx context.Context) error {
		return db.InTx(outerCtx, func(context.Context) error {
			time.Sleep(2 * cleanupTimeout)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("nested callback longer than cleanup budget = %v", err)
	}
}

func TestRunInTx_NestedCancellationRollsBackSavepoint(t *testing.T) {
	tests := []struct {
		name string
		mode string
	}{
		{name: "callback error", mode: "error"},
		{name: "nil callback result", mode: "nil"},
		{name: "panic", mode: "panic"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			createUsersTable(t, db)
			innerName := "inner-canceled-" + tt.mode
			outerName := "outer-after-" + tt.mode
			callbackErr := errors.New("nested callback error")
			panicValue := &struct{ mode string }{mode: tt.mode}

			err := db.InTx(t.Context(), func(outerCtx context.Context) error {
				innerCtx, cancel := context.WithCancel(outerCtx)
				defer cancel()
				runInner := func() error {
					return db.InTx(innerCtx, func(txCtx context.Context) error {
						if _, err := db.Insert(&User{Name: innerName, Email: innerName + "@b"}).Exec(txCtx); err != nil {
							return err
						}
						cancel()
						switch tt.mode {
						case "error":
							return callbackErr
						case "panic":
							panic(panicValue)
						default:
							return nil
						}
					})
				}

				switch tt.mode {
				case "panic":
					func() {
						defer func() {
							if got := recover(); got != panicValue {
								t.Fatalf("nested panic = %v, want exact value %p", got, panicValue)
							}
						}()
						_ = runInner()
					}()
				case "error":
					if innerErr := runInner(); innerErr != callbackErr {
						t.Fatalf("nested InTx = %v, want exact callback error", innerErr)
					}
				default:
					if innerErr := runInner(); !errors.Is(innerErr, context.Canceled) {
						t.Fatalf("nested InTx = %v, want context.Canceled", innerErr)
					}
				}

				var inner User
				innerErr := db.Select(&inner).Where("name = ?", innerName).Scan(outerCtx)
				if !errors.Is(innerErr, store.ErrNotFound) {
					return fmt.Errorf("canceled nested row remained visible: %w", innerErr)
				}
				_, err := db.Insert(&User{Name: outerName, Email: outerName + "@b"}).Exec(outerCtx)
				return err
			})
			if err != nil {
				t.Fatalf("outer InTx = %v", err)
			}

			var outer User
			if err := db.Select(&outer).Where("name = ?", outerName).Scan(t.Context()); err != nil {
				t.Fatalf("outer row after commit = %v", err)
			}
			var inner User
			if err := db.Select(&inner).Where("name = ?", innerName).Scan(t.Context()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("inner row after outer commit = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestRunInTx_SameTypeMultiDBIsolation(t *testing.T) {
	primary := openTestDB(t)
	analytics := openTestDB(t)
	createUsersTable(t, primary)
	createUsersTable(t, analytics)

	ctx := context.Background()
	if _, err := primary.Insert(&User{Name: "primary-only", Email: "p@b"}).Exec(ctx); err != nil {
		t.Fatalf("primary insert: %v", err)
	}
	if _, err := analytics.Insert(&User{Name: "analytics-only", Email: "a@b"}).Exec(ctx); err != nil {
		t.Fatalf("analytics insert: %v", err)
	}

	err := sqldb.RunInTx(ctx, primary, func(txCtx context.Context) error {
		var user User
		if err := analytics.Select(&user).Where("name = ?", "analytics-only").Scan(txCtx); err != nil {
			return err
		}
		if user.Name != "analytics-only" {
			t.Fatalf("user.Name = %q, want analytics-only", user.Name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunInTx multi-db isolation = %v", err)
	}
}

func TestRunInTx_SameTypeMultiDBNestedScopesRemainIndependent(t *testing.T) {
	primary := openTestDB(t)
	analytics := openTestDB(t)
	createUsersTable(t, primary)
	createUsersTable(t, analytics)
	rollback := errors.New("rollback both databases")

	err := primary.InTx(t.Context(), func(primaryCtx context.Context) error {
		if _, err := primary.Insert(&User{Name: "primary-tx", Email: "p@b"}).Exec(primaryCtx); err != nil {
			return err
		}

		innerErr := analytics.InTx(primaryCtx, func(bothCtx context.Context) error {
			if _, err := analytics.Insert(&User{Name: "analytics-tx", Email: "a@b"}).Exec(bothCtx); err != nil {
				return err
			}

			var primaryUser User
			if err := primary.Select(&primaryUser).Where("name = ?", "primary-tx").Scan(bothCtx); err != nil {
				return fmt.Errorf("primary scoped query inside analytics tx: %w", err)
			}
			if primaryUser.Name != "primary-tx" {
				t.Fatalf("primary query returned %q", primaryUser.Name)
			}
			return rollback
		})
		if innerErr != rollback {
			t.Fatalf("analytics InTx = %v, want exact rollback sentinel", innerErr)
		}
		return rollback
	})
	if err != rollback {
		t.Fatalf("primary InTx = %v, want exact rollback sentinel", err)
	}

	for name, db := range map[string]*sqldb.DB{"primary-tx": primary, "analytics-tx": analytics} {
		var user User
		err := db.Select(&user).Where("name = ?", name).Scan(t.Context())
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s after rollback = %v, want ErrNotFound", name, err)
		}
	}
}

func TestDB_Conn_TransactionAwareNativeBunEscapeHatch(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)

	if got := db.Conn(nil); got != db.Client() {
		t.Fatalf("Conn(nil) = %T, want underlying *bun.DB", got)
	}
	if _, err := db.RequireTx(t.Context()); !errors.Is(err, store.ErrTxMissing) {
		t.Fatalf("RequireTx outside transaction = %v, want ErrTxMissing", err)
	}

	rollback := errors.New("rollback native Bun work")
	err := db.InTx(t.Context(), func(txCtx context.Context) error {
		conn := db.Conn(txCtx)
		required, err := db.RequireTx(txCtx)
		if err != nil {
			return err
		}
		if conn == db.Client() {
			t.Fatal("Conn inside transaction fell back to the base DB")
		}

		user := &User{Name: "native-tx", Email: "native@b"}
		if _, err := conn.NewInsert().Model(user).Exec(txCtx); err != nil {
			return err
		}
		var got User
		if err := required.NewSelect().Model(&got).Where("name = ?", "native-tx").Scan(txCtx); err != nil {
			return err
		}
		if got.Name != "native-tx" {
			t.Fatalf("native select returned %q", got.Name)
		}
		if _, err := conn.ExecContext(txCtx,
			"INSERT INTO users (name, email) VALUES (?, ?)", "native-raw", "raw@b"); err != nil {
			return err
		}
		return rollback
	})
	if err != rollback {
		t.Fatalf("InTx = %v, want exact rollback sentinel", err)
	}

	for _, name := range []string{"native-tx", "native-raw"} {
		var user User
		err := db.Select(&user).Where("name = ?", name).Scan(t.Context())
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s after rollback = %v, want ErrNotFound", name, err)
		}
	}
}

func TestDB_Conn_MultiDBScopeIsolation(t *testing.T) {
	primary := openTestDB(t)
	analytics := openTestDB(t)

	rollback := errors.New("rollback primary")
	err := primary.InTx(t.Context(), func(txCtx context.Context) error {
		if primary.Conn(txCtx) == primary.Client() {
			t.Fatal("primary Conn did not select its transaction")
		}
		if analytics.Conn(txCtx) != analytics.Client() {
			t.Fatal("analytics Conn leaked primary transaction")
		}
		if _, err := analytics.RequireTx(txCtx); !errors.Is(err, store.ErrTxMissing) {
			t.Fatalf("analytics RequireTx = %v, want ErrTxMissing", err)
		}
		return rollback
	})
	if err != rollback {
		t.Fatalf("primary InTx = %v, want exact rollback sentinel", err)
	}
}

// --- Raw SQL tests ---

func TestDB_Exec_Raw(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	res, err := db.Exec(ctx, "INSERT INTO users (name, email) VALUES (?, ?)", "raw-user", "r@b")
	if err != nil {
		t.Fatalf("Exec() = %v", err)
	}
	rows, _ := res.RowsAffected()
	if rows != 1 {
		t.Errorf("RowsAffected = %d, want 1", rows)
	}
}

func TestDB_QueryRow_Raw(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Exec(ctx, "INSERT INTO users (name, email) VALUES (?, ?)", "qr-user", "q@b")

	var name string
	err := db.QueryRow(ctx, &name, "SELECT name FROM users WHERE name = ?", "qr-user")
	if err != nil {
		t.Fatalf("QueryRow() = %v", err)
	}
	if name != "qr-user" {
		t.Errorf("name = %q, want %q", name, "qr-user")
	}
}

func TestDB_Query_Raw(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Exec(ctx, "INSERT INTO users (name, email) VALUES (?, ?)", "q1", "a@b")
	db.Exec(ctx, "INSERT INTO users (name, email) VALUES (?, ?)", "q2", "b@b")

	var names []string
	err := db.Query(ctx, &names, "SELECT name FROM users ORDER BY name")
	if err != nil {
		t.Fatalf("Query() = %v", err)
	}
	if len(names) != 2 || names[0] != "q1" || names[1] != "q2" {
		t.Errorf("names = %v, want [q1, q2]", names)
	}
}

// --- Raw SQL with TX injection ---

func TestDB_Exec_Raw_WithTX(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	err := sqldb.RunInTx(ctx, db, func(txCtx context.Context) error {
		_, err := db.Exec(txCtx, "INSERT INTO users (name, email) VALUES (?, ?)", "raw-tx", "rt@b")
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx() = %v", err)
	}

	var user User
	if err := db.Select(&user).Where("name = ?", "raw-tx").Scan(ctx); err != nil {
		t.Errorf("raw-tx row should be committed: %v", err)
	}
}

// --- Query builder reuse (clone-before-mutate) ---

func TestSelectQuery_Reuse(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "reuse1", Email: "r1@b"}).Exec(ctx)
	db.Insert(&User{Name: "reuse2", Email: "r2@b"}).Exec(ctx)

	// Build a base query, then use it twice via Clone.
	base := db.Select((*User)(nil))

	var u1 User
	err := base.Clone().Model(&u1).Where("name = ?", "reuse1").Scan(ctx)
	if err != nil {
		t.Fatalf("first Scan() = %v", err)
	}
	if u1.Name != "reuse1" {
		t.Errorf("u1.Name = %q, want %q", u1.Name, "reuse1")
	}

	var u2 User
	err = base.Clone().Model(&u2).Where("name = ?", "reuse2").Scan(ctx)
	if err != nil {
		t.Fatalf("second Scan() = %v", err)
	}
	if u2.Name != "reuse2" {
		t.Errorf("u2.Name = %q, want %q", u2.Name, "reuse2")
	}
}

func TestSelectQuery_One(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"alice", "bob"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b.c"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	t.Run("found returns the row via the model-less Select form", func(t *testing.T) {
		user, err := db.Select().Where("name = ?", "alice").One[User](ctx)
		if err != nil {
			t.Fatalf("One() = %v", err)
		}
		if user.Name != "alice" {
			t.Errorf("One().Name = %q, want %q", user.Name, "alice")
		}
	})

	t.Run("no row maps to ErrNotFound and returns the zero value", func(t *testing.T) {
		user, err := db.Select().Where("name = ?", "nobody").One[User](ctx)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("One() error = %v, want store.ErrNotFound", err)
		}
		if user != (User{}) {
			t.Errorf("One() value on error = %+v, want the zero User", user)
		}
	})

	t.Run("multiple matches return the first row (LIMIT 1, not an error)", func(t *testing.T) {
		// Both rows match; One applies LIMIT 1 so this is not an error, and
		// OrderExpr makes the single returned row deterministic.
		user, err := db.Select().Where("id > ?", 0).OrderExpr("name DESC").One[User](ctx)
		if err != nil {
			t.Fatalf("One() with multiple matches = %v, want the first row", err)
		}
		if user.Name != "bob" {
			t.Errorf("One().Name = %q, want %q (first by name DESC)", user.Name, "bob")
		}
	})

	t.Run("pointer element type works like the value form", func(t *testing.T) {
		// *User as the element: the slice-backed scan resolves the table and
		// returns a non-nil *User. Regression for the pointer-element One bug.
		user, err := db.Select().Where("name = ?", "alice").One[*User](ctx)
		if err != nil {
			t.Fatalf("One[*User]() = %v", err)
		}
		if user == nil || user.Name != "alice" {
			t.Fatalf("One[*User]() = %+v, want non-nil *User{Name: alice}", user)
		}

		// A missing row still maps to ErrNotFound with the zero T (nil pointer).
		missing, err := db.Select().Where("name = ?", "nobody").One[*User](ctx)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("One[*User]() error = %v, want store.ErrNotFound", err)
		}
		if missing != nil {
			t.Errorf("One[*User]() value on error = %+v, want nil", missing)
		}
	})
}

func TestSelectQuery_All(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	t.Run("empty result is a non-nil empty slice with nil error", func(t *testing.T) {
		users, err := db.Select().All[User](ctx)
		if err != nil {
			t.Fatalf("All() on empty table = %v", err)
		}
		if users == nil {
			t.Error("All() = nil, want a non-nil empty slice")
		}
		if len(users) != 0 {
			t.Errorf("All() len = %d, want 0", len(users))
		}
	})

	for _, name := range []string{"anna", "beth", "cara"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	t.Run("returns every matching row in order", func(t *testing.T) {
		users, err := db.Select().OrderExpr("name ASC").All[User](ctx)
		if err != nil {
			t.Fatalf("All() = %v", err)
		}
		got := make([]string, len(users))
		for i, u := range users {
			got[i] = u.Name
		}
		if want := []string{"anna", "beth", "cara"}; !slices.Equal(got, want) {
			t.Errorf("All() names = %v, want %v", got, want)
		}
	})
}

func TestSelectQuery_TypedTerminals_AmbientTx(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	// Seed one row outside the transaction.
	if _, err := db.Insert(&User{Name: "outside", Email: "o@b"}).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := sqldb.RunInTx(ctx, db, func(txCtx context.Context) error {
		if _, err := db.Insert(&User{Name: "inside", Email: "i@b"}).Exec(txCtx); err != nil {
			return err
		}

		// One/All must join the ambient TX and see the uncommitted row.
		u, err := db.Select().Where("name = ?", "inside").One[User](txCtx)
		if err != nil {
			t.Errorf("One() inside TX = %v, want the uncommitted row", err)
		}
		if u.Name != "inside" {
			t.Errorf("One().Name = %q, want %q", u.Name, "inside")
		}

		all, err := db.Select().All[User](txCtx)
		if err != nil {
			t.Errorf("All() inside TX = %v", err)
		}
		if len(all) != 2 {
			t.Errorf("All() inside TX len = %d, want 2 (outside + inside)", len(all))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunInTx() = %v", err)
	}
}

func TestSelectQuery_TypedTerminals_Reuse(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "reuseA", Email: "a@b"}).Exec(ctx)
	db.Insert(&User{Name: "reuseB", Email: "b@b"}).Exec(ctx)

	base := db.Select().OrderExpr("name ASC")

	// One clones internally and applies LIMIT 1 to the clone, not to base.
	first, err := base.One[User](ctx)
	if err != nil {
		t.Fatalf("One() = %v", err)
	}
	if first.Name != "reuseA" {
		t.Errorf("One().Name = %q, want %q", first.Name, "reuseA")
	}

	// If One had mutated base, this All would inherit LIMIT 1 and return one row.
	all, err := base.All[User](ctx)
	if err != nil {
		t.Fatalf("All() after One() = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("All() len = %d, want 2 — One must not leak LIMIT 1 into the base query", len(all))
	}
}

func TestSelectQuery_Page(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"anna", "beth", "cara"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	// Page 2 of size 1, ordered by name ([anna, beth, cara]) → "beth". Built
	// model-less: T drives the table for both COUNT and SELECT.
	req := &pagination.PageRequest{Page: 2, PerPage: 1}
	page, err := db.Select().OrderExpr("name ASC").Page[User](ctx, req)
	if err != nil {
		t.Fatalf("Page() = %v", err)
	}
	if page.Total != 3 || page.Page != 2 || page.PerPage != 1 || page.TotalPages != 3 {
		t.Fatalf("page meta = %+v, want Total 3, Page 2, PerPage 1, TotalPages 3", page)
	}
	if len(page.Records) != 1 || page.Records[0].Name != "beth" {
		t.Fatalf("page.Records = %+v, want [beth]", page.Records)
	}
	if !page.HasPrev() || !page.HasNext() {
		t.Errorf("HasPrev/HasNext = %v/%v, want true/true", page.HasPrev(), page.HasNext())
	}
}

func TestSelectQuery_Page_PointerElement(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"anna", "beth", "cara"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	// A pointer element type (*User) must paginate like the value form. The
	// COUNT builds its model from a *[]T slice, so bun resolves the table for
	// []*User instead of rejecting a (**User)(nil) scalar. Regression for the
	// pointer-element COUNT bug; the SELECT side (All[*User]) was already sound.
	req := &pagination.PageRequest{Page: 2, PerPage: 1}
	page, err := db.Select().OrderExpr("name ASC").Page[*User](ctx, req)
	if err != nil {
		t.Fatalf("Page[*User]() = %v", err)
	}
	if page.Total != 3 || page.Page != 2 || page.PerPage != 1 || page.TotalPages != 3 {
		t.Fatalf("page meta = %+v, want Total 3, Page 2, PerPage 1, TotalPages 3", page)
	}
	if len(page.Records) != 1 {
		t.Fatalf("page.Records len = %d, want 1", len(page.Records))
	}
	if page.Records[0] == nil || page.Records[0].Name != "beth" {
		t.Fatalf("page.Records[0] = %+v, want non-nil *User{Name: beth}", page.Records[0])
	}
}

func TestSelectQuery_Page_EmptyResult(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	// No rows: the SELECT is skipped, yet the returned Page echoes the
	// requested page/per-page (NewPage with req values, not NewEmpty's
	// defaults of page 1 / per-page 50) and carries a non-nil empty slice.
	req := &pagination.PageRequest{Page: 3, PerPage: 10}
	page, err := db.Select().Page[User](ctx, req)
	if err != nil {
		t.Fatalf("Page() = %v", err)
	}
	if page.Total != 0 || page.TotalPages != 0 {
		t.Fatalf("page = %+v, want zero Total/TotalPages", page)
	}
	if page.Page != 3 || page.PerPage != 10 {
		t.Fatalf("page = %+v, want Page 3 / PerPage 10 preserved from req", page)
	}
	if page.Records == nil || len(page.Records) != 0 {
		t.Fatalf("page.Records = %#v, want non-nil empty slice", page.Records)
	}
}

func TestSelectQuery_Page_DoesNotNormalize(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"anna", "beth", "cara"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	// PerPage 100 is above pagination.MaxPerPage (50). Page assumes the
	// request is already normalized and must NOT clamp it: it uses the raw
	// value, echoes it back, and returns all three rows.
	req := &pagination.PageRequest{Page: 1, PerPage: 100}
	page, err := db.Select().OrderExpr("name ASC").Page[User](ctx, req)
	if err != nil {
		t.Fatalf("Page() = %v", err)
	}
	if page.PerPage != 100 {
		t.Errorf("page.PerPage = %d, want 100 (Page must not normalize/clamp)", page.PerPage)
	}
	if page.Total != 3 || page.TotalPages != 1 || len(page.Records) != 3 {
		t.Fatalf("page = %+v, want Total 3, TotalPages 1, 3 records", page)
	}
}

func TestSelectQuery_Page_NilRequest(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	if _, err := db.Select().Page[User](ctx, nil); err == nil {
		t.Error("Page(nil req) should return an error")
	}
}

func TestSelectQuery_Page_InsideTx(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"anna", "beth", "cara"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	// Both COUNT and SELECT must run on the ambient transaction: an
	// uncommitted insert inside the TX is visible to Page there, and
	// invisible after rollback.
	sentinelErr := errors.New("force rollback")
	err := sqldb.RunInTx(ctx, db, func(txCtx context.Context) error {
		if _, err := db.Insert(&User{Name: "dora", Email: "dora@b"}).Exec(txCtx); err != nil {
			return err
		}

		req := &pagination.PageRequest{Page: 1, PerPage: 10}
		page, err := db.Select().OrderExpr("name ASC").Page[User](txCtx, req)
		if err != nil {
			return err
		}
		if page.Total != 4 || len(page.Records) != 4 {
			t.Errorf(
				"page inside TX = Total %d / %d records, want 4 (uncommitted insert visible)",
				page.Total,
				len(page.Records),
			)
		}
		return sentinelErr
	})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("RunInTx() = %v, want sentinel rollback error", err)
	}

	req := &pagination.PageRequest{Page: 1, PerPage: 10}
	page, err := db.Select().OrderExpr("name ASC").Page[User](ctx, req)
	if err != nil {
		t.Fatalf("Page() after rollback = %v", err)
	}
	if page.Total != 3 {
		t.Errorf("page.Total after rollback = %d, want 3", page.Total)
	}
}

func TestSelectQuery_Page_Reuse(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"reuseA", "reuseB", "reuseC"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	base := db.Select().OrderExpr("name ASC")

	// Page clones internally and applies Offset/Limit to the clone, not base.
	page, err := base.Page[User](ctx, &pagination.PageRequest{Page: 1, PerPage: 1})
	if err != nil {
		t.Fatalf("Page() = %v", err)
	}
	if page.Total != 3 || len(page.Records) != 1 {
		t.Fatalf("page = %+v, want Total 3 / 1 record", page)
	}

	// If Page had mutated base, this All would inherit LIMIT 1 / OFFSET 0 and
	// return a single row.
	all, err := base.All[User](ctx)
	if err != nil {
		t.Fatalf("All() after Page() = %v", err)
	}
	if len(all) != 3 {
		t.Errorf("All() len = %d, want 3 — Page must not leak LIMIT/OFFSET into the base query", len(all))
	}
}

// --- Escape hatch tests ---

func TestSelectQuery_Apply(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "apply1", Email: "a@b"}).Exec(ctx)
	db.Insert(&User{Name: "apply2", Email: "b@b"}).Exec(ctx)

	var users []User
	err := db.Select(&users).
		Apply(func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.OrderExpr("name ASC")
		}, nil). // nil should be filtered
		Scan(ctx)
	if err != nil {
		t.Fatalf("Apply+Scan() = %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len = %d, want 2", len(users))
	}
	if users[0].Name != "apply1" {
		t.Errorf("users[0].Name = %q, want %q", users[0].Name, "apply1")
	}
}

func TestSelectQuery_Unwrap(t *testing.T) {
	db := openTestDB(t)
	raw := db.Select((*User)(nil)).Unwrap()
	if raw == nil {
		t.Fatal("Unwrap() returned nil")
	}
}

func TestApplyQueryBuilder_CrossQueryReuse(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"keep1", "target", "keep2"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@old"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	// One predicate, reused across select, update, and delete.
	scope := func(qb bun.QueryBuilder) bun.QueryBuilder {
		return qb.Where("name = ?", "target")
	}

	// Select: the predicate scopes the read.
	var got []User
	if err := db.Select(&got).ApplyQueryBuilder(scope).Scan(ctx); err != nil {
		t.Fatalf("Select+ApplyQueryBuilder: %v", err)
	}
	if len(got) != 1 || got[0].Name != "target" {
		t.Fatalf("select got %+v, want single 'target'", got)
	}

	// Update: the same predicate scopes the write.
	res, err := db.Update((*User)(nil)).
		Set("email = ?", "new").
		ApplyQueryBuilder(scope).
		Exec(ctx)
	if err != nil {
		t.Fatalf("Update+ApplyQueryBuilder: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("update affected %d rows, want 1", n)
	}

	// Only 'target' changed; siblings untouched (proves the WHERE scoped).
	var target User
	if err := db.Select(&target).Where("name = ?", "target").Scan(ctx); err != nil {
		t.Fatalf("reload target: %v", err)
	}
	if target.Email != "new" {
		t.Errorf("target.Email = %q, want %q", target.Email, "new")
	}
	var keep1 User
	if err := db.Select(&keep1).Where("name = ?", "keep1").Scan(ctx); err != nil {
		t.Fatalf("reload keep1: %v", err)
	}
	if keep1.Email != "keep1@old" {
		t.Errorf("keep1.Email = %q, want unchanged %q", keep1.Email, "keep1@old")
	}

	// Delete: the same predicate scopes the delete.
	res, err = db.Delete((*User)(nil)).ApplyQueryBuilder(scope).Exec(ctx)
	if err != nil {
		t.Fatalf("Delete+ApplyQueryBuilder: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("delete affected %d rows, want 1", n)
	}

	// Only the two 'keep' rows survive.
	remaining, err := db.Select((*User)(nil)).Count(ctx)
	if err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 2 {
		t.Errorf("remaining = %d, want 2", remaining)
	}
}

func TestApplyQueryBuilder_PreservesErrorMapping(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	// Building the WHERE through the builder must not bypass the terminal
	// interceptors: sql.ErrNoRows still maps to store.ErrNotFound.
	var user User
	err := db.Select(&user).
		ApplyQueryBuilder(func(qb bun.QueryBuilder) bun.QueryBuilder {
			return qb.Where("name = ?", "ghost")
		}).
		Scan(ctx)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound (interceptors preserved)", err)
	}
}

func TestApplyQueryBuilder_NilFnIsNoop(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	if _, err := db.Insert(&User{Name: "solo", Email: "s@b"}).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// nil predicate adds no WHERE; the query still runs and returns all rows.
	n, err := db.Select((*User)(nil)).ApplyQueryBuilder(nil).Count(ctx)
	if err != nil {
		t.Fatalf("nil ApplyQueryBuilder Count: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

func TestApplyQueryBuilder_WhereGroup(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	for _, name := range []string{"a", "b", "c"} {
		if _, err := db.Insert(&User{Name: name, Email: name + "@b"}).Exec(ctx); err != nil {
			t.Fatalf("insert %q: %v", name, err)
		}
	}

	// WhereGroup is not in the curated proxy set; it is reachable via the builder.
	n, err := db.Select((*User)(nil)).
		ApplyQueryBuilder(func(qb bun.QueryBuilder) bun.QueryBuilder {
			return qb.WhereGroup(" AND ", func(g bun.QueryBuilder) bun.QueryBuilder {
				return g.Where("name = ?", "a").WhereOr("name = ?", "b")
			})
		}).
		Count(ctx)
	if err != nil {
		t.Fatalf("WhereGroup Count: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

// --- store.Register integration ---

func TestRegister_Integration(t *testing.T) {
	db := openTestDB(t)
	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() = %v", err)
	}

	h := db.Health(context.Background())
	if h.Status != store.StatusUp {
		t.Errorf("Health() = %q, want UP", h.Status)
	}
}

// --- Conn explicit override ---

func TestSelectQuery_ExplicitConn(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "explicit", Email: "e@b"}).Exec(ctx)

	// Explicitly set Conn — TX injection should be bypassed.
	var user User
	err := db.Select(&user).
		Conn(db.Client()).
		Where("name = ?", "explicit").
		Scan(ctx)
	if err != nil {
		t.Fatalf("explicit Conn Scan() = %v", err)
	}
	if user.Name != "explicit" {
		t.Errorf("user.Name = %q, want %q", user.Name, "explicit")
	}
}

// --- RunInTx nil db ---

func TestRunInTx_NilDB(t *testing.T) {
	err := sqldb.RunInTx(context.Background(), nil, func(ctx context.Context) error {
		return nil
	})
	if err == nil {
		t.Fatal("RunInTx(nil) should return error")
	}
}

// --- DB.Conn fallback test ---

func TestDBConn_FallbackWhenNoTX(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "conntest", Email: "c@b"}).Exec(ctx)

	// No TX in context — should use DB directly.
	conn := db.Conn(ctx)
	var user User
	err := conn.NewSelect().Model(&user).Where("name = ?", "conntest").Scan(ctx)
	if err != nil {
		t.Fatalf("DB.Conn fallback Scan() = %v", err)
	}
}

// --- Health after shutdown ---

func TestDB_Health_AfterShutdown(t *testing.T) {
	db, err := sqldb.Open(&sqldb.Config{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	db.Shutdown(context.Background())

	h := db.Health(context.Background())
	if h.Status != store.StatusDown {
		t.Errorf("Health after shutdown = %q, want DOWN", h.Status)
	}
	if h.Cause == nil {
		t.Fatal("Health after shutdown Cause = nil, want typed ping failure")
	}
	if _, leaked := h.Details["error"]; leaked {
		t.Fatal("Health after shutdown must not copy the cause into Details[\"error\"]")
	}
	assertPoolHealthDetails(t, h, db.Stats())
}

func TestDB_Shutdown_ClosesPoolWhenContextCanceled(t *testing.T) {
	db, err := sqldb.Open(&sqldb.Config{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}

	// A shutdown driven by an already-canceled context must still close the
	// pool; otherwise connections leak. Regression: Shutdown returned ctx.Err()
	// before calling Close, skipping the close entirely.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := db.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown(canceled ctx) = %v, want nil (pool must close)", err)
	}

	// The pool is closed: a ping now fails.
	if err := db.Ping(context.Background()); err == nil {
		t.Fatal("Ping after Shutdown = nil, want error (pool should be closed)")
	}
}

// --- Verify mapError through query proxies ---

func TestInsertQuery_ErrorMapping_DuplicateKey(t *testing.T) {
	db := openTestDB(t)
	createUsersTable(t, db)
	ctx := context.Background()

	db.Insert(&User{Name: "unique", Email: "u@b"}).Exec(ctx)

	// Second insert with the same unique name returns the semantic kind while
	// retaining the deprecated ErrDuplicate compatibility match.
	_, err := db.Insert(&User{Name: "unique", Email: "u2@b"}).Exec(ctx)
	if !errors.Is(err, store.ErrAlreadyExists) || !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("duplicate insert via proxy: err = %v, want AlreadyExists/ErrDuplicate", err)
	}
}

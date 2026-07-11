package sqldb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"

	"github.com/credo-go/credo/pagination"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/store/sqldb"
)

const (
	realDBItemsTable          = "credo_realdb_items"
	realDBNotesTable          = "credo_realdb_notes"
	realDBMigrationsTable     = "credo_realdb_migrations"
	realDBMigrationLocksTable = "credo_realdb_migration_locks"
)

type realDBItem struct {
	bun.BaseModel `bun:"table:credo_realdb_items"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name"`
	RequiredValue string `bun:"required_value"`
}

func TestRealDB_Contracts(t *testing.T) {
	cfg := loadRealDBConfig(t)
	db, openErr := sqldb.Open(cfg)
	if openErr != nil {
		t.Fatalf("Open() = %v", openErr)
	}
	t.Cleanup(func() {
		if shutdownErr := db.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Errorf("Shutdown() = %v", shutdownErr)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	t.Run("connection config", func(t *testing.T) {
		if pingErr := db.Ping(ctx); pingErr != nil {
			t.Fatalf("Ping() = %v", pingErr)
		}
		if got := db.Stats().MaxOpenConnections; got != cfg.MaxOpen {
			t.Fatalf("Stats().MaxOpenConnections = %d, want %d", got, cfg.MaxOpen)
		}
	})

	if cleanupErr := dropRealDBTables(ctx, db); cleanupErr != nil {
		t.Fatalf("clean initial real database schema: %v", cleanupErr)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.WithoutCancel(t.Context()),
			10*time.Second,
		)
		defer cleanupCancel()
		if cleanupErr := dropRealDBTables(cleanupCtx, db); cleanupErr != nil {
			t.Errorf("clean real database schema: %v", cleanupErr)
		}
	})

	createRealDBItemsTable(t, ctx, db)
	t.Run("duplicate and not-null mapping", func(t *testing.T) {
		testRealDBErrorMapping(t, ctx, db, cfg.Driver)
	})
	if cfg.Driver == "mysql" {
		t.Run("MySQL logical count output names", func(t *testing.T) {
			testRealDBMySQLCountSource(t, ctx, db)
		})
	}
	t.Run("commit and rollback", func(t *testing.T) {
		testRealDBTransactions(t, ctx, db)
	})
	t.Run("migration up lock unlock and retry", func(t *testing.T) {
		testRealDBMigrations(t, ctx, db)
	})
}

func loadRealDBConfig(t *testing.T) *sqldb.Config {
	t.Helper()

	enabled, ok := os.LookupEnv("CREDO_REAL_DB_TEST")
	if !ok {
		t.Skip("real database contracts require CREDO_REAL_DB_TEST")
	}
	if enabled != "1" {
		t.Fatalf("CREDO_REAL_DB_TEST = %q, want 1 or unset", enabled)
	}

	driver := requiredRealDBEnv(t, "CREDO_REAL_DB_DRIVER")
	switch driver {
	case "pgx", "mysql":
	default:
		t.Fatalf("CREDO_REAL_DB_DRIVER = %q, want pgx or mysql", driver)
	}

	portValue := requiredRealDBEnv(t, "CREDO_REAL_DB_PORT")
	port, portErr := strconv.Atoi(portValue)
	if portErr != nil || port < 1 || port > 65535 {
		t.Fatalf("CREDO_REAL_DB_PORT = %q, want an integer from 1 through 65535", portValue)
	}

	return &sqldb.Config{
		Driver:         driver,
		Host:           requiredRealDBEnv(t, "CREDO_REAL_DB_HOST"),
		Port:           port,
		Name:           requiredRealDBEnv(t, "CREDO_REAL_DB_NAME"),
		User:           requiredRealDBEnv(t, "CREDO_REAL_DB_USER"),
		Password:       requiredRealDBEnv(t, "CREDO_REAL_DB_PASSWORD"),
		SSLMode:        requiredRealDBEnv(t, "CREDO_REAL_DB_SSL_MODE"),
		ConnectTimeout: 5 * time.Second,
		MaxOpen:        4,
		MaxIdle:        new(2),
	}
}

func requiredRealDBEnv(t *testing.T, name string) string {
	t.Helper()
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		t.Fatalf("%s must be set and non-empty when CREDO_REAL_DB_TEST is enabled", name)
	}
	return value
}

func dropRealDBTables(ctx context.Context, db *sqldb.DB) error {
	for _, table := range []string{
		realDBNotesTable,
		realDBMigrationLocksTable,
		realDBMigrationsTable,
		realDBItemsTable,
	} {
		if _, dropErr := db.Exec(ctx, "DROP TABLE IF EXISTS "+table); dropErr != nil {
			return fmt.Errorf("drop %s: %w", table, dropErr)
		}
	}
	return nil
}

func createRealDBItemsTable(t *testing.T, ctx context.Context, db *sqldb.DB) {
	t.Helper()
	_, createErr := db.Exec(ctx, `
		CREATE TABLE credo_realdb_items (
			id BIGINT PRIMARY KEY,
			name VARCHAR(191) NOT NULL UNIQUE,
			required_value VARCHAR(191) NOT NULL
		)
	`)
	if createErr != nil {
		t.Fatalf("create %s: %v", realDBItemsTable, createErr)
	}
}

func testRealDBErrorMapping(
	t *testing.T,
	ctx context.Context,
	db *sqldb.DB,
	driver string,
) {
	t.Helper()
	duplicateCode, constraintCode := realDBErrorCodes(t, driver)

	if _, insertErr := db.Insert(&realDBItem{
		ID:            1,
		Name:          "duplicate",
		RequiredValue: "present",
	}).Exec(ctx); insertErr != nil {
		t.Fatalf("seed duplicate row: %v", insertErr)
	}
	_, duplicateErr := db.Insert(&realDBItem{
		ID:            2,
		Name:          "duplicate",
		RequiredValue: "present",
	}).Exec(ctx)
	assertRealDBMappedError(
		t,
		duplicateErr,
		store.ErrAlreadyExists,
		store.ErrDuplicate,
		store.KindAlreadyExists,
		driver,
		duplicateCode,
	)

	_, constraintErr := db.Exec(ctx, `
		INSERT INTO credo_realdb_items (id, name, required_value)
		VALUES (3, 'not-null', NULL)
	`)
	assertRealDBMappedError(
		t,
		constraintErr,
		store.ErrConstraint,
		store.ErrConflict,
		store.KindConstraint,
		driver,
		constraintCode,
	)
}

func realDBErrorCodes(t *testing.T, driver string) (duplicate string, constraint string) {
	t.Helper()
	switch driver {
	case "pgx":
		return "23505", "23502"
	case "mysql":
		return "1062", "1048"
	default:
		t.Fatalf("unsupported real database driver %q", driver)
		return "", ""
	}
}

func assertRealDBMappedError(
	t *testing.T,
	got error,
	exact error,
	legacy error,
	wantKind store.Kind,
	driver string,
	wantCode string,
) {
	t.Helper()
	if got == nil {
		t.Fatalf("database operation error = nil, want %s", wantKind)
	}
	if !errors.Is(got, exact) || !errors.Is(got, legacy) {
		t.Fatalf("database operation error = %v, want %v and %v", got, exact, legacy)
	}
	if kind, ok := store.KindOf(got); !ok || kind != wantKind {
		t.Fatalf("KindOf = (%q, %v), want (%q, true)", kind, ok, wantKind)
	}
	if store.IsTransient(got) {
		t.Fatalf("%s error must not be transient", wantKind)
	}
	structured, ok := errors.AsType[*store.Error](got)
	if !ok {
		t.Fatalf("database operation error has type %T, want *store.Error", got)
	}
	if structured.Code != wantCode {
		t.Fatalf("store.Error.Code = %q, want %q", structured.Code, wantCode)
	}
	if structured.Cause == nil || !errors.Is(got, structured.Cause) {
		t.Fatalf("store.Error did not preserve its driver cause: %#v", structured)
	}
	assertRealDBDriverCause(t, structured.Cause, driver, wantCode)
}

func assertRealDBDriverCause(t *testing.T, cause error, driver string, wantCode string) {
	t.Helper()
	switch driver {
	case "pgx":
		pgErr, ok := errors.AsType[*pgconn.PgError](cause)
		if !ok {
			t.Fatalf("driver cause has type %T, want *pgconn.PgError", cause)
		}
		if pgErr.Code != wantCode {
			t.Fatalf("pgconn.PgError.Code = %q, want %q", pgErr.Code, wantCode)
		}
	case "mysql":
		mysqlErr, ok := errors.AsType[*mysql.MySQLError](cause)
		if !ok {
			t.Fatalf("driver cause has type %T, want *mysql.MySQLError", cause)
		}
		if got := strconv.FormatUint(uint64(mysqlErr.Number), 10); got != wantCode {
			t.Fatalf("mysql.MySQLError.Number = %q, want %q", got, wantCode)
		}
	default:
		t.Fatalf("unsupported real database driver %q", driver)
	}
}

func testRealDBMySQLCountSource(t *testing.T, ctx context.Context, db *sqldb.DB) {
	t.Helper()
	if _, deleteErr := db.Exec(ctx, "DELETE FROM "+realDBItemsTable); deleteErr != nil {
		t.Fatalf("clear MySQL count-source fixture: %v", deleteErr)
	}
	if _, insertErr := db.Insert(&realDBItem{
		ID:            20,
		Name:          "MiXeD",
		RequiredValue: "present",
	}).Exec(ctx); insertErr != nil {
		t.Fatalf("seed MySQL count-source fixture: %v", insertErr)
	}

	for _, mode := range []struct {
		name      string
		statement string
		predicate string
	}{
		{
			name:      "normal sql_mode",
			statement: "SET SESSION sql_mode = ''",
			predicate: "FIND_IN_SET('NO_BACKSLASH_ESCAPES', @@SESSION.sql_mode) = 0",
		},
		{
			name:      "NO_BACKSLASH_ESCAPES",
			statement: "SET SESSION sql_mode = 'NO_BACKSLASH_ESCAPES'",
			predicate: "FIND_IN_SET('NO_BACKSLASH_ESCAPES', @@SESSION.sql_mode) > 0",
		},
	} {
		t.Run(mode.name, func(t *testing.T) {
			withRealDBMySQLMode(t, ctx, db, mode.statement, func(conn *sql.Conn) {
				const bindSecret = "credo-count-bind-secret"

				_, countErr := db.Select((*realDBItem)(nil)).
					Conn(conn).
					ColumnExpr("?TableAlias.id AS duplicate_output").
					ColumnExpr("?TableAlias.name AS duplicate_output").
					Where("?TableAlias.required_value = ?", bindSecret).
					Where(mode.predicate).
					Count(ctx)
				assertRealDBMySQL1060(t, countErr, true, bindSecret)

				_, pageErr := db.Select().
					Conn(conn).
					ColumnExpr("?TableAlias.id AS duplicate_output").
					ColumnExpr("?TableAlias.name AS duplicate_output").
					Where("?TableAlias.required_value = ?", bindSecret).
					Where(mode.predicate).
					Page[realDBItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
				assertRealDBMySQL1060(t, pageErr, true, bindSecret)

				// These positive pages make both COUNT and data SELECT depend on
				// the leased connection's session mode. Losing Conn state makes the
				// NO_BACKSLASH_ESCAPES case return the wrong window.
				wildcardPage, wildcardErr := db.Select().
					Conn(conn).
					ColumnExpr("?TableAlias.*").
					Where(mode.predicate).
					Page[realDBItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
				if wildcardErr != nil {
					t.Fatalf("wildcard Page() = %v", wildcardErr)
				}
				if wildcardPage.Total != 1 || len(wildcardPage.Records) != 1 ||
					wildcardPage.Records[0].ID != 20 {
					t.Fatalf("wildcard Page() = %+v, want one complete fixture row", wildcardPage)
				}

				implicitAliasPage, implicitAliasErr := db.Select().
					Conn(conn).
					ColumnExpr("LOWER(?TableAlias.name) name").
					Where(mode.predicate).
					Page[realDBItem](ctx, &pagination.PageRequest{Page: 1, PerPage: 10})
				if implicitAliasErr != nil {
					t.Fatalf("implicit-alias Page() = %v", implicitAliasErr)
				}
				if implicitAliasPage.Total != 1 || len(implicitAliasPage.Records) != 1 ||
					implicitAliasPage.Records[0].Name != "mixed" {
					t.Fatalf("implicit-alias Page() = %+v, want one lower-cased name", implicitAliasPage)
				}
			})
		})
	}

	const rawBindSecret = "credo-raw-bind-secret"
	var ignored int
	rawErr := db.QueryRow(ctx, &ignored, `
		SELECT COUNT(*)
		FROM (
			SELECT ? AS duplicate_output, ? AS duplicate_output
		) AS raw_duplicate_source
	`, rawBindSecret, "other")
	assertRealDBMySQL1060(t, rawErr, false, rawBindSecret)
}

func withRealDBMySQLMode(
	t *testing.T,
	ctx context.Context,
	db *sqldb.DB,
	statement string,
	run func(*sql.Conn),
) {
	t.Helper()
	conn, connErr := db.Client().DB.Conn(ctx)
	if connErr != nil {
		t.Fatalf("lease explicit MySQL connection: %v", connErr)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, resetErr := conn.ExecContext(cleanupCtx, "SET SESSION sql_mode = DEFAULT"); resetErr != nil {
			t.Errorf("reset MySQL sql_mode: %v", resetErr)
		}
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("close explicit MySQL connection: %v", closeErr)
		}
	}()

	if _, modeErr := conn.ExecContext(ctx, statement); modeErr != nil {
		t.Fatalf("configure MySQL session with %q: %v", statement, modeErr)
	}
	run(conn)
}

func assertRealDBMySQL1060(t *testing.T, got error, wantUnsupported bool, forbidden string) {
	t.Helper()
	if got == nil {
		t.Fatal("MySQL duplicate-output query succeeded, want ER_DUP_FIELDNAME")
	}
	isUnsupported := errors.Is(got, sqldb.ErrUnsupportedCountQuery)
	if isUnsupported != wantUnsupported {
		t.Fatalf(
			"errors.Is(error, ErrUnsupportedCountQuery) = %v, want %v: %v",
			isUnsupported,
			wantUnsupported,
			got,
		)
	}
	mysqlErr, ok := errors.AsType[*mysql.MySQLError](got)
	if !ok {
		t.Fatalf("MySQL duplicate-output cause has type %T, want *mysql.MySQLError", got)
	}
	if mysqlErr.Number != 1060 {
		t.Fatalf("mysql.MySQLError.Number = %d, want 1060", mysqlErr.Number)
	}
	if state := string(mysqlErr.SQLState[:]); state != "42S21" {
		t.Fatalf("mysql.MySQLError.SQLState = %q, want 42S21", state)
	}
	if forbidden != "" && strings.Contains(got.Error(), forbidden) {
		t.Fatalf("MySQL duplicate-output error exposed bound value %q: %v", forbidden, got)
	}
}

func testRealDBTransactions(t *testing.T, ctx context.Context, db *sqldb.DB) {
	t.Helper()
	commitErr := db.InTx(ctx, func(txCtx context.Context) error {
		_, insertErr := db.Insert(&realDBItem{
			ID:            10,
			Name:          "committed",
			RequiredValue: "present",
		}).Exec(txCtx)
		return insertErr
	})
	if commitErr != nil {
		t.Fatalf("committing InTx() = %v", commitErr)
	}
	var committed realDBItem
	if scanErr := db.Select(&committed).Where("id = ?", 10).Scan(ctx); scanErr != nil {
		t.Fatalf("select committed row: %v", scanErr)
	}

	callbackErr := errors.New("real database rollback callback")
	rollbackErr := db.InTx(ctx, func(txCtx context.Context) error {
		if _, insertErr := db.Insert(&realDBItem{
			ID:            11,
			Name:          "rolled-back",
			RequiredValue: "present",
		}).Exec(txCtx); insertErr != nil {
			return insertErr
		}
		return callbackErr
	})
	if rollbackErr != callbackErr { //nolint:errorlint // Callback identity is the transaction contract.
		t.Fatalf("rolling back InTx() = %v, want exact callback error", rollbackErr)
	}
	var rolledBack realDBItem
	if scanErr := db.Select(&rolledBack).Where("id = ?", 11).Scan(ctx); !errors.Is(scanErr, store.ErrNotFound) {
		t.Fatalf("select rolled-back row = %v, want store.ErrNotFound", scanErr)
	}
}

func testRealDBMigrations(t *testing.T, ctx context.Context, db *sqldb.DB) {
	t.Helper()
	firstAttemptErr := errors.New("fail first real database migration attempt")
	attempts := 0
	migrations := newGoMigrations("001_realdb_contract", func(ctx context.Context, bunDB *bun.DB) error {
		attempts++
		locks, lockCountErr := countRealDBRows(ctx, bunDB, realDBMigrationLocksTable)
		if lockCountErr != nil {
			return fmt.Errorf("count held migration lock: %w", lockCountErr)
		}
		if locks != 1 {
			return fmt.Errorf("migration lock rows = %d, want 1", locks)
		}
		if attempts == 1 {
			return firstAttemptErr
		}
		_, createErr := bunDB.NewRaw(`
			CREATE TABLE credo_realdb_notes (
				id BIGINT PRIMARY KEY,
				title VARCHAR(191) NOT NULL
			)
		`).Exec(ctx)
		return createErr
	})
	db.RegisterMigrations(
		migrations,
		migrate.WithTableName(realDBMigrationsTable),
		migrate.WithLocksTableName(realDBMigrationLocksTable),
	)

	firstErr := db.Migrate(ctx)
	if !errors.Is(firstErr, firstAttemptErr) {
		t.Fatalf("first Migrate() = %v, want first-attempt error", firstErr)
	}
	if attempts != 1 {
		t.Fatalf("migration attempts after failure = %d, want 1", attempts)
	}
	assertRealDBLockRows(t, ctx, db, 0)

	if retryErr := db.Migrate(ctx); retryErr != nil {
		t.Fatalf("retry Migrate() = %v", retryErr)
	}
	if attempts != 2 {
		t.Fatalf("migration attempts after retry = %d, want 2", attempts)
	}
	assertRealDBLockRows(t, ctx, db, 0)
	if _, insertErr := db.Exec(ctx, `
		INSERT INTO credo_realdb_notes (id, title) VALUES (1, 'migrated')
	`); insertErr != nil {
		t.Fatalf("insert into migrated table: %v", insertErr)
	}

	if noopErr := db.Migrate(ctx); noopErr != nil {
		t.Fatalf("no-op Migrate() = %v", noopErr)
	}
	if attempts != 2 {
		t.Fatalf("migration attempts after no-op = %d, want 2", attempts)
	}
	assertRealDBLockRows(t, ctx, db, 0)
}

func assertRealDBLockRows(t *testing.T, ctx context.Context, db *sqldb.DB, want int) {
	t.Helper()
	got, countErr := countRealDBRows(ctx, db.Client(), realDBMigrationLocksTable)
	if countErr != nil {
		t.Fatalf("count migration lock rows: %v", countErr)
	}
	if got != want {
		t.Fatalf("migration lock rows = %d, want %d", got, want)
	}
}

func countRealDBRows(ctx context.Context, bunDB *bun.DB, table string) (int, error) {
	var count int
	if scanErr := bunDB.NewSelect().Table(table).ColumnExpr("COUNT(*)").Scan(ctx, &count); scanErr != nil {
		return 0, scanErr
	}
	return count, nil
}

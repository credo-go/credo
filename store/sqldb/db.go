package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"

	"github.com/credo-go/credo/store"
)

// DB wraps *bun.DB with lifecycle management, query builder proxies,
// error mapping, and transaction support.
type DB struct {
	db      *bun.DB
	txScope *store.TxScope

	// migrations is the set registered via RegisterMigrations; nil until then.
	migrations   *migrate.Migrations
	migratorOpts []migrate.MigratorOption
}

// Open creates a DB from Config.
//
// Steps:
//  1. Build or use the provided DSN
//  2. Open sql.DB (or use the provided Connector)
//  3. Detect or use the provided dialect
//  4. Create bun.DB
//  5. Apply connection pool settings
func Open(cfg *Config, opts ...Option) (*DB, error) {
	if cfg == nil {
		return nil, fmt.Errorf("sqldb: config must not be nil")
	}

	o := options{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	family, err := validateConfig(cfg, o)
	if err != nil {
		return nil, err
	}

	// Open the sql.DB.
	var sqlDB *sql.DB

	if o.connector != nil {
		sqlDB = sql.OpenDB(o.connector)
	} else {
		dsn, err := cfg.buildDSN(family)
		if err != nil {
			return nil, err
		}
		sqlDB, err = sql.Open(cfg.Driver, dsn)
		if err != nil {
			return nil, fmt.Errorf("sqldb: open %q: %w", cfg.Driver, err)
		}
	}

	// Apply pool settings.
	if cfg.MaxOpen > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpen)
	}
	if cfg.MaxIdle > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdle)
	}
	if cfg.MaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.MaxLifetime)
	}

	// Detect dialect.
	dialect := o.dialect
	if dialect == nil {
		dialect = family.dialect()
	}
	if dialect == nil {
		sqlDB.Close()
		return nil, fmt.Errorf("sqldb: cannot detect dialect for driver %q; use WithDialect option", cfg.Driver)
	}

	bunDB := bun.NewDB(sqlDB, dialect)

	return &DB{db: bunDB, txScope: store.NewTxScope()}, nil
}

// Client returns the underlying *bun.DB for raw SQL, model registration,
// advanced migration operations, and any Bun feature not covered by the
// proxy layer.
//
// Warning: queries executed via the returned *bun.DB bypass the proxy
// interceptors. There is no automatic TX injection from context (see
// [DB.InTx] / [store.Conn]) and no error mapping to store.Err* sentinels.
// Use the proxy layer ([DB.Select], [DB.Insert], [DB.Update], [DB.Delete])
// for normal repository code; reserve Client() for model registration,
// raw SQL the proxy layer cannot express, and migration operations beyond
// [DB.Migrate] (rollback, status, file generation — via
// migrate.NewMigrator(db.Client(), migrations)).
func (db *DB) Client() *bun.DB {
	return db.db
}

// Ping verifies the database connection is alive.
func (db *DB) Ping(ctx context.Context) error {
	return db.db.PingContext(ctx)
}

// Shutdown gracefully closes the database connection.
func (db *DB) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return db.db.Close()
}

// Health returns structured health information including status,
// round-trip latency, and connection pool statistics.
func (db *DB) Health(ctx context.Context) store.Health {
	if ctx == nil {
		ctx = context.Background()
	}

	start := time.Now()
	err := db.db.PingContext(ctx)
	latency := time.Since(start)

	h := store.Health{
		Latency: latency,
		Details: make(map[string]any),
	}

	if err != nil {
		h.Status = store.StatusDown
		h.Details["error"] = err.Error()
		return h
	}

	h.Status = store.StatusUp

	// Pool statistics.
	stats := db.db.DB.Stats()
	h.Details["open_connections"] = stats.OpenConnections
	h.Details["in_use"] = stats.InUse
	h.Details["idle"] = stats.Idle
	h.Details["max_open"] = stats.MaxOpenConnections

	return h
}

// Select creates a new SelectQuery proxy.
func (db *DB) Select(model ...any) *SelectQuery {
	q := db.db.NewSelect()
	if len(model) > 0 {
		q = q.Model(model[0])
	}
	return &SelectQuery{raw: q, state: newQueryState(db)}
}

// Insert creates a new InsertQuery proxy.
func (db *DB) Insert(model ...any) *InsertQuery {
	q := db.db.NewInsert()
	if len(model) > 0 {
		q = q.Model(model[0])
	}
	return &InsertQuery{raw: q, state: newQueryState(db)}
}

// Update creates a new UpdateQuery proxy.
func (db *DB) Update(model ...any) *UpdateQuery {
	q := db.db.NewUpdate()
	if len(model) > 0 {
		q = q.Model(model[0])
	}
	return &UpdateQuery{raw: q, state: newQueryState(db)}
}

// Delete creates a new DeleteQuery proxy.
func (db *DB) Delete(model ...any) *DeleteQuery {
	q := db.db.NewDelete()
	if len(model) > 0 {
		q = q.Model(model[0])
	}
	return &DeleteQuery{raw: q, state: newQueryState(db)}
}

func validateConfig(cfg *Config, o options) (driverFamily, error) {
	family := resolveDriverFamily(cfg.Driver)

	if cfg.Port < 0 || cfg.Port > 65535 {
		return driverFamilyUnknown, fmt.Errorf("sqldb: port must be between 0 and 65535, got %d", cfg.Port)
	}
	if cfg.ConnectTimeout < 0 {
		return driverFamilyUnknown, fmt.Errorf("sqldb: connect timeout must be >= 0, got %s", cfg.ConnectTimeout)
	}
	if cfg.MaxOpen < 0 {
		return driverFamilyUnknown, fmt.Errorf("sqldb: max open must be >= 0, got %d", cfg.MaxOpen)
	}
	if cfg.MaxIdle < 0 {
		return driverFamilyUnknown, fmt.Errorf("sqldb: max idle must be >= 0, got %d", cfg.MaxIdle)
	}
	if cfg.MaxLifetime < 0 {
		return driverFamilyUnknown, fmt.Errorf("sqldb: max lifetime must be >= 0, got %s", cfg.MaxLifetime)
	}

	if o.connector != nil {
		return family, nil
	}

	if cfg.Driver == "" {
		return driverFamilyUnknown, fmt.Errorf("sqldb: driver must be specified (or use WithConnector)")
	}

	if cfg.DSN == "" && family == driverFamilyUnknown {
		return driverFamilyUnknown, fmt.Errorf("sqldb: cannot build DSN for driver %q; provide Config.DSN or use WithConnector", cfg.Driver)
	}

	if o.dialect == nil && family == driverFamilyUnknown {
		return driverFamilyUnknown, fmt.Errorf("sqldb: cannot detect dialect for driver %q; use WithDialect option", cfg.Driver)
	}

	return family, nil
}

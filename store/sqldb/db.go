package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
)

// DefaultPingTimeout is the deadline of the ping [DB.Start] sends when
// Config.PingTimeout is zero.
const DefaultPingTimeout = 5 * time.Second

// DB wraps *bun.DB with lifecycle management, query builder proxies,
// error mapping, and transaction support. It is a component: bound with
// app.ProvideValue, it is pinged by [DB.Start] before its dependants start,
// reported in /ready through [DB.Ready], and shut down after its consumers.
type DB struct {
	db               *bun.DB
	family           driverFamily
	txScope          *store.TxScope[bun.IDB]
	txCleanupTimeout time.Duration
	pingTimeout      time.Duration
	logger           *slog.Logger
	optionalStart    bool

	// migrations is the set registered via RegisterMigrations; nil until then.
	migrations   *migrate.Migrations
	migratorOpts []migrate.MigratorOption
}

// DB is a credo.ResourceIdentifier.
var _ interface{ ResourceIdentity() any } = (*DB)(nil)

const maxOpenUnlimitedWarningCode = "sqldb.pool.max_open_unlimited"

type poolConfigurer interface {
	SetMaxOpenConns(int)
	SetMaxIdleConns(int)
	SetConnMaxLifetime(time.Duration)
	SetConnMaxIdleTime(time.Duration)
}

// ResourceIdentity names the physical database the DB holds. Wrapper types
// that embed *DB inherit it, so a *DB and the wrappers around it are one
// resource with one owner, and the wrappers bound beside the handle are bound
// with credo.View(); the App closes the database once, through its owner,
// after the consumers of every binding. Only a wrapper without state of its
// own to release may share the identity; one that releases state of its own
// holds the *DB in a named field and does not forward it.
func (db *DB) ResourceIdentity() any {
	return db
}

// poolWarningCodes returns the secret-safe warning codes of the effective
// pool: sqldb.pool.max_open_unlimited when its maximum is unlimited.
func (db *DB) poolWarningCodes() []string {
	if db.Stats().MaxOpenConnections != 0 {
		return nil
	}
	return []string{maxOpenUnlimitedWarningCode}
}

// Start pings the database under Config.PingTimeout, so the App's start walk
// reaches it before the components that depend on it start. On success it
// logs each pool warning once at Warn — today the code
// sqldb.pool.max_open_unlimited, read from the effective pool, so a
// SetMaxOpenConns on its client before the start counts. A failed ping
// closes the database before Start returns the error, since Open opened the
// pool and the App does not shut down a component whose Start failed; with
// [WithOptionalStart] it is logged instead, and Start returns nil.
//
// The records go to [WithLogger]'s logger, else to the logger of the App's
// call (credo.InfraFrom), scoped to the database's component, and carry
// module=sqldb.
func (db *DB) Start(ctx context.Context) error {
	logger := db.startLogger(ctx)
	pingCtx, cancel := context.WithTimeout(ctx, db.pingTimeout)
	defer cancel()
	if err := db.db.PingContext(pingCtx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(pingCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("sqldb: ping did not answer within %s: %w", db.pingTimeout, err)
		} else {
			err = fmt.Errorf("sqldb: ping: %w", err)
		}
		if db.optionalStart {
			logger.Warn("sqldb: ping failed; starting without the database, which gates readiness until it answers",
				"ping_timeout", db.pingTimeout, "error", err)
			db.logPoolWarnings(logger)
			return nil
		}
		if closeErr := db.db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("sqldb: close after the failed ping: %w", closeErr))
		}
		return err
	}
	db.logPoolWarnings(logger)
	return nil
}

// Ready reports whether the database answers: nil when [DB.Health] is up,
// else an error naming the status that wraps Health's cause, so errors.Is and
// errors.As reach it.
func (db *DB) Ready(ctx context.Context) error {
	h := db.Health(ctx)
	switch {
	case h.Status == store.StatusUp:
		return nil
	case h.Cause != nil:
		return fmt.Errorf("sqldb: database is %s: %w", h.Status, h.Cause)
	default:
		return fmt.Errorf("sqldb: database is %s", h.Status)
	}
}

// startLogger is the logger of what Start logs: WithLogger's, else the App
// call's, else the framework default, with module=sqldb.
func (db *DB) startLogger(ctx context.Context) *slog.Logger {
	logger := db.logger
	if logger == nil {
		logger = credo.InfraFrom(ctx).Logger
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return logger.With("module", "sqldb")
}

// logPoolWarnings logs each pool warning code once, at Warn.
func (db *DB) logPoolWarnings(logger *slog.Logger) {
	for _, code := range db.poolWarningCodes() {
		logger.Warn("credo: store configuration warning", "code", code)
	}
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

	o := options{txCleanupTimeout: defaultTxCleanupTimeout}
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

	if o.connectorSet {
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

	applyPoolConfig(sqlDB, cfg)

	// Detect dialect.
	dialect := o.dialect
	if dialect == nil {
		dialect = family.dialect()
	}
	if dialect == nil {
		sqlDB.Close()
		return nil, fmt.Errorf("sqldb: cannot detect dialect for driver %q; use WithDialect option", cfg.Driver)
	}
	if family == driverFamilyUnknown {
		family = resolveDialectFamily(dialect)
	}

	bunDB := bun.NewDB(sqlDB, dialect)

	pingTimeout := cfg.PingTimeout
	if pingTimeout == 0 {
		pingTimeout = DefaultPingTimeout
	}
	return &DB{
		db:               bunDB,
		family:           family,
		txScope:          store.NewTxScope[bun.IDB](),
		txCleanupTimeout: o.txCleanupTimeout,
		pingTimeout:      pingTimeout,
		logger:           o.logger,
		optionalStart:    o.optionalStart,
	}, nil
}

func applyPoolConfig(pool poolConfigurer, cfg *Config) {
	if cfg.MaxOpen > 0 {
		pool.SetMaxOpenConns(cfg.MaxOpen)
	}
	if cfg.MaxIdle != nil {
		pool.SetMaxIdleConns(*cfg.MaxIdle)
	}
	if cfg.MaxLifetime > 0 {
		pool.SetConnMaxLifetime(cfg.MaxLifetime)
	}
	if cfg.MaxIdleTime > 0 {
		pool.SetConnMaxIdleTime(cfg.MaxIdleTime)
	}
}

// Client returns the underlying *bun.DB for raw SQL, model registration,
// advanced migration operations, and any Bun feature not covered by the
// proxy layer.
//
// Warning: queries executed via the returned *bun.DB bypass the proxy
// interceptors. There is no automatic TX injection from context (see
// [DB.InTx] / [DB.Conn]) and no error mapping to store.Err* sentinels.
// Use the proxy layer ([DB.Select], [DB.Insert], [DB.Update], [DB.Delete])
// for normal repository code. For an advanced Bun operation that must join
// an ambient transaction, build it through db.Conn(ctx). Reserve Client() for
// model registration and operations intentionally tied to the base DB, such
// as migration operations beyond [DB.Migrate]. A directly constructed Bun
// migrator does not inherit RegisterMigrations options. Status/generation
// callers repeat the options they need; DB-mutating apply/rollback callers
// must also own Init, Lock, and bounded cancellation-detached Unlock.
func (db *DB) Client() *bun.DB {
	return db.db
}

// Stats returns a point-in-time snapshot of the underlying database/sql
// connection pool. OpenConnections, InUse, and Idle are instantaneous gauges;
// WaitCount, WaitDuration, and the closed-connection fields are cumulative for
// the lifetime of the pool. MaxOpenConnections equal to zero means the pool is
// unlimited. Cumulative values reset when a new DB and pool are opened. Stats
// does not ping the database.
func (db *DB) Stats() sql.DBStats {
	return db.db.DB.Stats()
}

// Ping verifies the database connection is alive.
func (db *DB) Ping(ctx context.Context) error {
	return db.db.PingContext(ctx)
}

// Shutdown closes the database connection pool. The pool is always closed —
// even when ctx is already canceled or past its deadline — because leaving it
// open leaks connections; sql.DB.Close is a fast local operation that takes no
// context. ctx is accepted only to satisfy the credo.Component interface.
func (db *DB) Shutdown(_ context.Context) error {
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

	stats := db.Stats()
	h := store.Health{
		Latency: latency,
		Details: map[string]any{
			"max_open":             stats.MaxOpenConnections,
			"open_connections":     stats.OpenConnections,
			"in_use":               stats.InUse,
			"idle":                 stats.Idle,
			"wait_count":           stats.WaitCount,
			"wait_duration":        stats.WaitDuration,
			"max_idle_closed":      stats.MaxIdleClosed,
			"max_idle_time_closed": stats.MaxIdleTimeClosed,
			"max_lifetime_closed":  stats.MaxLifetimeClosed,
		},
	}

	if err != nil {
		h.Status = store.StatusDown
		h.Cause = err
		return h
	}

	h.Status = store.StatusUp

	return h
}

// Select creates a new SelectQuery proxy. It accepts zero or one model; more
// than one records a builder error that the terminal method returns.
func (db *DB) Select(model ...any) *SelectQuery {
	q := db.db.NewSelect()
	if len(model) > 1 {
		q = q.Err(optionalModelCountError("Select", len(model)))
	} else if len(model) == 1 {
		q = q.Model(model[0])
	}
	return &SelectQuery{raw: q, state: newQueryState(db)}
}

// Insert creates a new InsertQuery proxy. It accepts zero or one model; more
// than one records a builder error that Exec returns.
func (db *DB) Insert(model ...any) *InsertQuery {
	q := db.db.NewInsert()
	if len(model) > 1 {
		q = q.Err(optionalModelCountError("Insert", len(model)))
	} else if len(model) == 1 {
		q = q.Model(model[0])
	}
	return &InsertQuery{raw: q, state: newQueryState(db)}
}

// Update creates a new UpdateQuery proxy. It accepts zero or one model; more
// than one records a builder error that Exec returns.
func (db *DB) Update(model ...any) *UpdateQuery {
	q := db.db.NewUpdate()
	if len(model) > 1 {
		q = q.Err(optionalModelCountError("Update", len(model)))
	} else if len(model) == 1 {
		q = q.Model(model[0])
	}
	return &UpdateQuery{raw: q, state: newQueryState(db)}
}

// Delete creates a new DeleteQuery proxy. It accepts zero or one model; more
// than one records a builder error that Exec returns.
func (db *DB) Delete(model ...any) *DeleteQuery {
	q := db.db.NewDelete()
	if len(model) > 1 {
		q = q.Err(optionalModelCountError("Delete", len(model)))
	} else if len(model) == 1 {
		q = q.Model(model[0])
	}
	return &DeleteQuery{raw: q, state: newQueryState(db)}
}

func optionalModelCountError(operation string, count int) error {
	return fmt.Errorf("sqldb: %s accepts at most one model, got %d", operation, count)
}

// validateConfig checks the construction inputs in a fixed order — driver
// selection options, Config limits, option timeouts, then family resolution —
// and returns the driver family Open should build for.
func validateConfig(cfg *Config, o options) (driverFamily, error) {
	if err := validateDriverSelection(cfg, o); err != nil {
		return driverFamilyUnknown, err
	}
	if err := cfg.validateLimits(); err != nil {
		return driverFamilyUnknown, err
	}
	if o.txCleanupTimeout <= 0 {
		return driverFamilyUnknown, fmt.Errorf(
			"sqldb: tx cleanup timeout must be > 0, got %s",
			o.txCleanupTimeout,
		)
	}
	return resolveConfiguredFamily(cfg, o)
}

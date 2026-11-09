package sqldb

import (
	"database/sql/driver"
	"log/slog"
	"time"

	"github.com/uptrace/bun/schema"
)

const defaultTxCleanupTimeout = 5 * time.Second

// Option configures an [Open] call.
type Option func(*options)

type options struct {
	dialect          schema.Dialect
	dialectSet       bool
	connector        driver.Connector
	connectorSet     bool
	txCleanupTimeout time.Duration
	logger           *slog.Logger
	optionalStart    bool
}

// WithLogger gives the DB a logger of its own for what [DB.Start] logs: the
// pool warnings and, with [WithOptionalStart], a failed ping. Without it the
// DB logs through the logger of the App that starts it, scoped to the
// database's component. Either way the records carry module=sqldb. A nil
// logger is ignored.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		o.logger = logger
	}
}

// WithOptionalStart makes a failed ping in [DB.Start] non-fatal: Start logs it
// at Warn, with the deadline and the error, logs the pool warnings as after a
// successful ping, and returns nil, leaving the pool open, since database/sql
// opens connections on demand once the server is back. [DB.Ready] keeps
// pinging, so the database gates readiness until it answers unless it is
// bound with credo.OptionalReadiness().
func WithOptionalStart() Option {
	return func(o *options) {
		o.optionalStart = true
	}
}

// WithTxCleanupTimeout sets how long Credo waits for each nested savepoint
// creation, release, or rollback and for the fail-safe ambient transaction
// abort. The default is 5 seconds. A driver operation that ignores context may
// continue in its goroutine, but the transaction is marked rollback-only and
// the caller stops waiting. d must be greater than zero; Open returns an error
// for invalid values.
func WithTxCleanupTimeout(d time.Duration) Option {
	return func(o *options) {
		o.txCleanupTimeout = d
	}
}

// WithDialect overrides the auto-detected dialect.
// Use this when the driver name does not match an exact known driver alias.
// Open rejects an explicitly nil dialect and a known dialect that conflicts
// with the configured known driver family.
func WithDialect(dialect schema.Dialect) Option {
	return func(o *options) {
		o.dialect = dialect
		o.dialectSet = true
	}
}

// WithConnector provides a custom driver.Connector, bypassing DSN-based
// connection creation. When set, Config.DSN and the DSN built from
// Config fields are ignored for sql.Open. Open rejects nil and typed-nil
// connectors.
func WithConnector(connector driver.Connector) Option {
	return func(o *options) {
		o.connector = connector
		o.connectorSet = true
	}
}

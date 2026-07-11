package sqldb

import (
	"database/sql/driver"
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

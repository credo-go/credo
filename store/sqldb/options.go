package sqldb

import (
	"database/sql/driver"

	"github.com/uptrace/bun/schema"
)

// Option configures an [Open] call.
type Option func(*options)

type options struct {
	dialect   schema.Dialect
	connector driver.Connector
}

// WithDialect overrides the auto-detected dialect.
// Use this when the driver name does not match a known dialect pattern.
func WithDialect(dialect schema.Dialect) Option {
	return func(o *options) {
		o.dialect = dialect
	}
}

// WithConnector provides a custom driver.Connector, bypassing DSN-based
// connection creation. When set, Config.DSN and the DSN built from
// Config fields are ignored for sql.Open.
func WithConnector(connector driver.Connector) Option {
	return func(o *options) {
		o.connector = connector
	}
}

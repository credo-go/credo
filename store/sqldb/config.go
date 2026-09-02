package sqldb

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds connection parameters for SQL databases.
type Config struct {
	// Driver is the SQL driver name (e.g., "postgres", "pgx", "mysql", "sqlite3").
	// This must match the driver registered via a blank import in the application.
	Driver string

	// Host is the database server hostname or IP address.
	Host string

	// Port is the database server port number. It must be between 1 and 65535
	// when Credo builds a PostgreSQL or MySQL DSN. Use DSN or WithConnector for
	// driver-specific default-port behavior.
	Port int

	// Name is the database name.
	Name string

	// User is the database user.
	User string

	// Password is the database password.
	Password string

	// DSN is an optional raw DSN string. When set, it is used as-is and Credo
	// does not merge Host, credentials, SSLMode, ConnectTimeout, or Options into
	// it.
	DSN string

	// ConnectTimeout is the maximum time to wait for a connection to be
	// established. Zero means no timeout. PostgreSQL DSNs represent this value
	// in whole seconds, so positive fractional seconds are rounded up.
	ConnectTimeout time.Duration

	// MaxOpen is the maximum number of open connections. Zero keeps the
	// database/sql default, which is unlimited.
	MaxOpen int

	// MaxIdle is the maximum number of idle connections. Nil makes Credo leave
	// the idle limit unset; the effective database/sql default remains subject
	// to MaxOpen. A non-nil zero disables idle connections.
	MaxIdle *int

	// MaxLifetime is the maximum lifetime of a connection. Zero disables the
	// lifetime limit.
	MaxLifetime time.Duration

	// MaxIdleTime is the maximum amount of time a connection may remain idle.
	// Zero disables the idle-time limit.
	MaxIdleTime time.Duration

	// SSLMode sets the driver-specific SSL/TLS mode. PostgreSQL receives it as
	// sslmode (for example, "require" or "verify-full"); MySQL receives it as
	// tls (for example, "true" or a registered TLS config name). Credo does not
	// impose a cross-driver TLS default.
	SSLMode string

	// Options holds additional driver-specific connection parameters. Core
	// PostgreSQL endpoint and credential keys are reserved, and an option may not
	// duplicate SSLMode or ConnectTimeout when the corresponding field is set.
	// MySQL parseTime is always true and cannot be overridden. Ambiguous sources
	// fail when Credo builds the DSN; use DSN for full driver-native control.
	Options map[string]string
}

// validateLimits rejects out-of-range pool and timeout settings.
func (c *Config) validateLimits() error {
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("sqldb: port must be between 0 and 65535, got %d", c.Port)
	}
	if c.ConnectTimeout < 0 {
		return fmt.Errorf("sqldb: connect timeout must be >= 0, got %s", c.ConnectTimeout)
	}
	if c.MaxOpen < 0 {
		return fmt.Errorf("sqldb: max open must be >= 0, got %d", c.MaxOpen)
	}
	if c.MaxIdle != nil && *c.MaxIdle < 0 {
		return fmt.Errorf("sqldb: max idle must be >= 0, got %d", *c.MaxIdle)
	}
	if c.MaxLifetime < 0 {
		return fmt.Errorf("sqldb: max lifetime must be >= 0, got %s", c.MaxLifetime)
	}
	if c.MaxIdleTime < 0 {
		return fmt.Errorf("sqldb: max idle time must be >= 0, got %s", c.MaxIdleTime)
	}
	if c.MaxOpen > 0 && c.MaxIdle != nil && *c.MaxIdle > c.MaxOpen {
		return fmt.Errorf(
			"sqldb: max idle (%d) must be <= max open (%d)",
			*c.MaxIdle,
			c.MaxOpen,
		)
	}
	return nil
}

// buildDSN constructs a DSN string from the config fields.
// If Config.DSN is set, it is returned as-is.
func (c *Config) buildDSN(family driverFamily) (string, error) {
	if c.DSN != "" {
		return c.DSN, nil
	}
	if err := c.validateStructuredDSN(family); err != nil {
		return "", err
	}

	switch family {
	case driverFamilyPostgres:
		return c.buildPostgresDSN(), nil
	case driverFamilyMySQL:
		return c.buildMySQLDSN(), nil
	case driverFamilySQLite:
		return c.buildSQLiteDSN(), nil
	default:
		return "", fmt.Errorf("sqldb: cannot build DSN for driver %q", c.Driver)
	}
}

func (c *Config) validateStructuredDSN(family driverFamily) error {
	switch family {
	case driverFamilyPostgres, driverFamilyMySQL:
		if c.Port < 1 || c.Port > 65535 {
			return fmt.Errorf(
				"sqldb: port must be between 1 and 65535 when building a network DSN, got %d",
				c.Port,
			)
		}
	}

	switch family {
	case driverFamilyPostgres:
		for _, key := range []string{"host", "port", "dbname", "user", "password"} {
			if _, exists := c.Options[key]; exists {
				return structuredOptionConflict(key)
			}
		}
		if c.SSLMode != "" {
			if _, exists := c.Options["sslmode"]; exists {
				return structuredOptionConflict("sslmode")
			}
		}
		if c.ConnectTimeout > 0 {
			if _, exists := c.Options["connect_timeout"]; exists {
				return structuredOptionConflict("connect_timeout")
			}
		}
	case driverFamilyMySQL:
		if _, exists := c.Options["parseTime"]; exists {
			return structuredOptionConflict("parseTime")
		}
		if c.SSLMode != "" {
			if _, exists := c.Options["tls"]; exists {
				return structuredOptionConflict("tls")
			}
		}
		if c.ConnectTimeout > 0 {
			if _, exists := c.Options["timeout"]; exists {
				return structuredOptionConflict("timeout")
			}
		}
	}

	return nil
}

func structuredOptionConflict(key string) error {
	return fmt.Errorf(
		"sqldb: option %q is reserved or conflicts with structured DSN configuration; use one source or Config.DSN",
		key,
	)
}

func (c *Config) buildPostgresDSN() string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:   c.Name,
	}
	if c.User != "" {
		if c.Password != "" {
			u.User = url.UserPassword(c.User, c.Password)
		} else {
			u.User = url.User(c.User)
		}
	}

	q := u.Query()
	if c.SSLMode != "" {
		q.Set("sslmode", c.SSLMode)
	}
	if c.ConnectTimeout > 0 {
		seconds := c.ConnectTimeout / time.Second
		if c.ConnectTimeout%time.Second != 0 {
			seconds++
		}
		q.Set("connect_timeout", strconv.FormatInt(int64(seconds), 10))
	}
	for k, v := range c.Options {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Config) buildMySQLDSN() string {
	// Format: user:password@tcp(host:port)/dbname?params
	//
	// User and password are written raw — exactly what go-sql-driver's
	// mysql.Config.FormatDSN does. The driver's DSN grammar parses
	// credentials as [first ':' .. last '@' before the last '/'], so a
	// password may contain '@', ':', '/', or '?' without escaping
	// (URL-encoding here would be WRONG: ParseDSN does not decode these
	// fields). Known grammar limit, shared with FormatDSN: a username
	// containing ':' is not representable. We deliberately do not import
	// go-sql-driver/mysql for FormatDSN — its init() would force-register
	// the mysql driver for every sqldb user, for byte-identical output.
	var b strings.Builder

	if c.User != "" {
		b.WriteString(c.User)
		if c.Password != "" {
			b.WriteByte(':')
			b.WriteString(c.Password)
		}
		b.WriteByte('@')
	}

	b.WriteString("tcp(")
	b.WriteString(net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	b.WriteByte(')')
	b.WriteByte('/')
	b.WriteString(c.Name)

	params := url.Values{}
	params.Set("parseTime", "true")
	if c.SSLMode != "" {
		params.Set("tls", c.SSLMode)
	}
	if c.ConnectTimeout > 0 {
		params.Set("timeout", c.ConnectTimeout.String())
	}
	for k, v := range c.Options {
		params.Set(k, v)
	}
	b.WriteByte('?')
	b.WriteString(params.Encode())
	return b.String()
}

func (c *Config) buildSQLiteDSN() string {
	if c.Name != "" {
		return c.Name
	}
	return ":memory:"
}

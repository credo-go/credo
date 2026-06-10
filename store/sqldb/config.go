package sqldb

import (
	"fmt"
	"net/url"
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

	// Port is the database server port number.
	Port int

	// Name is the database name.
	Name string

	// User is the database user.
	User string

	// Password is the database password.
	Password string

	// DSN is an optional raw DSN string. When set, Host, Port, Name,
	// User, and Password are ignored.
	DSN string

	// ConnectTimeout is the maximum time to wait for a connection
	// to be established. Zero means no timeout.
	ConnectTimeout time.Duration

	// MaxOpen is the maximum number of open connections (0 = unlimited).
	MaxOpen int

	// MaxIdle is the maximum number of idle connections.
	MaxIdle int

	// MaxLifetime is the maximum lifetime of a connection.
	MaxLifetime time.Duration

	// SSLMode sets the SSL/TLS mode (e.g., "disable", "require", "verify-full").
	SSLMode string

	// Options holds additional driver-specific connection parameters.
	Options map[string]string
}

// buildDSN constructs a DSN string from the config fields.
// If Config.DSN is set, it is returned as-is.
func (c *Config) buildDSN(family driverFamily) (string, error) {
	if c.DSN != "" {
		return c.DSN, nil
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

func (c *Config) buildPostgresDSN() string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   fmt.Sprintf("%s:%d", c.Host, c.Port),
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
		q.Set("connect_timeout", fmt.Sprintf("%d", int(c.ConnectTimeout.Seconds())))
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

	fmt.Fprintf(&b, "tcp(%s:%d)", c.Host, c.Port)
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

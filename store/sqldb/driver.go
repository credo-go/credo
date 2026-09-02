package sqldb

import (
	"fmt"
	"strings"

	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
)

type driverFamily uint8

const (
	driverFamilyUnknown driverFamily = iota
	driverFamilyPostgres
	driverFamilyMySQL
	driverFamilySQLite
)

func resolveDriverFamily(driver string) driverFamily {
	switch strings.ToLower(driver) {
	case "postgres", "pgx":
		return driverFamilyPostgres
	case "mysql":
		return driverFamilyMySQL
	case "sqlite", "sqlite3", "sqliteshim":
		return driverFamilySQLite
	default:
		return driverFamilyUnknown
	}
}

func (f driverFamily) dialect() schema.Dialect {
	switch f {
	case driverFamilyPostgres:
		return pgdialect.New()
	case driverFamilyMySQL:
		return mysqldialect.New()
	case driverFamilySQLite:
		return sqlitedialect.New()
	default:
		return nil
	}
}

func resolveDialectFamily(d schema.Dialect) driverFamily {
	if d == nil {
		return driverFamilyUnknown
	}
	switch d.Name() {
	case dialect.PG:
		return driverFamilyPostgres
	case dialect.MySQL:
		return driverFamilyMySQL
	case dialect.SQLite:
		return driverFamilySQLite
	default:
		return driverFamilyUnknown
	}
}

// validateDriverSelection rejects nil WithDialect/WithConnector values and a
// dialect that contradicts the configured driver family.
func validateDriverSelection(cfg *Config, o options) error {
	if o.dialectSet && isNilDynamicValue(o.dialect) {
		return fmt.Errorf("sqldb: WithDialect requires a non-nil dialect")
	}
	if o.connectorSet && isNilDynamicValue(o.connector) {
		return fmt.Errorf("sqldb: WithConnector requires a non-nil connector")
	}
	if o.dialectSet {
		family := resolveDriverFamily(cfg.Driver)
		dialectFamily := resolveDialectFamily(o.dialect)
		if family != driverFamilyUnknown &&
			dialectFamily != driverFamilyUnknown &&
			family != dialectFamily {
			return fmt.Errorf(
				"sqldb: WithDialect is incompatible with driver %q",
				cfg.Driver,
			)
		}
	}
	return nil
}

// resolveConfiguredFamily returns the driver family Open builds for. With
// WithConnector the family may stay unknown (the connector owns the
// connection); otherwise a driver is required and either a DSN or a known
// family must make the DSN buildable and the dialect detectable.
func resolveConfiguredFamily(cfg *Config, o options) (driverFamily, error) {
	family := resolveDriverFamily(cfg.Driver)
	if o.connectorSet {
		return family, nil
	}

	if cfg.Driver == "" {
		return driverFamilyUnknown, fmt.Errorf("sqldb: driver must be specified (or use WithConnector)")
	}

	if cfg.DSN == "" && family == driverFamilyUnknown {
		return driverFamilyUnknown, fmt.Errorf(
			"sqldb: cannot build DSN for driver %q; provide Config.DSN or use WithConnector",
			cfg.Driver,
		)
	}

	if o.dialect == nil && family == driverFamilyUnknown {
		return driverFamilyUnknown, fmt.Errorf(
			"sqldb: cannot detect dialect for driver %q; use WithDialect option",
			cfg.Driver,
		)
	}

	return family, nil
}

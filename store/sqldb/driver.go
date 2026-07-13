package sqldb

import (
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

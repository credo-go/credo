package sqldb

import (
	"strings"

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
	d := strings.ToLower(driver)
	switch {
	case strings.Contains(d, "postgres") || strings.Contains(d, "pgx"):
		return driverFamilyPostgres
	case strings.Contains(d, "mysql"):
		return driverFamilyMySQL
	case strings.Contains(d, "sqlite"):
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

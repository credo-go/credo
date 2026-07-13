package sqldb

import (
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/credo-go/credo/pagination"
)

// ErrTypedTerminalModel reports that One, All, or Page was called on a
// SelectQuery that already has a model bound to it. Typed terminals own their
// destination model, so overriding a pre-bound model would discard relation
// and model state silently.
var ErrTypedTerminalModel = errors.New("sqldb: typed terminal requires a model-less query")

// ErrInvalidLimitOffset reports a Limit or Offset value that Bun v1.2.18
// cannot represent without narrowing. The builder records this error and the
// next terminal returns it without executing a query.
var ErrInvalidLimitOffset = errors.New("sqldb: limit/offset is outside Bun v1.2.18 int32 range")

// ErrUnsupportedCountQuery reports a query shape that Count and Page cannot
// execute safely. Direct compound queries and HAVING without GROUP BY fail with
// this sentinel before I/O and must be restructured behind an outer derived
// table or CTE. Relation callbacks are rendered once and may not replace the
// model or add root ORDER/LIMIT/OFFSET/FOR or another unsupported count shape.
//
// MySQL validates derived-table output names at execution. When the generated
// logical COUNT returns ER_DUP_FIELDNAME (1060), Count and Page wrap this
// sentinel while preserving the original driver cause. That mapping is local
// to logical COUNT; the same MySQL error from any other operation passes through
// unchanged.
var ErrUnsupportedCountQuery = errors.New("sqldb: unsupported Count/Page query shape")

const mysqlErrDuplicateFieldName uint16 = 1060

func wrapMySQLCountExecutionError(family driverFamily, err error) error {
	if err == nil || family != driverFamilyMySQL ||
		extractMySQLErrNum(err) != mysqlErrDuplicateFieldName {
		return err
	}
	return fmt.Errorf(
		"%w: MySQL logical count returned ER_DUP_FIELDNAME (1060): %w",
		ErrUnsupportedCountQuery,
		err,
	)
}

// Bun v1.2.18 stores LIMIT and OFFSET in signed int32 fields even though its
// public methods accept int. Keep these bounds beside the curated builder and
// Page guards so an upgrade must deliberately re-evaluate the conversion
// contract.
const (
	minBunLimitOffset = int(-1 << 31)
	maxBunLimitOffset = int(1<<31 - 1)
)

func validateBunLimitOffset(name string, value int) error {
	if value < minBunLimitOffset || value > maxBunLimitOffset {
		return fmt.Errorf(
			"%w: %s=%d, allowed range [%d, %d]",
			ErrInvalidLimitOffset,
			name,
			value,
			minBunLimitOffset,
			maxBunLimitOffset,
		)
	}
	return nil
}

func typedTerminalModelError(terminal string) error {
	return fmt.Errorf(
		"%w: %s cannot override a model bound with Select, Model, or Apply; "+
			"use Model(&dest).Relation(...).Scan(ctx) for bound models and relations",
		ErrTypedTerminalModel,
		terminal,
	)
}

func validatedPageOffset(req pagination.PageRequest) (int, error) {
	offset, err := req.Offset()
	if err != nil {
		return 0, err
	}
	if req.PerPage > maxBunLimitOffset {
		return 0, fmt.Errorf(
			"%w: per_page exceeds Bun v1.2.18 maximum %d, got %d",
			pagination.ErrInvalidPageRequest,
			maxBunLimitOffset,
			req.PerPage,
		)
	}
	if offset > maxBunLimitOffset {
		return 0, fmt.Errorf(
			"%w: offset exceeds Bun v1.2.18 maximum %d for page=%d per_page=%d",
			pagination.ErrInvalidPageRequest,
			maxBunLimitOffset,
			req.Page,
			req.PerPage,
		)
	}
	return offset, nil
}

type countQueryShape struct {
	group    bool
	having   bool
	compound bool
}

func inspectCountQueryShape(raw *bun.SelectQuery) (countQueryShape, error) {
	if raw == nil {
		return countQueryShape{}, fmt.Errorf("sqldb: inspect count query shape: query must not be nil")
	}
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return countQueryShape{}, err
	}
	return countQueryShape{
		group:    bunSelectSliceLen(raw, layout.group) > 0,
		having:   bunSelectSliceLen(raw, layout.having) > 0,
		compound: bunSelectSliceLen(raw, layout.compound) > 0,
	}, nil
}

func validateCountQueryShape(raw *bun.SelectQuery) error {
	shape, err := inspectCountQueryShape(raw)
	if err != nil {
		return err
	}
	if shape.compound {
		return fmt.Errorf(
			"%w: direct UNION/INTERSECT/EXCEPT queries must be wrapped in an outer derived-table or CTE source",
			ErrUnsupportedCountQuery,
		)
	}
	if shape.having && !shape.group {
		return fmt.Errorf(
			"%w: HAVING without GROUP BY does not have a safe Bun v1.2.18 row-count contract",
			ErrUnsupportedCountQuery,
		)
	}
	return nil
}

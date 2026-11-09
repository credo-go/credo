package sqldb

import (
	"errors"
	"fmt"
)

// ErrTypedTerminalModel reports that One, All, Page, or PageWithCount was
// called on a SelectQuery that already has a model bound to it. Typed
// terminals own their destination model, so overriding a pre-bound model would
// discard relation and model state silently.
var ErrTypedTerminalModel = errors.New("sqldb: typed terminal requires a model-less query")

// ErrUnsupportedCountQuery reports a query whose total Count and Page cannot
// prove. They run Bun's native count only on a query built without Apply,
// Unwrap or a Relation callback, whose model's type does not implement
// bun.BeforeSelectHook, and that either carries GroupExpr or Distinct or has no
// raw ColumnExpr and no Having. Every other query fails with this sentinel
// before I/O; the message names the rule, its culprit, and the remedy:
// page the query with [SelectQuery.PageWithCount] and a count of your own.
//
// MySQL validates the output names of the count's derived source at execution.
// When the count returns ER_DUP_FIELDNAME (1060), Count and Page wrap this
// sentinel while preserving the original driver cause. That mapping is local
// to the count; the same MySQL error from any other operation passes through
// unchanged.
var ErrUnsupportedCountQuery = errors.New("sqldb: unsupported Count/Page query")

const mysqlErrDuplicateFieldName uint16 = 1060

func wrapMySQLCountExecutionError(family driverFamily, err error) error {
	if err == nil || family != driverFamilyMySQL ||
		extractMySQLErrNum(err) != mysqlErrDuplicateFieldName {
		return err
	}
	return fmt.Errorf(
		"%w: MySQL count returned ER_DUP_FIELDNAME (1060); give the projection's columns unique aliases: %w",
		ErrUnsupportedCountQuery,
		err,
	)
}

// unsupportedCountError names the terminal, the rule that made the query
// unknown with its culprit (reason), and the remedy.
func unsupportedCountError(terminal, reason string) error {
	return fmt.Errorf(
		"%w: %s cannot prove the total of this query: %s; count it with a query of your own and page it with PageWithCount",
		ErrUnsupportedCountQuery,
		terminal,
		reason,
	)
}

func typedTerminalModelError(terminal string) error {
	return fmt.Errorf(
		"%w: %s cannot override a model bound with Select, Model, or Apply; "+
			"use Model(&dest).Relation(...).Scan(ctx) for bound models and relations",
		ErrTypedTerminalModel,
		terminal,
	)
}

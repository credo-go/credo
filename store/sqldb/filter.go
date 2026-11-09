package sqldb

import "github.com/uptrace/bun"

// Filter adds predicates to the WHERE clause of the select, update or delete
// query that [SelectQuery.ApplyFilter], [UpdateQuery.ApplyFilter] or
// [DeleteQuery.ApplyFilter] runs it on, so one function scopes all three:
//
//	func tenantScope(id int64) func(*sqldb.Filter) {
//		return func(f *sqldb.Filter) { f.Where("tenant_id = ?", id) }
//	}
//
//	page, err := db.Select().ApplyFilter(tenantScope(id)).Page[Order](ctx, req)
//	_, err = db.Update((*Order)(nil)).Set("status = ?", s).ApplyFilter(tenantScope(id)).Exec(ctx)
//	_, err = db.Delete((*Order)(nil)).ApplyFilter(tenantScope(id)).Exec(ctx)
//
// Filter has the predicate methods and nothing else, so a callback cannot
// change a select's projection, sort or window: a query scoped through it
// keeps its automatic total in Count and Page. A callback that needs more than
// predicates uses the query's Apply. A Filter is valid only during the callback
// it is passed to.
type Filter struct {
	qb bun.QueryBuilder
}

// Where adds a WHERE condition joined with AND.
func (f *Filter) Where(query string, args ...any) *Filter {
	f.qb = f.qb.Where(query, args...)
	return f
}

// WhereOr adds a WHERE condition joined with OR.
func (f *Filter) WhereOr(query string, args ...any) *Filter {
	f.qb = f.qb.WhereOr(query, args...)
	return f
}

// WhereGroup adds the conditions fn adds to its Filter as one parenthesized
// group, joined to the preceding conditions with sep (" AND " or " OR ").
func (f *Filter) WhereGroup(sep string, fn func(*Filter)) *Filter {
	f.qb = f.qb.WhereGroup(sep, func(qb bun.QueryBuilder) bun.QueryBuilder {
		group := &Filter{qb: qb}
		if fn != nil {
			fn(group)
		}
		return group.qb
	})
	return f
}

// WherePK adds a WHERE condition on the model's primary key columns, or on
// cols when given.
func (f *Filter) WherePK(cols ...string) *Filter {
	f.qb = f.qb.WherePK(cols...)
	return f
}

// WhereDeleted limits a soft-delete model's query to the soft-deleted rows.
func (f *Filter) WhereDeleted() *Filter {
	f.qb = f.qb.WhereDeleted()
	return f
}

// WhereAllWithDeleted includes a soft-delete model's soft-deleted rows.
func (f *Filter) WhereAllWithDeleted() *Filter {
	f.qb = f.qb.WhereAllWithDeleted()
	return f
}

// applyFilter runs fn on a Filter over qb. Bun's query builders write through
// to the query they were taken from, so the conditions land on that query.
func applyFilter(qb bun.QueryBuilder, fn func(*Filter)) {
	if fn == nil {
		return
	}
	fn(&Filter{qb: qb})
}

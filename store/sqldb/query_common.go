package sqldb

import (
	"context"

	"github.com/uptrace/bun"
)

type queryState struct {
	db      *DB
	connSet bool
}

func newQueryState(db *DB) queryState {
	return queryState{db: db}
}

func (s *queryState) markConnSet() {
	s.connSet = true
}

func (s queryState) conn(ctx context.Context) bun.IDB {
	return s.db.conn(ctx)
}

type applicableQuery[P any] interface {
	Apply(...func(P) P) P
}

type connSetter[P any] interface {
	Conn(bun.IConn) P
}

func filterApply[P any](fns ...func(P) P) []func(P) P {
	filtered := make([]func(P) P, 0, len(fns))
	for _, fn := range fns {
		if fn != nil {
			filtered = append(filtered, fn)
		}
	}
	return filtered
}

func applyFiltered[P applicableQuery[P]](raw P, fns ...func(P) P) P {
	return raw.Apply(filterApply(fns...)...)
}

func prepareQuery[P connSetter[P]](ctx context.Context, raw P, state queryState, clone func(P) P) P {
	prepared := clone(raw)
	if !state.connSet {
		prepared = prepared.Conn(state.conn(ctx))
	}
	return prepared
}

func shallowCopy[P any](raw *P) *P {
	copied := *raw
	return &copied
}

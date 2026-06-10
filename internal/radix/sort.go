// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package radix

import (
	"cmp"
	"slices"
)

// Nodes is a sortable slice of Node pointers.
type Nodes[V any] []*Node[V]

// Sort orders nodes for correct routing precedence:
// Static > Regexp > Param > CatchAll
// Within the same type, longer prefixes come first.
//
// Each Children slot holds a single node type, so the type comparison below is
// normally a no-op; it stays as a guard in case a mixed slice is ever sorted.
func (ns Nodes[V]) Sort() {
	slices.SortFunc(ns, func(a, b *Node[V]) int {
		// Primary: node type (lower = higher priority)
		if c := cmp.Compare(a.Typ, b.Typ); c != 0 {
			return c
		}
		// Secondary: longer prefix first
		return cmp.Compare(len(b.Prefix), len(a.Prefix))
	})
}

// FindEdge returns the child node whose prefix starts with the given label byte.
func (ns Nodes[V]) FindEdge(label byte) *Node[V] {
	for _, n := range ns {
		if n.Label == label {
			return n
		}
	}
	return nil
}

// Package query is a generic, in-memory filter / sort / page engine for the
// operator's GraphQL collections. It is the normalized internal form of the
// nestjs-query convention: a recursive and/or filter of per-field comparisons,
// a multi-key sort, and offset paging. Each resolver supplies a [FieldMap] that
// extracts a normalized [Value] per filterable/sortable field; the GraphQL layer
// binds the typed *FieldComparison inputs down into [Comparison].
//
// Collections are small (manifest/runtime state, not a database), so the engine
// operates over a plain slice rather than translating to a query language.
package query

import "sort"

// Args is the bound form of the GraphQL (filter, sorting, paging) arguments.
type Args struct {
	Filter  Filter
	Sorting []Sort
	Paging  Paging
}

// Filter is a recursive AND/OR tree of per-field comparisons — the internal
// normalization of nestjs-query's <T>Filter { and, or, <field>: Comparison }.
// The zero Filter (no And/Or/Fields) matches everything: the "no filter" default.
type Filter struct {
	And    []Filter
	Or     []Filter
	Fields map[string]Comparison
}

// Comparison is one field's operators; a nil pointer / empty slice means unset.
// Every typed *FieldComparison input collapses into this single struct.
type Comparison struct {
	Is    *bool // boolean-field equality (and refine's only common null-ish op)
	IsNot *bool

	Eq  *Value
	Neq *Value
	Gt  *Value
	Gte *Value
	Lt  *Value
	Lte *Value

	Like     *string
	NotLike  *string
	ILike    *string
	NotILike *string

	In    []Value
	NotIn []Value
}

// Sort is one ordering key.
type Sort struct {
	Field     string
	Desc      bool // direction == DESC
	NullsLast bool // nulls == NULLS_LAST
}

// Paging is offset paging. Limit <= 0 means unbounded.
type Paging struct {
	Limit  int
	Offset int
}

// FieldMap exposes, per filterable/sortable field name, an accessor returning
// that field's normalized [Value]. Build values with [Str], [StrPtr], [Int],
// [IntPtr], and [Bool].
type FieldMap[T any] map[string]func(T) Value

// Apply filters, then sorts, then pages — returning the page and the pre-page
// total (what refine reads as totalCount). The input slice is not mutated.
func Apply[T any](items []T, a Args, fm FieldMap[T]) (page []T, total int) {
	matched := make([]T, 0, len(items))
	for _, it := range items {
		if matchFilter(a.Filter, it, fm) {
			matched = append(matched, it)
		}
	}
	if len(a.Sorting) > 0 {
		sortItems(matched, a.Sorting, fm)
	}
	total = len(matched)
	return pageItems(matched, a.Paging), total
}

// matchFilter reports whether item satisfies the filter tree. An empty filter
// matches everything; otherwise every And subfilter and every populated field
// comparison must hold, and (when present) at least one Or subfilter must hold.
func matchFilter[T any](f Filter, item T, fm FieldMap[T]) bool {
	for _, sub := range f.And {
		if !matchFilter(sub, item, fm) {
			return false
		}
	}
	if len(f.Or) > 0 {
		matched := false
		for _, sub := range f.Or {
			if matchFilter(sub, item, fm) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for field, cmp := range f.Fields {
		acc, ok := fm[field]
		if !ok {
			return false // unknown field: nothing matches (defensive)
		}
		if !matchComparison(cmp, acc(item)) {
			return false
		}
	}
	return true
}

func matchComparison(c Comparison, v Value) bool {
	if c.Is != nil && (v.Bool == nil || *v.Bool != *c.Is) {
		return false
	}
	if c.IsNot != nil && (v.Bool == nil || *v.Bool == *c.IsNot) {
		return false
	}
	if c.Eq != nil && !valueEqual(v, *c.Eq) {
		return false
	}
	if c.Neq != nil && valueEqual(v, *c.Neq) {
		return false
	}
	if c.Gt != nil && !cmpHolds(v, *c.Gt, func(o int) bool { return o > 0 }) {
		return false
	}
	if c.Gte != nil && !cmpHolds(v, *c.Gte, func(o int) bool { return o >= 0 }) {
		return false
	}
	if c.Lt != nil && !cmpHolds(v, *c.Lt, func(o int) bool { return o < 0 }) {
		return false
	}
	if c.Lte != nil && !cmpHolds(v, *c.Lte, func(o int) bool { return o <= 0 }) {
		return false
	}
	if c.Like != nil && !matchLike(v, *c.Like, true) {
		return false
	}
	if c.NotLike != nil && matchLike(v, *c.NotLike, true) {
		return false
	}
	if c.ILike != nil && !matchLike(v, *c.ILike, false) {
		return false
	}
	if c.NotILike != nil && matchLike(v, *c.NotILike, false) {
		return false
	}
	if c.In != nil && !valueIn(v, c.In) {
		return false
	}
	if c.NotIn != nil && valueIn(v, c.NotIn) {
		return false
	}
	return true
}

func sortItems[T any](items []T, sorts []Sort, fm FieldMap[T]) {
	sort.SliceStable(items, func(i, j int) bool {
		for _, s := range sorts {
			acc, ok := fm[s.Field]
			if !ok {
				continue
			}
			vi, vj := acc(items[i]), acc(items[j])
			ni, nj := vi.IsNull(), vj.IsNull()
			if ni || nj {
				if ni && nj {
					continue // both null: tie on this key
				}
				// Exactly one null. Null placement is absolute — it must not be
				// flipped by Desc, so it's decided here rather than via the
				// direction-sensitive comparison below.
				if s.NullsLast {
					return nj // non-null sorts before null
				}
				return ni // null sorts first
			}
			c, ok := compareNonNull(vi, vj)
			if !ok || c == 0 {
				continue
			}
			if s.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
}

func pageItems[T any](items []T, p Paging) []T {
	off := p.Offset
	if off < 0 {
		off = 0
	}
	if off >= len(items) {
		return []T{}
	}
	items = items[off:]
	if p.Limit > 0 && p.Limit < len(items) {
		items = items[:p.Limit]
	}
	return items
}

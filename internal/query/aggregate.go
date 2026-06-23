package query

import (
	"sort"
	"strings"
)

// KV is one group-key component or one numeric reducer result (stringified).
type KV struct {
	Field string
	Value string
}

// Group is one aggregation bucket: a group key plus the count and, for each
// requested numeric field, the min/max/sum over the group's members.
type Group struct {
	Key   []KV
	Count int
	Min   map[string]float64
	Max   map[string]float64
	Sum   map[string]float64
}

// NumericFieldMap exposes per-field numeric accessors for reducers. ok is false
// when the field is null/absent on a record, so it is skipped in min/max/sum.
type NumericFieldMap[T any] map[string]func(T) (float64, bool)

// Aggregate filters items (reusing the same engine as [Apply]), partitions the
// survivors by the stringified values of the groupBy fields, and computes the
// count plus min/max/sum of each reduce field per group. Null group-key values
// normalize to "" (matching the FieldMap "empty == absent" convention). Groups
// are returned sorted by key tuple for deterministic output.
func Aggregate[T any](items []T, f Filter, groupBy []string, fm FieldMap[T], nfm NumericFieldMap[T], reduce []string) []Group {
	type bucket struct {
		key   []KV
		count int
		min   map[string]float64
		max   map[string]float64
		sum   map[string]float64
	}
	buckets := map[string]*bucket{}
	var order []string

	for _, it := range items {
		if !matchFilter(f, it, fm) {
			continue
		}
		key := make([]KV, 0, len(groupBy))
		var ks strings.Builder
		for _, field := range groupBy {
			val := ""
			if acc, ok := fm[field]; ok {
				val = ValueString(acc(it))
			}
			key = append(key, KV{Field: field, Value: val})
			ks.WriteString(field)
			ks.WriteByte('=')
			ks.WriteString(val)
			ks.WriteByte(0)
		}
		id := ks.String()
		b, ok := buckets[id]
		if !ok {
			b = &bucket{key: key, min: map[string]float64{}, max: map[string]float64{}, sum: map[string]float64{}}
			buckets[id] = b
			order = append(order, id)
		}
		b.count++
		for _, field := range reduce {
			acc, ok := nfm[field]
			if !ok {
				continue
			}
			n, ok := acc(it)
			if !ok {
				continue
			}
			if _, seen := b.sum[field]; !seen {
				b.min[field], b.max[field], b.sum[field] = n, n, n
				continue
			}
			b.sum[field] += n
			if n < b.min[field] {
				b.min[field] = n
			}
			if n > b.max[field] {
				b.max[field] = n
			}
		}
	}

	sort.Strings(order)
	out := make([]Group, 0, len(order))
	for _, id := range order {
		b := buckets[id]
		g := Group{Key: b.key, Count: b.count}
		if len(b.min) > 0 {
			g.Min, g.Max, g.Sum = b.min, b.max, b.sum
		}
		out = append(out, g)
	}
	return out
}

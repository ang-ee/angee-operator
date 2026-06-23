package query

import (
	"reflect"
	"testing"
)

type rec struct {
	Name    string
	Runtime string
	Order   int
	Health  *string
	Ready   bool
}

func ptr[T any](v T) *T { return &v }

var recs = []rec{
	{Name: "api", Runtime: "container", Order: 2, Health: ptr("healthy"), Ready: true},
	{Name: "web", Runtime: "container", Order: 1, Health: ptr("starting"), Ready: false},
	{Name: "worker", Runtime: "local", Order: 3, Health: nil, Ready: true},
}

func fields() FieldMap[rec] {
	return FieldMap[rec]{
		"name":    func(r rec) Value { return Str(r.Name) },
		"runtime": func(r rec) Value { return Str(r.Runtime) },
		"order":   func(r rec) Value { return Int(r.Order) },
		"health":  func(r rec) Value { return StrPtr(r.Health) },
		"ready":   func(r rec) Value { return Bool(r.Ready) },
	}
}

func names(rs []rec) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

func TestApplyEmptyMatchesAll(t *testing.T) {
	page, total := Apply(recs, Args{}, fields())
	if total != 3 || len(page) != 3 {
		t.Fatalf("want 3 got total=%d page=%d", total, len(page))
	}
}

func TestFilterComparisons(t *testing.T) {
	tests := []struct {
		name string
		cmp  map[string]Comparison
		want []string
	}{
		{"eq", map[string]Comparison{"runtime": {Eq: ptr(Str("container"))}}, []string{"api", "web"}},
		{"neq", map[string]Comparison{"runtime": {Neq: ptr(Str("container"))}}, []string{"worker"}},
		{"in", map[string]Comparison{"name": {In: []Value{Str("api"), Str("worker")}}}, []string{"api", "worker"}},
		{"notIn", map[string]Comparison{"name": {NotIn: []Value{Str("api")}}}, []string{"web", "worker"}},
		{"like", map[string]Comparison{"name": {Like: ptr("w%")}}, []string{"web", "worker"}},
		{"iLike", map[string]Comparison{"name": {ILike: ptr("%API%")}}, []string{"api"}},
		{"gt-num", map[string]Comparison{"order": {Gt: ptr(Int(1))}}, []string{"api", "worker"}},
		{"lte-num", map[string]Comparison{"order": {Lte: ptr(Int(2))}}, []string{"api", "web"}},
		{"bool-is", map[string]Comparison{"ready": {Is: ptr(true)}}, []string{"api", "worker"}},
		{"bool-isNot", map[string]Comparison{"ready": {IsNot: ptr(true)}}, []string{"web"}},
		{"null-eq-skips", map[string]Comparison{"health": {Eq: ptr(Str("healthy"))}}, []string{"api"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, _ := Apply(recs, Args{Filter: Filter{Fields: tt.cmp}}, fields())
			if got := names(page); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("want %v got %v", tt.want, got)
			}
		})
	}
}

func TestFilterAndOr(t *testing.T) {
	// runtime == container AND order < 2  => web
	f := Filter{Fields: map[string]Comparison{
		"runtime": {Eq: ptr(Str("container"))},
		"order":   {Lt: ptr(Int(2))},
	}}
	page, _ := Apply(recs, Args{Filter: f}, fields())
	if got := names(page); !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("AND want [web] got %v", got)
	}

	// name == worker OR order == 1 => web, worker
	or := Filter{Or: []Filter{
		{Fields: map[string]Comparison{"name": {Eq: ptr(Str("worker"))}}},
		{Fields: map[string]Comparison{"order": {Eq: ptr(Int(1))}}},
	}}
	page, _ = Apply(recs, Args{Filter: or}, fields())
	if got := names(page); !reflect.DeepEqual(got, []string{"web", "worker"}) {
		t.Fatalf("OR want [web worker] got %v", got)
	}
}

func TestSorting(t *testing.T) {
	asc, _ := Apply(recs, Args{Sorting: []Sort{{Field: "order"}}}, fields())
	if got := names(asc); !reflect.DeepEqual(got, []string{"web", "api", "worker"}) {
		t.Fatalf("asc want [web api worker] got %v", got)
	}
	desc, _ := Apply(recs, Args{Sorting: []Sort{{Field: "name", Desc: true}}}, fields())
	if got := names(desc); !reflect.DeepEqual(got, []string{"worker", "web", "api"}) {
		t.Fatalf("desc want [worker web api] got %v", got)
	}
}

func TestSortingNulls(t *testing.T) {
	first, _ := Apply(recs, Args{Sorting: []Sort{{Field: "health"}}}, fields())
	if first[0].Name != "worker" { // null health sorts first by default
		t.Fatalf("nulls-first want worker first got %v", names(first))
	}
	last, _ := Apply(recs, Args{Sorting: []Sort{{Field: "health", NullsLast: true}}}, fields())
	if last[len(last)-1].Name != "worker" {
		t.Fatalf("nulls-last want worker last got %v", names(last))
	}
}

func TestSortingMultiKey(t *testing.T) {
	// runtime ASC, then name DESC: container group {web,api} ordered web>api,
	// then the local group {worker}.
	got, _ := Apply(recs, Args{Sorting: []Sort{
		{Field: "runtime"},
		{Field: "name", Desc: true},
	}}, fields())
	if names := names(got); !reflect.DeepEqual(names, []string{"web", "api", "worker"}) {
		t.Fatalf("multi-key want [web api worker] got %v", names)
	}
}

func TestSortingNullsWithDesc(t *testing.T) {
	// Null placement must be absolute, not flipped by Desc: descending health
	// orders starting>healthy, and the null-health row stays last.
	got, _ := Apply(recs, Args{Sorting: []Sort{
		{Field: "health", Desc: true, NullsLast: true},
	}}, fields())
	if names := names(got); !reflect.DeepEqual(names, []string{"web", "api", "worker"}) {
		t.Fatalf("desc+nullsLast want [web api worker] got %v", names)
	}
}

func TestPaging(t *testing.T) {
	sorted := Args{Sorting: []Sort{{Field: "name"}}}

	sorted.Paging = Paging{Limit: 2}
	page, total := Apply(recs, sorted, fields())
	if total != 3 || !reflect.DeepEqual(names(page), []string{"api", "web"}) {
		t.Fatalf("limit want total=3 [api web] got total=%d %v", total, names(page))
	}

	sorted.Paging = Paging{Limit: 2, Offset: 2}
	page, total = Apply(recs, sorted, fields())
	if total != 3 || !reflect.DeepEqual(names(page), []string{"worker"}) {
		t.Fatalf("offset want total=3 [worker] got total=%d %v", total, names(page))
	}

	sorted.Paging = Paging{Offset: 10}
	page, total = Apply(recs, sorted, fields())
	if total != 3 || len(page) != 0 {
		t.Fatalf("over-offset want total=3 empty got total=%d %v", total, names(page))
	}
}

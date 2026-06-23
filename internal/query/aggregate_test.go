package query

import "testing"

func TestAggregateGroupByCount(t *testing.T) {
	// recs: api(container), web(container), worker(local) -> container:2, local:1,
	// returned sorted by key tuple.
	groups := Aggregate(recs, Filter{}, []string{"runtime"}, fields(), nil, nil)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(groups), groups)
	}
	if groups[0].Key[0].Value != "container" || groups[0].Count != 2 {
		t.Fatalf("group0 = %+v, want container:2", groups[0])
	}
	if groups[1].Key[0].Value != "local" || groups[1].Count != 1 {
		t.Fatalf("group1 = %+v, want local:1", groups[1])
	}
}

func TestAggregateNumericReducers(t *testing.T) {
	nfm := NumericFieldMap[rec]{"order": func(r rec) (float64, bool) { return float64(r.Order), true }}
	groups := Aggregate(recs, Filter{}, []string{"runtime"}, fields(), nfm, []string{"order"})
	// container group orders: api=2, web=1 -> min 1, max 2, sum 3.
	g := groups[0]
	if g.Min["order"] != 1 || g.Max["order"] != 2 || g.Sum["order"] != 3 {
		t.Fatalf("container reducers = min:%v max:%v sum:%v", g.Min["order"], g.Max["order"], g.Sum["order"])
	}
}

func TestAggregateFilterScoped(t *testing.T) {
	// Only container services, grouped by ready -> false:1 (web), true:1 (api).
	f := Filter{Fields: map[string]Comparison{"runtime": {Eq: ptr(Str("container"))}}}
	groups := Aggregate(recs, f, []string{"ready"}, fields(), nil, nil)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(groups), groups)
	}
	for _, g := range groups {
		if g.Count != 1 {
			t.Fatalf("group %+v count = %d, want 1", g.Key, g.Count)
		}
	}
}

func TestAggregateNullKey(t *testing.T) {
	// worker has nil Health -> empty-string group key (the strOrNull convention).
	groups := Aggregate(recs, Filter{}, []string{"health"}, fields(), nil, nil)
	var nullGroup *Group
	for i := range groups {
		if groups[i].Key[0].Value == "" {
			nullGroup = &groups[i]
		}
	}
	if nullGroup == nil || nullGroup.Count != 1 {
		t.Fatalf("want one null-health group with count 1, got %+v", groups)
	}
}

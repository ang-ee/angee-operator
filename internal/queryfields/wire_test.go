package queryfields

import (
	"reflect"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/query"
)

func strp(s string) *string { return &s }

func TestToArgs(t *testing.T) {
	yes := true
	lq := api.ListQuery{
		Filter: &api.ListFilter{
			Fields: map[string]api.FieldComparison{
				"name":     {Eq: strp("api")},
				"hasValue": {Is: &yes},
			},
			Or: []api.ListFilter{{Fields: map[string]api.FieldComparison{"kind": {In: []string{"git", "local"}}}}},
		},
		Sorting: []api.ListSort{{Field: "name", Direction: "DESC", Nulls: "NULLS_LAST"}},
		Paging:  &api.ListPaging{Limit: 10, Offset: 20},
	}
	args := ToArgs(lq)

	if args.Paging.Limit != 10 || args.Paging.Offset != 20 {
		t.Fatalf("paging = %+v", args.Paging)
	}
	if len(args.Sorting) != 1 || args.Sorting[0].Field != "name" || !args.Sorting[0].Desc || !args.Sorting[0].NullsLast {
		t.Fatalf("sorting = %+v", args.Sorting)
	}
	if eq := args.Filter.Fields["name"].Eq; eq == nil || query.ValueString(*eq) != "api" {
		t.Fatalf("name eq = %+v", eq)
	}
	if is := args.Filter.Fields["hasValue"].Is; is == nil || !*is {
		t.Fatalf("hasValue is = %+v", is)
	}
	if len(args.Filter.Or) != 1 || len(args.Filter.Or[0].Fields["kind"].In) != 2 {
		t.Fatalf("or/in = %+v", args.Filter.Or)
	}
}

func TestFromArgsRoundTrip(t *testing.T) {
	// String/bool fields round-trip losslessly (the only filterable kinds today).
	lq := api.ListQuery{
		Filter:  &api.ListFilter{Fields: map[string]api.FieldComparison{"name": {Like: strp("w%")}}},
		Sorting: []api.ListSort{{Field: "name", Direction: "ASC"}},
		Paging:  &api.ListPaging{Limit: 5, Offset: 0},
	}
	back := FromArgs(ToArgs(lq))
	if !reflect.DeepEqual(back.Sorting, lq.Sorting) {
		t.Fatalf("sorting round-trip: %+v vs %+v", back.Sorting, lq.Sorting)
	}
	if back.Paging == nil || *back.Paging != *lq.Paging {
		t.Fatalf("paging round-trip: %+v vs %+v", back.Paging, lq.Paging)
	}
	if back.Filter == nil || back.Filter.Fields["name"].Like == nil || *back.Filter.Fields["name"].Like != "w%" {
		t.Fatalf("filter round-trip: %+v", back.Filter)
	}
}

func TestToArgsEmpty(t *testing.T) {
	args := ToArgs(api.ListQuery{})
	if len(args.Filter.Fields) != 0 || len(args.Sorting) != 0 || args.Paging.Limit != 0 || args.Paging.Offset != 0 {
		t.Fatalf("empty ListQuery should yield empty Args, got %+v", args)
	}
}

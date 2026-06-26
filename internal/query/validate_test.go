package query

import (
	"errors"
	"testing"
)

func TestValidateUnknownFilterField(t *testing.T) {
	err := Validate(Args{Filter: Filter{Fields: map[string]Comparison{"nope": {}}}}, fields())
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Kind != "filter" || fe.Field != "nope" {
		t.Fatalf("want filter FieldError for nope, got %v", err)
	}
}

func TestValidateUnknownNestedFilterField(t *testing.T) {
	f := Filter{Or: []Filter{{Fields: map[string]Comparison{"ghost": {}}}}}
	err := Validate(Args{Filter: f}, fields())
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "ghost" {
		t.Fatalf("want nested filter FieldError, got %v", err)
	}
}

func TestValidateUnknownSortField(t *testing.T) {
	err := Validate(Args{Sorting: []Sort{{Field: "nope"}}}, fields())
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Kind != "sort" {
		t.Fatalf("want sort FieldError, got %v", err)
	}
}

func TestValidateKnownFieldsOK(t *testing.T) {
	args := Args{
		Filter:  Filter{Fields: map[string]Comparison{"name": {}}},
		Sorting: []Sort{{Field: "order"}},
	}
	if err := Validate(args, fields()); err != nil {
		t.Fatalf("Validate(known) = %v, want nil", err)
	}
}

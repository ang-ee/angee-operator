package query

import "fmt"

// FieldError reports a filter or sort that references a field the FieldMap does
// not expose. Callers map it to a 400 / invalid-input at the surface layer.
type FieldError struct {
	Field string
	Kind  string // "filter" or "sort"
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("unknown %s field %q", e.Kind, e.Field)
}

// Validate checks that every field referenced by the filter tree and the sort
// keys exists in fm, returning a *FieldError on the first unknown field. An
// empty Args validates trivially. Run it before [Apply] so an unknown field is
// an explicit error rather than silently matching nothing / being skipped.
func Validate[T any](a Args, fm FieldMap[T]) error {
	if err := validateFilter(a.Filter, fm); err != nil {
		return err
	}
	for _, s := range a.Sorting {
		if _, ok := fm[s.Field]; !ok {
			return &FieldError{Field: s.Field, Kind: "sort"}
		}
	}
	return nil
}

func validateFilter[T any](f Filter, fm FieldMap[T]) error {
	for field := range f.Fields {
		if _, ok := fm[field]; !ok {
			return &FieldError{Field: field, Kind: "filter"}
		}
	}
	for _, sub := range f.And {
		if err := validateFilter(sub, fm); err != nil {
			return err
		}
	}
	for _, sub := range f.Or {
		if err := validateFilter(sub, fm); err != nil {
			return err
		}
	}
	return nil
}

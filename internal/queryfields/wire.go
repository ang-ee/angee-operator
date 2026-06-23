package queryfields

import (
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/query"
)

// ToArgs converts the REST wire query (api.ListQuery) into the engine form
// (query.Args). The empty ListQuery yields the match-all Args.
func ToArgs(q api.ListQuery) query.Args {
	args := query.Args{}
	if q.Filter != nil {
		args.Filter = toFilter(*q.Filter)
	}
	for _, s := range q.Sorting {
		args.Sorting = append(args.Sorting, query.Sort{
			Field:     s.Field,
			Desc:      strings.EqualFold(s.Direction, "DESC"),
			NullsLast: strings.EqualFold(s.Nulls, "NULLS_LAST"),
		})
	}
	if q.Paging != nil {
		args.Paging = query.Paging{Limit: q.Paging.Limit, Offset: q.Paging.Offset}
	}
	return args
}

// FromArgs converts the engine form back to the wire query, for the remote
// client to encode. The value-bearing operators (Eq/Neq/Gt/.../In) assume
// string Values — they round-trip losslessly today because every filter the
// GraphQL/REST layers build uses string comparisons (is/isNot bools are carried
// separately). A future numeric pushdown through the remote client would need a
// typed wire representation here rather than the stringified form.
func FromArgs(a query.Args) api.ListQuery {
	q := api.ListQuery{}
	if f := fromFilter(a.Filter); f != nil {
		q.Filter = f
	}
	for _, s := range a.Sorting {
		ls := api.ListSort{Field: s.Field, Direction: "ASC"}
		if s.Desc {
			ls.Direction = "DESC"
		}
		if s.NullsLast {
			ls.Nulls = "NULLS_LAST"
		}
		q.Sorting = append(q.Sorting, ls)
	}
	if a.Paging.Limit != 0 || a.Paging.Offset != 0 {
		q.Paging = &api.ListPaging{Limit: a.Paging.Limit, Offset: a.Paging.Offset}
	}
	return q
}

func toFilter(f api.ListFilter) query.Filter {
	out := query.Filter{}
	if len(f.Fields) > 0 {
		out.Fields = make(map[string]query.Comparison, len(f.Fields))
		for name, c := range f.Fields {
			out.Fields[name] = toComparison(c)
		}
	}
	for _, sub := range f.And {
		out.And = append(out.And, toFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, toFilter(sub))
	}
	return out
}

func toComparison(c api.FieldComparison) query.Comparison {
	return query.Comparison{
		Is:       c.Is,
		IsNot:    c.IsNot,
		Eq:       strVal(c.Eq),
		Neq:      strVal(c.Neq),
		Gt:       strVal(c.Gt),
		Gte:      strVal(c.Gte),
		Lt:       strVal(c.Lt),
		Lte:      strVal(c.Lte),
		Like:     c.Like,
		NotLike:  c.NotLike,
		ILike:    c.ILike,
		NotILike: c.NotILike,
		In:       strVals(c.In),
		NotIn:    strVals(c.NotIn),
	}
}

func fromFilter(f query.Filter) *api.ListFilter {
	if len(f.Fields) == 0 && len(f.And) == 0 && len(f.Or) == 0 {
		return nil
	}
	out := &api.ListFilter{}
	if len(f.Fields) > 0 {
		out.Fields = make(map[string]api.FieldComparison, len(f.Fields))
		for name, c := range f.Fields {
			out.Fields[name] = fromComparison(c)
		}
	}
	for _, sub := range f.And {
		if s := fromFilter(sub); s != nil {
			out.And = append(out.And, *s)
		}
	}
	for _, sub := range f.Or {
		if s := fromFilter(sub); s != nil {
			out.Or = append(out.Or, *s)
		}
	}
	return out
}

func fromComparison(c query.Comparison) api.FieldComparison {
	return api.FieldComparison{
		Is:       c.Is,
		IsNot:    c.IsNot,
		Eq:       valStr(c.Eq),
		Neq:      valStr(c.Neq),
		Gt:       valStr(c.Gt),
		Gte:      valStr(c.Gte),
		Lt:       valStr(c.Lt),
		Lte:      valStr(c.Lte),
		Like:     c.Like,
		NotLike:  c.NotLike,
		ILike:    c.ILike,
		NotILike: c.NotILike,
		In:       valStrs(c.In),
		NotIn:    valStrs(c.NotIn),
	}
}

func strVal(s *string) *query.Value {
	if s == nil {
		return nil
	}
	v := query.Str(*s)
	return &v
}

func strVals(ss []string) []query.Value {
	if len(ss) == 0 {
		return nil
	}
	out := make([]query.Value, len(ss))
	for i, s := range ss {
		out[i] = query.Str(s)
	}
	return out
}

func valStr(v *query.Value) *string {
	if v == nil {
		return nil
	}
	s := query.ValueString(*v)
	return &s
}

func valStrs(vs []query.Value) []string {
	if len(vs) == 0 {
		return nil
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = query.ValueString(v)
	}
	return out
}

package gql

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/operator/gql/model"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
)

// This file binds the Hasura-dialect arguments (where / order_by / limit /
// offset) and mutation inputs onto the generic internal/query engine and the
// service.API request DTOs. _not is intentionally unsupported: the
// @refinedev/hasura provider never emits it.

func pagingFrom(limit, offset *int) query.Paging {
	p := query.Paging{}
	if limit != nil {
		p.Limit = *limit
	}
	if offset != nil {
		p.Offset = *offset
	}
	return p
}

// --- comparison_exp -> query.Comparison --------------------------------------

func stringCmp(c *model.StringComparisonExp) query.Comparison {
	out := query.Comparison{
		IsNull:   c.IsNull,
		Eq:       strVal(c.Eq),
		Neq:      strVal(c.Neq),
		Gt:       strVal(c.Gt),
		Gte:      strVal(c.Gte),
		Lt:       strVal(c.Lt),
		Lte:      strVal(c.Lte),
		Like:     c.Like,
		NotLike:  c.Nlike,
		ILike:    c.Ilike,
		NotILike: c.Nilike,
		In:       strVals(c.In),
		NotIn:    strVals(c.Nin),
	}
	// _similar is mapped to LIKE. Exact only for the value% / %value patterns
	// refine emits for startswiths/endswiths; general SQL SIMILAR TO regex
	// syntax (| * + ( )) is NOT faithfully evaluated.
	if c.Similar != nil && out.Like == nil {
		out.Like = c.Similar
	}
	return out
}

func boolCmp(c *model.BooleanComparisonExp) query.Comparison {
	out := query.Comparison{IsNull: c.IsNull, In: boolVals(c.In), NotIn: boolVals(c.Nin)}
	if c.Eq != nil {
		v := query.Bool(*c.Eq)
		out.Eq = &v
	}
	if c.Neq != nil {
		v := query.Bool(*c.Neq)
		out.Neq = &v
	}
	return out
}

func intCmp(c *model.IntComparisonExp) query.Comparison {
	return query.Comparison{
		IsNull: c.IsNull,
		Eq:     intVal(c.Eq),
		Neq:    intVal(c.Neq),
		Gt:     intVal(c.Gt),
		Gte:    intVal(c.Gte),
		Lt:     intVal(c.Lt),
		Lte:    intVal(c.Lte),
		In:     intVals(c.In),
		NotIn:  intVals(c.Nin),
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

func intVal(i *int) *query.Value {
	if i == nil {
		return nil
	}
	v := query.Int(*i)
	return &v
}

func intVals(is []int) []query.Value {
	if len(is) == 0 {
		return nil
	}
	out := make([]query.Value, len(is))
	for i, n := range is {
		out[i] = query.Int(n)
	}
	return out
}

func boolVals(bs []bool) []query.Value {
	if len(bs) == 0 {
		return nil
	}
	out := make([]query.Value, len(bs))
	for i, b := range bs {
		out[i] = query.Bool(b)
	}
	return out
}

// --- order_by -> query.Sort --------------------------------------------------

func sortFrom(field string, ob *model.OrderBy) (query.Sort, bool) {
	if ob == nil {
		return query.Sort{}, false
	}
	s := query.Sort{Field: field}
	switch *ob {
	case model.OrderByAsc, model.OrderByAscNullsLast:
		s.Desc, s.NullsLast = false, true // Postgres ASC default = NULLS LAST
	case model.OrderByAscNullsFirst:
		s.Desc, s.NullsLast = false, false
	case model.OrderByDesc, model.OrderByDescNullsFirst:
		s.Desc, s.NullsLast = true, false // Postgres DESC default = NULLS FIRST
	case model.OrderByDescNullsLast:
		s.Desc, s.NullsLast = true, true
	}
	return s, true
}

// --- per-table where binders -------------------------------------------------

func bindServicesWhere(w *model.ServicesBoolExp) query.Filter {
	if w == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if w.ID != nil {
		out.Fields["id"] = stringCmp(w.ID)
	}
	if w.Name != nil {
		out.Fields["name"] = stringCmp(w.Name)
	}
	if w.Runtime != nil {
		out.Fields["runtime"] = stringCmp(w.Runtime)
	}
	if w.Status != nil {
		out.Fields["status"] = stringCmp(w.Status)
	}
	if w.Health != nil {
		out.Fields["health"] = stringCmp(w.Health)
	}
	for _, sub := range w.And {
		out.And = append(out.And, bindServicesWhere(sub))
	}
	for _, sub := range w.Or {
		out.Or = append(out.Or, bindServicesWhere(sub))
	}
	return out
}

func bindJobsWhere(w *model.JobsBoolExp) query.Filter {
	if w == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if w.ID != nil {
		out.Fields["id"] = stringCmp(w.ID)
	}
	if w.Name != nil {
		out.Fields["name"] = stringCmp(w.Name)
	}
	if w.Runtime != nil {
		out.Fields["runtime"] = stringCmp(w.Runtime)
	}
	for _, sub := range w.And {
		out.And = append(out.And, bindJobsWhere(sub))
	}
	for _, sub := range w.Or {
		out.Or = append(out.Or, bindJobsWhere(sub))
	}
	return out
}

func bindSourcesWhere(w *model.SourcesBoolExp) query.Filter {
	if w == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if w.ID != nil {
		out.Fields["id"] = stringCmp(w.ID)
	}
	if w.Name != nil {
		out.Fields["name"] = stringCmp(w.Name)
	}
	if w.Kind != nil {
		out.Fields["kind"] = stringCmp(w.Kind)
	}
	if w.State != nil {
		out.Fields["state"] = stringCmp(w.State)
	}
	if w.Branch != nil {
		out.Fields["branch"] = stringCmp(w.Branch)
	}
	if w.Exists != nil {
		out.Fields["exists"] = boolCmp(w.Exists)
	}
	if w.Dirty != nil {
		out.Fields["dirty"] = boolCmp(w.Dirty)
	}
	if w.Pushed != nil {
		out.Fields["pushed"] = boolCmp(w.Pushed)
	}
	if w.Ahead != nil {
		out.Fields["ahead"] = intCmp(w.Ahead)
	}
	if w.Behind != nil {
		out.Fields["behind"] = intCmp(w.Behind)
	}
	for _, sub := range w.And {
		out.And = append(out.And, bindSourcesWhere(sub))
	}
	for _, sub := range w.Or {
		out.Or = append(out.Or, bindSourcesWhere(sub))
	}
	return out
}

func bindWorkspacesWhere(w *model.WorkspacesBoolExp) query.Filter {
	if w == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if w.ID != nil {
		out.Fields["id"] = stringCmp(w.ID)
	}
	if w.Name != nil {
		out.Fields["name"] = stringCmp(w.Name)
	}
	if w.Template != nil {
		out.Fields["template"] = stringCmp(w.Template)
	}
	for _, sub := range w.And {
		out.And = append(out.And, bindWorkspacesWhere(sub))
	}
	for _, sub := range w.Or {
		out.Or = append(out.Or, bindWorkspacesWhere(sub))
	}
	return out
}

func bindTemplatesWhere(w *model.TemplatesBoolExp) query.Filter {
	if w == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if w.ID != nil {
		out.Fields["id"] = stringCmp(w.ID)
	}
	if w.Ref != nil {
		out.Fields["ref"] = stringCmp(w.Ref)
	}
	if w.Kind != nil {
		out.Fields["kind"] = stringCmp(w.Kind)
	}
	if w.Name != nil {
		out.Fields["name"] = stringCmp(w.Name)
	}
	for _, sub := range w.And {
		out.And = append(out.And, bindTemplatesWhere(sub))
	}
	for _, sub := range w.Or {
		out.Or = append(out.Or, bindTemplatesWhere(sub))
	}
	return out
}

func bindSecretsWhere(w *model.SecretsBoolExp) query.Filter {
	if w == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if w.ID != nil {
		out.Fields["id"] = stringCmp(w.ID)
	}
	if w.Name != nil {
		out.Fields["name"] = stringCmp(w.Name)
	}
	if w.EnvVar != nil {
		out.Fields["envVar"] = stringCmp(w.EnvVar)
	}
	if w.Declared != nil {
		out.Fields["declared"] = boolCmp(w.Declared)
	}
	if w.HasValue != nil {
		out.Fields["hasValue"] = boolCmp(w.HasValue)
	}
	if w.Required != nil {
		out.Fields["required"] = boolCmp(w.Required)
	}
	if w.Generated != nil {
		out.Fields["generated"] = boolCmp(w.Generated)
	}
	for _, sub := range w.And {
		out.And = append(out.And, bindSecretsWhere(sub))
	}
	for _, sub := range w.Or {
		out.Or = append(out.Or, bindSecretsWhere(sub))
	}
	return out
}

// --- per-table order_by binders ----------------------------------------------

func bindServicesOrderBy(in []*model.ServicesOrderBy) []query.Sort {
	var out []query.Sort
	for _, ob := range in {
		if ob == nil {
			continue
		}
		appendSort(&out, "id", ob.ID)
		appendSort(&out, "name", ob.Name)
		appendSort(&out, "runtime", ob.Runtime)
		appendSort(&out, "status", ob.Status)
		appendSort(&out, "health", ob.Health)
	}
	return out
}

func bindJobsOrderBy(in []*model.JobsOrderBy) []query.Sort {
	var out []query.Sort
	for _, ob := range in {
		if ob == nil {
			continue
		}
		appendSort(&out, "id", ob.ID)
		appendSort(&out, "name", ob.Name)
		appendSort(&out, "runtime", ob.Runtime)
	}
	return out
}

func bindSourcesOrderBy(in []*model.SourcesOrderBy) []query.Sort {
	var out []query.Sort
	for _, ob := range in {
		if ob == nil {
			continue
		}
		appendSort(&out, "id", ob.ID)
		appendSort(&out, "name", ob.Name)
		appendSort(&out, "kind", ob.Kind)
		appendSort(&out, "state", ob.State)
		appendSort(&out, "branch", ob.Branch)
		appendSort(&out, "ahead", ob.Ahead)
		appendSort(&out, "behind", ob.Behind)
	}
	return out
}

func bindWorkspacesOrderBy(in []*model.WorkspacesOrderBy) []query.Sort {
	var out []query.Sort
	for _, ob := range in {
		if ob == nil {
			continue
		}
		appendSort(&out, "id", ob.ID)
		appendSort(&out, "name", ob.Name)
		appendSort(&out, "template", ob.Template)
	}
	return out
}

func bindTemplatesOrderBy(in []*model.TemplatesOrderBy) []query.Sort {
	var out []query.Sort
	for _, ob := range in {
		if ob == nil {
			continue
		}
		appendSort(&out, "id", ob.ID)
		appendSort(&out, "ref", ob.Ref)
		appendSort(&out, "kind", ob.Kind)
		appendSort(&out, "name", ob.Name)
	}
	return out
}

func bindSecretsOrderBy(in []*model.SecretsOrderBy) []query.Sort {
	var out []query.Sort
	for _, ob := range in {
		if ob == nil {
			continue
		}
		appendSort(&out, "id", ob.ID)
		appendSort(&out, "name", ob.Name)
		appendSort(&out, "envVar", ob.EnvVar)
		appendSort(&out, "declared", ob.Declared)
		appendSort(&out, "hasValue", ob.HasValue)
	}
	return out
}

func appendSort(out *[]query.Sort, field string, ob *model.OrderBy) {
	if s, ok := sortFrom(field, ob); ok {
		*out = append(*out, s)
	}
}

// --- mutation input -> service.API request -----------------------------------

func serviceSetRequest(id string, set model.ServicesSetInput) api.ServiceInitRequest {
	return api.ServiceInitRequest{
		Name:    id,
		Runtime: stringPtrValue(set.Runtime),
		Image:   stringPtrValue(set.Image),
		Command: set.Command,
		Mounts:  set.Mounts,
		Env:     keyValuesFrom(set.Env),
		Ports:   set.Ports,
		Workdir: stringPtrValue(set.Workdir),
		Start:   boolPtrValue(set.Start),
	}
}

func workspaceInsertRequest(o model.WorkspacesInsertInput) api.WorkspaceCreateRequest {
	return api.WorkspaceCreateRequest{
		Template: o.Template,
		Name:     stringPtrValue(o.Name),
		Inputs:   keyValuesFrom(o.Inputs),
		TTL:      stringPtrValue(o.TTL),
	}
}

// --- sources aggregate numeric fields ----------------------------------------

func sourcesAggregateFields(total int, items []*api.SourceState) *model.SourcesAggregateFields {
	if total == 0 || len(items) == 0 {
		return &model.SourcesAggregateFields{Count: total}
	}
	rows := make([]api.SourceState, len(items))
	for i, it := range items {
		rows[i] = *it
	}
	groups := query.Aggregate(rows, query.Filter{}, nil, queryfields.Source, queryfields.SourceNumeric, []string{"ahead", "behind"})
	if len(groups) == 0 {
		return &model.SourcesAggregateFields{Count: total}
	}
	out := sourcesAggregateFromGroup(groups[0])
	out.Count = total // count reflects the full filtered set the caller measured
	return out
}

// aggInt truncates an aggregate (sum/min/max) back to an int column pointer.
func aggInt(f float64) *int {
	v := int(f)
	return &v
}

func aggFloat(f float64) *float64 { return &f }

// sourcesAggregateFromGroup builds the per-group sources aggregate (count plus
// the numeric reducers) from one engine group — shared by the ungrouped
// sources_aggregate and the grouped sources_groups roots.
func sourcesAggregateFromGroup(g query.Group) *model.SourcesAggregateFields {
	out := &model.SourcesAggregateFields{Count: g.Count}
	if g.Count == 0 || len(g.Sum) == 0 {
		return out
	}
	out.Sum = &model.SourcesSumFields{Ahead: aggInt(g.Sum["ahead"]), Behind: aggInt(g.Sum["behind"])}
	out.Min = &model.SourcesMinFields{Ahead: aggInt(g.Min["ahead"]), Behind: aggInt(g.Min["behind"])}
	out.Max = &model.SourcesMaxFields{Ahead: aggInt(g.Max["ahead"]), Behind: aggInt(g.Max["behind"])}
	n := float64(g.Count)
	out.Avg = &model.SourcesAvgFields{Ahead: aggFloat(g.Sum["ahead"] / n), Behind: aggFloat(g.Sum["behind"] / n)}
	return out
}

// --- NDC grouping helpers ----------------------------------------------------
// The <t>_groups roots mirror the strawberry-django-hasura grouping contract: a
// typed `key` (one field per selected dimension) paired with the FREE
// <t>_aggregate_fields aggregate. group_by specs, having (a predicate over the
// aggregate measures), group order_by, and offset paging are applied here over
// the materialized engine groups, keeping the queryfields FieldMaps out of the
// generated resolver file.

// specDimensions flattens a [<t>_group_by_spec] list to engine dimension field
// names (the UPPERCASE groupable-field enum value lowercased). A non-nil
// granularity is rejected: the operator has no date/numeric dimensions to
// bucket, matching strawberry-django-aggregates' GranularityNotApplicable.
func specDimensions[S any](specs []*S, field func(*S) string, gran func(*S) *model.Granularity) ([]string, error) {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		if s == nil {
			continue
		}
		if g := gran(s); g != nil {
			return nil, fmt.Errorf("granularity %q not applicable: the operator has no granular group-by dimensions", string(*g))
		}
		out = append(out, field(s))
	}
	return out, nil
}

func serviceGroups(items []api.ServiceState, where *model.ServicesBoolExp, specs []*model.ServicesGroupBySpec) ([]query.Group, error) {
	dims, err := specDimensions(specs,
		func(s *model.ServicesGroupBySpec) string { return strings.ToLower(string(s.Field)) },
		func(s *model.ServicesGroupBySpec) *model.Granularity { return s.Granularity })
	if err != nil {
		return nil, err
	}
	return query.Aggregate(items, bindServicesWhere(where), dims, queryfields.Service, nil, nil), nil
}

func sourceGroups(items []api.SourceState, where *model.SourcesBoolExp, specs []*model.SourcesGroupBySpec) ([]query.Group, error) {
	dims, err := specDimensions(specs,
		func(s *model.SourcesGroupBySpec) string { return strings.ToLower(string(s.Field)) },
		func(s *model.SourcesGroupBySpec) *model.Granularity { return s.Granularity })
	if err != nil {
		return nil, err
	}
	return query.Aggregate(items, bindSourcesWhere(where), dims, queryfields.Source, queryfields.SourceNumeric, []string{"ahead", "behind"}), nil
}

// --- typed group keys --------------------------------------------------------
// Only the selected dimensions are populated (one KV per group_by field); the
// rest stay nil. Engine values are strings (null normalized to ""), so the bool
// dimensions parse "true"/"false" back to *bool.

func servicesGroupKey(kvs []query.KV) *model.ServicesGroupKey {
	k := &model.ServicesGroupKey{}
	for _, kv := range kvs {
		v := kv.Value
		switch kv.Field {
		case "status":
			k.Status = &v
		case "runtime":
			k.Runtime = &v
		case "health":
			k.Health = &v
		}
	}
	return k
}

func sourcesGroupKey(kvs []query.KV) *model.SourcesGroupKey {
	k := &model.SourcesGroupKey{}
	for _, kv := range kvs {
		v := kv.Value
		switch kv.Field {
		case "kind":
			k.Kind = &v
		case "state":
			k.State = &v
		case "branch":
			k.Branch = &v
		case "dirty":
			b := v == "true"
			k.Dirty = &b
		case "pushed":
			b := v == "true"
			k.Pushed = &b
		}
	}
	return k
}

// servicesAggregateFromGroup is the free services aggregate for one group (count
// only); sourcesAggregateFromGroup adds the numeric reducers.
func servicesAggregateFromGroup(g query.Group) *model.ServicesAggregateFields {
	return &model.ServicesAggregateFields{Count: g.Count}
}

// --- having (predicate over the aggregate measures) --------------------------
// Each switch case fires only on a FAILING comparison and returns false; an
// unset (nil) comparison makes its case-guard false and is skipped, so a group
// passes when none of the set comparisons fail. A non-nil empty `in: []` matches
// nothing (SQL `IN ()` semantics); an unset (nil) `in` is a no-op (same for
// not_in).

func havingInt(v int, gt, lt, lte, gte, eq, neq *int, in, notIn []int) bool {
	switch {
	case gt != nil && v <= *gt:
		return false
	case lt != nil && v >= *lt:
		return false
	case lte != nil && v > *lte:
		return false
	case gte != nil && v < *gte:
		return false
	case eq != nil && v != *eq:
		return false
	case neq != nil && v == *neq:
		return false
	case in != nil && !slices.Contains(in, v):
		return false
	case notIn != nil && slices.Contains(notIn, v):
		return false
	}
	return true
}

// havingFloat mirrors havingInt for float measures (the `avg_*` reducers). Note
// `eq`/`neq`/`in`/`not_in` use exact float equality: against a divided average
// (e.g. 10/3) they will essentially never match. This matches SQL/Hasura HAVING
// semantics on a float aggregate; callers wanting a band should use gt/lt.
func havingFloat(v float64, gt, lt, lte, gte, eq, neq *float64, in, notIn []float64) bool {
	switch {
	case gt != nil && v <= *gt:
		return false
	case lt != nil && v >= *lt:
		return false
	case lte != nil && v > *lte:
		return false
	case gte != nil && v < *gte:
		return false
	case eq != nil && v != *eq:
		return false
	case neq != nil && v == *neq:
		return false
	case in != nil && !slices.Contains(in, v):
		return false
	case notIn != nil && slices.Contains(notIn, v):
		return false
	}
	return true
}

func servicesHavingOK(g query.Group, h *model.ServicesHaving) bool {
	if h == nil {
		return true
	}
	return havingInt(g.Count, h.CountGt, h.CountLt, h.CountLte, h.CountGte, h.CountEq, h.CountNeq, h.CountIn, h.CountNotIn)
}

func sourcesHavingOK(g query.Group, h *model.SourcesHaving) bool {
	if h == nil {
		return true
	}
	m := sourcesMeasures(g)
	return havingInt(int(m["count"]), h.CountGt, h.CountLt, h.CountLte, h.CountGte, h.CountEq, h.CountNeq, h.CountIn, h.CountNotIn) &&
		havingInt(int(m["sum_ahead"]), h.SumAheadGt, h.SumAheadLt, h.SumAheadLte, h.SumAheadGte, h.SumAheadEq, h.SumAheadNeq, h.SumAheadIn, h.SumAheadNotIn) &&
		havingInt(int(m["sum_behind"]), h.SumBehindGt, h.SumBehindLt, h.SumBehindLte, h.SumBehindGte, h.SumBehindEq, h.SumBehindNeq, h.SumBehindIn, h.SumBehindNotIn) &&
		havingFloat(m["avg_ahead"], h.AvgAheadGt, h.AvgAheadLt, h.AvgAheadLte, h.AvgAheadGte, h.AvgAheadEq, h.AvgAheadNeq, h.AvgAheadIn, h.AvgAheadNotIn) &&
		havingFloat(m["avg_behind"], h.AvgBehindGt, h.AvgBehindLt, h.AvgBehindLte, h.AvgBehindGte, h.AvgBehindEq, h.AvgBehindNeq, h.AvgBehindIn, h.AvgBehindNotIn) &&
		havingInt(int(m["min_ahead"]), h.MinAheadGt, h.MinAheadLt, h.MinAheadLte, h.MinAheadGte, h.MinAheadEq, h.MinAheadNeq, h.MinAheadIn, h.MinAheadNotIn) &&
		havingInt(int(m["min_behind"]), h.MinBehindGt, h.MinBehindLt, h.MinBehindLte, h.MinBehindGte, h.MinBehindEq, h.MinBehindNeq, h.MinBehindIn, h.MinBehindNotIn) &&
		havingInt(int(m["max_ahead"]), h.MaxAheadGt, h.MaxAheadLt, h.MaxAheadLte, h.MaxAheadGte, h.MaxAheadEq, h.MaxAheadNeq, h.MaxAheadIn, h.MaxAheadNotIn) &&
		havingInt(int(m["max_behind"]), h.MaxBehindGt, h.MaxBehindLt, h.MaxBehindLte, h.MaxBehindGte, h.MaxBehindEq, h.MaxBehindNeq, h.MaxBehindIn, h.MaxBehindNotIn)
}

// --- group measures + ordering + paging --------------------------------------

func servicesMeasures(g query.Group) map[string]float64 {
	return map[string]float64{"count": float64(g.Count)}
}

func sourcesMeasures(g query.Group) map[string]float64 {
	m := map[string]float64{
		"count":      float64(g.Count),
		"sum_ahead":  g.Sum["ahead"],
		"sum_behind": g.Sum["behind"],
		"min_ahead":  g.Min["ahead"],
		"min_behind": g.Min["behind"],
		"max_ahead":  g.Max["ahead"],
		"max_behind": g.Max["behind"],
	}
	if n := float64(g.Count); n > 0 {
		m["avg_ahead"] = g.Sum["ahead"] / n
		m["avg_behind"] = g.Sum["behind"] / n
	}
	return m
}

func isDescOrder(d *model.OrderBy) bool {
	if d == nil {
		return false
	}
	switch *d {
	case model.OrderByDesc, model.OrderByDescNullsFirst, model.OrderByDescNullsLast:
		return true
	}
	return false
}

func groupKeyValue(g query.Group, field string) (string, bool) {
	for _, kv := range g.Key {
		if kv.Field == field {
			return kv.Value, true
		}
	}
	return "", false
}

// orderGroups sorts groups in place by each (field, desc) term. A field naming
// an aggregate measure (count, sum_ahead, …) sorts numerically; a field naming a
// selected dimension sorts by its key value; an unknown field is a no-op term. A
// measure alias shadows a dimension of the same name (none collide today: the
// dimensions are status/runtime/health, kind/state/branch/dirty/pushed). The
// engine already returns groups in key-tuple order, so a stable sort keeps that
// as the tiebreaker (and the whole result deterministic with no order_by).
func orderGroups(groups []query.Group, fields []string, descs []bool, measures func(query.Group) map[string]float64) {
	if len(fields) == 0 {
		return
	}
	sort.SliceStable(groups, func(i, j int) bool {
		for t, f := range fields {
			if vi, ok := measures(groups[i])[f]; ok {
				vj := measures(groups[j])[f]
				if vi != vj {
					if descs[t] {
						return vi > vj
					}
					return vi < vj
				}
				continue
			}
			si, oki := groupKeyValue(groups[i], f)
			sj, _ := groupKeyValue(groups[j], f)
			if oki && si != sj {
				if descs[t] {
					return si > sj
				}
				return si < sj
			}
		}
		return false
	})
}

func groupOrderTerms[O any](orders []*O, get func(*O) (string, *model.OrderBy)) ([]string, []bool) {
	var fields []string
	var descs []bool
	for _, o := range orders {
		if o == nil {
			continue
		}
		f, d := get(o)
		fields = append(fields, f)
		descs = append(descs, isDescOrder(d))
	}
	return fields, descs
}

func sortServiceGroups(groups []query.Group, orders []*model.ServicesGroupOrder) {
	fields, descs := groupOrderTerms(orders, func(o *model.ServicesGroupOrder) (string, *model.OrderBy) { return o.Field, o.Direction })
	orderGroups(groups, fields, descs, servicesMeasures)
}

func sortSourceGroups(groups []query.Group, orders []*model.SourcesGroupOrder) {
	fields, descs := groupOrderTerms(orders, func(o *model.SourcesGroupOrder) (string, *model.OrderBy) { return o.Field, o.Direction })
	orderGroups(groups, fields, descs, sourcesMeasures)
}

func filterGroups(groups []query.Group, keep func(query.Group) bool) []query.Group {
	var out []query.Group
	for _, g := range groups {
		if keep(g) {
			out = append(out, g)
		}
	}
	return out
}

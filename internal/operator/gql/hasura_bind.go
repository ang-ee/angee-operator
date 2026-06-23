package gql

import (
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
	out := &model.SourcesAggregateFields{Count: total}
	if total == 0 {
		return out
	}
	rows := make([]api.SourceState, len(items))
	for i, it := range items {
		rows[i] = *it
	}
	groups := query.Aggregate(rows, query.Filter{}, nil, queryfields.Source, queryfields.SourceNumeric, []string{"ahead", "behind"})
	if len(groups) == 0 {
		return out
	}
	g := groups[0]
	out.Sum = &model.SourcesSumFields{Ahead: aggInt(g.Sum["ahead"]), Behind: aggInt(g.Sum["behind"])}
	out.Min = &model.SourcesMinFields{Ahead: aggInt(g.Min["ahead"]), Behind: aggInt(g.Min["behind"])}
	out.Max = &model.SourcesMaxFields{Ahead: aggInt(g.Max["ahead"]), Behind: aggInt(g.Max["behind"])}
	n := float64(g.Count)
	out.Avg = &model.SourcesAvgFields{Ahead: aggFloat(g.Sum["ahead"] / n), Behind: aggFloat(g.Sum["behind"] / n)}
	return out
}

// aggInt truncates an aggregate (sum/min/max) back to an int column pointer.
func aggInt(f float64) *int {
	v := int(f)
	return &v
}

func aggFloat(f float64) *float64 { return &f }

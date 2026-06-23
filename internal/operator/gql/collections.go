package gql

import (
	"context"
	"sort"
	"strconv"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/operator/gql/model"
	"github.com/ang-ee/angee-operator/internal/query"
)

// aggregateKeyValues renders an aggregate group key as KeyValue pairs.
func aggregateKeyValues(kvs []query.KV) []*model.KeyValue {
	out := make([]*model.KeyValue, len(kvs))
	for i, kv := range kvs {
		out[i] = &model.KeyValue{Key: kv.Field, Value: kv.Value}
	}
	return out
}

// numericKeyValues renders a numeric reducer map (min/max/sum) as KeyValue pairs
// in a deterministic field order; nil when no numeric reducers were computed.
func numericKeyValues(m map[string]float64) []*model.KeyValue {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*model.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, &model.KeyValue{Key: k, Value: strconv.FormatFloat(m[k], 'f', -1, 64)})
	}
	return out
}

func enumStrings[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

// This file binds the nestjs-query-shaped GraphQL collection arguments
// (filter / sorting / paging) onto the generic in-memory engine in
// internal/query, plus the per-entity FieldMaps and connection helpers. The
// list resolvers fetch the full slice from service.API and page it here.

// --- shared comparison / sort / paging binders -------------------------------

func stringCmp(c *model.StringFieldComparison) query.Comparison {
	return query.Comparison{
		Is:       c.Is,
		IsNot:    c.IsNot,
		Eq:       cmpVal(c.Eq),
		Neq:      cmpVal(c.Neq),
		Gt:       cmpVal(c.Gt),
		Gte:      cmpVal(c.Gte),
		Lt:       cmpVal(c.Lt),
		Lte:      cmpVal(c.Lte),
		Like:     c.Like,
		NotLike:  c.NotLike,
		ILike:    c.ILike,
		NotILike: c.NotILike,
		In:       cmpVals(c.In),
		NotIn:    cmpVals(c.NotIn),
	}
}

func boolCmp(c *model.BooleanFieldComparison) query.Comparison {
	return query.Comparison{Is: c.Is, IsNot: c.IsNot}
}

func cmpVal(s *string) *query.Value {
	if s == nil {
		return nil
	}
	v := query.Str(*s)
	return &v
}

func cmpVals(ss []string) []query.Value {
	if len(ss) == 0 {
		return nil
	}
	out := make([]query.Value, len(ss))
	for i, s := range ss {
		out[i] = query.Str(s)
	}
	return out
}

func bindSort(field string, dir model.SortDirection, nulls *model.SortNulls) query.Sort {
	s := query.Sort{Field: field, Desc: dir == model.SortDirectionDesc}
	if nulls != nil && *nulls == model.SortNullsNullsLast {
		s.NullsLast = true
	}
	return s
}

func bindPaging(p *model.OffsetPaging) query.Paging {
	if p == nil {
		return query.Paging{}
	}
	return query.Paging{Limit: intPtrValue(p.Limit), Offset: intPtrValue(p.Offset)}
}

func intPtrValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func offsetPageInfo(p query.Paging, total, pageLen int) *model.OffsetPageInfo {
	hasPrev := p.Offset > 0
	hasNext := p.Offset+pageLen < total
	return &model.OffsetPageInfo{HasNextPage: &hasNext, HasPreviousPage: &hasPrev}
}

// FieldMaps were moved to internal/queryfields so service.Platform applies the
// query (push-down); this file keeps only the gqlgen model -> query.Args binders.

// --- per-entity filter binders -----------------------------------------------

func bindServiceFilter(f *model.ServiceStateFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	if f.Runtime != nil {
		out.Fields["runtime"] = stringCmp(f.Runtime)
	}
	if f.Status != nil {
		out.Fields["status"] = stringCmp(f.Status)
	}
	if f.Health != nil {
		out.Fields["health"] = stringCmp(f.Health)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindServiceFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindServiceFilter(sub))
	}
	return out
}

func bindJobFilter(f *model.JobStateFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	if f.Runtime != nil {
		out.Fields["runtime"] = stringCmp(f.Runtime)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindJobFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindJobFilter(sub))
	}
	return out
}

func bindSourceFilter(f *model.SourceStateFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	if f.Kind != nil {
		out.Fields["kind"] = stringCmp(f.Kind)
	}
	if f.State != nil {
		out.Fields["state"] = stringCmp(f.State)
	}
	if f.Branch != nil {
		out.Fields["branch"] = stringCmp(f.Branch)
	}
	if f.Exists != nil {
		out.Fields["exists"] = boolCmp(f.Exists)
	}
	if f.Dirty != nil {
		out.Fields["dirty"] = boolCmp(f.Dirty)
	}
	if f.Pushed != nil {
		out.Fields["pushed"] = boolCmp(f.Pushed)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindSourceFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindSourceFilter(sub))
	}
	return out
}

func bindWorkspaceFilter(f *model.WorkspaceRefFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	if f.Template != nil {
		out.Fields["template"] = stringCmp(f.Template)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindWorkspaceFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindWorkspaceFilter(sub))
	}
	return out
}

func bindTemplateFilter(f *model.TemplateDescriptorFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Ref != nil {
		out.Fields["ref"] = stringCmp(f.Ref)
	}
	if f.Kind != nil {
		out.Fields["kind"] = stringCmp(f.Kind)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindTemplateFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindTemplateFilter(sub))
	}
	return out
}

func bindSecretFilter(f *model.SecretRefFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	if f.EnvVar != nil {
		out.Fields["envVar"] = stringCmp(f.EnvVar)
	}
	if f.Declared != nil {
		out.Fields["declared"] = boolCmp(f.Declared)
	}
	if f.HasValue != nil {
		out.Fields["hasValue"] = boolCmp(f.HasValue)
	}
	if f.Required != nil {
		out.Fields["required"] = boolCmp(f.Required)
	}
	if f.Generated != nil {
		out.Fields["generated"] = boolCmp(f.Generated)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindSecretFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindSecretFilter(sub))
	}
	return out
}

// --- per-entity sort binders -------------------------------------------------

func bindServiceSorts(in []*model.ServiceStateSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindJobSorts(in []*model.JobStateSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindSourceSorts(in []*model.SourceStateSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindWorkspaceSorts(in []*model.WorkspaceRefSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindTemplateSorts(in []*model.TemplateDescriptorSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindSecretSorts(in []*model.SecretRefSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

// --- relation binders --------------------------------------------------------

func bindGitOpsLinkFilter(f *model.GitOpsLinkFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Source != nil {
		out.Fields["source"] = stringCmp(f.Source)
	}
	if f.Workspace != nil {
		out.Fields["workspace"] = stringCmp(f.Workspace)
	}
	if f.Slot != nil {
		out.Fields["slot"] = stringCmp(f.Slot)
	}
	if f.Kind != nil {
		out.Fields["kind"] = stringCmp(f.Kind)
	}
	if f.Mode != nil {
		out.Fields["mode"] = stringCmp(f.Mode)
	}
	if f.State != nil {
		out.Fields["state"] = stringCmp(f.State)
	}
	if f.Branch != nil {
		out.Fields["branch"] = stringCmp(f.Branch)
	}
	if f.Dirty != nil {
		out.Fields["dirty"] = boolCmp(f.Dirty)
	}
	if f.Pushed != nil {
		out.Fields["pushed"] = boolCmp(f.Pushed)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindGitOpsLinkFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindGitOpsLinkFilter(sub))
	}
	return out
}

func bindGitOpsLinkSorts(in []*model.GitOpsLinkSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindWorkspaceStatusFilter(f *model.WorkspaceStatusFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.ID != nil {
		out.Fields["id"] = stringCmp(f.ID)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	if f.State != nil {
		out.Fields["state"] = stringCmp(f.State)
	}
	if f.Template != nil {
		out.Fields["template"] = stringCmp(f.Template)
	}
	if f.Exists != nil {
		out.Fields["exists"] = boolCmp(f.Exists)
	}
	if f.Expired != nil {
		out.Fields["expired"] = boolCmp(f.Expired)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindWorkspaceStatusFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindWorkspaceStatusFilter(sub))
	}
	return out
}

func bindWorkspaceStatusSorts(in []*model.WorkspaceStatusSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindWorkspaceSourceStatusFilter(f *model.WorkspaceSourceStatusFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.Slot != nil {
		out.Fields["slot"] = stringCmp(f.Slot)
	}
	if f.Source != nil {
		out.Fields["source"] = stringCmp(f.Source)
	}
	if f.Kind != nil {
		out.Fields["kind"] = stringCmp(f.Kind)
	}
	if f.Mode != nil {
		out.Fields["mode"] = stringCmp(f.Mode)
	}
	if f.State != nil {
		out.Fields["state"] = stringCmp(f.State)
	}
	if f.Branch != nil {
		out.Fields["branch"] = stringCmp(f.Branch)
	}
	if f.Dirty != nil {
		out.Fields["dirty"] = boolCmp(f.Dirty)
	}
	if f.Pushed != nil {
		out.Fields["pushed"] = boolCmp(f.Pushed)
	}
	if f.Exists != nil {
		out.Fields["exists"] = boolCmp(f.Exists)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindWorkspaceSourceStatusFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindWorkspaceSourceStatusFilter(sub))
	}
	return out
}

func bindWorkspaceSourceStatusSorts(in []*model.WorkspaceSourceStatusSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

func bindWorkspaceMountRefFilter(f *model.WorkspaceMountRefFilter) query.Filter {
	if f == nil {
		return query.Filter{}
	}
	out := query.Filter{Fields: map[string]query.Comparison{}}
	if f.Kind != nil {
		out.Fields["kind"] = stringCmp(f.Kind)
	}
	if f.Name != nil {
		out.Fields["name"] = stringCmp(f.Name)
	}
	if f.Field != nil {
		out.Fields["field"] = stringCmp(f.Field)
	}
	if f.Value != nil {
		out.Fields["value"] = stringCmp(f.Value)
	}
	for _, sub := range f.And {
		out.And = append(out.And, bindWorkspaceMountRefFilter(sub))
	}
	for _, sub := range f.Or {
		out.Or = append(out.Or, bindWorkspaceMountRefFilter(sub))
	}
	return out
}

func bindWorkspaceMountRefSorts(in []*model.WorkspaceMountRefSort) []query.Sort {
	out := make([]query.Sort, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, bindSort(string(s.Field), s.Direction, s.Nulls))
		}
	}
	return out
}

// --- single-entity lookups (no service.API getter exists for these) ----------

func (r *Resolver) serviceByID(ctx context.Context, id string) (*api.ServiceState, error) {
	items, _, err := r.Platform.ServiceList(ctx, query.Args{})
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Name == id {
			return &items[i], nil
		}
	}
	return nil, nil
}

func (r *Resolver) jobByID(ctx context.Context, id string) (*api.JobState, error) {
	items, _, err := r.Platform.JobList(ctx, query.Args{})
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Name == id {
			return &items[i], nil
		}
	}
	return nil, nil
}

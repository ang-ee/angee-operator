package gql

import (
	"context"
	"fmt"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/operator/gql/model"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/service"
)

func strptr(s string) *string { return &s }

// fakeAPI embeds service.API so any method a test does not override is left nil
// and panics if called — pinning down exactly the contract surface exercised.
type fakeAPI struct {
	service.API
	services   []api.ServiceState
	sources    []api.SourceState
	secrets    []api.SecretRef
	workspaces []api.WorkspaceRef
	files      map[string]api.FileContent // keyed by source+"/"+path
}

// ServiceList / SourceList / SecretsList apply the real engine so resolver tests
// exercise the same filter/sort/paging path the production Platform uses.
func (f fakeAPI) ServiceList(_ context.Context, q query.Args) ([]api.ServiceState, int, error) {
	page, total := query.Apply(f.services, q, queryfields.Service)
	return page, total, nil
}

func (f fakeAPI) SourceList(_ context.Context, q query.Args) ([]api.SourceState, int, error) {
	page, total := query.Apply(f.sources, q, queryfields.Source)
	return page, total, nil
}

func (f fakeAPI) SecretsList(_ context.Context, q query.Args) ([]api.SecretRef, int, error) {
	page, total := query.Apply(f.secrets, q, queryfields.Secret)
	return page, total, nil
}

// SecretGet mirrors the real backend: a zero-ish ref (no error) for an unknown
// name, so the resolver's own existence guard is what's under test.
func (f fakeAPI) SecretGet(_ context.Context, name string) (api.SecretRef, error) {
	for _, s := range f.secrets {
		if s.Name == name {
			return s, nil
		}
	}
	return api.SecretRef{Name: name}, nil
}

func (f fakeAPI) SecretDelete(context.Context, string) error { return nil }

func (f fakeAPI) WorkspaceCreate(_ context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceRef, error) {
	return api.WorkspaceRef{Name: req.Name, Template: req.Template}, nil
}

// WorkspaceGet errors for an unknown name (unlike SecretGet), which is what the
// delete_workspaces_by_pk not-found path relies on.
func (f fakeAPI) WorkspaceGet(_ context.Context, name string) (api.WorkspaceRef, error) {
	for _, w := range f.workspaces {
		if w.Name == name {
			return w, nil
		}
	}
	return api.WorkspaceRef{}, fmt.Errorf("workspace %q not found", name)
}

func (f fakeAPI) WorkspaceDestroy(context.Context, string, bool) error { return nil }

// FileRead returns canned content for a known source/path, or a typed
// *service.NotFoundError so the resolver's error passthrough is exercised.
func (f fakeAPI) FileRead(_ context.Context, source, path string) (api.FileContent, error) {
	if c, ok := f.files[source+"/"+path]; ok {
		return c, nil
	}
	return api.FileContent{}, &service.NotFoundError{Kind: "file", Name: path}
}

// FileWrite records the write (so a follow-up FileRead observes it) and echoes
// back a metadata-only ref with a fresh etag.
func (f fakeAPI) FileWrite(_ context.Context, source, path, content, etag string) (api.FileRef, error) {
	if f.files != nil {
		f.files[source+"/"+path] = api.FileContent{Path: path, Source: source, Content: content, Etag: "etag1"}
	}
	return api.FileRef{Path: path, Source: source, Etag: "etag1"}, nil
}

// TestServicesListFilter drives the Hasura where binding (status._eq) through the
// engine and back as a plain array.
func TestServicesListFilter(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{services: []api.ServiceState{
		{Name: "web", Runtime: "container", Status: "running"},
		{Name: "api", Runtime: "container", Status: "exited"},
	}}}
	eq := "running"
	where := &model.ServicesBoolExp{Status: &model.StringComparisonExp{Eq: &eq}}
	got, err := r.Query().Services(context.Background(), where, nil, nil, nil)
	if err != nil {
		t.Fatalf("Services() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("Services(status _eq running) = %#v, want [web]", got)
	}
}

// TestSecretsListSortPageAggregate covers where + order_by + limit on the list and
// the filtered count from _aggregate.
func TestSecretsListSortPageAggregate(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{secrets: []api.SecretRef{
		{Name: "db-password", Declared: true, HasValue: true},
		{Name: "api-key", Declared: true, HasValue: true},
		{Name: "unset-token", Declared: true, HasValue: false},
	}}}
	yes := true
	asc := model.OrderByAsc
	limit := 1
	where := &model.SecretsBoolExp{HasValue: &model.BooleanComparisonExp{Eq: &yes}}
	orderBy := []*model.SecretsOrderBy{{Name: &asc}}

	got, err := r.Query().Secrets(context.Background(), where, orderBy, &limit, nil)
	if err != nil {
		t.Fatalf("Secrets() error = %v", err)
	}
	// hasValue _eq true leaves 2; name asc -> api-key first; limit 1 -> [api-key].
	if len(got) != 1 || got[0].Name != "api-key" {
		t.Fatalf("Secrets = %#v, want [api-key]", got)
	}

	agg, err := r.Query().SecretsAggregate(context.Background(), where, nil, nil, nil)
	if err != nil {
		t.Fatalf("SecretsAggregate() error = %v", err)
	}
	if agg.Aggregate == nil || agg.Aggregate.Count != 2 {
		t.Fatalf("SecretsAggregate count = %#v, want 2", agg.Aggregate)
	}
}

// TestDeleteSecretsByPkNotFound verifies the existence guard.
func TestDeleteSecretsByPkNotFound(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{}}
	if _, err := r.Mutation().DeleteSecretsByPk(context.Background(), "ghost"); err == nil {
		t.Fatal("DeleteSecretsByPk(ghost) error = nil, want not-found")
	}
}

// TestFileQueryAndMutation drives the scalar file read query and fileWrite
// mutation resolvers, including the not-found passthrough and read-after-write.
func TestFileQueryAndMutation(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{files: map[string]api.FileContent{
		"app/config.yaml": {Path: "config.yaml", Source: "app", Content: "hello", Etag: "etag0"},
	}}}

	got, err := r.Query().File(context.Background(), "app", "config.yaml")
	if err != nil {
		t.Fatalf("File() error = %v", err)
	}
	if got == nil || got.Content != "hello" || got.Etag != "etag0" || got.Source != "app" {
		t.Fatalf("File = %#v, want hello/etag0/app", got)
	}

	// An unknown path surfaces the backend's not-found error unchanged.
	if _, err := r.Query().File(context.Background(), "app", "ghost"); err == nil {
		t.Fatal("File(ghost) error = nil, want not-found")
	}

	etag := "etag0"
	ref, err := r.Mutation().FileWrite(context.Background(), "app", "config.yaml", "updated", &etag)
	if err != nil {
		t.Fatalf("FileWrite() error = %v", err)
	}
	if ref == nil || ref.Path != "config.yaml" || ref.Source != "app" || ref.Etag != "etag1" {
		t.Fatalf("FileWrite = %#v, want config.yaml/app/etag1", ref)
	}

	// The mutation is observable through a subsequent read.
	after, err := r.Query().File(context.Background(), "app", "config.yaml")
	if err != nil {
		t.Fatalf("File() after write error = %v", err)
	}
	if after.Content != "updated" || after.Etag != "etag1" {
		t.Fatalf("post-write file = %#v, want updated/etag1", after)
	}
}

// TestServicesGroupsByStatus drives the typed-key grouped aggregation (the
// strawberry-django-hasura Option A shape): group by status, each group pairing
// a typed key (one field per selected dimension) with the free aggregate.
func TestServicesGroupsByStatus(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{services: []api.ServiceState{
		{Name: "a", Runtime: "container", Status: "running"},
		{Name: "b", Runtime: "container", Status: "running"},
		{Name: "c", Runtime: "local", Status: "exited"},
	}}}
	groups, err := r.Query().ServicesGroups(context.Background(),
		[]*model.ServicesGroupBySpec{{Field: model.ServicesGroupableFieldStatus}}, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("ServicesGroups() error = %v", err)
	}
	got := map[string]int{}
	for _, g := range groups {
		if g.Key == nil || g.Key.Status == nil {
			t.Fatalf("group key = %#v, want a typed status dimension", g.Key)
		}
		got[*g.Key.Status] = g.Aggregate.Count
	}
	if got["running"] != 2 || got["exited"] != 1 {
		t.Fatalf("grouped counts = %#v, want running:2 exited:1", got)
	}
}

// TestServicesGroupsHavingOrder covers the two new in-memory passes: having
// (a predicate over the aggregate measures) and order_by (sort groups by a
// measure alias). having count_gt:1 drops the single-member group; order_by
// count desc then sorts the survivors.
func TestServicesGroupsHavingOrder(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{services: []api.ServiceState{
		{Name: "a", Runtime: "container", Status: "running"},
		{Name: "b", Runtime: "container", Status: "running"},
		{Name: "c", Runtime: "container", Status: "exited"},
		{Name: "d", Runtime: "local", Status: "starting"},
	}}}
	one := 1
	desc := model.OrderByDesc
	groups, err := r.Query().ServicesGroups(context.Background(),
		[]*model.ServicesGroupBySpec{{Field: model.ServicesGroupableFieldStatus}},
		nil,
		&model.ServicesHaving{CountGt: &one},
		[]*model.ServicesGroupOrder{{Field: "count", Direction: &desc}},
		nil, nil)
	if err != nil {
		t.Fatalf("ServicesGroups() error = %v", err)
	}
	// exited (1) and starting (1) are filtered by count_gt:1; only running (2) survives.
	if len(groups) != 1 || groups[0].Key.Status == nil || *groups[0].Key.Status != "running" || groups[0].Aggregate.Count != 2 {
		t.Fatalf("having/order groups = %#v, want [running count 2]", groups)
	}
}

// TestServicesGroupsRejectGranularity: the operator has no granular dimensions,
// so a group_by spec carrying a granularity is rejected (matching upstream's
// GranularityNotApplicable) rather than silently ignored.
func TestServicesGroupsRejectGranularity(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{services: []api.ServiceState{
		{Name: "a", Runtime: "container", Status: "running"},
	}}}
	gran := model.GranularityMonth
	_, err := r.Query().ServicesGroups(context.Background(),
		[]*model.ServicesGroupBySpec{{Field: model.ServicesGroupableFieldStatus, Granularity: &gran}},
		nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("ServicesGroups(granularity=MONTH) error = nil, want not-applicable")
	}
}

// TestServicesGroupsEmpty: an empty group_by ([] is valid for [Spec!]!) collapses
// to a single all-items group with an all-nil typed key.
func TestServicesGroupsEmpty(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{services: []api.ServiceState{
		{Name: "a", Status: "running"}, {Name: "b", Status: "exited"},
	}}}
	groups, err := r.Query().ServicesGroups(context.Background(),
		[]*model.ServicesGroupBySpec{}, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("ServicesGroups([]) error = %v", err)
	}
	if len(groups) != 1 || groups[0].Aggregate.Count != 2 {
		t.Fatalf("empty group_by = %#v, want one group of count 2", groups)
	}
	if k := groups[0].Key; k.Status != nil || k.Runtime != nil || k.Health != nil {
		t.Fatalf("empty group_by key = %#v, want all-nil", k)
	}
}

// TestSourcesGroupsMeasures covers the numeric reducers (sum/avg/min/max) and
// order_by on a dimension key.
func TestSourcesGroupsMeasures(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{sources: []api.SourceState{
		{Name: "a", Kind: "git", Ahead: 1, Behind: 0},
		{Name: "b", Kind: "git", Ahead: 3, Behind: 2},
		{Name: "c", Kind: "local", Ahead: 10, Behind: 0},
	}}}
	asc := model.OrderByAsc
	groups, err := r.Query().SourcesGroups(context.Background(),
		[]*model.SourcesGroupBySpec{{Field: model.SourcesGroupableFieldKind}},
		nil, nil,
		[]*model.SourcesGroupOrder{{Field: "kind", Direction: &asc}}, // order_by on a dimension key
		nil, nil)
	if err != nil {
		t.Fatalf("SourcesGroups() error = %v", err)
	}
	if len(groups) != 2 || groups[0].Key.Kind == nil || *groups[0].Key.Kind != "git" || *groups[1].Key.Kind != "local" {
		t.Fatalf("groups = %#v, want [git, local] by kind asc", groups)
	}
	git := groups[0].Aggregate
	if git.Count != 2 || *git.Sum.Ahead != 4 || *git.Min.Ahead != 1 || *git.Max.Ahead != 3 || *git.Avg.Ahead != 2 {
		t.Fatalf("git aggregate = %#v, want count2 sum4 min1 max3 avg2", git)
	}
}

// TestSourcesGroupsHavingBoolKey covers having over a measure and a bool typed
// key (group by dirty -> *bool key).
func TestSourcesGroupsHavingBoolKey(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{sources: []api.SourceState{
		{Name: "a", Kind: "git", Dirty: false},
		{Name: "b", Kind: "git", Dirty: false},
		{Name: "c", Kind: "git", Dirty: true},
	}}}
	one := 1
	groups, err := r.Query().SourcesGroups(context.Background(),
		[]*model.SourcesGroupBySpec{{Field: model.SourcesGroupableFieldDirty}},
		nil,
		&model.SourcesHaving{CountGt: &one}, // drops the single dirty=true group
		nil, nil, nil)
	if err != nil {
		t.Fatalf("SourcesGroups() error = %v", err)
	}
	if len(groups) != 1 || groups[0].Key.Dirty == nil || *groups[0].Key.Dirty != false || groups[0].Aggregate.Count != 2 {
		t.Fatalf("having/bool-key groups = %#v, want [dirty=false count 2]", groups)
	}
}

// TestSecretsByPkNotFound: getOne on an unknown secret resolves to null, not a
// fabricated row (Hasura getOne semantics; SecretGet synthesizes a zero ref).
func TestSecretsByPkNotFound(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{}}
	got, err := r.Query().SecretsByPk(context.Background(), "ghost")
	if err != nil || got != nil {
		t.Fatalf("SecretsByPk(ghost) = (%#v, %v), want (nil, nil)", got, err)
	}
}

// TestWorkspaceInsertDelete covers insert_one and delete_by_pk (returns pre-delete
// row; missing workspace surfaces WorkspaceGet's not-found error).
func TestWorkspaceInsertDelete(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{workspaces: []api.WorkspaceRef{{Name: "feat", Template: "dev-pr"}}}}

	created, err := r.Mutation().InsertWorkspacesOne(context.Background(), model.WorkspacesInsertInput{Template: "dev-pr", Name: strptr("feat2")})
	if err != nil || created == nil || created.Name != "feat2" || created.Template != "dev-pr" {
		t.Fatalf("InsertWorkspacesOne = %#v, err = %v", created, err)
	}

	deleted, err := r.Mutation().DeleteWorkspacesByPk(context.Background(), "feat")
	if err != nil || deleted == nil || deleted.Name != "feat" {
		t.Fatalf("DeleteWorkspacesByPk = %#v, err = %v", deleted, err)
	}

	if _, err := r.Mutation().DeleteWorkspacesByPk(context.Background(), "ghost"); err == nil {
		t.Fatal("DeleteWorkspacesByPk(ghost) error = nil, want not-found")
	}
}

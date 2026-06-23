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
	secrets    []api.SecretRef
	workspaces []api.WorkspaceRef
}

// ServiceList / SecretsList apply the real engine so resolver tests exercise the
// same filter/sort/paging path the production Platform uses.
func (f fakeAPI) ServiceList(_ context.Context, q query.Args) ([]api.ServiceState, int, error) {
	page, total := query.Apply(f.services, q, queryfields.Service)
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

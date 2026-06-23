package gql

import (
	"context"
	"fmt"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/operator/gql/model"
	"github.com/ang-ee/angee-operator/internal/service"
)

func strptr(s string) *string { return &s }

// fakeAPI embeds service.API so that any method a test does not override is
// left nil and panics if called — letting a test pin down exactly the
// contract surface it exercises. This is the testability win of routing every
// adapter through service.API: resolvers and handlers can now be driven by a
// fake instead of a real *service.Platform backed by an on-disk stack.
type fakeAPI struct {
	service.API
	services   []api.ServiceState
	secrets    []api.SecretRef
	workspaces []api.WorkspaceRef
}

func (f fakeAPI) ServiceList(context.Context) ([]api.ServiceState, error) {
	return f.services, nil
}

func (f fakeAPI) SecretsList(context.Context) ([]api.SecretRef, error) {
	return f.secrets, nil
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

func (f fakeAPI) WorkspaceCreate(_ context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceRef, error) {
	return api.WorkspaceRef{Name: req.Name, Template: req.Template}, nil
}

// WorkspaceGet errors for an unknown name (unlike SecretGet), which is what the
// deleteOneWorkspace not-found path relies on.
func (f fakeAPI) WorkspaceGet(_ context.Context, name string) (api.WorkspaceRef, error) {
	for _, w := range f.workspaces {
		if w.Name == name {
			return w, nil
		}
	}
	return api.WorkspaceRef{}, fmt.Errorf("workspace %q not found", name)
}

func (f fakeAPI) WorkspaceDestroy(context.Context, string, bool) error { return nil }

func TestQueryServicesDispatchesThroughAPI(t *testing.T) {
	want := []api.ServiceState{{Name: "web", Runtime: "container", Status: "running"}}
	r := &Resolver{Platform: fakeAPI{services: want}}

	got, err := r.Query().Services(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("Services() error = %v", err)
	}
	if got.TotalCount != 1 || len(got.Nodes) != 1 || got.Nodes[0].Name != "web" || got.Nodes[0].Runtime != "container" {
		t.Fatalf("Services() = %#v, want one service web/container", got)
	}
}

// TestQuerySecretsFilterSortPage drives the whole nestjs-query binding path:
// model filter/sort/paging -> internal/query engine -> offset connection.
func TestQuerySecretsFilterSortPage(t *testing.T) {
	secrets := []api.SecretRef{
		{Name: "db-password", Declared: true, HasValue: true},
		{Name: "api-key", Declared: true, HasValue: true},
		{Name: "unset-token", Declared: true, HasValue: false},
	}
	r := &Resolver{Platform: fakeAPI{secrets: secrets}}

	yes := true
	filter := &model.SecretRefFilter{HasValue: &model.BooleanFieldComparison{Is: &yes}}
	sorting := []*model.SecretRefSort{{Field: model.SecretRefSortFieldsName, Direction: model.SortDirectionAsc}}
	limit := 1
	paging := &model.OffsetPaging{Limit: &limit}

	got, err := r.Query().Secrets(context.Background(), filter, sorting, paging)
	if err != nil {
		t.Fatalf("Secrets() error = %v", err)
	}
	// hasValue==true leaves 2 secrets; name-ASC orders api-key before db-password;
	// limit 1 returns the first page with hasNextPage true.
	if got.TotalCount != 2 {
		t.Fatalf("TotalCount = %d, want 2 (pre-page total)", got.TotalCount)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Name != "api-key" {
		t.Fatalf("Nodes = %#v, want [api-key]", got.Nodes)
	}
	if got.PageInfo == nil || got.PageInfo.HasNextPage == nil || !*got.PageInfo.HasNextPage {
		t.Fatalf("PageInfo.HasNextPage = %#v, want true", got.PageInfo)
	}
}

// TestDeleteOneSecretNotFound verifies the existence guard: deleting a name that
// was never declared and has no value is an error, not a fabricated success.
func TestDeleteOneSecretNotFound(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{}}
	if _, err := r.Mutation().DeleteOneSecret(context.Background(), model.DeleteOneSecretInput{ID: "ghost"}); err == nil {
		t.Fatal("DeleteOneSecret(ghost) error = nil, want not-found")
	}
}

// TestWorkspaceCRUD covers the nestjs-query createOne/deleteOne workspace path:
// create returns the new ref, delete returns the pre-delete ref, and deleting a
// missing workspace surfaces WorkspaceGet's not-found error.
func TestWorkspaceCRUD(t *testing.T) {
	r := &Resolver{Platform: fakeAPI{workspaces: []api.WorkspaceRef{{Name: "feat", Template: "dev-pr"}}}}

	created, err := r.Mutation().CreateOneWorkspace(context.Background(), model.CreateOneWorkspaceInput{
		Workspace: &model.WorkspaceCreateInput{Template: "dev-pr", Name: strptr("feat2")},
	})
	if err != nil || created == nil || created.Name != "feat2" || created.Template != "dev-pr" {
		t.Fatalf("CreateOneWorkspace = %#v, err = %v", created, err)
	}

	deleted, err := r.Mutation().DeleteOneWorkspace(context.Background(), model.DeleteOneWorkspaceInput{ID: "feat"})
	if err != nil || deleted == nil || deleted.Name != "feat" {
		t.Fatalf("DeleteOneWorkspace = %#v, err = %v", deleted, err)
	}

	if _, err := r.Mutation().DeleteOneWorkspace(context.Background(), model.DeleteOneWorkspaceInput{ID: "ghost"}); err == nil {
		t.Fatal("DeleteOneWorkspace(ghost) error = nil, want not-found")
	}
}

package gql

import (
	"context"
	"testing"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
)

// fakeAPI embeds service.API so that any method a test does not override is
// left nil and panics if called — letting a test pin down exactly the
// contract surface it exercises. This is the testability win of routing every
// adapter through service.API: resolvers and handlers can now be driven by a
// fake instead of a real *service.Platform backed by an on-disk stack.
type fakeAPI struct {
	service.API
	services []api.ServiceState
}

func (f fakeAPI) ServiceList(context.Context) ([]api.ServiceState, error) {
	return f.services, nil
}

func TestQueryServicesDispatchesThroughAPI(t *testing.T) {
	want := []api.ServiceState{{Name: "web", Runtime: "container", Status: "running"}}
	r := &Resolver{Platform: fakeAPI{services: want}}

	got, err := r.Query().Services(context.Background())
	if err != nil {
		t.Fatalf("Services() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "web" || got[0].Runtime != "container" {
		t.Fatalf("Services() = %#v, want one service web/container", got)
	}
}

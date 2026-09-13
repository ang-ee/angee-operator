package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/service"
)

type jobRunAPI struct {
	service.API
	name             string
	inputs           map[string]string
	chainedRestart   bool
	startedOperation api.JobRunOperation
	storedOperation  api.JobRunOperation
}

func (f *jobRunAPI) JobRunStart(_ context.Context, name string, inputs map[string]string, chainedRestart bool) (api.JobRunOperation, error) {
	f.name = name
	f.inputs = inputs
	f.chainedRestart = chainedRestart
	return f.startedOperation, nil
}

func (f *jobRunAPI) JobRunGet(_ context.Context, id string) (api.JobRunOperation, error) {
	if id != f.storedOperation.ID {
		return api.JobRunOperation{}, &service.NotFoundError{Kind: "job run", Name: id}
	}
	return f.storedOperation, nil
}

func TestOperatorJobRunReturnsAcceptedReceiptAndForwardsPlan(t *testing.T) {
	startedAt := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	fake := &jobRunAPI{startedOperation: api.JobRunOperation{
		ID:             "run-1",
		RootJob:        "codegen",
		ChainedRestart: true,
		Status:         api.JobRunPending,
		StartedAt:      startedAt,
		Nodes:          []api.JobRunNode{{Name: "codegen", Kind: "job", Status: api.JobRunPending}},
	}}
	server := &Server{platform: fake}
	req := httptest.NewRequest(http.MethodPost, "/jobs/codegen/run", strings.NewReader(`{"inputs":{"target":"web"},"chained_restart":true}`))
	req.SetPathValue("name", "codegen")
	res := httptest.NewRecorder()

	server.jobRun(res, req)

	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusAccepted, res.Body.String())
	}
	if fake.name != "codegen" || !fake.chainedRestart || !reflect.DeepEqual(fake.inputs, map[string]string{"target": "web"}) {
		t.Fatalf("forwarded request = name %q inputs %v chained %v", fake.name, fake.inputs, fake.chainedRestart)
	}
	var got api.JobRunOperation
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if !reflect.DeepEqual(got, fake.startedOperation) {
		t.Fatalf("response = %#v, want %#v", got, fake.startedOperation)
	}
}

func TestOperatorJobRunGetReturnsStoredOperation(t *testing.T) {
	startedAt := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	fake := &jobRunAPI{storedOperation: api.JobRunOperation{
		ID:        "run-1",
		RootJob:   "codegen",
		Status:    api.JobRunSucceeded,
		StartedAt: startedAt,
		Nodes:     []api.JobRunNode{{Name: "codegen", Kind: "job", Status: api.JobRunSucceeded}},
	}}
	server := &Server{platform: fake}
	req := httptest.NewRequest(http.MethodGet, "/job-runs/run-1", nil)
	req.SetPathValue("id", "run-1")
	res := httptest.NewRecorder()

	server.jobRunGet(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusOK, res.Body.String())
	}
	var got api.JobRunOperation
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if !reflect.DeepEqual(got, fake.storedOperation) {
		t.Fatalf("response = %#v, want %#v", got, fake.storedOperation)
	}
}

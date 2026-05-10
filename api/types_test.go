package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRequestDTOJSONRoundTrip(t *testing.T) {
	job := JobRunRequest{Inputs: map[string]string{"env": "dev"}}
	jobData, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Marshal(JobRunRequest) error = %v", err)
	}
	var decodedJob JobRunRequest
	if err := json.Unmarshal(jobData, &decodedJob); err != nil {
		t.Fatalf("Unmarshal(JobRunRequest) error = %v", err)
	}
	if !reflect.DeepEqual(decodedJob, job) {
		t.Fatalf("JobRunRequest round-trip = %#v, want %#v", decodedJob, job)
	}

	workspace := WorkspaceUpdateRequest{Inputs: map[string]string{"ticket": "123"}, TTL: "2h"}
	workspaceData, err := json.Marshal(workspace)
	if err != nil {
		t.Fatalf("Marshal(WorkspaceUpdateRequest) error = %v", err)
	}
	var decodedWorkspace WorkspaceUpdateRequest
	if err := json.Unmarshal(workspaceData, &decodedWorkspace); err != nil {
		t.Fatalf("Unmarshal(WorkspaceUpdateRequest) error = %v", err)
	}
	if !reflect.DeepEqual(decodedWorkspace, workspace) {
		t.Fatalf("WorkspaceUpdateRequest round-trip = %#v, want %#v", decodedWorkspace, workspace)
	}
}

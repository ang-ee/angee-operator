package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestRequestDTOJSONRoundTrip(t *testing.T) {
	job := JobRunRequest{Inputs: map[string]string{"env": "dev"}, ChainedRestart: true}
	jobData, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Marshal(JobRunRequest) error = %v", err)
	}
	if !bytes.Contains(jobData, []byte(`"chained_restart":true`)) {
		t.Fatalf("JobRunRequest JSON = %s, want snake_case chained_restart", jobData)
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

// The jj fields of a slot sit at the top level of every DTO that reports
// one, under the names the plan gives them: conflicted is always present, a
// flag beside state, and the others only when set, so a git slot's JSON
// gains only "conflicted": false. A source's jj workspaces are a list of
// {name, owned, path}, path only for an owned one. Each decodes back as it
// went out.
func TestJJSlotStateJSONShape(t *testing.T) {
	jj := JJSlotState{ChangeID: "kxyz", BaseBehind: 2, Conflicted: true, ConflictPaths: []string{"a.txt"}, UntrackedPaths: []string{"big.bin", "local/"}}
	wantSet := map[string]any{
		"change_id":       "kxyz",
		"base_behind":     float64(2),
		"conflicted":      true,
		"conflict_paths":  []any{"a.txt"},
		"untracked_paths": []any{"big.bin", "local/"},
	}
	jjKeys := []string{"change_id", "base_behind", "conflicted", "conflict_paths", "untracked_paths"}
	for _, tc := range []struct {
		name     string
		set, git any
	}{
		{name: "WorkspaceSourceStatus", set: WorkspaceSourceStatus{Slot: "work", State: "ahead", JJSlotState: jj}, git: WorkspaceSourceStatus{Slot: "tree", State: "clean"}},
		{name: "GitOpsLink", set: GitOpsLink{ID: "ws:work", State: "ahead", JJSlotState: jj}, git: GitOpsLink{ID: "ws:tree", State: "clean"}},
		{name: "SourceState", set: SourceState{Name: "app", Slot: "work", State: "ahead", JJSlotState: jj}, git: SourceState{Name: "app", Slot: "tree", State: "clean"}},
	} {
		got := marshalObject(t, tc.set)
		for _, key := range jjKeys {
			if !reflect.DeepEqual(got[key], wantSet[key]) {
				t.Errorf("%s JSON %q = %#v, want %#v", tc.name, key, got[key], wantSet[key])
			}
		}
		if _, nested := got["JJSlotState"]; nested || got["state"] != "ahead" {
			t.Errorf("%s JSON = %v, want the jj fields beside state, not nested", tc.name, got)
		}
		gitGot := marshalObject(t, tc.git)
		if gitGot["conflicted"] != false {
			t.Errorf("%s JSON of a git slot conflicted = %#v, want false", tc.name, gitGot["conflicted"])
		}
		for _, key := range []string{"change_id", "base_behind", "conflict_paths", "untracked_paths", "workspaces"} {
			if _, ok := gitGot[key]; ok {
				t.Errorf("%s JSON of a git slot has %q, want it omitted", tc.name, key)
			}
		}
	}
	roundTrip(t, WorkspaceSourceStatus{Slot: "work", State: "ahead", JJSlotState: jj})
	roundTrip(t, GitOpsLink{ID: "ws:work", State: "ahead", JJSlotState: jj})
	roundTrip(t, SourceState{Name: "app", Slot: "work", State: "ahead", JJSlotState: jj})

	source := SourceState{Name: "app", Kind: "git", Path: "/stack/sources/app", Exists: true, State: "clean", Pushed: true, Workspaces: []SourceWorkspace{
		{Name: "default"},
		{Name: "dev--ws", Owned: true, Path: "/stack/workspaces/ws/work"},
		{Name: "other--ws"},
	}}
	got := marshalObject(t, source)
	want := []any{
		map[string]any{"name": "default", "owned": false},
		map[string]any{"name": "dev--ws", "owned": true, "path": "/stack/workspaces/ws/work"},
		map[string]any{"name": "other--ws", "owned": false},
	}
	if !reflect.DeepEqual(got["workspaces"], want) {
		t.Errorf("SourceState workspaces JSON = %#v, want %#v", got["workspaces"], want)
	}
	roundTrip(t, source)

	summary := marshalObject(t, GitOpsSummary{Conflicted: 1})
	if summary["conflicted"] != float64(1) {
		t.Errorf("GitOpsSummary JSON = %v, want conflicted 1", summary)
	}
	if summary := marshalObject(t, GitOpsSummary{}); summary["conflicted"] != float64(0) {
		t.Errorf("GitOpsSummary JSON = %v, want conflicted 0 present", summary)
	}
}

// marshalObject encodes v and decodes it as a JSON object.
func marshalObject(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%T) error = %v", v, err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("Unmarshal(%T) error = %v", v, err)
	}
	return object
}

// roundTrip checks that v decodes back into its own type unchanged.
func roundTrip[T any](t *testing.T, v T) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%T) error = %v", v, err)
	}
	var decoded T
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal(%T) error = %v", v, err)
	}
	if !reflect.DeepEqual(decoded, v) {
		t.Errorf("%T round-trip = %#v, want %#v", v, decoded, v)
	}
}

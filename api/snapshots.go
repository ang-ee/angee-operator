package api

// SnapshotSourceEvidence pins recovery to native immutable VCS identities.
type SnapshotSourceEvidence struct {
	Policy       string            `json:"policy"`
	GitRevision  string            `json:"git_revision"`
	JJRevision   string            `json:"jj_revision,omitempty"`
	JJChangeID   string            `json:"jj_change_id,omitempty"`
	JJOperation  string            `json:"jj_operation,omitempty"`
	JJWorkspaces []string          `json:"jj_workspaces,omitempty"`
	Remotes      map[string]string `json:"remotes"`
}

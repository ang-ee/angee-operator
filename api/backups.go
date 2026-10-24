package api

// BackupSelection chooses only manifest-owned resources. Empty modes mean none.
type BackupSelection struct {
	Version      int                   `json:"version,omitempty"`
	Databases    BackupGroupSelection  `json:"databases"`
	ServiceState BackupGroupSelection  `json:"service_state"`
	Workspaces   BackupGroupSelection  `json:"workspaces"`
	Sources      BackupSourceSelection `json:"sources"`
	Storage      BackupGroupSelection  `json:"storage"`
}
type BackupGroupSelection struct {
	Mode    string   `json:"mode"`
	Names   []string `json:"names,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}
type BackupSourceSelection struct {
	BackupGroupSelection
	Policy string `json:"policy,omitempty"`
}
type BackupRequest struct {
	Version   int             `json:"version"`
	RequestID string          `json:"request_id,omitempty"`
	Selection BackupSelection `json:"selection"`
}
type BackupResource struct {
	CaptureJob     string `json:"capture_job,omitempty"`
	Group          string `json:"group"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	Path           string `json:"path,omitempty"`
	EstimatedBytes *int64 `json:"estimated_bytes"`
}
type BackupPreview struct {
	Version         int              `json:"version"`
	Resources       []BackupResource `json:"resources"`
	EstimatedBytes  int64            `json:"estimated_bytes"`
	HasUnknownSizes bool             `json:"has_unknown_sizes"`
}
type BackupEntry struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Mode      uint32 `json:"mode"`
}
type BackupManifest struct {
	Version    int                             `json:"version"`
	Stack      string                          `json:"stack"`
	Selection  BackupSelection                 `json:"selection"`
	Resources  []BackupResource                `json:"resources"`
	Entries    []BackupEntry                   `json:"entries"`
	Recovery   []string                        `json:"recovery"`
	SourceRefs map[string]BackupSourceEvidence `json:"source_refs"`
}
type BackupArtifact struct {
	Path            string         `json:"path"`
	SizeBytes       int64          `json:"size_bytes"`
	SHA256          string         `json:"sha256"`
	ManifestVersion int            `json:"manifest_version"`
	Manifest        BackupManifest `json:"manifest"`
}

// BackupSourceEvidence pins recovery to native immutable VCS identities.
type BackupSourceEvidence struct {
	Policy       string            `json:"policy"`
	GitRevision  string            `json:"git_revision"`
	JJRevision   string            `json:"jj_revision,omitempty"`
	JJChangeID   string            `json:"jj_change_id,omitempty"`
	JJOperation  string            `json:"jj_operation,omitempty"`
	JJWorkspaces []string          `json:"jj_workspaces,omitempty"`
	Remotes      map[string]string `json:"remotes"`
}

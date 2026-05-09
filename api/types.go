package api

import "time"

type OperationStatus string

const (
	OperationPending   OperationStatus = "pending"
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
)

type Operation struct {
	ID        string          `json:"id"`
	Status    OperationStatus `json:"status"`
	Message   string          `json:"message,omitempty"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
}

type StackInitRequest struct {
	Template string            `json:"template"`
	Path     string            `json:"path,omitempty"`
	Inputs   map[string]string `json:"inputs,omitempty"`
	Force    bool              `json:"force,omitempty"`
	Yes      bool              `json:"yes,omitempty"`
}

type StackPrepareRequest struct {
	Root string `json:"root,omitempty"`
}

type StackRuntimeRequest struct {
	Services []string `json:"services,omitempty"`
	Build    bool     `json:"build,omitempty"`
}

type StackStatusResponse struct {
	Root       string                  `json:"root"`
	Name       string                  `json:"name"`
	Services   map[string]ServiceState `json:"services,omitempty"`
	Jobs       map[string]JobState     `json:"jobs,omitempty"`
	Workspaces map[string]WorkspaceRef `json:"workspaces,omitempty"`
}

type ServiceState struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Status  string `json:"status"`
}

type JobState struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
}

type WorkspaceRef struct {
	Name         string         `json:"name"`
	Path         string         `json:"path"`
	Template     string         `json:"template"`
	ChainRoot    string         `json:"chain_root,omitempty"`
	Lifecycle    string         `json:"lifecycle,omitempty"`
	Allocations  map[string]int `json:"allocations,omitempty"`
	TTL          string         `json:"ttl,omitempty"`
	TTLExpiresAt *time.Time     `json:"ttl_expires_at,omitempty"`
}

type WorkspaceStatusResponse struct {
	Name         string                          `json:"name"`
	Path         string                          `json:"path"`
	Exists       bool                            `json:"exists"`
	State        string                          `json:"state"`
	Error        string                          `json:"error,omitempty"`
	Template     string                          `json:"template"`
	Inputs       map[string]string               `json:"inputs,omitempty"`
	Sources      []WorkspaceSourceStatus         `json:"sources,omitempty"`
	Chain        []string                        `json:"chain,omitempty"`
	ChainRoot    string                          `json:"chain_root,omitempty"`
	Lifecycle    string                          `json:"lifecycle,omitempty"`
	Allocations  map[string]int                  `json:"allocations,omitempty"`
	PersistPaths map[string]WorkspacePersistPath `json:"persist_paths,omitempty"`
	TTL          string                          `json:"ttl,omitempty"`
	TTLExpiresAt *time.Time                      `json:"ttl_expires_at,omitempty"`
	Expired      bool                            `json:"expired"`
	MountedBy    []WorkspaceMountRef             `json:"mounted_by,omitempty"`
	InnerStack   *StackStatusResponse            `json:"inner_stack,omitempty"`
	InnerError   string                          `json:"inner_error,omitempty"`
}

type WorkspaceSourceStatus struct {
	Slot           string `json:"slot"`
	Source         string `json:"source"`
	Kind           string `json:"kind"`
	Mode           string `json:"mode,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Ref            string `json:"ref,omitempty"`
	Subpath        string `json:"subpath,omitempty"`
	Path           string `json:"path"`
	Exists         bool   `json:"exists"`
	State          string `json:"state"`
	CurrentRef     string `json:"current_ref,omitempty"`
	Dirty          bool   `json:"dirty"`
	Upstream       string `json:"upstream,omitempty"`
	Ahead          int    `json:"ahead,omitempty"`
	Behind         int    `json:"behind,omitempty"`
	Pushed         bool   `json:"pushed"`
	UnpushedReason string `json:"unpushed_reason,omitempty"`
	Error          string `json:"error,omitempty"`
}

type WorkspacePersistPath struct {
	Subpath string `json:"subpath"`
	Scope   string `json:"scope"`
}

type WorkspaceMountRef struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Field string `json:"field"`
	Value string `json:"value"`
}

type ServiceInitRequest struct {
	Name    string            `json:"name"`
	Runtime string            `json:"runtime,omitempty"`
	Image   string            `json:"image,omitempty"`
	Command []string          `json:"command,omitempty"`
	Mounts  []string          `json:"mounts,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Ports   []string          `json:"ports,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Start   bool              `json:"start,omitempty"`
}

type WorkspaceCreateRequest struct {
	Template string            `json:"template"`
	Name     string            `json:"name,omitempty"`
	Inputs   map[string]string `json:"inputs,omitempty"`
	TTL      string            `json:"ttl,omitempty"`
	Start    bool              `json:"start,omitempty"`
}

type SourceOperationRequest struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
}

type SourceState struct {
	Name   string `json:"name"`
	Slot   string `json:"slot,omitempty"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Ref    string `json:"ref,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
	Error  string `json:"error,omitempty"`
}

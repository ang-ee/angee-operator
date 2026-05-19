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

type ErrorResponse struct {
	Kind   string `json:"kind,omitempty"`
	Name   string `json:"name,omitempty"`
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error"`
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

type JobRunRequest struct {
	Inputs map[string]string `json:"inputs,omitempty"`
}

type WorkspaceRef struct {
	Name               string         `json:"name"`
	Path               string         `json:"path"`
	Template           string         `json:"template"`
	ChainRoot          string         `json:"chain_root,omitempty"`
	Allocations        map[string]int `json:"allocations,omitempty"`
	ProcessComposePort int            `json:"process_compose_port,omitempty"`
	PlaywrightMCPName  string         `json:"playwright_mcp_name,omitempty"`
	PlaywrightMCPURL   string         `json:"playwright_mcp_url,omitempty"`
	TTL                string         `json:"ttl,omitempty"`
	TTLExpiresAt       *time.Time     `json:"ttl_expires_at,omitempty"`
}

type WorkspaceStatusResponse struct {
	Name               string                          `json:"name"`
	Path               string                          `json:"path"`
	Exists             bool                            `json:"exists"`
	State              string                          `json:"state"`
	Error              string                          `json:"error,omitempty"`
	Template           string                          `json:"template"`
	Inputs             map[string]string               `json:"inputs,omitempty"`
	Sources            []WorkspaceSourceStatus         `json:"sources,omitempty"`
	Chain              []string                        `json:"chain,omitempty"`
	ChainRoot          string                          `json:"chain_root,omitempty"`
	Allocations        map[string]int                  `json:"allocations,omitempty"`
	ProcessComposePort int                             `json:"process_compose_port,omitempty"`
	PlaywrightMCPName  string                          `json:"playwright_mcp_name,omitempty"`
	PlaywrightMCPURL   string                          `json:"playwright_mcp_url,omitempty"`
	PersistPaths       map[string]WorkspacePersistPath `json:"persist_paths,omitempty"`
	TTL                string                          `json:"ttl,omitempty"`
	TTLExpiresAt       *time.Time                      `json:"ttl_expires_at,omitempty"`
	Expired            bool                            `json:"expired"`
	MountedBy          []WorkspaceMountRef             `json:"mounted_by,omitempty"`
	InnerStack         *StackStatusResponse            `json:"inner_stack,omitempty"`
	InnerError         string                          `json:"inner_error,omitempty"`
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

type GitOpsTopologyResponse struct {
	Root       string                    `json:"root"`
	Name       string                    `json:"name"`
	Sources    []SourceState             `json:"sources"`
	Workspaces []WorkspaceStatusResponse `json:"workspaces"`
	Links      []GitOpsLink              `json:"links"`
	Summary    GitOpsSummary             `json:"summary"`
}

type GitOpsLink struct {
	ID             string `json:"id"`
	Source         string `json:"source"`
	Workspace      string `json:"workspace"`
	Slot           string `json:"slot"`
	Kind           string `json:"kind"`
	Mode           string `json:"mode,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Ref            string `json:"ref,omitempty"`
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

type GitOpsSummary struct {
	Sources        int `json:"sources"`
	Workspaces     int `json:"workspaces"`
	Worktrees      int `json:"worktrees"`
	Clean          int `json:"clean"`
	Dirty          int `json:"dirty"`
	Ahead          int `json:"ahead"`
	Behind         int `json:"behind"`
	Diverged       int `json:"diverged"`
	BranchMismatch int `json:"branch_mismatch"`
	Missing        int `json:"missing"`
	Error          int `json:"error"`
	Unpushed       int `json:"unpushed"`
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
}

// ServiceCreateRequest renders a single manifest.Service into the
// outer stack via a Copier template with `_angee.kind: service`.
// Workspace is required because service templates mount
// `workspace://<name>` paths from the named workspace.
type ServiceCreateRequest struct {
	Template  string            `json:"template"`
	Workspace string            `json:"workspace"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	Name      string            `json:"name,omitempty"`
	Start     bool              `json:"start,omitempty"`
}

type WorkspaceUpdateRequest struct {
	Inputs map[string]string `json:"inputs,omitempty"`
	TTL    string            `json:"ttl,omitempty"`
}

type PreflightFailure struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

type WorkspaceCreatePreflightResponse struct {
	OK               bool               `json:"ok"`
	Template         string             `json:"template"`
	ResolvedTemplate string             `json:"resolved_template"`
	EffectiveInputs  map[string]string  `json:"effective_inputs"`
	MissingRequired  []string           `json:"missing_required,omitempty"`
	InvalidInputs    []PreflightFailure `json:"invalid_inputs,omitempty"`
}

type TemplateInputDescriptor struct {
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Required  bool   `json:"required"`
	Immutable bool   `json:"immutable"`
	Generated bool   `json:"generated"`
	Default   string `json:"default,omitempty"`
}

type TemplateDescriptor struct {
	Ref    string                    `json:"ref"`
	Kind   string                    `json:"kind"`
	Name   string                    `json:"name,omitempty"`
	Path   string                    `json:"path"`
	Inputs []TemplateInputDescriptor `json:"inputs"`
}

type ConnectionTokenResponse struct {
	Token     string `json:"token"`
	Actor     string `json:"actor"`
	ExpiresAt string `json:"expires_at"`
}

type CommitRef struct {
	SHA     string   `json:"sha"`
	Parents []string `json:"parents"`
	Refs    []string `json:"refs"`
	Time    string   `json:"time"`
	Summary string   `json:"summary"`
	Author  string   `json:"author,omitempty"`
}

type DiffHunk struct {
	OldStart int    `json:"old_start"`
	OldLines int    `json:"old_lines"`
	NewStart int    `json:"new_start"`
	NewLines int    `json:"new_lines"`
	Header   string `json:"header,omitempty"`
	Body     string `json:"body"`
}

type GitOpResult struct {
	OK            bool                   `json:"ok"`
	Conflicted    bool                   `json:"conflicted"`
	ConflictFiles []string               `json:"conflict_files"`
	Message       string                 `json:"message"`
	Source        *WorkspaceSourceStatus `json:"source,omitempty"`
}

type DiffFile struct {
	OldPath   string     `json:"old_path,omitempty"`
	NewPath   string     `json:"new_path,omitempty"`
	Mode      string     `json:"mode,omitempty"`
	IsBinary  bool       `json:"is_binary"`
	IsNew     bool       `json:"is_new"`
	IsDeleted bool       `json:"is_deleted"`
	IsRename  bool       `json:"is_rename"`
	Hunks     []DiffHunk `json:"hunks"`
}

type SourceOperationRequest struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
}

// WorkspaceSourceGitOpRequest is the body for the REST convergence
// endpoints (merge / rebase / publish). `Ref` is the merge/rebase
// target; `Remote` and `Branch` only matter for `publish`.
type WorkspaceSourceGitOpRequest struct {
	Ref    string `json:"ref,omitempty"`
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// MintConnectionTokenRequest is the body for `POST /tokens/mint`.
// `Actor` is required and becomes the `sub` claim; `TTL` is a Go
// duration string capped at 24h.
type MintConnectionTokenRequest struct {
	Actor string `json:"actor"`
	TTL   string `json:"ttl,omitempty"`
}

// SecretRef is metadata about a secret — never includes the value.
// `Declared` is true when the secret is declared in `stack.secrets`;
// `HasValue` reflects whether the configured backend currently holds a
// value for the name.
type SecretRef struct {
	Name      string `json:"name"`
	Declared  bool   `json:"declared"`
	HasValue  bool   `json:"has_value"`
	Required  bool   `json:"required,omitempty"`
	Generated bool   `json:"generated,omitempty"`
	Import    string `json:"import,omitempty"`
	EnvVar    string `json:"env_var,omitempty"`
}

// SecretValueResponse carries the resolved value. Returned only by the
// dedicated value-read endpoint so the privileged read is obvious in
// every audit trail and code review.
type SecretValueResponse struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SecretSetRequest is the body of `POST /secrets/{name}` and the
// `secretSet` mutation. Value is required and stored verbatim.
type SecretSetRequest struct {
	Value string `json:"value"`
}

type WorkspaceSyncBaseRequest struct {
	Method string `json:"method,omitempty"`
}

type SourceState struct {
	Name           string      `json:"name"`
	Slot           string      `json:"slot,omitempty"`
	Kind           string      `json:"kind"`
	Path           string      `json:"path"`
	Exists         bool        `json:"exists"`
	State          string      `json:"state,omitempty"`
	Branch         string      `json:"branch,omitempty"`
	Ref            string      `json:"ref,omitempty"`
	CurrentRef     string      `json:"current_ref,omitempty"`
	Dirty          bool        `json:"dirty,omitempty"`
	Upstream       string      `json:"upstream,omitempty"`
	Ahead          int         `json:"ahead,omitempty"`
	Commits        []CommitRef `json:"commits,omitempty"`
	Behind         int         `json:"behind,omitempty"`
	Pushed         bool        `json:"pushed"`
	UnpushedReason string      `json:"unpushed_reason,omitempty"`
	Error          string      `json:"error,omitempty"`
}

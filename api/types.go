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

// ErrorResponse is a REST error body: the message and the error contract's
// keys, shared with GraphQL extensions (docs/reference/operator-api.md,
// "Errors").
type ErrorResponse struct {
	Error     string   `json:"error"`
	Code      string   `json:"code,omitempty"`
	Cause     string   `json:"cause,omitempty"`
	Operation string   `json:"operation,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Slot      string   `json:"slot,omitempty"`
	Source    string   `json:"source,omitempty"`
	Remote    string   `json:"remote,omitempty"`
	Job       string   `json:"job,omitempty"`
	Service   string   `json:"service,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Hint      string   `json:"hint,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	RequestID string   `json:"request_id,omitempty"`
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
	// Summary counts the services and jobs above by observed outcome.
	Summary StackSummary `json:"summary"`
}

// StackSummary counts a stack's services and jobs by the outcome its runtime
// backends report.
type StackSummary struct {
	Services StatusCounts `json:"services"`
	Jobs     StatusCounts `json:"jobs"`
}

// StatusCounts counts declared entries by outcome. Total counts every declared
// entry; pending, stopped, never-run and unobserved entries count only there.
// Failed counts entries that exited non-zero or could not start; a service
// stopped by a signal (`angee stop`, Ctrl-C) is stopped, not failed.
// Restarting counts services the runtime is restarting after they exited.
// Skipped counts entries the runtime skipped because a dependency failed.
type StatusCounts struct {
	Total      int `json:"total"`
	Running    int `json:"running"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	Restarting int `json:"restarting"`
	Skipped    int `json:"skipped"`
}

type ServiceState struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	// Status is the runtime's own state, such as process-compose's
	// `Completed` or docker compose's `exited`. Outcome is the verdict.
	Status string `json:"status"`
	// Outcome classifies the runtime state as the stack summary counts it:
	// running, completed (exit 0), failed (a non-zero exit, or the command
	// could not start), restarting (it exited and the runtime's restart
	// policy is starting it again), stopped (ended by a stop signal), skipped
	// (a dependency failed), pending, never-run (the runtime holds no state
	// for it) or unknown (the runtime could not be queried). Empty when the
	// runtime was not read, as in a create or update response.
	//
	// A local service killed by any signal reads as stopped, not failed:
	// process-compose reports exit code -1 alike for a crash by signal (such as
	// SIGSEGV), a SIGKILL from outside (such as the OOM killer) and a
	// deliberate stop, so the outcome cannot tell them apart.
	Outcome string `json:"outcome,omitempty"`
	// ExitCode is the exit status of a service that ended, when the runtime
	// reports one. A restarting local service reports the exit that ended its
	// last run; docker compose reports none for a restarting container.
	ExitCode *int `json:"exit_code,omitempty"`
	// Restarts is how many times the runtime's restart policy has restarted
	// the service since it was last started. Only process-compose reports it;
	// docker compose does not, so a container service leaves it zero.
	Restarts int `json:"restarts,omitempty"`
	// Health mirrors the container healthcheck verdict from the
	// runtime backend: typically `healthy`, `unhealthy`, or `starting`.
	// Empty when no healthcheck is declared, the service has not been
	// brought up, or the local-runtime backend doesn't expose one.
	Health string `json:"health,omitempty"`
	// Reason explains a skipped service: the failed job or service its
	// dependency chain stopped at, such as `job deps failed (exit 1)`. For a
	// failed service whose command could not start, it says so. For a pending
	// service waiting on a service the runtime keeps restarting, it names
	// that service.
	Reason string `json:"reason,omitempty"`
}

// Job status values describe the latest native run or isolated operation attempt.
const (
	// JobNeverRun means the runtime holds no run: the stack is down, or the
	// job has not run since the runtime came up.
	JobNeverRun  = "never-run"
	JobPending   = "pending"
	JobRunning   = "running"
	JobCompleted = "completed"
	// JobFailed means the job exited non-zero or its command could not start.
	JobFailed = "failed"
	// JobSkipped means the runtime did not run the job because a dependency
	// failed.
	JobSkipped = "skipped"
	// JobUnknown means the runtime backend could not be queried.
	JobUnknown = "unknown"
	// ServiceStopped is a service's outcome when it ended without failing:
	// it exited after a stop signal, or it is a container that exists without
	// running. Services otherwise take the Job* outcomes; a job has none.
	ServiceStopped = "stopped"
	// ServiceRestarting is a service's outcome while it waits out the
	// runtime's restart backoff after it exited: process-compose's
	// `Restarting` or docker compose's `restarting`.
	ServiceRestarting = "restarting"
)

type JobState struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	// Status is one of the Job* status values.
	Status string `json:"status"`
	// ExitCode is the exit status of a completed or failed run, when the
	// runtime reports one.
	ExitCode *int `json:"exit_code,omitempty"`
	// Reason explains a failed or skipped job, such as `job deps failed (exit
	// 1)` for a job skipped because deps failed, or a pending job waiting on a
	// service the runtime keeps restarting, such as `waiting for service db:
	// restarting (exit 1, 3 restarts)`.
	Reason string `json:"reason,omitempty"`
}

type JobRunRequest struct {
	Inputs         map[string]string `json:"inputs,omitempty"`
	ChainedRestart bool              `json:"chained_restart,omitempty"`
}

type JobRunStatus string

const (
	JobRunPending   JobRunStatus = "pending"
	JobRunRunning   JobRunStatus = "running"
	JobRunSucceeded JobRunStatus = "succeeded"
	JobRunFailed    JobRunStatus = "failed"
	JobRunBlocked   JobRunStatus = "blocked"
)

type JobRunNode struct {
	Name    string       `json:"name"`
	Kind    string       `json:"kind"`
	Status  JobRunStatus `json:"status"`
	Message string       `json:"message,omitempty"`
	// Isolated attempts are owned by this receipt, not shared supervisor state.
	Isolated bool `json:"isolated,omitempty"`
	// RuntimeGeneration is the native job generation before isolated execution.
	// A different subsequent native generation takes precedence over this attempt.
	RuntimeGeneration string `json:"runtime_generation,omitempty"`
	Output            string `json:"output,omitempty"`
	ExitCode          *int   `json:"exit_code,omitempty"`
}

type JobRunOperation struct {
	Artifact       *BackupArtifact `json:"artifact,omitempty"`
	RequestDigest  string          `json:"request_digest,omitempty"`
	ID             string          `json:"id"`
	RootJob        string          `json:"root_job"`
	ChainedRestart bool            `json:"chained_restart"`
	Status         JobRunStatus    `json:"status"`
	CurrentStep    string          `json:"current_step,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	EndedAt        *time.Time      `json:"ended_at,omitempty"`
	Nodes          []JobRunNode    `json:"nodes"`
	Output         string          `json:"output,omitempty"`
	Error          string          `json:"error,omitempty"`
	// ErrorCode and ErrorCause classify a failed run with the error
	// contract's code (JOB_FAILED or TIMEOUT) and cause.
	ErrorCode  string `json:"error_code,omitempty"`
	ErrorCause string `json:"error_cause,omitempty"`
}

type JobRunPreview struct {
	Jobs     []string `json:"jobs"`
	Services []string `json:"services"`
}

type WorkspaceRef struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Template string `json:"template"`
	// Deprecated: a workspace no longer renders a stack (#110), so ChainRoot
	// is always empty. It is removed in a later release.
	ChainRoot          string         `json:"chain_root,omitempty"`
	Allocations        map[string]int `json:"allocations,omitempty"`
	ProcessComposePort int            `json:"process_compose_port,omitempty"`
	PlaywrightMCPName  string         `json:"playwright_mcp_name,omitempty"`
	PlaywrightMCPURL   string         `json:"playwright_mcp_url,omitempty"`
	TTL                string         `json:"ttl,omitempty"`
	TTLExpiresAt       *time.Time     `json:"ttl_expires_at,omitempty"`
}

type WorkspaceStatusResponse struct {
	Name     string                  `json:"name"`
	Path     string                  `json:"path"`
	Exists   bool                    `json:"exists"`
	State    string                  `json:"state"`
	Error    string                  `json:"error,omitempty"`
	Template string                  `json:"template"`
	Inputs   map[string]string       `json:"inputs,omitempty"`
	Sources  []WorkspaceSourceStatus `json:"sources,omitempty"`
	// Deprecated: a workspace no longer renders a stack (#110), so Chain and
	// ChainRoot are always empty. They are removed in a later release. Chain is
	// always sent (`"chain": []`), as it was when every workspace listed its
	// own template there, so REST clients that read it keep a list.
	Chain              []string                        `json:"chain"`
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
	// Deprecated: the operator no longer reads a stack inside a workspace
	// (#110); that stack's own operator reports it. InnerStack is always nil
	// and InnerError always empty. They are removed in a later release.
	InnerStack *StackStatusResponse `json:"inner_stack,omitempty"`
	InnerError string               `json:"inner_error,omitempty"`
}

// WorkspaceSourceStatus is one slot of a workspace as its driver reads it.
// State is one of missing, ready (a linked local source), clean, dirty,
// ahead, behind, diverged, branch-mismatch or error. JJSlotState's fields
// are read from jj slots only: Conflicted is a flag beside State, never a
// state of its own, so a client that does not know it reads the slot as
// before.
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
	JJSlotState
}

// JJSlotState is what the status of a jj slot reads beyond a git slot's,
// shared by every DTO that reports a slot. Its fields are zero for any other
// slot. jj records a conflict in the commit and goes on, so a slot can be
// conflicted in any state.
type JJSlotState struct {
	// ChangeID is the change id of the slot's working-copy commit.
	ChangeID string `json:"change_id,omitempty"`
	// BaseBehind counts the commits of the slot's base it lacks, so a moved
	// base shows even when Behind counts against the slot's upstream.
	BaseBehind int `json:"base_behind,omitempty"`
	// Conflicted reports a conflicted commit among the slot's own, and
	// ConflictPaths lists the conflicted files, sorted.
	Conflicted    bool     `json:"conflicted"`
	ConflictPaths []string `json:"conflict_paths,omitempty"`
	// UntrackedPaths lists the new files jj leaves out of the working-copy
	// commit (over its snapshot.max-new-file-size, or outside its
	// snapshot.auto-track), relative to the slot, a directory ending in "/".
	// Only the slot holds them, so they make it dirty.
	UntrackedPaths []string `json:"untracked_paths,omitempty"`
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
	JJSlotState
}

// GitOpsSummary counts the topology's sources and links by state. Worktrees
// counts the links of git worktree slots, so not jj slots, although git
// lists a colocated jj slot among its primary's worktrees. Unpushed and
// Conflicted count beside the states: a source or link in any state can be
// unpushed, and a jj slot in any state conflicted.
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
	Conflicted     int `json:"conflicted"`
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
	// Sync reclaims a populated worktree left at a source's destination by an
	// earlier create that failed after materializing it, instead of failing
	// with "already exists and is not empty": that one worktree is removed and
	// re-added. It is gated behind Sync because it discards whatever the
	// leftover worktree held. (A stale "missing but already registered"
	// worktree, which has no working tree to lose, is always reclaimed, even
	// without Sync.) Reclaim targets only that path, so sibling workspaces
	// sharing the source cache are untouched.
	Sync bool `json:"sync,omitempty"`
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

type ServiceUpdateTemplateRequest struct {
	Inputs    map[string]string `json:"inputs,omitempty"`
	DryRun    bool              `json:"dry_run,omitempty"`
	Overwrite bool              `json:"overwrite,omitempty"`
}

type TemplateChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type TemplateConflict struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type ServiceTemplateUpdateResult struct {
	Name      string             `json:"name"`
	Service   ServiceState       `json:"service"`
	Changed   bool               `json:"changed"`
	Changes   []TemplateChange   `json:"changes,omitempty"`
	Conflicts []TemplateConflict `json:"conflicts,omitempty"`
}

type WorkspaceUpdateRequest struct {
	Inputs    map[string]string `json:"inputs,omitempty"`
	TTL       string            `json:"ttl,omitempty"`
	Overwrite bool              `json:"overwrite,omitempty"`
}

type PreflightFailure struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

type WorkspaceCreatePreflightResponse struct {
	OK               bool              `json:"ok"`
	Template         string            `json:"template"`
	ResolvedTemplate string            `json:"resolved_template"`
	EffectiveInputs  map[string]string `json:"effective_inputs"`
	// StackDefaults are the host stack's workspace_defaults inputs for the
	// resolved template, already folded into EffectiveInputs beneath the
	// request's own inputs.
	StackDefaults   map[string]string  `json:"stack_defaults,omitempty"`
	MissingRequired []string           `json:"missing_required,omitempty"`
	InvalidInputs   []PreflightFailure `json:"invalid_inputs,omitempty"`
}

type TemplateInputDescriptor struct {
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Required  bool   `json:"required"`
	Immutable bool   `json:"immutable"`
	Generated bool   `json:"generated"`
	Default   string `json:"default,omitempty"`
	// Question is true for top-level Copier questions the user can answer
	// interactively, and false for metadata-only `_angee.inputs` entries
	// (typically generated/immutable). Interactive prompting consumes this
	// to prompt only for real questions.
	Question bool `json:"question"`
	// Order is the zero-based question position in copier.yml, or -1 for metadata-only inputs.
	Order int `json:"order"`
	// Help describes the input to the user.
	Help string `json:"help,omitempty"`
	// Placeholder is the hint shown when the input has no value.
	Placeholder string `json:"placeholder,omitempty"`
	// Secret marks values that must be hidden in prompts and text output.
	Secret bool `json:"secret,omitempty"`
	// Multiselect allows selecting multiple choices.
	Multiselect bool `json:"multiselect,omitempty"`
	// Choices contains allowed values and labels in declaration order.
	Choices []TemplateInputChoice `json:"choices,omitempty"`
	// ChoicesExpr is the raw Jinja expression for dynamically computed choices.
	ChoicesExpr string `json:"choices_expr,omitempty"`
	// When is the raw condition, with boolean conditions rendered as "true" or "false".
	When string `json:"when,omitempty"`
	// Validator is the raw Jinja validation expression, informational to clients.
	Validator string `json:"validator,omitempty"`
}

// TemplateInputChoice pairs a stored input value with its display label.
type TemplateInputChoice struct {
	// Value is the answer passed to Copier.
	Value string `json:"value"`
	// Label is the human-readable choice text.
	Label string `json:"label"`
}

type TemplateDescriptor struct {
	Ref    string                    `json:"ref"`
	Kind   string                    `json:"kind"`
	Name   string                    `json:"name,omitempty"`
	Path   string                    `json:"path"`
	Inputs []TemplateInputDescriptor `json:"inputs"`
}

// TemplateInputsResponse pairs a template descriptor with the answers a
// previous render recorded for one target, for interactive re-rendering.
type TemplateInputsResponse struct {
	Target     string             `json:"target"`
	Template   TemplateDescriptor `json:"template"`
	Recorded   map[string]string  `json:"recorded"`
	Unrecorded []string           `json:"unrecorded,omitempty"`
}

type ConnectionTokenResponse struct {
	Token     string `json:"token"`
	Actor     string `json:"actor"`
	ExpiresAt string `json:"expires_at"`
}

type ServiceEndpoint struct {
	Routed       bool       `json:"routed"`
	URL          string     `json:"url"`
	InternalHost string     `json:"internal_host"`
	InternalPort int        `json:"internal_port"`
	LogStream    *LogStream `json:"log_stream,omitempty"`
}

// LogStream is the descriptor a consumer uses to open a service's live log
// socket: the operator resolves the endpoint (its own port, the Caddy edge, or
// a production backend) and hands back the matching short-lived credential.
// Connecting is identical regardless of Target.
type LogStream struct {
	URL      string `json:"url"`
	Target   string `json:"target"`
	Protocol string `json:"protocol"`
	// Token and ExpiresAt are nil in open (tokenless) dev — omitted over REST
	// and null over GraphQL, rather than an empty string.
	Token     *string `json:"token,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// LogLine is one structured log line from a single service's live stream.
// Service is known from the per-service socket (no parsing); Level is
// best-effort inferred and nil when it can't be determined.
type LogLine struct {
	Service string  `json:"service"`
	Runtime string  `json:"runtime"`
	Message string  `json:"message"`
	Level   *string `json:"level,omitempty"`
}

type IngressStatus struct {
	Type   string     `json:"type"`
	Domain string     `json:"domain,omitempty"`
	Routes []RouteRef `json:"routes"`
}

type RouteRef struct {
	Service string `json:"service"`
	URL     string `json:"url"`
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

// WorkspaceSourceMigrateRequest is the body of `POST
// /workspaces/{name}/sources/{slot}/migrate`: the mode to move the slot to,
// "jj" or "worktree"; empty moves it to the other of the two.
type WorkspaceSourceMigrateRequest struct {
	To string `json:"to,omitempty"`
}

// MintConnectionTokenRequest is the body for `POST /tokens/mint`.
// `Actor` is required and becomes the `sub` claim; `Scope` is the optional
// capability set carried on the operator-API token; `TTL` is a Go duration
// string capped at 24h.
type MintConnectionTokenRequest struct {
	Actor string   `json:"actor"`
	Scope []string `json:"scope,omitempty"`
	TTL   string   `json:"ttl,omitempty"`
}

// MintRouteTokenRequest is the body for `POST /tokens/route`. `Actor` and
// `Service` are required; the minted token's audience is `svc:<Service>`.
// `TTL` is a Go duration string capped at 24h.
type MintRouteTokenRequest struct {
	Actor   string `json:"actor"`
	Service string `json:"service"`
	TTL     string `json:"ttl,omitempty"`
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

// FileContent is the full read result for one file under a stack source.
// Content is the raw UTF-8 text; Etag is the sha256 version token used for
// optimistic-concurrency writes.
type FileContent struct {
	Path    string `yaml:"path" json:"path"`
	Source  string `yaml:"source" json:"source"`
	Content string `yaml:"content" json:"content"`
	Etag    string `yaml:"etag" json:"etag"`
}

// FileRef is the metadata-only result of a write (no content echoed back).
type FileRef struct {
	Path   string `yaml:"path" json:"path"`
	Source string `yaml:"source" json:"source"`
	Etag   string `yaml:"etag" json:"etag"`
}

// FileWriteRequest is the body of `PUT /files` and the `fileWrite` mutation.
// Content is required. Etag, when non-empty, is a compare-and-set precondition;
// a mismatch is a 409 / conflict.
type FileWriteRequest struct {
	Content string `yaml:"content" json:"content"`
	Etag    string `yaml:"etag,omitempty" json:"etag,omitempty"`
}

type WorkspaceSyncBaseRequest struct {
	Method string `json:"method,omitempty"`
}

// Outcomes of `workspace repair` for one source slot.
const (
	// WorkspaceRepairCreated means the slot was missing and repair
	// materialized it.
	WorkspaceRepairCreated = "created"
	// WorkspaceRepairOK means the slot is on disk in a state repair accepts.
	WorkspaceRepairOK = "ok"
	// WorkspaceRepairNeedsAttention means something is at the slot's path but
	// the slot is dirty, diverged, on the wrong branch or unreadable, or the
	// path holds something other than the slot. Repair left it untouched.
	WorkspaceRepairNeedsAttention = "needs-attention"
	// WorkspaceRepairFailed means repair could not materialize the slot (or
	// did not attempt it); nothing it started is left at the slot's path.
	WorkspaceRepairFailed = "failed"
)

// WorkspaceRepairResult is the outcome of `workspace repair`: one entry per
// source slot the manifest declares for the workspace, in slot order. OK is
// false when any slot failed to materialize.
type WorkspaceRepairResult struct {
	Workspace string                `yaml:"workspace" json:"workspace"`
	Path      string                `yaml:"path" json:"path"`
	OK        bool                  `yaml:"ok" json:"ok"`
	Slots     []WorkspaceRepairSlot `yaml:"slots" json:"slots"`
}

// WorkspaceRepairSlot reports what repair did with one source slot. Action is
// one of the WorkspaceRepair* constants and Reason, a single line, says why.
// Status is the slot's state after the repair, present when the slot is on
// disk or repair tried to create it.
type WorkspaceRepairSlot struct {
	Slot   string                 `yaml:"slot" json:"slot"`
	Source string                 `yaml:"source" json:"source"`
	Path   string                 `yaml:"path" json:"path"`
	Action string                 `yaml:"action" json:"action"`
	Reason string                 `yaml:"reason,omitempty" json:"reason,omitempty"`
	Status *WorkspaceSourceStatus `yaml:"status,omitempty" json:"status,omitempty"`
}

// SourceState is a source's cache or local store, or, with Slot set, one
// workspace slot of it (workspace git, push and sync-base). Workspaces is
// set for a source whose cache or store is a jj repository, when the
// operator found jj on PATH the first time it looked.
type SourceState struct {
	Name           string            `json:"name"`
	Slot           string            `json:"slot,omitempty"`
	Kind           string            `json:"kind"`
	Path           string            `json:"path"`
	Exists         bool              `json:"exists"`
	State          string            `json:"state,omitempty"`
	Branch         string            `json:"branch,omitempty"`
	Ref            string            `json:"ref,omitempty"`
	CurrentRef     string            `json:"current_ref,omitempty"`
	Dirty          bool              `json:"dirty,omitempty"`
	Upstream       string            `json:"upstream,omitempty"`
	Ahead          int               `json:"ahead,omitempty"`
	Commits        []CommitRef       `json:"commits,omitempty"`
	Behind         int               `json:"behind,omitempty"`
	Pushed         bool              `json:"pushed"`
	UnpushedReason string            `json:"unpushed_reason,omitempty"`
	Error          string            `json:"error,omitempty"`
	Workspaces     []SourceWorkspace `json:"workspaces,omitempty"`
	JJSlotState
}

// SourceWorkspace is one jj workspace of a source's jj repository, as jj
// lists it. Owned reports that the checkout at the path this stack's
// manifest gives one of its jj slots of that repository is that workspace,
// and Path is then that path. Every other workspace, the repository's own
// (default), other stacks' slots, and a slot of this stack whose checkout is
// gone or cannot be read alike, is not owned and has no path: jj's record of
// a workspace's path goes stale once the workspace moves, so it is not
// reported. Nor is a workspace owned whose checkout holds another of the
// names this stack could have given the slot, as when one `jj edit`ed the
// other's working-copy commit, since which is this stack's cannot be told.
type SourceWorkspace struct {
	Name  string `json:"name"`
	Owned bool   `json:"owned"`
	Path  string `json:"path,omitempty"`
}

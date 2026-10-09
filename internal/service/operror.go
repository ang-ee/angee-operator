package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
)

// Error codes are the stable category of a failed operation. GraphQL errors
// carry one in extensions.code and REST error bodies in "code"; clients branch
// on them, so they never change meaning. See docs/reference/operator-api.md.
const (
	CodeNotFound           = "NOT_FOUND"
	CodeInvalidInput       = "INVALID_INPUT"
	CodeConflict           = "CONFLICT"
	CodePreconditionFailed = "PRECONDITION_FAILED"
	CodeGitFailed          = "GIT_FAILED"
	CodeTimeout            = "TIMEOUT"
	CodeJobFailed          = "JOB_FAILED"
	CodeInternal           = "INTERNAL"
)

// Causes refine a code. They come from angee's own state and from what a
// step was doing, never from reading a tool's output: git's own words are
// passed through in the message and the detail.
const (
	CauseUncommittedChanges = "uncommitted_changes"
	CauseSlotMissing        = "slot_missing"
	CauseBranchMismatch     = "branch_mismatch"
	CauseDetachedHead       = "detached_head"
	CauseSourceCacheMissing = "source_cache_missing"
	CauseStackRootExists    = "stack_root_exists"
	CauseGitUnavailable     = "git_unavailable"
	CauseFetchFailed        = "fetch_failed"
	CausePushFailed         = "push_failed"
	CauseCloneFailed        = "clone_failed"
	CauseMergeConflict      = "merge_conflict"
	CauseDependencyFailed   = "dependency_failed"
)

// maxErrorDetail caps OperationError.Detail; the tail is kept.
const maxErrorDetail = 4 << 10

// maxErrorPaths caps OperationError.Paths, and maxMessagePaths how many of
// them a message lists.
const (
	maxErrorPaths   = 100
	maxMessagePaths = 10
)

// OperationError is a failed operation in the shape every surface reports: a
// code, the cause when known, the operation and the objects it concerns, a
// remedy, and the failing tool's output. Its message reads
// "<Operation> for <target>: <what failed>. <remedy>", followed by
// " git: <line>" when a git command failed.
type OperationError struct {
	Code      string
	Cause     string
	Operation string // machine name, e.g. "workspace.sync-base"
	Workspace string
	Slot      string
	Source    string
	Remote    string // credentials masked
	Job       string
	Service   string
	Paths     []string
	Hint      string
	// Detail is the redacted tail of the failing tool's output.
	Detail string
	// Title is the operation and its target, e.g. `Sync base for workspace
	// "src"`; Summary is what failed and the remedy; GitLine is git's line
	// that names the failure.
	Title   string
	Summary string
	GitLine string
	Err     error
}

func (e *OperationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.Summary
	if message == "" && e.Err != nil {
		message = e.Err.Error()
	}
	if e.Title != "" {
		message = e.Title + ": " + message
	}
	if e.GitLine != "" {
		message += " git: " + e.GitLine
	}
	return message
}

func (e *OperationError) Unwrap() error { return e.Err }

// operation names a Platform verb and the objects it acts on, so the verb's
// failures can say what was being done.
type operation struct {
	name      string // e.g. "workspace.sync-base"
	title     string // e.g. `Sync base for workspace "src"`
	workspace string
	slot      string
	source    string
	job       string
	service   string
}

// annotate is deferred by a Platform verb over its error result. It turns the
// verb's failure into an *OperationError carrying the verb's name, title and
// targets. A NotFound, Conflict or InvalidInput error keeps its own message,
// which already names its object, and gains a code; a git failure is
// classified; anything else becomes INTERNAL.
func (op operation) annotate(errp *error) {
	err := *errp
	if err == nil {
		return
	}
	var opErr *OperationError
	if !errors.As(err, &opErr) || opErr == nil {
		opErr = classifyOperationError(err)
		*errp = opErr
	} else if opErr != err {
		// Wrapping text would put the title mid-message; the typed error
		// carries the context instead.
		*errp = opErr
	}
	if opErr.Operation == "" {
		opErr.Operation = op.name
	}
	if opErr.Title == "" && opErr.Summary != "" {
		opErr.Title = op.title
	}
	fillEmpty(&opErr.Workspace, op.workspace)
	fillEmpty(&opErr.Slot, op.slot)
	fillEmpty(&opErr.Source, op.source)
	fillEmpty(&opErr.Job, op.job)
	fillEmpty(&opErr.Service, op.service)
}

func fillEmpty(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

// classifyOperationError wraps an error that is not yet an *OperationError.
// A NotFound, Conflict or InvalidInput error keeps its message unchanged
// (Summary stays empty, so no title is prefixed), and a NotFound names its
// object in the matching context field.
func classifyOperationError(err error) *OperationError {
	var notFound *NotFoundError
	var conflict *ConflictError
	var invalid *InvalidInputError
	switch {
	case errors.As(err, &notFound):
		opErr := &OperationError{Code: CodeNotFound, Err: err}
		switch notFound.Kind {
		case "workspace":
			opErr.Workspace = notFound.Name
		case "workspace-source", "workspace source":
			opErr.Slot = notFound.Name
		case "source":
			opErr.Source = notFound.Name
		case "job":
			opErr.Job = notFound.Name
		case "service":
			opErr.Service = notFound.Name
		}
		return opErr
	case errors.As(err, &conflict):
		return &OperationError{Code: CodeConflict, Err: err}
	case errors.As(err, &invalid):
		return &OperationError{Code: CodeInvalidInput, Err: err}
	}
	var command *git.CommandError
	if git.IsTimeout(err) || errors.As(err, &command) {
		var opErr *OperationError
		errors.As(gitFailure(err, gitStep{}), &opErr)
		return opErr
	}
	code := CodeInternal
	if errors.Is(err, context.DeadlineExceeded) {
		code = CodeTimeout
	}
	return &OperationError{Code: code, Summary: err.Error(), Err: err}
}

// AsOperationError returns err's *OperationError, or classifies err into one:
// a legacy NotFound, Conflict or InvalidInput error gets its code and keeps
// its message, a git failure is classified, and anything else is INTERNAL.
// It returns nil for a nil err.
func AsOperationError(err error) *OperationError {
	if err == nil {
		return nil
	}
	var opErr *OperationError
	if errors.As(err, &opErr) && opErr != nil {
		return opErr
	}
	return classifyOperationError(err)
}

// gitStep says which git action failed, for the message and context of the
// error built from its failure.
type gitStep struct {
	action string // e.g. "fetching", "pushing", "cloning", "merging origin/main into"
	object string // e.g. `source "angee-arp"`
	remote string // the remote URL, masked before use
	cause  string // the cause of a failure of this step
	slot   string
	source string
}

func (s gitStep) describe() string {
	text := strings.TrimSpace(s.action + " " + s.object)
	if s.remote != "" {
		text += " (" + logctx.RedactURL(s.remote) + ")"
	}
	return text
}

// gitFailure turns a failed git command into a GIT_FAILED (or TIMEOUT)
// *OperationError whose message names the action and remote. git's own
// words follow (gitErrorLine) and its whole output goes to Detail: angee does
// not interpret them, since git and ssh own their diagnostics. An
// *OperationError passes through.
func gitFailure(err error, step gitStep) error {
	if err == nil {
		return nil
	}
	var opErr *OperationError
	if errors.As(err, &opErr) {
		return err
	}
	code, cause := CodeGitFailed, step.cause
	summary := step.describe()
	if summary == "" {
		summary = gitErrorAction(err)
	}
	summary += " failed"
	hint := ""
	var timeout *git.TimeoutError
	switch {
	case errors.As(err, &timeout):
		code = CodeTimeout
		hint = "Raise ANGEE_GIT_TIMEOUT for slow remotes."
		summary += fmt.Sprintf(": timed out after %s. %s", timeout.Timeout, hint)
	case errors.Is(err, exec.ErrNotFound):
		cause = CauseGitUnavailable
		summary += ": git is not installed."
	default:
		summary += "."
	}
	return &OperationError{
		Code:    code,
		Cause:   cause,
		Slot:    step.slot,
		Source:  step.source,
		Remote:  logctx.RedactURL(step.remote),
		Hint:    hint,
		Detail:  gitErrorDetail(err),
		Summary: summary,
		GitLine: gitErrorLine(err),
		Err:     err,
	}
}

// gitErrorAction names the git command a failure without a gitStep came from,
// keeping the context its callers wrapped it in, such as
// `source "app": set remote.origin.url: git config remote.origin.url <url>`.
func gitErrorAction(err error) string {
	full := err.Error()
	var command *git.CommandError
	if errors.As(err, &command) {
		return strings.TrimSuffix(full, command.Error()) + "git " + strings.Join(command.Args, " ")
	}
	var timeout *git.TimeoutError
	if errors.As(err, &timeout) {
		return strings.TrimSuffix(full, timeout.Error()) + timeout.Operation
	}
	return full
}

// gitErrorLine quotes git's own account of a failure: its first fatal: or
// error: line, with the line before it, where git relays what the transport
// or remote said (ssh's "Permission denied (publickey)", say). Without such a
// line it is git's first line of output.
func gitErrorLine(err error) string {
	lines := gitOutputLines(err)
	for i, line := range lines {
		if strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "error:") {
			return strings.Join(lines[max(0, i-1):i+1], " ")
		}
	}
	if len(lines) > 0 {
		return lines[0]
	}
	return ""
}

// gitOutputLines returns the non-empty lines of a git failure's output,
// leaving out its progress ("Cloning into ...") and advice ("hint: ...").
func gitOutputLines(err error) []string {
	var lines []string
	for _, candidate := range strings.Split(gitErrorOutput(err), "\n") {
		candidate = strings.TrimSpace(strings.TrimRight(candidate, "\r"))
		if candidate == "" || strings.HasPrefix(candidate, "Cloning into") || strings.HasPrefix(candidate, "hint:") {
			continue
		}
		lines = append(lines, candidate)
	}
	return lines
}

// gitErrorOutput is the output a failed git command printed, as redacted
// where the command ran.
func gitErrorOutput(err error) string {
	var command *git.CommandError
	if errors.As(err, &command) {
		return command.Output
	}
	return ""
}

// gitErrorDetail is the redacted tail of a git failure's output, at most
// maxErrorDetail bytes.
func gitErrorDetail(err error) string {
	return tailBytes(strings.TrimSpace(gitErrorOutput(err)), maxErrorDetail)
}

// tailBytes returns the last max bytes of s, starting at a line boundary
// when one is near.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < 256 {
		s = s[i+1:]
	}
	return s
}

// requireCleanCheckout fails with uncommitted_changes, listing the changed
// paths, when the checkout at path has uncommitted changes.
func requireCleanCheckout(ctx context.Context, client git.Client, path, object, slot, source string) error {
	paths, err := client.DirtyPaths(ctx, path)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	shown := paths[:min(len(paths), maxMessagePaths)]
	listed := strings.Join(shown, ", ")
	if more := len(paths) - len(shown); more > 0 {
		listed += fmt.Sprintf(" and %d more", more)
	}
	hint := "Commit or restore the changes, then retry."
	return &OperationError{
		Code:    CodePreconditionFailed,
		Cause:   CauseUncommittedChanges,
		Slot:    slot,
		Source:  source,
		Paths:   paths[:min(len(paths), maxErrorPaths)],
		Hint:    hint,
		Summary: fmt.Sprintf("%s has uncommitted changes in %s. %s", object, listed, hint),
	}
}

// originURL returns the URL of the checkout's origin remote, or of its only
// remote, for naming it in a message.
func originURL(ctx context.Context, client git.Client, dir string) string {
	if url, ok, err := client.RemoteURL(ctx, dir, "origin"); err == nil && ok {
		return url
	}
	remotes, err := client.Remotes(ctx, dir)
	if err != nil || len(remotes) != 1 {
		return ""
	}
	if url, ok, err := client.RemoteURL(ctx, dir, remotes[0]); err == nil && ok {
		return url
	}
	return ""
}

// pushRemoteURL returns the URL of the remote a push from the checkout at dir
// goes to, or "" when it cannot be resolved.
func pushRemoteURL(ctx context.Context, client git.Client, dir string) string {
	remote, err := client.PushRemote(ctx, dir)
	if err != nil {
		return ""
	}
	url, ok, err := client.RemoteURL(ctx, dir, remote)
	if err != nil || !ok {
		return ""
	}
	return url
}

// mutationBusyError is the CONFLICT returned while another mutation holds the
// lease.
func mutationBusyError(reason string) error {
	return &OperationError{
		Code:    CodeConflict,
		Hint:    "Retry when it finishes.",
		Summary: reason + "; retry when it finishes",
	}
}

// syncConflictOrFailure reports a failed merge or rebase in a slot. When git
// left conflicted paths (git ls-files -u, as runGitOpAt reads them), it is a
// merge_conflict listing them; otherwise the git failure.
func syncConflictOrFailure(ctx context.Context, err error, path string, step gitStep) error {
	out, lsErr := runGitCapture(ctx, path, "ls-files", "-u")
	conflicted := parseConflictedPaths(out)
	if lsErr != nil || len(conflicted) == 0 {
		return gitFailure(err, step)
	}
	opErr := AsOperationError(gitFailure(err, step))
	hint := "Resolve the conflicts in the slot and commit, or abort the merge or rebase there."
	opErr.Cause = CauseMergeConflict
	opErr.Paths = conflicted
	opErr.Hint = hint
	opErr.Summary = fmt.Sprintf("%s stopped on conflicts in %s. %s", step.describe(), strings.Join(conflicted[:min(len(conflicted), maxMessagePaths)], ", "), hint)
	opErr.GitLine = ""
	return opErr
}

package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/vcs"
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
	CauseUncommittedChanges    = "uncommitted_changes"
	CauseSlotMissing           = "slot_missing"
	CauseBranchMismatch        = "branch_mismatch"
	CauseDetachedHead          = "detached_head"
	CauseSourceCacheMissing    = "source_cache_missing"
	CauseStackRootExists       = "stack_root_exists"
	CauseTemplateTooNew        = "template_too_new"
	CauseGitUnavailable        = "git_unavailable"
	CauseFetchFailed           = "fetch_failed"
	CausePushFailed            = "push_failed"
	CauseCloneFailed           = "clone_failed"
	CauseMergeConflict         = "merge_conflict"
	CauseDependencyFailed      = "dependency_failed"
	CauseBackupRequestConflict = "backup_request_conflict"
	CauseJJSlotUnsupported     = "jj_slot_unsupported"
	CauseJJUnavailable         = "jj_unavailable"
	CauseSharedCommits         = "shared_commits"
	CauseImmutableCommits      = "immutable_commits"
	CauseUntrackedPaths        = "untracked_paths"
	CauseConflictedCommits     = "conflicted_commits"
	CauseUndescribedCommits    = "undescribed_commits"
	CauseBranchMoved           = "branch_moved"
	// CauseBranchCheckedOut: the branch a slot is to be checked out on is
	// checked out by another git worktree of its source, and git checks a
	// branch out in one worktree only.
	CauseBranchCheckedOut = "branch_checked_out"
	// CauseSlotNotHeld: the checkout at a jj slot's path is not the jj
	// workspace this stack made there.
	CauseSlotNotHeld = "slot_not_held"
	// CauseMigrateIncomplete: a migrate of the slot did not finish, so its
	// checkout may be set aside under a private name.
	CauseMigrateIncomplete = "migrate_incomplete"
	// CauseNestedRepositories: a slot holds a repository of its own (a .git
	// or .jj below its own), which a migrate does not carry across.
	CauseNestedRepositories = "nested_repositories"
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
// " <tool>: <line>" when a version-control command failed (" git: <line>",
// " jj: <line>").
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
	// "src"`; Summary is what failed and the remedy; ToolLine is the failing
	// tool's own line that names the failure, and Tool names that tool (git
	// when empty).
	Title    string
	Summary  string
	Tool     string
	ToolLine string
	Err      error
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
	if e.ToolLine != "" {
		message += " " + e.tool() + ": " + e.ToolLine
	}
	return message
}

// tool names the tool ToolLine quotes.
func (e *OperationError) tool() string {
	if e.Tool == "" {
		return "git"
	}
	return e.Tool
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

// unsupportedInput reports a verb the slot's driver does not support as the
// caller's invalid input, in the InvalidInputError's words, while still
// matching vcs.ErrUnsupported and the driver's error.
type unsupportedInput struct {
	input  *InvalidInputError
	driver error
}

func (e unsupportedInput) Error() string   { return e.input.Error() }
func (e unsupportedInput) Unwrap() []error { return []error{e.input, e.driver} }

// classifyOperationError wraps an error that is not yet an *OperationError.
// A NotFound, Conflict or InvalidInput error keeps its message unchanged
// (Summary stays empty, so no title is prefixed), and a NotFound names its
// object in the matching context field.
func classifyOperationError(err error) *OperationError {
	var notFound *NotFoundError
	var notLoaded *runtime.ProcessNotLoadedError
	var conflict *ConflictError
	var invalid *InvalidInputError
	switch {
	case errors.As(err, &notLoaded) && notLoaded.Replacing:
		// The supervisor is swapping in an updated definition; a retry reads it.
		return &OperationError{Code: CodeConflict, Err: err}
	case errors.As(err, &notLoaded):
		// Declared, but the running supervisor does not know it.
		return &OperationError{Code: CodeNotFound, Err: err}
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
	// A verb the slot's version control does not support, as its driver says.
	if errors.Is(err, vcs.ErrUnsupported) {
		return &OperationError{Code: CodeInvalidInput, Err: unsupportedInput{&InvalidInputError{Field: "slot", Reason: err.Error()}, err}}
	}
	// copier refusing an answer (a validator, a choice, a missing required
	// input) is the caller's input at fault; its message names the question.
	if copierx.IsAnswerError(err) {
		return &OperationError{Code: CodeInvalidInput, Err: err}
	}
	// A template that needs a newer Copier than angee's copier-go implements
	// is a requirement of the template, met by upgrading angee.
	if copierx.IsUnsupportedTemplate(err) {
		hint := "Upgrade angee, or use a template version that supports this Copier."
		return &OperationError{Code: CodePreconditionFailed, Cause: CauseTemplateTooNew, Hint: hint, Summary: err.Error() + ". " + hint, Err: err}
	}
	var repair *WorkspaceRepairError
	if errors.As(err, &repair) {
		// A repair that ran and could not materialize some slots keeps its
		// message, which names each slot and why; the result carries them too.
		opErr := &OperationError{Code: CodeInternal, Workspace: repair.Workspace, Err: err}
		if len(repair.Failed) == 1 {
			opErr.Slot = repair.Failed[0].Slot
		}
		return opErr
	}
	// A version-control command that failed, git's or a driver's tool's (a
	// jj command's), quoting the tool.
	var command *git.CommandError
	var tool vcs.ToolError
	if git.IsTimeout(err) || errors.As(err, &command) || errors.As(err, &tool) {
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

// gitFailure turns a failed version-control command into a GIT_FAILED (or
// TIMEOUT) *OperationError whose message names the action and remote. The
// tool's own words follow and its whole output goes to Detail: git's line is
// gitErrorLine, and a driver's vcs.ToolError (a jj command's) names its tool
// and line itself. angee does not interpret them, since the tools and ssh own
// their diagnostics. An *OperationError passes through.
func gitFailure(err error, step gitStep) error {
	if err == nil {
		return nil
	}
	var opErr *OperationError
	if errors.As(err, &opErr) || errors.Is(err, vcs.ErrUnsupported) {
		// A verb the slot's driver does not support is the caller's input at
		// fault; classifyOperationError reports it.
		return err
	}
	code, cause := CodeGitFailed, step.cause
	tool, line, detail := "", gitErrorLine(err), gitErrorDetail(err)
	var toolErr vcs.ToolError
	if errors.As(err, &toolErr) {
		tool, line, detail = toolErr.Tool(), toolErr.Line(), tailBytes(strings.TrimSpace(toolErr.Output()), maxErrorDetail)
	}
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
	case errors.Is(err, exec.ErrNotFound) && tool == "":
		cause = CauseGitUnavailable
		summary += ": git is not installed."
	case errors.Is(err, exec.ErrNotFound):
		code, cause = CodePreconditionFailed, CauseJJUnavailable
		summary += ": " + tool + " is not installed."
	case line == "" && tool == "" && !errors.As(err, new(*git.CommandError)):
		// Neither git's nor a tool's words: the error is all there is.
		summary += ": " + logctx.RedactText(err.Error()) + "."
	default:
		summary += "."
	}
	return &OperationError{
		Code:     code,
		Cause:    cause,
		Slot:     step.slot,
		Source:   step.source,
		Remote:   logctx.RedactURL(step.remote),
		Hint:     hint,
		Detail:   detail,
		Summary:  summary,
		Tool:     tool,
		ToolLine: line,
		Err:      err,
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

// requireCleanSlot fails with uncommitted_changes, listing the changed paths,
// when the checkout target, a source cache, has uncommitted changes. A
// workspace slot's verbs ask its driver instead (requireSlotReady), which
// knows what the verb needs.
func requireCleanSlot(ctx context.Context, driver vcs.Driver, target vcs.Slot, object, slot, source string) error {
	paths, err := driver.DirtyPaths(ctx, target)
	if err != nil {
		return err
	}
	return uncommittedChanges(paths, "", object, slot, source)
}

// uncommittedChanges is the uncommitted_changes refusal of object, a
// workspace slot or a source cache, listing paths; nil when there are none.
// fix, when set, is the version-control tool's own command that records them
// (vcs.UncommittedError.Fix).
func uncommittedChanges(paths []string, fix, object, slot, source string) error {
	if len(paths) == 0 {
		return nil
	}
	shown := paths[:min(len(paths), maxMessagePaths)]
	listed := strings.Join(shown, ", ")
	if more := len(paths) - len(shown); more > 0 {
		listed += fmt.Sprintf(" and %d more", more)
	}
	hint := "Commit or restore the changes, then retry."
	if fix != "" {
		hint = fmt.Sprintf("Commit the working copy first with `%s`, or restore the changes, then retry.", fix)
	}
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

// unpushableCommits is the refusal of a push or publish of slot that would
// send commits jj will not push (a *vcs.UnpushableCommitsError, plan D6):
// conflicted_commits when some hold conflicts, else undescribed_commits. The
// message names every commit, and nothing is pushed or changed. It is nil for
// any other error.
func unpushableCommits(err error, slot, source string) *OperationError {
	var refused *vcs.UnpushableCommitsError
	if !errors.As(err, &refused) {
		return nil
	}
	var hints []string
	cause := CauseUndescribedCommits
	if len(refused.Conflicted) > 0 {
		cause = CauseConflictedCommits
		hints = append(hints, "Resolve the conflicts in the slot with jj")
	}
	if len(refused.Undescribed) > 0 {
		hints = append(hints, "describe each commit with `jj describe -r <commit> -m <message>` in the slot")
	}
	hint := strings.Join(hints, ", and ") + ", then retry."
	hint = strings.ToUpper(hint[:1]) + hint[1:]
	return &OperationError{
		Code:    CodePreconditionFailed,
		Cause:   cause,
		Slot:    slot,
		Source:  source,
		Hint:    hint,
		Summary: fmt.Sprintf("source slot %q was not pushed: %v. %s", slot, refused, hint),
		Err:     err,
	}
}

// toolFailure is gitFailure for a failure a version-control tool reported (a
// git or driver tool's command, or a network timeout), and err as it is for
// anything else, for the operation's annotation to classify: a cancelled
// context, an OS error or a bug keep the code classifyOperationError gives
// them, rather than reading as GIT_FAILED.
func toolFailure(err error, step gitStep) error {
	var command *git.CommandError
	var tool vcs.ToolError
	if git.IsTimeout(err) || errors.As(err, &command) || errors.As(err, &tool) {
		return gitFailure(err, step)
	}
	return err
}

// branchMoved is the refusal of a publish or push of slot whose branch holds
// work the slot does not (a *vcs.BranchMovedError): a collaborator's commits
// on the remote branch, fetched, or on the repository's own. Publishing would
// drop them from the remote, so the slot must take them in first: the hint
// names what to merge, the remote branch that holds them, else the
// repository's own, else, when that is conflicted, the stray commits
// themselves. Nothing is pushed. It is nil for any other error.
func branchMoved(err error, workspaceName, slot, source string) *OperationError {
	return branchMovedRefusal(err, workspaceName, slot, source, "pushed", "which pushing would drop")
}

// branchMovedRefusal is branchMoved for a verb, done, that the moved branch
// stops, with what the verb would do to the stray commits.
func branchMovedRefusal(err error, workspaceName, slot, source, done, consequence string) *OperationError {
	var moved *vcs.BranchMovedError
	if !errors.As(err, &moved) {
		return nil
	}
	ref := moved.Branch
	if moved.Remote != "" {
		ref += "@" + moved.Remote
	}
	hint := fmt.Sprintf("Take them in first with `angee workspace source merge %s %s %s`, then retry.", workspaceName, slot, ref)
	switch {
	case moved.Remote != "":
	case len(moved.Heads) == 1:
		// The branch is conflicted, so its name is no one commit to merge.
		hint = fmt.Sprintf("Take them in first with `angee workspace source merge %s %s %s`, then retry.", workspaceName, slot, moved.Heads[0])
	case len(moved.Heads) > 1:
		hint = fmt.Sprintf("Take them in first by merging each of %s with `angee workspace source merge %s %s <commit>`, then retry.", strings.Join(moved.Heads, ", "), workspaceName, slot)
	}
	return &OperationError{
		Code:      CodePreconditionFailed,
		Cause:     CauseBranchMoved,
		Workspace: workspaceName,
		Slot:      slot,
		Source:    source,
		Hint:      hint,
		Summary:   fmt.Sprintf("source slot %q was not %s: %v, %s. %s", slot, done, moved, consequence, hint),
		Err:       err,
	}
}

// jjBranchExists is the refusal of a publish or push of jj slot slot under a
// branch that exists and is not the slot's (vcs.ErrBranchExists from its
// driver): it points at commits the slot does not own, and publishing would
// move it onto the slot's. own is the slot's manifest branch, which it may
// always publish under.
func jjBranchExists(slot, branch, own string) error {
	remedy := "pass another branch"
	if own != "" {
		remedy = fmt.Sprintf("pass another branch, or the slot's own, %q", own)
	}
	return &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("branch %q already exists and is not jj slot %q's: it points at commits the slot does not own; %s", branch, slot, remedy)}
}

// branchTaken is the refusal of a publish or push of slot under a branch that
// is not the slot's to set (a *vcs.BranchTakenError): a git worktree of the
// same source has it checked out, or the remote has a branch of that name the
// repository does not track. It is the caller's choice of branch at fault, as
// a git publish's existing branch is. It is nil for any other error.
func branchTaken(err error, slot string) error {
	var taken *vcs.BranchTakenError
	if !errors.As(err, &taken) {
		return nil
	}
	remedy := "pass another branch"
	if taken.Remote != "" {
		remedy = fmt.Sprintf("pass another branch, or track it with `jj bookmark track %s@%s` and rebase the slot onto it", taken.Branch, taken.Remote)
	}
	return &InvalidInputError{Field: "branch", Reason: fmt.Sprintf("source slot %q cannot be published under branch %q: %v; %s", slot, taken.Branch, taken, remedy)}
}

// checkoutMoved is the refusal of a publish or push of slot whose checkout
// moved after its push was planned (vcs.ErrCheckoutMoved): nothing was pushed,
// and the verb run again plans afresh. It is nil for any other error.
func checkoutMoved(err error, slot, source, verb string) *OperationError {
	if !errors.Is(err, vcs.ErrCheckoutMoved) {
		return nil
	}
	hint := fmt.Sprintf("Run %s again.", verb)
	return &OperationError{
		Code:    CodeConflict,
		Slot:    slot,
		Source:  source,
		Hint:    hint,
		Summary: fmt.Sprintf("source slot %q changed while its %s was checked, so nothing was pushed: %v. %s", slot, verb, err, hint),
		Err:     err,
	}
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

// syncConflict is the merge_conflict a merge or rebase in a slot stopped on,
// listing the conflicted files, with the output it stopped with as Detail.
func syncConflict(result api.GitOpResult, step gitStep) error {
	conflicted := result.ConflictFiles
	hint := "Resolve the conflicts in the slot and commit, or abort the merge or rebase there."
	return &OperationError{
		Code:    CodeGitFailed,
		Cause:   CauseMergeConflict,
		Slot:    step.slot,
		Source:  step.source,
		Remote:  logctx.RedactURL(step.remote),
		Paths:   conflicted,
		Hint:    hint,
		Detail:  tailBytes(strings.TrimSpace(result.Message), maxErrorDetail),
		Summary: fmt.Sprintf("%s stopped on conflicts in %s. %s", step.describe(), strings.Join(conflicted[:min(len(conflicted), maxMessagePaths)], ", "), hint),
	}
}

// stackOperation names a verb on the whole stack, such as "Stack up".
func stackOperation(name, title string) operation {
	return operation{name: name, title: title}
}

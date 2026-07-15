# Manual Job Terminal Output Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore live manual-job output in the active `angee dev` terminal without changing browser logs or duplicating direct CLI output.

**Architecture:** `service.Platform` receives an optional job-output sink at construction. Local and container commands tee their combined stdout/stderr to the existing response buffer and that sink; the operator supplies its stdout, while direct CLI platforms leave the sink unset.

**Tech Stack:** Go 1.25, `os/exec`, standard-library `io.Writer`, Go tests with the race detector.

## Global Constraints

- Make no changes in `angee-django` or its browser log flow.
- Preserve `JobRun(ctx, name, inputs) ([]byte, error)` and all REST/GraphQL response shapes.
- Stream local and container job output before process completion.
- Preserve partial output on process failure.
- A job-output writer failure must not change the job result.
- Direct `angee job run` output must remain single-print.

---

### Task 1: Stream successful local jobs through the operator

**Files:**
- Create: `internal/operator/job_output_test.go`
- Create: `internal/service/job_output.go`
- Modify: `internal/service/platform.go`
- Modify: `internal/service/jobs.go`
- Modify: `internal/operator/operator.go`

**Interfaces:**
- Produces: `service.WithJobOutput(io.Writer) Option`
- Produces: `(*jobOutputSink).status(name, state string)` and best-effort `io.Writer`
- Preserves: `JobRun(context.Context, string, map[string]string) ([]byte, error)`

- [ ] **Step 1: Write the failing live-output regression test**

Create a local job that prints `first`, waits for a release file, then prints
`second`. Construct `NewServer` while stdout is an `os.Pipe`, invoke
`server.platform.JobRun` in a goroutine, and assert `first` is read from the
pipe while the job is still blocked. After releasing it, assert the returned
bytes are exactly `first\nsecond\n` and the pipe includes:

```text
[job codegen] running
first
second
[job codegen] finished
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
go test -race -run TestOperatorJobRunStreamsLocalOutputWhileRunning ./internal/operator
```

Expected: FAIL because no job output reaches the operator stdout pipe.

- [ ] **Step 3: Add the optional, synchronized job-output sink**

Add a variadic platform constructor option:

```go
type Option func(*Platform)

func WithJobOutput(w io.Writer) Option {
	return func(p *Platform) {
		if w != nil {
			p.jobOutput = newJobOutputSink(w)
		}
	}
}
```

`jobOutputSink` serializes writes, tracks whether the last byte ended a line,
adds a separating newline before status markers when needed, and always returns
`len(p), nil` after best-effort downstream writes.

- [ ] **Step 4: Tee local command output and wire operator stdout**

Replace local `CombinedOutput` with one shared writer assigned to both
`cmd.Stdout` and `cmd.Stderr`:

```go
var captured bytes.Buffer
output := io.Writer(&captured)
if sink != nil {
	output = io.MultiWriter(&captured, sink)
}
cmd.Stdout = output
cmd.Stderr = output
err := cmd.Run()
```

Configure the platform in `NewServer` with the operator's job-output writer;
`Execute` supplies its `stdout`, and direct `service.New(root)` calls supply no
sink. Emit `running` and `finished` around successful local execution.

- [ ] **Step 5: Run the focused test and verify GREEN**

Run:

```bash
go test -race -run TestOperatorJobRunStreamsLocalOutputWhileRunning ./internal/operator
```

Expected: PASS.

### Task 2: Preserve failed output and cover container jobs

**Files:**
- Modify: `internal/operator/job_output_test.go`
- Modify: `internal/service/jobs.go`

**Interfaces:**
- Consumes: `service.WithJobOutput(io.Writer)` and `jobOutputSink`
- Produces: shared `runCommand(*exec.Cmd, io.Writer) ([]byte, error)` capture-and-tee behavior

- [ ] **Step 1: Write the failing partial-output test**

Add a local `broken` job that runs:

```sh
printf 'partial\n'; exit 7
```

Assert `JobRun` returns `partial\n`, returns an error, streams `partial`, and
ends with `[job broken] failed` rather than `finished`.

- [ ] **Step 2: Verify the failed marker test is RED**

Run:

```bash
go test -race -run TestOperatorJobRunStreamsPartialOutputOnFailure ./internal/operator
```

Expected: FAIL until failed status is emitted.

- [ ] **Step 3: Emit completion status from the actual command result**

After local execution, emit `failed` when `err != nil` and `finished` otherwise,
then return the unchanged output and error.

- [ ] **Step 4: Verify the failed marker test is GREEN**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Write the failing container-runtime regression test**

Put an executable fake `docker` script first on `PATH`; it prints a distinctive
line and exits successfully. Run a container job through the operator and
assert the returned output and terminal sink each contain the line exactly
once, bracketed by the job markers.

- [ ] **Step 6: Verify the container test is RED**

Run:

```bash
go test -race -run TestOperatorJobRunStreamsContainerOutput ./internal/operator
```

Expected: FAIL because the container branch still uses buffered-only
`CombinedOutput`.

- [ ] **Step 7: Reuse capture-and-tee execution for containers**

Move the shared command execution into `runCommand`, call it from local and
container branches, and preserve the local error wrapper:

```go
if err != nil {
	return out, fmt.Errorf("job command failed: %w: %s", err, out)
}
```

The container branch continues returning the underlying `exec.Cmd` error, as
before.

- [ ] **Step 8: Run focused package verification**

Run:

```bash
go test -race ./internal/service ./internal/operator ./internal/cli
```

Expected: PASS with no race reports.

### Task 3: Format and verify the complete repository

**Files:**
- Modify mechanically: all changed Go files through `gofmt`

**Interfaces:**
- Consumes: completed terminal-output implementation
- Produces: repository-wide verification evidence

- [ ] **Step 1: Format changed Go files**

Run:

```bash
gofmt -w internal/operator/job_output_test.go internal/operator/operator.go internal/service/job_output.go internal/service/jobs.go internal/service/platform.go
```

- [ ] **Step 2: Review the diff for scope and generated-file drift**

Run:

```bash
git diff --check
git diff --stat
git status --short
```

Expected: only the two internal planning notes and the five scoped Go files are
changed; no generated GraphQL or Django files change.

- [ ] **Step 3: Run the complete project gate**

Run:

```bash
make check
```

Expected: formatting, vet, and all race-enabled tests pass.

- [ ] **Step 4: Re-read the design acceptance criteria**

Confirm terminal-only scope, live output, both runtimes, partial failure
output, unchanged API return, and duplicate-free direct CLI behavior against
the final diff and test output.

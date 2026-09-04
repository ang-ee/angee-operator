# `-v` / `-vv` verbosity and hang diagnosis for `angee` and `angee-operator`

**Status:** Done 2026-09-04 · operator PRs #77 to #82 merged (v0.12.0, v0.12.1); templates PR angee-django#52 merged; live check done on the OVH dev stack · **Created:** 2026-09-04 · **Source:** three read-only sweeps of
`internal/` at v0.11.0 (`6061303`). File:line references are valid at that commit.

## 1. Problem

Both binaries are silent between accepting a command and returning its result,
and "hangs for no reason" is a long or stuck step with no narration. Three
things combine:

1. **There is no logging facility.** No non-test file imports `log` or
   `log/slog`. All user-facing output is ad-hoc `fmt.Fprint*`: a handful of
   `warning:` lines to `os.Stderr` in the service layer, `operator: ...` lines
   to a hardcoded `os.Stderr` in the operator, and final results on stdout.
   There is no level concept and no verbose, debug, or quiet flag or env var.
2. **Nothing is bounded.** The CLI root context is a signal context with no
   deadline (`internal/cli/root.go:30`); the operator runs handlers on
   `r.Context()` with only `ReadHeaderTimeout: 5s` on the server
   (`internal/operator/operator.go:222-226`). Every git, docker, process-compose,
   and remote-operator call inherits a deadline-free context. The only bounded
   calls in the repo are `platformclient.Ping` (2 s), `doctor.commandVersion`
   (2 s), the OpenBao KV client (10 s), and the OpenBao readiness probe
   (1 s per probe, 30 s loop).
3. **One silent file lock serialises everything.** `run/operator.lock`
   (`internal/fslock/lock.go:21-23`) is taken by `StackPrepare`
   (`internal/service/platform.go:96-127`), `ServiceCreate`
   (`service_create.go:57-68`) and `ServiceUpdateFromTemplate`
   (`service_template_update.go:28-38`). Acquisition is a 10 ms spin on
   `flock(LOCK_NB)` with no timeout and no message (`lock.go:25-56`). CLI and
   operator share it, and `StackPrepare` holds it across network git fetches.

## 2. Inventory

### 2.1 Output plumbing that exists today

| Item | Where |
|---|---|
| Root persistent flags: `--root`, `--operator`, `--json` only | `internal/cli/root.go:49-51` |
| `-v` today is cobra's default shorthand for `--version` (`angee -v` prints the version) | cobra default; `root.go:44` |
| `service.Platform` has no logger or observer; the only option is `WithJobOutput` | `internal/service/platform.go:27,38` |
| Operator stderr lines, hardcoded `os.Stderr`, `operator:` prefix | `operator.go:121,303,307`; `rest_secrets.go:85,93`; `rest_files.go:52,59`; `gql/events.go:277,282`; `service/commits.go:73` |
| Operator middleware: `auth` only, no request logging | `operator.go:795` |
| Service-layer warnings to `os.Stderr` | `sources.go:369` (best-effort refresh, PR #75), `workspaces.go:1624` (branch exists) |
| OpenBao progress goes to an injected stderr that is `nil` on the main paths | `openbao.go:27,49,55`; callers `runtime.go:23,45,103,379` |
| Copier renderer explicitly silenced | `internal/copierx/copierx.go:225` (`copier.WithQuiet(true)`) |
| Existing context-value pattern to copy | `internal/operator/auth_context.go:13`, `graphql.go:148` |
| Job output streamed only when `WithJobOutput` is set (operator yes, local CLI no) | `internal/service/job_output.go`, `operator.go:102` |

### 2.2 Subprocess and network chokepoints (where `-vv` tracing goes)

Instrumenting these covers every external call in the repo.

| Chokepoint | Signature / site | Runs |
|---|---|---|
| `git.Client.Run` | `internal/git/git.go:34-48` | every git command: clone/fetch/pull/push (`:70-88,185-213`), merge/rebase (`:90-98`), worktree (`:100-143`), read fallbacks (`:217-508`) |
| `runGitOpAt`, `runGitCapture` | `internal/service/gitops_merge.go:95-102,133-137` | workspace-source merge/rebase/publish; the only place `GIT_TERMINAL_PROMPT=0` is set (`:180`) |
| `runDiffAt` | `internal/service/diff.go:57-63` | `git diff` |
| compose `ExecRunner.Run`, `run`, `runLimited`, `runForeground`, `StreamLogs` | `internal/runtime/compose/backend.go:23,174,188,206,159` | `docker compose build/up/down/start/stop/restart/ps/logs` |
| proccompose `ExecRunner.Run`, `run`, `runLimited`, `runForeground`, `StreamLogs` | `internal/runtime/proccompose/backend.go:28,252,271,297,166` | `process-compose up/down/process */list/logs` |
| `goBinPath`, `installProcessCompose` | `proccompose/backend.go:382,398-401` | `go env GOPATH`, `go install ...@latest` (network-bound, unbounded) |
| `runtime.StreamCommand` | `internal/runtime/stream.go:31-76` | follow-mode log streams |
| `JobRun` docker path, `runLocalCommand` | `internal/service/jobs.go:108-111,122-137` | `docker run --rm ...`, local job commands (no `cmd.Cancel`/`WaitDelay`) |
| `refreshTemplateRepo` | `internal/service/templates.go:68-88` | registry clone/fetch/checkout for `init`, `stack init/update`, `service create`, `workspace create`, `template list/get` |
| `platformclient.doJSON`, `doBytes`, `stream`, `streamTo` | `internal/platformclient/client.go:633,663,686,717` | every remote-mode call, `http.DefaultClient`, no timeout (`:82`); only `Ping` is bounded (`:95-99`) |
| secrets OpenBao `request` | `internal/secrets/openbao.go:79-119` | KV reads/writes, 10 s client timeout |
| `openBaoReady` | `internal/service/openbao.go:72-80` | health probe, 1 s |
| `startDetachedCommand` | `internal/cli/workspace_open.go:111-116` | editor launch, fire-and-forget |

Facts that matter for the design: no child process ever inherits stdin
(`cmd.Stdin` is never set); no runner echoes its command (the argv appears only
inside error messages); `GIT_SSH_COMMAND`, `BatchMode`, and
`StrictHostKeyChecking` are set nowhere, so git and ssh can open `/dev/tty` for
credentials or host-key confirmation from clone/fetch/pull/push.

### 2.3 Waits, locks, prompts

| Wait | Where | Bound | Visible? |
|---|---|---|---|
| Root file lock spin | `internal/fslock/lock.go:25-56` | none (ctx only) | no |
| OpenBao readiness loop | `internal/service/openbao.go:51-65` | 30 s, then **returns nil** | only if stderr injected |
| OpenBao compose `Up` (pull/create/start) | `openbao.go:42-45` | none | no |
| Graceful child teardown after cancel | `internal/runtime/backend.go:13`, used at `compose/backend.go:223`, `proccompose/backend.go:317` | 10 s | no |
| Port availability probes (`net.Listen` per candidate) | `internal/service/port_availability.go:17-30` | local | no |
| Operator EventHub pollers (2 s tick, call into Platform) | `internal/operator/gql/events.go:24,120,156,240` | unbounded lifetime | poll errors logged once |
| Hasura live-list poll | `internal/operator/gql/collections.go:74-113` | 2 s tick | no |
| Log WS keepalive (no read deadline by design) | `internal/operator/logstream.go:102-128` | 10 s ping | no |
| Doctor tool probes, sequential | `internal/cli/doctor.go:126-159` | 2 s each; git/port probes unbounded (`:233,313-326`) | no |
| process-compose install prompt (guarded by char-device check) | `proccompose/backend.go:405-434` | blocks on stdin | prompt on stderr |
| Template input prompts (no is-terminal guard, only `--yes`) | `internal/cli/root.go:766-800` | blocks on stdin | prompt on stderr |
| `file set --stdin`, `secret set --stdin` | `files.go:80`, `secrets.go:134` | until EOF | no |

### 2.4 Long synchronous phases with no intermediate output

- **`StackPrepare`** (`internal/service/platform.go:95-128`), under the root
  lock: `LoadStack` → `materializeStackResources` (persist dirs +
  `materializeReferencedSources`: `git fetch --all --prune` or `git clone` per
  git source, `sources.go:20-31,356-376`) → `materializeDeclaredWorkspaces`
  (`WorkspaceCreate` per declared workspace, `platform.go:153-176`) →
  `compileStackArtifacts` (secret resolution + `Compile`) → `writeRuntimeEnv`
  → `writeCompiled`. Reached by `up`, `dev`, `build`, `service *`, `logs`, and
  the operator's stack routes.
- **`bootstrapOpenBao`** (`openbao.go:15-66`): probe → compile bootstrap →
  write → compose `Up` → 30 s readiness loop.
- **`WorkspaceCreate`** (`workspaces.go:32-248`): parent transaction →
  template resolve (may clone the registry) → validate → allocate ports →
  materialize sources (clone or worktree, `workspaces.go:1523-1678`) → copier
  render → apply files → persist paths → save. Runs synchronously inside the
  operator's POST handler.
- **`JobRun`** (`jobs.go:37-120`): resolve secrets → substitute → run → return
  buffered output once.
- **`angee doctor`** (`doctor.go:78-114`): everything sequential.

### 2.5 Errors that turn into silence

| Site | Effect |
|---|---|
| `proccompose.Status` returns `nil, nil` on any error (`backend.go:180-185`) | supervisor down or wrong port reads as "nothing running" |
| `runtimeServiceStates` drops both backends' errors (`platform.go:491-505`) | docker unreachable reads as "declared" |
| `bootstrapOpenBao` returns `nil` after 30 s without readiness (`openbao.go:65`) | the real cause surfaces later as an unrelated secret error |
| `materializeSource` best-effort refresh warns and continues (`sources.go:362-371`) | a hung fetch is not bounded, only its failure is tolerated |
| `sourceCommits` returns `nil, nil` (`commits.go:42,50`) | commits silently omitted |
| `logStreamDescriptor` drops the `MintRoute` error (`logstream.go:210-216`) | tokenless descriptor, no error |
| EventHub poll errors rate-limited (`gql/events.go:274-286`) | one line, then quiet |
| `runForeground` returns `nil` when ctx is cancelled (`compose/backend.go:229-231`, `proccompose/backend.go:319-321`) | intentional, but hides why a child exited |

### 2.6 Operator server specifics

- Long operations run synchronously in handlers with `r.Context()`:
  `operator.go:316,325,361,374,387,395,494,527,542,555,691,723`.
- Streaming handlers commit a 200 and then block; setup errors go in-band
  (`operator.go:420-436`).
- Client disconnect does propagate to children via `exec.CommandContext`
  (SIGKILL, or SIGINT + 10 s for the foreground runners).
- Shutdown 5 s, teardown 60 s (`operator.go:281,304`).

### 2.7 Ranked hypotheses for "hangs for no reason"

1. **Network git inside `StackPrepare` under the lock.** Every `up`, `dev`,
   `service *`, and operator stack route refreshes every referenced git source
   with no timeout while holding `run/operator.lock`. A slow VPN, an SSH
   connect timeout, a credential or host-key prompt on `/dev/tty`, or a large
   fetch stalls the caller and everything queued on the lock, silently. PR #75
   made the failure best-effort; it did not bound the wait.
2. **Lock convoy in the operator.** Concurrent requests and the 2 s pollers
   serialise on the same lock with no log line, so one slow request makes the
   whole operator look stuck.
3. **OpenBao bring-up** with `stderr == nil`: docker pull/start plus up to
   30 s of polling with nothing printed, then `nil` on timeout.
4. **Remote mode** against a busy or dead operator: `http.DefaultClient`, no
   timeout, nothing printed.
5. **Local job runs** buffer all output until exit.
6. **Status probe failures** degrade to "declared" instead of naming the cause.
7. **`go install` of process-compose** after the prompt, unbounded and
   network-bound.

## 3. Approach

### 3.1 Principles

- **One facility, standard library.** `log/slog`, no new dependency. A
  `*slog.Logger` travels in `context.Context` (new package `internal/logctx`:
  `With(ctx, l)`, `From(ctx)` returning a discard logger when unset, and
  `Step(ctx, name, attrs...) func(error)`). Platform methods already take a
  context everywhere, and the operator already uses the context-value pattern.
- **stderr only.** stdout stays the result stream so `--json` and pipes are
  unaffected.
- **Levels.** Default = WARN: byte-identical output to today, except the
  existing ad-hoc warnings are routed through the logger with one prefix.
  `-v` = INFO: phase narration and anything slow. `-vv` = DEBUG: every
  subprocess (argv, cwd, duration, exit code), every HTTP call (method, URL,
  status, duration), lock wait and hold times, captured child output
  (truncated, redacted).
- **Instrument chokepoints, not call sites.** The runners in 2.2 are about a
  dozen functions; one change each covers all subprocess and network events.
  Phase narration goes into the orchestration methods: `StackPrepare`,
  `materializeSource`, `materializeDeclaredWorkspaces`, `WorkspaceCreate`,
  `bootstrapOpenBao`, `serviceRuntimeAction`, `JobRun`, `resolveTemplate`.
- **Slow-step heartbeat.** `Step` logs start at DEBUG; if the step is still
  running after 3 s it emits `still <name> (Ns)` at INFO every 5 s; completion
  at DEBUG with duration; failure at WARN with duration. This is what makes
  `-v` answer "what is it doing right now".
- **Redaction in the facility.** Strip userinfo from URLs, mask `--token` and
  `X-Vault-Token`, log env keys but never values, cap captured output at 4 KiB.
- **Operator.** Same facility. Request middleware assigns a request id and
  logs method, path, actor, status, and duration at INFO; the request-scoped
  logger goes into `r.Context()` so Platform narration carries the request id.
  The existing `operator:` prints migrate to it.
- **Remote mode.** `-v` on the CLI logs each HTTP call with its duration; the
  operator's own log shows the internals. A later phase can stream progress
  events back (the `202 + operation_id` / SSE idea in `.agents/notes/ideas.md`).

### 3.2 Flags and environment

- `angee -v` / `-vv` via a counted `--verbose` persistent flag. This
  repurposes `-v` from cobra's version shorthand; `--version` stays and an
  `angee version` subcommand lands in the same PR (closes #53).
- `ANGEE_VERBOSE=0|1|2` for both binaries (the operator runs in a container
  where flags live in compose); the flag wins over the env var.
- `angee operator -v/-vv` and `angee-operator -v/-vv` accept the same.

### 3.3 Output format

- CLI `-v`: `angee: refreshing source django (git fetch)` style lines, no
  timestamps. `-vv`: adds elapsed time and `key=value` attributes.
- Operator: slog text with time, level, request id, `key=value`.

### 3.4 Tests

- A buffer-backed slog handler for tests; assert narration for
  `StackPrepare` with fake runners, argv logging and redaction in each runner,
  heartbeat timing with an injected clock, and the operator request line.
- Default-level output must remain identical for every existing CLI test.

### 3.5 Beyond verbosity: hang hardening (separate PRs)

1. **git.** Set `GIT_TERMINAL_PROMPT=0` and `GIT_SSH_COMMAND=ssh -o BatchMode=yes`
   in the operator (it has no tty) and in the CLI when stdin is not a
   terminal. Add a network timeout to clone/fetch/pull/push (default 120 s,
   `ANGEE_GIT_TIMEOUT` override) that fails with the remote host named.
2. **Lock.** Write holder pid and command into `run/operator.lock`; log at
   INFO after 1 s of waiting, naming the holder; optional acquisition timeout.
   Longer term, refresh sources before taking the lock so network time is not
   spent inside the critical section.
3. **Errors.** `proccompose.Status` returns its error and Platform maps it to
   an explicit "unknown (process-compose unreachable)" state with a WARN;
   `bootstrapOpenBao` returns an error on timeout; `runtimeServiceStates`
   warns.
4. **Operator and client.** `IdleTimeout` on the server; a client timeout for
   non-streaming remote calls so a dead operator fails instead of hanging.
5. **Doctor probes** (#50, #51) separately.

## 4. Phases

| PR | Scope | Acceptance |
|---|---|---|
| 1 | `internal/logctx`; root `-v/-vv` + `ANGEE_VERBOSE`; `angee version`; route existing warnings; DEBUG tracing in every chokepoint in 2.2; `Step` narration in the orchestration methods; docs (`commands.md`) and CHANGELOG | `angee -v up` names each phase and heartbeats anything over 3 s; `angee -vv up` shows every docker/git command with duration; default output unchanged |
| 2 | Operator flags/env, request middleware with request id, migrate `operator:` prints, logger in request context; `operator-api.md` logging section | one line per request with duration; narration under `-v` carries the request id |
| 3 | Hang hardening items 1 to 4 in 3.5, each with tests | a stalled fetch fails within the timeout with the host named; a lock wait over 1 s is reported with its holder; status errors are named |
| 4 (optional) | Progress events over the operator API so remote `-v` shows operator internals | `angee --operator -v up` narrates the same phases as local |

## 5. Decisions needed

1. Repurpose `-v` from version to verbose and add `angee version`. Recommended.
2. Print a single "still working on <step>" hint at the default level after
   30 s. Recommended: it addresses the complaint for users who did not pass
   `-v`, at the cost of one new stderr line in slow runs.
3. `ANGEE_VERBOSE=1|2` versus `ANGEE_LOG_LEVEL=info|debug`. Recommended:
   `ANGEE_VERBOSE`, mirroring the flag.
4. git `BatchMode` in the CLI on a terminal, where a prompt may be wanted.
   Recommended: only when stdin is not a terminal.

## 6. Readiness in the manifest (fixes the template wait-loop floods)

The `dev` template in Docker mode fakes readiness with per-container shell
loops (`stacks/_shared/stack-body.yaml.jinja` in the templates repo, added in
`d8bec949` on 2026-08-28): `frontend` (schemas, 600 s), `storybook` and
`playwright-server` (`caches/js-deps.done`, 300 s), every `celery-*` (a Python
DB probe per second, 300 s), plus `frontend-build` (600 s) and `caddy` (180 s)
in the instance flavour. Each prints one line per second. Root cause: `after:`
compiles to `condition: service_started` / `process_started`
(`internal/service/platform.go:676-703`); the manifest has no readiness field,
the compose document has no `healthcheck`, and the process-compose document has
no readiness probe. Two bugs ride on the noise: dependents budget 300 s while
frontend's own schema wait is 600 s, so slow starts kill storybook and
playwright-server with exit 1 and no restart policy; and frontend deletes the
sentinel on every start, replaying the wait.

Design: a `ready:` block on a service with exactly one probe kind (`http`,
`tcp`, `cmd`, `file`) plus `interval`, `timeout`, `retries`, `start_period`.
Container runtime compiles it to a compose `healthcheck` and dependents get
`condition: service_healthy`; local runtime compiles it to a process-compose
`readiness_probe` and dependents get `condition: process_healthy`. `after:` on
a dependency without `ready:` keeps today's started semantics. Old CLIs reject
unknown manifest fields, so the templates change ships only after the operator
release that understands `ready:`.

## 7. Session todo (execution order)

Legend: `[ ]` todo · `[x]` done · `[~]` in progress · `[!]` blocked.
Each PR: branch from `main`, `make check` + `golangci-lint run`, go-code-reviewer,
CHANGELOG entry under `## Unreleased`, PR with session link, squash-merge.

### PR A — verbosity facility, CLI flags, chokepoint tracing, narration

- [x] A1 `internal/logctx`: `With`/`From` (discard logger by default), level from
      count (0 warn, 1 info, 2 debug) and `ANGEE_VERBOSE`, CLI text handler
      (no timestamps at info, elapsed + attrs at debug), `RedactURL`/`RedactArgs`
      (userinfo, `--token`, `X-Vault-Token`, env values), output cap 4 KiB,
      `Step(ctx, msg, attrs...) func(error)` with heartbeat (3 s first then every
      5 s at info; one 30 s hint at warn when quiet), injectable clock. Tests.
- [x] A2 `internal/cli/root.go`: persistent `--verbose/-v` count flag, env
      fallback, logger built in a persistent pre-run and stored in the command
      context; `angee version` subcommand (plain and `--json`) since `-v` no
      longer means version (closes #53). Tests in `root_test.go`.
- [x] A3 Debug tracing at the chokepoints: `git.Client.Run`; `runGitOpAt`,
      `runGitCapture`, `runDiffAt`; compose `ExecRunner.Run`, `runLimited`,
      `runForeground`, `StreamLogs`; proccompose `ExecRunner.Run`, `runLimited`,
      `runForeground`, `goBinPath`, `installProcessCompose`, `StreamLogs`;
      `runtime.StreamCommand`; `jobs.go` docker run and `runLocalCommand`;
      `platformclient` `doJSON`/`doBytes`/`stream`/`streamTo`; `secrets` OpenBao
      `request`; `service` `openBaoReady`; `fslock.Lock` (info after 1 s waiting,
      hold duration at debug). One test per runner asserting the line and
      redaction with a buffer handler.
- [x] A4 Info narration with `Step`: `StackPrepare` phases, `materializeSource`
      (refresh vs clone, per source), `materializeDeclaredWorkspaces`,
      `WorkspaceCreate` phases and source materialisation, `bootstrapOpenBao`
      (keep the existing stderr lines), `serviceRuntimeAction`, `JobRun`,
      `resolveTemplate`/`refreshTemplateRepo`. Route the three `warning:` prints
      (`sources.go:369`, `workspaces.go:1624`, `commits.go:73`) through the
      logger at warn with identical default-level text. Narration test for
      `StackPrepare` with fake runners.
- [x] A5 Docs: global flags in `docs/guide/commands.md` and `README.md`;
      CHANGELOG. Verify default output unchanged across the CLI test suite.
- [x] A6 Review, PR, CI, merge.

### PR D — readiness in the manifest

- [x] D1 `internal/manifest`: `Ready` on `Service` (`http {port,path}`, `tcp
      {port}`, `cmd []string`, `file string`, `interval`, `timeout`, `retries`,
      `start_period`), validation (exactly one kind, durations parse, port in
      range). Tests.
- [x] D2 `internal/runtime/compose/doc.go`: `Healthcheck` (`test`, `interval`,
      `timeout`, `retries`, `start_period`). `internal/runtime/proccompose/doc.go`:
      `ReadinessProbe` (`http_get`, `exec`, `initial_delay_seconds`,
      `period_seconds`, `timeout_seconds`, `failure_threshold`).
- [x] D3 `Compile`: emit probes per runtime (`file` and `cmd` as `CMD-SHELL`
      inside the container; `http`/`tcp` via `wget`/`nc` with a documented image
      caveat; native `http_get`/`exec` for process-compose); `composeDependsOn`
      and `processDependsOn` switch to `service_healthy`/`process_healthy` when
      the dependency declares `ready`. Golden tests for both runtimes.
- [x] D4 `docs/guide/manifest.md`, `npm run gen:schema`, CHANGELOG.
- [x] D5 Review, PR, CI, merge.

### PR B — operator logging

- [x] B1 `internal/operator/operator.go`: `-v/--verbose` count flag and
      `ANGEE_VERBOSE`; logger construction; request middleware assigning a
      request id and logging method, path, actor, status, duration at info
      (streaming and WebSocket routes log start and end); logger placed in the
      request context so Platform narration carries the id.
- [x] B2 Migrate the `operator:` prints (`operator.go:121,303,307`,
      `rest_secrets.go:85,93`, `rest_files.go:52,59`, `gql/events.go:277,282`)
      to the logger; adjust tests that assert stderr text.
- [x] B3 `docs/reference/operator-api.md` logging section; CHANGELOG.
- [x] B4 Review, PR, CI, merge.

### PR C — hang hardening

- [x] C1 git: `GIT_TERMINAL_PROMPT=0` always in the operator and in the CLI when
      stdin is not a terminal; `GIT_SSH_COMMAND=ssh -o BatchMode=yes` under the
      same rule; a network timeout (default 120 s, `ANGEE_GIT_TIMEOUT`) on
      clone/fetch/pull/push and the template registry refresh, failing with the
      remote host named. Plumb an interactive flag through `service.New`.
- [x] C2 fslock: write holder pid and command into `run/operator.lock`; the
      1 s wait log names the holder; `ANGEE_LOCK_TIMEOUT` optional.
- [x] C3 Surface errors: `proccompose.Status` returns its error and Platform
      reports `unknown` with a warn naming the cause; `bootstrapOpenBao` returns
      an error on the 30 s timeout; `runtimeServiceStates` warns.
- [x] C4 Timeouts: `IdleTimeout` on the operator server; a client timeout for
      non-streaming remote calls (separate client for streams).
- [x] C5 Tests for each; CHANGELOG; review, PR, CI, merge.

### Release

- [x] R1 Final PR renames `## Unreleased` to `## v0.12.0 — <date>`; merge; tag
      the squash commit; push; watch the release run; `make install` locally.

### Templates repo (after R1; follow its own AGENTS.md)

- [x] T1 `stacks/_shared/stack-body.yaml.jinja`: `ready:` on `django` (cmd probe
      on migrations or the health endpoint), `frontend` (file
      `caches/js-deps.done`), `frontend-build` (file `dist/index.html`); delete
      the seven wait loops; keep `after:`; only remove the sentinel when the
      lockfile changed.
- [x] T2 Render both flavours in both runtime modes and diff against the
      previous render; start the Docker-mode dev stack once and confirm no
      waiter output and that dependents start after frontend is healthy.
- [x] T3 PR in the templates repo with a compatibility note (requires angee
      v0.12.0).

### Deferred

- Progress events over the operator API so remote `-v` matches local.
- Doctor probe timeouts (#50, #51).

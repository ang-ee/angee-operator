# Runtime stream spikes: can Angee's job runtimes carry binary save/restore streams?

Date: 2026-11-06. Host: macOS 26 (Darwin 25.4.0) arm64, OrbStack Docker 29.8.2,
Docker Compose v5.5.0, process-compose v1.122.0. Linux cross-check: an
`alpine:3.24 + docker-cli + docker-cli-compose` image (the runtime layer of
`Dockerfile.operator`), Docker CLI 29.9.3 / Compose v5.6.1, driving the same
daemon through the mounted socket.

Payloads: `data/p64` (64 MiB, sha256 `80d6fa0d…7993`) and `data/p512`
(512 MiB, sha256 `d45578d4…9c41`), both from `/dev/urandom`.

Everything lives in this directory:

| File | What |
|---|---|
| `lib.sh` | shared helpers (`sha`, timers, `COMPOSE_RUN`) |
| `cfg/compose.yaml`, `cfg/compose-rel.yaml`, `cfg/compose-fail.yaml` | compose job configs (absolute bind, relative bind, failure/throughput services) |
| `cfg/pc.yaml`, `cfg/pc-extra.yaml` | process-compose job configs shaped like Angee's compiled local job (shutdown 10 s/SIGTERM, `log_location` + Angee's text `log_configuration`) |
| `gorun/` (`bin/gorun`, `bin/gorun-linux`) | a Go launcher that runs a command the way a backend would: config on an `os.Pipe` passed as `ExtraFiles[0]` (fd 3), stdin/stdout wrapped as non-`*os.File` so `os/exec` copies through its own pipes, optional `Setpgid`, context cancel with SIGKILL (exec default, = compose backend) or SIGINT (= proccompose backend's `cmd.Cancel`), `WaitDelay`, and injected reader/writer failures |
| `s1_…sh` … `s7b_…sh` | one script per spike; `out/sN.log` is each script's captured output |
| `gen_payloads.sh` | regenerates `data/p64`, `data/p512` (deleted after the run) and rebuilds `bin/gorun*` |

The Angee invocations copied:

* compose (`internal/runtime/compose/backend.go` `runTransientJob`):
  `docker compose -f - [--env-file F] run --rm --no-deps -T --name <container> <job>`,
  YAML on stdin, `CombinedOutput()`, cwd = stack root, cancel = SIGKILL of the
  client (`exec.CommandContext` default) then `docker rm -f <container>`.
* process-compose (`internal/runtime/proccompose/backend.go` `runTransientJob`):
  `process-compose --no-server --log-file <dir>/process-compose.log --log-no-color run --no-deps --config <dir>/process-compose.yaml <name>`,
  0600 config in a `MkdirTemp` dir, stdout+stderr into one collector, stdin
  unset (= `/dev/null`), `Setpgid`, cancel = SIGINT, `WaitDelay` = shutdown + 1 s.

---

## Spike 1 — compose `run -T`, stdout is the artifact — **PASS**

Command (config on stdin exactly as Angee does it; `s1_compose_stdout.sh`):

```sh
docker compose -f - run --rm --no-deps -T [--quiet-pull|-q] --name angee-spike-s1-<id> dump \
  <cfg/compose.yaml >out 2>err
# dump: sh -c 'echo container-stderr-before >&2; cat "$PAYLOAD"; echo container-stderr-after >&2; sha256sum "$PAYLOAD" >&2'
```

| case | exit | stdout bytes | sha matches | what went to stderr |
|---|---|---|---|---|
| a image present, project network absent | 0 | 67108864 | YES | ` Network angee-spike-rt_default Creating/Created`, ` Container … Creating/Created`, then the container's stderr lines |
| b image present | 0 | 67108864 | YES | ` Container … Creating/Created` + container stderr |
| c `--quiet-pull`, image present | 0 | 67108864 | YES | same as b |
| d image absent (busybox pulled) | 0 | 67108864 | YES | ` Image busybox:1.36.1 Pulling`, per-layer `Pulling fs layer/Downloading/Download complete/Pull complete`, `Pulled`, `Container … Creating/Created`, container stderr |
| e image absent, `--quiet-pull` | 0 | 67108864 | YES | only ` Image … Pulling` / ` Image … Pulled` + `Container …` lines + container stderr |
| f `run -q` | 0 | 67108864 | YES | **only the container's stderr** — `-q` drops compose's own progress lines but not the container's stdout |
| g `--progress plain`, image absent | 0 | 67108864 | YES | same as d (plain is what non-TTY gets anyway) |
| 1b stderr is a real pty (`script`), stdout to file, pull + present | 0 | 67108864 | YES | ANSI-coloured progress on the pty; stdout still clean (`s1b_…sh`) |

Container stderr (`container-stderr-before/after`, the in-container sha) lands
on compose's stderr, never on stdout. No banners, no CRLF translation (with
`-T`), no progress on stdout in any case, including a pty on stderr.

**Implication for Angee:** a compose save job's stdout can be the artifact as
long as stdout is collected separately from stderr (today's `CombinedOutput()`
merges them and keeps only a tail, so the backend needs a stdout-writer path);
`run -q` is a cheap way to keep compose's own chatter out of the diagnostics
stream.

## Spike 2 — compose `run -T`, stdin is the artifact — **PASS (config not on stdin)**

`s2_compose_stdin.sh`; `restore` = `sh -c 'echo restore-start >&2; sha256sum >&2; echo restore-done >&2'`.

| case | command | container saw | exit |
|---|---|---|---|
| a | `cat p64 \| docker compose -f cfg/compose.yaml run --rm --no-deps -T --name N restore` (no `-i`) | `80d6fa0d…7993` = payload, YES | 0 |
| b | same with `--interactive=false` | `e3b0c442…b855` = empty | 0 |
| c | `docker compose -f cfg … run … restore < p64` (file, not pipe) | payload, YES | 0 |
| d | **today's shape** `docker compose -f - run … restore < cfg` | empty (compose reads stdin to EOF for the YAML) | 0 |
| e | `cat cfg p64 \| docker compose -f - run … restore` | — compose rejects: `go-yaml load error … control characters are not allowed` | 1 |

`run --help`: `-i, --interactive  Keep STDIN open even if not attached (default true)`;
case b proves the default: stdin is forwarded with `-T` and no `-i`. The
container gets EOF when the client's stdin ends, so `sha256sum` terminates.

**Implication for Angee:** compose stdin can carry a restore stream only if the
YAML moves off stdin; with today's `-f -` a restore job silently reads an
empty stdin and exits 0 (case d) — a restore built on today's backend would
"succeed" having restored nothing.

## Spike 3 — freeing stdin from the config — **PASS with `--project-directory`**

`s3_compose_config_off_stdin.sh` (shell, cwd deliberately elsewhere) and
`s3b_go_extrafiles.sh` (Go `ExtraFiles` pipe, macOS + Linux). Config
`cfg/compose-rel.yaml` has a relative bind `./data:/data:ro`; job prints
`bind-ok`/`bind-MISSING` and the sha of stdin.

| case | command | resolved bind source | stdin payload | verdict |
|---|---|---|---|---|
| a1 | `docker compose -f /dev/fd/3 run … restore-rel 3<cfg <p64` | `/dev/fd/data` | — `Error response from daemon: error while creating mount source path '/dev/fd/data': mkdir /dev/fd/data: no such file or directory`, exit 1 | FAIL without project dir |
| a2 | `docker compose --project-directory ROOT -f /dev/fd/3 run … 3<cfg <p64` | `ROOT/data` | match YES, `bind-ok` | **PASS** |
| a3 | a2 with fd 3 a **pipe** (`3< <(cat cfg)`) | `ROOT/data` | match YES, `bind-ok` | **PASS** (compose reads the file once; a pipe works) |
| a4 | project name with no `name:` | `fd` without, `runtime` (= basename ROOT) with `--project-directory` | — | Angee configs carry `name:`, so only cosmetic |
| b1 | 0600 temp file in a 0700 dir, no project dir | `PRIV/data` | match YES but **`bind-MISSING`**, exit 0 | **silent wrong mount**: docker creates an empty `PRIV/data` and the job runs against it |
| b2 | temp file + `--project-directory ROOT` | `ROOT/data` | match YES, `bind-ok` | PASS (but the config touches disk) |
| c1 | `-f -` + `--project-directory ROOT` | `ROOT/data` | empty stdin | resolves fine, cannot carry the payload |
| c2/c3 | `-f -`, no project dir | cwd/data (Angee sets `cmd.Dir = root`, so `ROOT/data`) | — | today's behaviour: relative paths resolve against cmd.Dir |
| 3b mac | `gorun -config compose-rel.yaml -wrap -- docker compose --project-directory ROOT -f /dev/fd/3 run … restore-rel <p64` (YAML on an `os.Pipe` as `ExtraFiles[0]`, stdin via os/exec copy goroutine) | `ROOT/data` | `in=67108864`, match YES, `bind-ok` | **PASS** |
| 3b mac save | same, `dump` service, stdout wrapped | — | stdout sha YES (`out=67108864`) | **PASS** |
| 3b linux | same Go launcher (linux/arm64) inside the alpine+docker-cli-compose image | `ROOT/data` | match YES, `bind-ok` (pipe on fd 3) and YES with `3<cfg` (regular file on fd 3) | **PASS** |
| 3b linux save | same, `dump` | — | stdout sha YES | **PASS** |

**Implication for Angee:** move the compose YAML from stdin to
`cmd.ExtraFiles[0]` (an `os.Pipe` written by a goroutine) and pass
`-f /dev/fd/3 --project-directory <root>`; the config never touches disk,
works on macOS and Linux, and stdin is free for the restore stream.
`--project-directory` is mandatory: without it relative paths resolve
against `/dev/fd` (hard failure) or, for a temp file, against the temp dir
(silent empty bind mount).

## Spike 4 — process-compose foreground `run`, stdout is the artifact — **FAIL as-is (43-byte prefix), PASS after an exact prefix strip or via fd 3**

`s4_pc_stdout.sh` with Angee's exact flags and an Angee-shaped job (incl.
`log_location`):

```sh
process-compose --no-server --log-file PRIV/process-compose.log --log-no-color \
  run --no-deps --config PRIV/process-compose.yaml dump </dev/null >out 2>err
```

| case | exit | stdout bytes | sha |
|---|---|---|---|
| a stdin /dev/null, stdout file | 0 | **67108905** (payload + 41) | NO |
| b via Go `os/exec` (wrapped writer, `Setpgid`) | 0 | 67108905 | NO |
| c stdout a pipe | 0 | 67108905 | NO |
| d stderr a pty | 0 | 67108905 | NO |

The extra bytes are at the very start of stdout:
`ESC ] 0 ; process-compose: <host>/runtime BEL` (`1b5d 303b 7072 6f63 …07`)
— an OSC-0 terminal-title escape. Source (v1.110 in the module cache, behaviour
unchanged in v1.122): `src/cmd/project_runner.go` `runHeadless()` calls
`util.SetProjectNameAsTerminalTitle` →
`fmt.Printf("\033]0;process-compose: %s\007", hostname + "/" + path.Base(project name or cwd))`,
**unconditionally, to stdout, even when stdout is not a TTY**. There is no flag
or `PC_*` env to disable it (checked `--help`, `run --help` and the binary's
strings). Stripping those first 41 bytes gives sha `80d6fa0d…7993` = payload.

Otherwise the stream is clean: the `run` main process gets
`command.AttachIo()` (`Stdin/Stdout/Stderr = os.Stdin/os.Stdout/os.Stderr`,
`src/app/process.go`), so process-compose does not touch the bytes, does not
line-split them, and **does not write them to `log_location`** (no
`run/logs/dump.log` was created) nor to `--log-file` (797 bytes of its own INF
lines). The process's stderr stays on process-compose's stderr
(`pc-stderr-before/after`, sha line); process-compose prints nothing of its
own to stderr on success (its log goes to `--log-file`).

`s4b_pc_title_workarounds.sh`:

| case | result |
|---|---|
| a config with top-level `name: angee-job` | prefix is exactly `ESC]0;process-compose: <host>/angee-job BEL` (43 bytes = `printf '\033]0;process-compose: %s/%s\007' "$(hostname)" angee-job`); stripping exactly that prefix → sha YES |
| b no `name:`, cwd `/` | title becomes `<host>//` — it follows cwd, so pin `name:` |
| c job writes the artifact to an inherited fd 3 (`cat "$PAYLOAD" >&3`, `process-compose … fd3dump 3>fd3.out`) | fd 3 bytes 67108864, sha YES; stdout carries only the 43-byte title |

**Implication for Angee:** process-compose stdout can carry a save stream only
if the backend strips (and verifies) the exact leading title escape —
`"\x1b]0;process-compose: " + os.Hostname() + "/" + <pinned project name> + "\x07"` —
before the first artifact byte, and fails the run if the prefix differs. That
is a dependency on an upstream implementation detail; an upstream fix (title
only when stdout is a TTY) is the owner-side remedy. The alternative that
needs no stripping is to give the job its artifact on an inherited fd 3
(`cmd.ExtraFiles`), which process-compose passes through untouched, but then
job commands write to `>&3` instead of stdout.

## Spike 5 — process-compose foreground `run`, stdin is the artifact — **PASS**

`s5_pc_stdin.sh`, `restore` = `echo restore-start >&2; shasum -a 256 >&2; echo restore-done >&2`.

| case | command | job saw | exit |
|---|---|---|---|
| a pipe | `cat p64 \| PC restore` | payload, YES | 0 |
| b file redirect | `PC restore < p64` | payload, YES | 0 |
| c /dev/null (Angee today: `cmd.Stdin` unset) | `PC restore </dev/null` | empty | 0 |
| d Go os/exec, wrapped stdin + `Setpgid` | `gorun -wrap -pgid -- PC restore < p64` | payload, YES (`in=67108864`) | 0 |
| f stdin a **TTY** + `Setpgid` | `script -q /dev/null gorun -pgid -- PC restore-count` | **hangs**: process-compose, bash and `wc` all in state `T` (stopped by job control: a background process group touching the terminal) until the 10 s timeout | 124 |
| g stdin a TTY, no `Setpgid` | same without `-pgid` | reads the terminal (here EOF → `restore-bytes=0`) | 0 |

process-compose has no stdin flag; `run` is documented as "Run selected
process with std(in|out|err) attached", and the main process gets the real
`os.Stdin` (`AttachIo`). The per-process `is_tty` / `is_interactive` /
`is_foreground` / `is_elevated` settings only change how *non-main*
processes are wired (pty or pipes for the TUI); they are irrelevant to `run`.

**Implication for Angee:** process-compose stdin carries a restore stream
as-is; the backend only has to set `cmd.Stdin` (today it is left unset, so a
restore job reads EOF and exits 0, like compose's case 2d). Never hand it a
terminal as stdin: with the isolated process group a TTY stdin stops the
whole job (SIGTTIN) instead of failing.

## Spike 6 — failure semantics — **exit codes PASS; cleanup and truncation need backend work**

`s6_failures.sh` (`out/s6.log`; `out/s6-run1.log` is the first run, which
hung in 6d-restore-pc-kill until the orphan was killed by hand — see below).
Compose config on fd 3 (spike 3). `C = docker compose --project-directory ROOT -f /dev/fd/3 run --rm --no-deps -T`,
`PC = process-compose --no-server --log-file … --log-no-color run --no-deps --config …`.

### compose

| case | command | result |
|---|---|---|
| 6a producer exits 7 after 32 MiB | `C --name N fail-mid-save >out` | exit **7**, stdout 33554432 bytes, container removed — **PASS** |
| 6b1 consumer dies (shell) | `C --name N cat-only \| head -c 1000` (512 MiB) | client exit **1** after 0.3 s, stderr `write /dev/stdout: broken pipe`; the **container keeps running** to completion (~10 s, its output now going only to the log driver), then AutoRemove |
| 6b2 consumer dies, endless producer | `C --name N stream \| head -c 1000` | client exit 1; **container `Up 5 seconds` and still running** — never ends on its own; needed `docker rm -f` |
| 6b3 Go writer errors after 1 MiB | `gorun -wrap -out-fail-after 1048576 -- C --name N stream` | `os/exec` closes the pipe, client exit 1 (`err=exit status 1`), container still `Up 5 seconds`, needed `docker rm -f` |
| 6c restore job exits 9 after 1 MiB | `cat p512 \| C --name N fail-mid-restore` | client exit **9** in 0.4 s, no hang, producer gets SIGPIPE (141), container removed — **PASS** |
| 6d save, SIGKILL client (Angee's cancel) | `gorun -cancel-after 3s -- C --name N stream` | client 137; container **still `Up 7 seconds`** 5 s later → `docker rm -f` required (what `removeCanceledJob` does) |
| 6d save, SIGINT client | `gorun -cancel-after 3s -cancel-sigint -- C …` | client exits 130 at once; container **still running** (no signal proxied to it with `-T`) → `docker rm -f` required |
| 6d restore, SIGKILL / SIGINT client | `cat /dev/zero \| gorun -cancel-after 3s [-cancel-sigint] -- C --name N sink` | client 137 / 130; container gone within 5 s without `docker rm`: once the client's attach drops, the container's stdin ends, the reader exits on its own and AutoRemove deletes it (inferred from the state; its stderr went nowhere) |
| 6e restore source errors after 1 MiB | `gorun -wrap -in-fail-after 1048576 -- C --name N restore-count <p64` | job printed `restore-bytes=1048576` and **exited 0** (it saw a clean EOF); `cmd.Run()` still returned the reader's error (`err=spike: peer gone`) |

### process-compose

| case | command | result |
|---|---|---|
| 6a producer exits 7 | `PC fail-mid-save >out` | exit **7**, 33554432 + 43 title bytes — **PASS** |
| 6b1 consumer dies (shell) | `PC cat-only \| head -c 1000` | the job owns the fd, so the job itself gets SIGPIPE; process-compose logs `exited with code -1` and exits **255** in 0.05 s; nothing left running |
| 6b3 Go writer errors after 1 MiB | `gorun -wrap -pgid -out-fail-after 1048576 -- PC cat-only` | same: 255, job gone, no hang |
| 6b4 same, endless `while :; do cat; done` job | `gorun … -cancel-after 8s -cancel-sigint -- PC stream` | each `cat` dies of SIGPIPE and the loop goes on; ran until the 8 s cancel, then 255 — job scripts must stop on write failure (`set -e`/`pipefail`), the runtime cannot |
| 6c restore job exits 9 | `cat p512 \| PC fail-mid-restore` | exit **9**, no hang, producer 141 — **PASS** |
| 6d save/restore, SIGINT (Angee's cancel) | `gorun -pgid -wait-delay 11s -cancel-after 3s -cancel-sigint -- PC stream\|sink` | `Caught interrupt - Shutting down the running processes...`, job gone at once, exit **255** (job's signal death reported as -1) — clean |
| 6d save, SIGKILL | `gorun -pgid -cancel-after 3s -- PC stream` | 137; the job is **orphaned and keeps running** (reparented to pid 1, same pgid) |
| 6d restore, SIGKILL | `cat /dev/zero \| gorun -pgid -cancel-after 3s -- PC sink` | 137; orphan keeps reading the pipe, so the upstream writer never gets SIGPIPE: run 1 blocked **136 s** until killed by hand; run 2's watchdog found `orphan sink … ppid=1 pgid=7371 still reading` at +8 s |
| 6e restore source errors after 1 MiB | `gorun -wrap -pgid -in-fail-after 1048576 -- PC restore-count <p64` | job saw `restore-bytes=1048576` + EOF and exited 0; `cmd.Run()` returned the reader error |

Exit codes of process-compose: the job's own code when it exits; **255 for
any signal death** (SIGPIPE, SIGINT shutdown) — the signal is not visible.

**Implications for Angee:**
* Exit codes propagate on both runtimes (6a, 6c), so "job exited non-zero ⇒ artifact invalid" works.
* compose: a container outlives its client in every save failure, not only on
  cancel. `removeCanceledJob` must also run when the client exits non-zero
  (broken pipe, consumer gone), or an endless/large `pg_dump` keeps running
  detached. On restore the container ends by itself (stdin EOF) but only
  after it has consumed a truncated stream.
* Both runtimes turn an upstream failure into a **clean EOF** for the job
  (6e, 6d-restore). A restore job cannot tell "stream ended" from "stream
  broke"; the backend must treat a stdin copy error from `cmd.Wait` as a
  failed restore even when the job exited 0, and restores should use formats
  that detect truncation (`pg_restore -Fc` does; `psql < plain.sql` does not)
  or apply inside one transaction.
* process-compose must keep being cancelled with SIGINT (as today); a SIGKILL
  — including the one `os/exec` sends when `WaitDelay` expires — orphans the
  job, and an orphaned restore job blocks its upstream writer forever. Killing
  the whole process group (`syscall.Kill(-pgid, SIGKILL)`) after the
  `WaitDelay` would close that gap.

## Spike 7 — throughput, 512 MiB — **PASS, one pathology: docker's log driver on the save path**

`s7_throughput.sh` (single runs with `/usr/bin/time -l`, hashes checked) and
`s7b_repeats.sh` (3 repeats, wall time only). Another agent's database
containers were sharing the machine during the runs, so read the numbers as
orders of magnitude. Compose times include ~0.8 s of container create/start
(a 64 MiB restore took 0.85 s in spike 2).

| path | direction | wall (3 runs) | notes |
|---|---|---|---|
| host `cat p512` / `shasum -a 256 p512` | — | 0.03 s / 1.27 s | baselines |
| container reads bind mount internally | — | 0.97 s | no stream |
| plain `docker run`, container stdout → host | save | 8.0 s | default json-file logging |
| plain `docker run -i`, stdin → container | restore | 1.2 / 0.9 / 0.9 s | |
| compose `run -T`, **json-file logging** (default) | save | **7.5 / 7.8 / 8.4 s** (~65 MiB/s) | sha YES; client user 0.18 s, sys 0.64 s, max RSS 53 MiB |
| compose `run -T`, `logging: {driver: none}` | save | **1.24 / 1.20 / 1.15 s** | sha YES; same client CPU/RSS |
| same, through Go `os/exec` copy goroutine | save | 1.22 / 1.19 / 1.33 s | sha YES |
| compose `run -T`, stdin → `cat >/dev/null` | restore | 1.63 / 1.44 / 1.13 s | `restore-sha` in container: 3.49 s, sha YES (busybox sha256sum cost) |
| same, through Go `os/exec` copy goroutine | restore | 1.34 / 1.42 / 1.58 s | |
| process-compose `run`, stdout → file | save | 0.24 / 0.25 / 0.25 s | sha YES after stripping the 43-byte title |
| same, Go-wrapped + `Setpgid` | save | 0.27 / 0.25 / 0.25 s | |
| process-compose `run`, stdin → `cat >/dev/null` | restore | 0.26 / 0.25 / 0.29 s | `restore-sha` (perl shasum) 1.67 s, sha YES |
| same, Go-wrapped + `Setpgid` | restore | 0.24 / 0.24 / 0.27 s | |

Client memory stays flat (max RSS ~53 MiB for a 512 MiB stream on every
path): neither docker compose nor process-compose buffers the stream.

The one pathology is the **container's logging driver**: docker feeds a
`run`'s stdout to the log driver as well as to the attached client, so a dump
on stdout is also line-split and JSON-encoded into
`/var/lib/docker/containers/<id>/<id>-json.log`. It costs ~6.5 s per 512 MiB
here (5–6× slower than with `driver: none`), and the daemon keeps a lossy
partial copy of the database dump on disk until the container is removed
(`docker logs` replayed 40231784 bytes; the file was 8234893 bytes because
OrbStack's daemon sets `log-opts {max-size: 20m, max-file: 5}` — a stock Linux
dockerd has no rotation by default and would write the whole stream). With
`logging: {driver: none}` `docker logs` answers `configured logging driver
does not support reading` and the attached stream is unaffected. `run --rm`
sets `HostConfig.AutoRemove=true` (daemon-side removal). process-compose's
`--log-file` stayed at 761 bytes after all 512 MiB runs: the main process's
output bypasses its loggers.

**Implication for Angee:** both runtimes move 512 MiB in about a second or
less once container start is excluded; compose save jobs must render
`logging: {driver: none}` (or `local` with a tiny size) for the job's
service in the transient config, otherwise every snapshot is slowed ~6× and a
copy of the dump lands in docker's log store.

---

## Summary

| question | compose `run -T` | process-compose `run` |
|---|---|---|
| stdout byte-identical? | **yes** — all compose chatter on stderr, also when pulling and with a pty | **no** — 41–43-byte OSC title `ESC]0;process-compose: <host>/<name>BEL` prefixed, unconditional, no off switch; rest identical |
| stderr separate? | yes | yes |
| stdin forwarded? | **yes** (`-i` defaults to true) — but today's `-f -` eats it: job reads EOF | **yes** (`AttachIo`) — today's unset `cmd.Stdin` gives the job /dev/null |
| config off stdin | `-f /dev/fd/3` + `ExtraFiles` pipe + `--project-directory <root>` (macOS + Linux) | already a 0600 temp file; nothing to change |
| non-zero exit propagated | yes | yes (255 for signal deaths) |
| consumer gone | client exits 1, **container keeps running** → `docker rm -f` on any failure | job gets SIGPIPE, pc exits 255, nothing left |
| cancel | SIGKILL/SIGINT leave a save container running → `docker rm -f` (as today); restore container ends on stdin EOF | SIGINT clean; SIGKILL orphans the job |
| upstream breaks mid-restore | job sees clean EOF and may exit 0; `cmd.Wait` returns the copy error | same |
| 512 MiB | ~1.2 s with `logging: none`, ~8 s with default json-file | ~0.25 s |

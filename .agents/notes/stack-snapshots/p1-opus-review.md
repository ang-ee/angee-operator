# P1 reviews (2026-11-06)

Codex built P1 (`gpt-5.6-sol`, thread `01a1b2cd-4f17-7161-b3cb-5f916f77b51b`). Opus
(`go-code-reviewer`) then reviewed it three times, with a Codex fix round after each review.
Plan §7 was amended to describe the design as built.

## Round 1: two blockers

1. **Cancel during create leaks containers.** A cancel while the container was being
   created left a `Created` container (17 of 36 sweep points; the old ticker leaked 3 of 18).
   Fixed with Docker's own pushed events, without polling:
   - a `docker events` subscription for the job's unique container name, with a `--since`
     replay;
   - an immediate SIGKILL of the docker client's process group;
   - event-driven detection of the create;
   - exactly one `docker rm -f -v`, confirmed by its `destroy` event.
2. **A local save could hang.** A local save could hang and ignore cancel because the fd-3
   copy had no bound. It now has an idle-only bound after the process exits, and survivors
   in the process group are killed.

The eight should-fixes were also applied:
- one `Stdout` contract across both backends, and validated roles;
- size-bound leak assertions;
- test network cleanup;
- the missing processgroup, INT-trap, Stdin-error and config-untouched tests;
- the compose writer error joined into the result;
- the CI proof for the local fd-3 path;
- Windows refusing up front.

## Round 2: two new blockers

1. **The CI install used the wrong case.** It installed
   `github.com/F1bonacc1/process-compose`; the module path is lowercase.
2. **A slow consumer failed complete saves.** The post-`Wait` read deadline also counted
   time spent in the consumer's `Write`, so a slow consumer failed saves that were complete.
   The deadline now bounds idle reads only.

Should-fixes, all applied:
- only a killed client waits for the create window; any other failure removes the container
  at once;
- the container is still removed when the event stream ends early;
- `-v` removes anonymous volumes;
- the events code has unit tests.

## Round 3: one blocker

`docker rm -f` on a missing container exits 0 silently on Docker CLI 29. Every failure
before the container existed therefore waited 30 s and reported a container that never
existed.

The fix treats an exit-0 output that doesn't name the container as "already gone". Opus
checked it in a scratch copy: a missing image fails in 0.14 s instead of 30 s. The flaky 1 s
test deadlines and the nits were folded into the same Codex round.

## What the probes showed after round 2

- **Cancel sweep:** 0 of 36 cancel points leaked.
- **Short-deadline save:** a `sleep 30 &` save with a short deadline returned in 2.3 s.
- **Slow consumer:** with the consumer stalled for 4 s, all 20,000 bytes arrived.
- **Anonymous volumes:** no leaks with `postgres:17-alpine`.
- **Leftovers:** no containers, networks, volumes, `docker events` or `sleep` processes
  were left behind.

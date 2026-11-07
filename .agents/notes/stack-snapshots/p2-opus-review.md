# P2 reviews (2026-11-07)

Codex built P2 (`gpt-5.6-sol`) from a brief covering plan §8 and §17 item 2. Opus
(`go-code-reviewer`) then reviewed it. Each finding marked "confirmed" was reproduced
against real restic 0.19.1 in a scratch copy.

## Round 1: two blockers

1. **Cancel left a stale lock.** `exec.CommandContext` SIGKILLs by default, and restic only
   removes its lock on SIGTERM (S7, S8). A cancelled backup left a lock behind (confirmed).
   The fix sends SIGTERM on cancel and gives restic time to clean up before escalating.
2. **`BackupStdin` could hang.** When restic exited early (a wrong password exits 12 at
   once) while the caller's reader was blocked, `exec.Cmd.Wait` waited on its own stdin
   copier. It returned only at the deadline, and reported `Interrupted` instead of
   `WrongPassword` (confirmed). The fix uses its own stdin pipe and closes the reader once
   restic exits. On a reader error it sends restic SIGTERM before closing stdin, so restic
   aborts instead of committing a partial file.

What already held up: passwords travel only on fd 3 and fd 4 (pipes, closed correctly, no
deadlock when restic doesn't read them); the repository URL is never in argv; all
inherited `RESTIC_*` variables are stripped; `--retry-lock` is used only on locking calls;
`Unlock` never passes `--remove-all`. Leak probes with a fake restic printing the secrets
found nothing in errors or traces.

Should-fixes:
- **Metadata calls under lock contention** were reported as `Interrupted`. The 5-minute
  deadline equalled `--retry-lock 5m`. The policy dry run also took an exclusive lock;
  read-only calls now use `--no-lock`.
- **`Dump`** lost the writer's error (confirmed).
- **`ExpectedBytes`** hid restic's own error kind (confirmed).
- **`Copy` of a copy** failed, because restic keeps the first `original` (confirmed).
- **`Forget`** accepted ID prefixes (confirmed). Full IDs are now required everywhere and
  `latest` is refused.
- **Tags** containing commas or padding were silently rewritten by restic, which can widen
  a forget filter (confirmed). They are now refused.
- **Secret registration** was process-wide and too broad: usernames and paths blanked
  unrelated log text (confirmed). URL redaction moves into `logctx`.
- **Child environment:** restic inherited all of Angee's environment except `RESTIC_*`,
  including unrelated tokens and any ambient cloud credentials. Decision: an allowlist,
  plus explicit backend env, matching the plan's "credentials in the child env only".
  Conflicting source and destination env keys are refused.
- **toolinstall:** process-compose still kept a second default installer and lookups. They
  now pass straight through.
- **`Check`** dropped its summary on failure.
- **Test gaps:** KeyAdd, CatConfig, Restore, the policy dry run, Prune and Check had no
  real-binary tests; the stale lock wasn't proven before `Unlock`.

Plus nits on fixtures, naming, exit codes, an `Unavailable` kind, password whitespace, and
caching the binary lookup.

## Round 2: one should-fix

Every round-1 finding was verified fixed, by re-running the round-1 probes against real
restic:

| Probe | Result |
|---|---|
| Cancels (8) | No stale locks and no commits |
| Wrong password, blocked reader | `WrongPassword` within 1.3 s |
| Reader errors (27) | No commits |
| Copy of a copy, rerun | Works, and the rerun is idempotent |
| Prefix forget, `latest` and bad tags | Refused |
| Usernames | No longer blanked |
| `Dump` writer error | Kept |
| `Check` on a corrupted repository | Returns its summary |

The probes also showed that SIGTERM alone doesn't stop restic while its stdin is open. It
waits for EOF, so the order is SIGTERM, then close stdin.

**Should-fix:** a reader whose `Close` doesn't unblock `Read` (`io.NopCloser`, a gzip
reader) still hung past the deadline. The fix drops the closable special case: after a
failed exit, the copier gets a bounded drain.

**Nits:**
- cancel-test readiness without a sleep;
- cleanup for the paused prune;
- Unix-only signal tests, so Windows vet passes;
- process-wide registration only for query values that look like credentials;
- `Unlock` without `--retry-lock`;
- adapter error codes;
- `Unavailable` on Windows;
- docs;
- `user` instead of a real username in the logctx tests.

Deferred to P6: a validated `Subfolder` option for `id:subfolder` addressing in
Dump/Restore/Ls.

Note for P8 docs: with the env allowlist, `RCLONE_CONFIG*` and cloud workload-identity
variables (`AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`,
`GOOGLE_APPLICATION_CREDENTIALS`, …) must be passed through the destination's env.

## Round 3: ready

A focused check of the round-2 changes found no regressions:
- the `NopCloser` probe now returns `WrongPassword` in 0.6 s;
- the clean-exit path is race-free (six exact `ExpectedBytes` runs);
- 8 cancels left no locks and no snapshots;
- reader-error aborts still commit nothing.

Its one optional nit was applied: only an uncancelled clean exit waits for the stdin
copier without a bound.

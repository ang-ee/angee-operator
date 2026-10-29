# Operator manual backups and cold recovery

Manual capture is implemented by `service.Platform` through the existing
`JobRunOperation` receipt, mutation lease, runtime `JobBackend`, and Go ZIP
support. Django owns plans, authorization, workflow attempts, retention policy,
and the completed catalog record. Scheduling and cold restore are deferred.
There is no plugin registry, second job engine, or operator catalog.

## API

Admin-bearer-only `POST /backups/preview` discovers manifest-owned resources with
included/excluded/unsupported status and explicit unknown size estimates. Quick
discovery bounds filesystem enumeration per resource and across the request,
including excluded entries. Unfinished sizes remain null, with included status
and a reason; the total sums known sizes and has_unknown_sizes stays true.
All component roots are checked independently of optional size estimation.
`POST /backups` starts capture with a stable `request_id`; polling uses existing
`GET /job-runs/{id}`. Identical requests return the same durable receipt,
including after restart; changing the selection under that ID conflicts.
`GET /backups/{id}/archive` streams a completed artifact. Artifacts live only
under `backups/archives/<id>.zip`; staging uses `state/backups/<id>`. Nothing
deletes `backups/archives/*.zip` or `state/job-runs/*.json` yet: retention is
the caller's.

A running backup holds the mutation lease and `run/operator.lock` until its
receipt is final, so other stack mutations (job runs, restarts, applies, and
the rest) answer 409 until it ends. It cannot be cancelled through the API.
`ANGEE_JOB_TIMEOUT` (30 minutes by default, 0 for none) bounds the whole
backup: capture, archive and verify.

Requests carry `version: 1` and `selection` groups `databases`, `service_state`,
`workspaces`, `sources`, and `storage`. Every group uses `mode` (`all`, `selected`,
`none`), `names`, and `exclude`; sources also use `policy` (`bundle`,
`fetch_published`). All means resources owned by this stack, never every database
on a shared server. Source IDs are `stack:<source>` and
`workspace:<workspace>/<slot>`; workspace files and source payloads are independent. With all optional groups set
to none, capture still preserves stack declarations and original recovery secrets.
Public preview reasons contain capability guidance, never raw filesystem errors.

## Native capture declaration

A trusted manifest job declares `capture: {group: databases, name: app,
output: database.dump, exclude_persist: [pgdata]}`. It writes that non-empty
regular output file under `${inputs.backup_output}` (stack-relative) or
`${inputs.backup_output_absolute}` (local absolute). Containers may mount the
relative directory at a fixed output mount. The service template owns `pg_dump`
and its exact database identity. URI connections are passed explicitly with
`pg_dump --dbname "$PGDATABASE"`; setting PGDATABASE alone does not expand the
URI through pg_dump’s libpq environment-default path. No UI command or archive command is executed.
Services without a capture declaration do not promise logical backup support.
Capture jobs cannot declare run_on triggers or serve as dependencies: their
outputs and lifecycle belong to the explicit backup attempt.
Named volumes are reported as unsupported without a capture protocol. Ordinary
persisted bind files belong to storage; state superseded by a trusted capture's
exclude_persist belongs to that logical capture. An unsafe persistence declaration
without a logical capture is reported as an unsupported service state. Stateless
services do not acquire a fictional state resource just because a process exists. Nested persist
resources always retain their independent selection through a parent tree.

`persist.<name>.backup: exclude` declares state unsafe to copy. Database paths,
operator backups/staging/runtime directories are excluded through ancestors,
even when the database selection is none. Old `pgdata` entries remain excluded
until their template migration. Never use raw pgdata or `pg_dumpall`.

## Format and recovery

The ZIP contains `manifest.json` version 1, selected payloads, exact source
revisions, explicit directories (including empty repository roots), per-entry size/mode/SHA256, and original manifest and key material
under `recovery/`. ZIP entries and hashes are read back before atomic publication;
only then can an operation succeed. Capture preflight checks selection and
resource roots without repeating size estimation; the asynchronous capture
performs full traversal and verification before publishing. A failed or interrupted operation never
produces a completed Django Backup record. A file deleted between a directory's
listing and its copy is not part of a live capture, except in repository
metadata, where a repack deleting the pack it replaced would leave the captured
store incomplete; there, as for any other change to a listed file, the backup
fails. An operator stopped mid-backup cannot run the backup's cleanup, so
recovering its receipt, which fails it, removes the backup's staging (which may
hold a plaintext dump) and any archive it published without a completed
receipt, and names in the receipt any of them it cannot remove. Recovery runs
only when that receipt is read; a capture process or container that outlives a
killed operator may still write into staging after it.

Original application encryption secrets are recovery material. In particular,
Django encrypted fields depend on the original SECRET_KEY: regenerating secrets
would lose access to captured ciphertext. Secrets are included as restricted
recovery files, never as JSON manifest values or log output. Treat archives as
sensitive. Key encryption/sealing is a future transport/storage concern.

Receipt output and errors are redacted for every job run, backups' included.
A multi-line secret is registered whole and per line, but only for lines that
can carry secret material: not PEM armour, and either holding at least 16
consecutive base64, base64url or hex characters (key material) or having a
value, less any `"key":` or `key:`, of at least 8 bytes with a character other
than a letter, a space, `_`, `-` or `.` (a password). Plainer lines, such as
`"enabled": true`, would otherwise be masked in all later output. A backup
receipt's output keeps each capture's output, every line labelled with its
group/name, stripped of terminal control sequences (CSI and OSC).

The native Git and JJ drivers return checked backup component plans and immutable
evidence. Platform applies stack confinement and selection and copies their
components into the archive; it does not interpret VCS pointer files.
Bundle source capture (`policy: bundle`) preserves worktree files, Git
objects/refs, and JJ repository and workspace metadata as separately located
components; `fetch_published` refusals point to it. External
Git/JJ pointers must resolve to the explicitly declared source store; capture
never follows arbitrary pointers. Cold restore must validate all paths and
remap repository metadata explicitly, retain configuration/lockfiles/generated
migrations, and use trusted recovery tooling. It must never execute ZIP-provided
shell commands. Independent live database dumps are internally consistent per
database; v1 does not claim a global cross-service point-in-time snapshot.

## Authorization boundary

Backup preview/start/download require the configured operator admin bearer.
Minted browser/connection tokens are rejected, regardless of their advisory
scope. Django supplies the admin bearer after enforcing application policy.

Existing non-backup APIs still accept minted tokens without enforcing their
advisory scopes, including job, file and secret operations. This pre-existing
control-plane authority debt requires a coordinated migration at the shared
authentication/API boundary; the backup guard does not make those tokens safe
for untrusted clients. No backup archive or JSON manifest claims otherwise.

## Native local jobs

Capture jobs run through the existing runtime JobBackend using process-compose's
foreground `run --no-server --no-deps`. The selected trusted job gets a private
configuration and process group; it cannot mutate the live supervisor project.
The existing operation receipt owns its outcome. Capture jobs never reach the
shared supervisor: a transient job runs in the foreground before any project
update, so the full-project registration of a process the supervisor has not
loaded (upstream v1.120.0 compares file-loaded int variables to JSON float64
values and restarts the file-loaded processes on that post) cannot fire during
a backup. Ordinary jobs and services keep that registration path, as in v0.22.0.

Container capture jobs run the same way, as one-off `docker compose run --rm
--no-deps -T` containers with a private name, not by recreating the job's
service in the shared compose project. A capture that times out or is
cancelled while the operator runs has its container removed with `docker rm
-f`, since killing the compose client alone leaves it running. Stopping the
operator mid-capture is best-effort: nothing waits for that removal, so the
container may run on until it finishes. Container captures write into the
backup's staging directory, which the operator owns with mode 0700, so on Linux
they must run as root or as the operator's uid.

Local transient capture currently refuses Windows before spawning: Go cannot
interrupt Windows process groups, and killing only the supervisor could orphan
its capture child. Discovery reports this native capability limit. Container
capture remains available; Windows process-tree cancellation requires a native
job-object lifecycle and Windows verification before enabling local capture.

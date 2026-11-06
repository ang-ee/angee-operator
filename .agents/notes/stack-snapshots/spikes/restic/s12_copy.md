# S12: `restic copy` changes snapshot IDs

Added after the Codex review (finding 16) with restic 0.19.1, run on 2026-11-06.

The source repository holds two stdin snapshots tagged `angee.snapshot=s1`, a member
and a manifest. The destination is initialised with its own password and
`--copy-chunker-params` (`init --from-repo … --copy-chunker-params`), then filled with
`restic copy --from-repo <src> --tag angee.snapshot=s1`.

| Repository | ID | `original` | Tags | Paths |
|---|---|---|---|---|
| src | `e4aa74e23459` | none | `angee.snapshot=s1`, `angee.kind=member` | `/databases/app/app.pgdump` |
| src | `c661595589a4` | none | `angee.snapshot=s1`, `angee.kind=manifest` | `/stack/snapshot.json` |
| dst | `dba046fb7461` | `e4aa74e23459` | `angee.snapshot=s1`, `angee.kind=member` | `/databases/app/app.pgdump` |
| dst | `247b6b4faa87` | `c661595589a4` | `angee.snapshot=s1`, `angee.kind=manifest` | `/stack/snapshot.json` |

**Verdict.** A copy gets new snapshot IDs. Tags and paths are kept, and `original`
records the source ID.

**Implication for Angee.** A manifest that names its members by restic snapshot ID is
wrong after an export. Name members by tag instead
(`angee.ns`, `angee.snapshot`, `angee.member`), keep restic IDs only as hints that
match `id` or `original`, and verify each member's content hash on restore.

## Follow-up (same day): passwords on file descriptors, and a member-only copy

Initialising the destination with `RESTIC_PASSWORD_FILE=/dev/fd/3` and
`--from-password-file /dev/fd/4` (both inherited pipes) works. So does
`restic copy --tag angee.snapshot=s1,angee.kind=member`, which copies the member and
not the manifest. Exit is 0, and the destination lists only
`[angee.snapshot=s1, angee.kind=member]`. Both passwords can stay off disk, and a
backup can copy the members first and the manifest last.

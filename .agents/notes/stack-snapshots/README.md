# Stack snapshots: evidence

Supporting material for [`../../plans/stack-snapshots.md`](../../plans/stack-snapshots.md).

- `prior-art.md`: the research report, which covers terms, per-service hooks, database
  methods, restore semantics, and restic against kopia and borg.
- `spikes/runtime/`: job stdin/stdout through `docker compose run -T` and
  process-compose 1.122.0 `run`, failure handling and throughput (R1–R7).
- `spikes/restic/`: restic 0.19.1 and rest-server 0.14.0, covering stdin members,
  multi-stream limits, producer failures, change detection, request cost, retention
  over tagged sets, locks, exit codes, dump archives, JSON shapes and stable paths
  (S1–S11).
- `spikes/databases/`: Postgres/pgvector 17 flags, verification, error handling,
  restores from stdin, a restic round trip, the version direction, SQLite live-copy
  and restore hazards, and Redis AOF against RDB (D1–D10).
- `spikes/restic/s12_copy.md`: `restic copy` gives new snapshot IDs but keeps tags and
  records `original`. Added after the review.
- `spikes/restic/s13_symlinks_and_copy_ids.md`: restic stores symlinks inside a tree as links but follows a symlinked ancestor; `copy <id>` copies exactly that ID.
- `codex-review-v2.md`: the second Codex review (of plan v2.1), saved verbatim. Plan v3 §21 maps its findings.
- `codex-review.md`: Codex's adversarial review of plan v1, saved verbatim. Plan v2
  §17 maps each finding to its resolution.

Each `RESULTS.md` records the commands, key output, verdict and implication. The
scripts beside it ran against throwaway containers and repositories under a session
scratchpad. Their paths point there and must be adjusted to rerun them. Large
payloads, repositories and dumps were not kept; the scripts regenerate them.

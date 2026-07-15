# Manual Job Terminal Output Design

**Date:** 2026-07-15

## Problem

Manual jobs started through the operator REST or GraphQL APIs still execute and
return their buffered output, but the May runtime refactor removed the output
sink that also forwarded that output into the active `angee dev` terminal.
The Django operator UI invokes GraphQL correctly; its browser log system is a
separate service/workspace log mechanism and is intentionally out of scope.

## Decision

The Go operator will configure `service.Platform` with an optional, best-effort
job-output writer. Job commands will write stdout and stderr to one shared tee:
the existing response buffer plus that writer. The operator executable supplies
its stdout as the writer, which process-compose already forwards to the active
`angee dev` terminal. Local CLI platforms do not configure the writer, so their
existing single buffered print remains unchanged.

Each run writes `[job <name>] running` before process execution and a terminal
`finished` or `failed` marker afterward. Output remains live, partial output is
preserved on failure, and local and container jobs use the same capture-and-tee
helper. Writer errors are ignored so a broken terminal sink cannot change the
job's exit result.

## Non-goals

- No Django or React changes.
- No browser job-log subscription or persistence.
- No process-compose job registration or runtime-specific rerouting.
- No REST or GraphQL response-shape change.

## Verification

Regression tests cover output arriving before a blocked local job completes,
partial output plus a failed marker, container output through a fake `docker`
executable, unchanged returned output, and no sink for direct CLI platforms.
The final gate is `make check`.

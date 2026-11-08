# S13: symlinks in file members, and `copy` by explicit IDs

Run with restic 0.19.1 on 2026-11-06, after the second Codex review (its findings B5 and
B9). Script inline in the session; the layout is below.

## Symlinks

Layout: `root/data/media/{a.txt, link-dir -> ../../outside, link-file -> outside/secret.txt}`.
The backup ran `cd root && restic backup data/media`.

| Case | Stored |
|---|---|
| Symlinks inside the captured tree | `/data/media/link-dir symlink` and `/data/media/link-file symlink`. They are stored as links, **not followed** |
| The intermediate component `data` replaced by a symlink to `outside/` before the run | `/data/media/leak.txt file`. **Followed**: content from outside the stack root was captured as `data/media/leak.txt` |

**Verdict.** restic never follows a symlink inside a tree. Path resolution of its
*arguments* follows symlinks in any ancestor component, which is the OS's normal
behaviour.

**Implication for Angee.** A file member is confined only if no untrusted writer can
change an ancestor of a captured path while restic reads it. Two consequences:
- The confined walk must Lstat every ancestor component before and after.
- Any container service with a writable mount on a strict ancestor of a captured path
  must be held for the run, or the create is refused.

A service mounting the captured directory itself cannot replace its own mount point,
and any symlinks it creates inside are stored as links.

## `copy` by explicit IDs

`restic copy --from-repo <src> <id>` with an explicit source snapshot ID copies exactly
that snapshot. The destination snapshot records `original: <id>` (`edb72a9ee461` here).
Exit is 0.

**Implication for Angee.** A backup copies the source manifest's exact, validated member
IDs, never a tag filter. It verifies one destination snapshot per member, matched by
`original`, before copying the manifest.

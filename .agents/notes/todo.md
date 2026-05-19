# Angee Operator TODO

Status snapshot after the GraphQL surface push tracked under
branch `feat/graphql-subscriptions`. Everything in the original
"GitOps API", "Operator Hardening", and "From angee-django
operator-addon plan" sections has shipped — the remaining open items
sit downstream of the six-wave push (host-side wiring, hosting
follow-ups, etc.).

See `CHANGELOG.md` (Unreleased) for the per-wave summary and
`docs/reference/operator-api.md` + `docs/guide/templates.md` for the
client-facing reference.

## Open follow-ups

- [ ] **Wire the agent-runtime template's `ACP_PORT` allocation into the
  default Stack template's port pool.** The template assumes the host
  stack declares `operator.port_pool.acp` — `templates/default/` doesn't
  do that today. Track in a follow-up once the angee-django agents
  addon's provisioning shape lands.
- [ ] **Replace the agent-runtime placeholder service command.** v1
  ships a `sleep infinity` skeleton; downstream consumers (the
  angee-django `agents` addon) will fork the template to inject the
  real ACP runtime invocation. If a canonical runtime binary lands in
  this repo, swap the placeholder for it.
- [ ] **Surface convergence + diff + token + preflight mutations on the
  CLI and REST.** Today they are GraphQL-only (tracked in
  `docs/reference/surfaces.md`). Add `angee workspace merge|rebase|...`
  CLI subcommands and matching REST endpoints when a CLI workflow
  emerges that needs them.
- [ ] **`onServiceLogs` and `onWorkspaceLogs` follow-channel sharing.**
  Each subscriber today spawns its own `docker compose logs --follow`
  process. Acceptable for v1 (operator is single-stack); revisit if
  multi-subscriber overhead becomes visible.
- [ ] **`fsnotify`-based topology change detection.** The current 2 s
  polling tick is fine for the polling-friendly UI but inefficient for
  larger stacks. Migrate when subscribers report latency complaints;
  policy note: the maintainer governance scare around fsnotify (2026-05)
  is unresolved, so pin via the community fork if/when we adopt.
- [ ] **Operator-managed `acp_token` provisioning.** The agent-runtime
  template resolves `${secret:acp_token}` against the secret backend;
  the operator currently relies on the host to put it there. Add an
  operator helper that generates a per-workspace ACP token on
  workspaceCreate when the template declares an `acp_token` requirement.
- [ ] **Cap diff response size.** `internal/service/diff.go` buffers
  `git diff` output into an unbounded `bytes.Buffer`. Mirror the
  `*LogsLimited` pattern (max bytes cap with `[truncated]` marker)
  once a real client hits a multi-GB binary diff.

## Notes

- Schema is in `internal/operator/schema.graphql`; gqlgen runtime files
  live under `internal/operator/gql/`.
- Surface-parity matrix is `docs/reference/surfaces.md` and is enforced
  by `internal/service/surface_matrix_test.go`.
- Branch: `feat/graphql-subscriptions`. Commits land per-wave.

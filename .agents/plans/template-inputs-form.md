# Template Inputs: Interactive Editing Form

**Status:** Draft · **Created:** 2026-09-05 · **Owner:** CLI / operator

`angee init`, `angee stack init`, `angee workspace create`, and
`angee service create` all take template inputs, but only the two init
commands prompt at all, and they prompt with a bare `key [default]:` line
reader. This plan replaces that with a descriptor-driven form that shows
help and choices, navigates with the keyboard, reviews every answer before
rendering, and lets the user go back and change any of them.

## 1. Inventory: how inputs flow today

| Command | Prompting today | Source of the input schema |
|---|---|---|
| `angee init` / `angee stack init` | `resolveStackTemplateInputs` in `internal/cli/root.go`: one `bufio` line per question, `key [default]: `, alphabetical, skipped for path templates (`/…`, `..`), skipped with `--yes`. | `platform.Template(ref)` → `api.TemplateDescriptor` (local, REST `/templates/{ref}`, GraphQL `template`). |
| `angee workspace create` | none; `--input k=v` only. Required inputs surface as a render error. | `WorkspaceCreatePreflight` (validates, folds in `workspace_defaults`, reports `missing_required` / `invalid_inputs`). |
| `angee service create` | none; `--input k=v` only. | `Template()` cannot describe `services/…` refs (`templateKindFromRef` maps only `workspaces/` and `stacks/`). |
| `stack update --template`, `workspace update`, `service update --template` | none; overrides via `--input`. | Recorded answers in `.copier-answers*.yml` + render state. |
| `job run` | none; `--input` (job inputs, not Copier questions). | Manifest job spec. Out of scope. |

Copier itself never prompts: `copierx.LocalRenderer.Copy` passes
`WithData(inputs)`, `WithDefaults(true)`, `WithQuiet(true)`, so every
answer must be decided before render. That is the right shape for a form:
the CLI owns the conversation, the platform owns the schema and the render.

### What the schema loses on the way to the CLI

`copierx.Input` (`internal/copierx/copierx.go`) keeps only `type`,
`required`, `default`, `immutable`, `generated`, `length`. Everything a
form needs is dropped before it reaches `api.TemplateInputDescriptor`:

- `help` — every question in `stacks/dev`, `services/claude-code`,
  `workspaces/src` has one; none is ever shown.
- `choices` — `runtime_mode`, `addons_profile`, `flavor`,
  `permission_mode` are choice questions; the prompt accepts any string and
  the bad value only fails (or silently renders garbage) later.
- `secret`, `placeholder`, `multiselect`, `when`, `validator` — not used
  by any template today (verified across the eight `copier.yml` files), but
  part of the Copier contract templates will grow into.
- **Order.** `questionsFromRaw` parses into a map and the descriptor sorts
  by name, so the dev stack asks `addon_namespace`, `addons_profile`,
  `django_image`, … before `project_name`. Copier asks in file order,
  which is the order authors design.

Other gaps found while reading the path:

- Non-TTY detection is by EOF only: piped stdin with too few lines fails
  half-way after consuming answers. `service.WithInteractive` already
  knows whether stdin is a terminal; the CLI does not use it for prompts.
- There is no `angee template describe` command, so the only way to "see
  all available options" is to open `copier.yml` in the templates repo.
- No confirmation step and no way back: a typo means Ctrl-C and restart,
  and `.copier-answers*.yml` is only readable after the render.
- `_angee.inputs` and top-level questions are declared twice in
  `stacks/dev` (both carry `operator_image`, `runtime_mode`, …); the
  descriptor already merges them, top-level wins. Keep that rule.
- Tests drive the CLI through `NewRootWithIO(stdin, …)`; the form must be
  scriptable through that reader, not only through a real TTY.

## 2. Requirements

1. Each input shows its help text; choice inputs show every option.
2. Keyboard navigation: up/down inside a choice list, left/right between
   the answers of a yes/no, next/previous between inputs without losing
   typed text.
3. Every answer is visible on one scrollable screen (defaults marked,
   secrets masked) before anything is rendered; any of them can be
   changed by moving back to it, and rendering needs an explicit confirm.
4. Non-interactive paths keep working: `--yes` accepts defaults, `--input`
   pre-fills, a non-TTY stdin never hangs and names the missing inputs with
   the exact `--input` flags to pass.
5. Works against `--operator`: the form is driven by the descriptor JSON,
   never by reading `copier.yml` on the client.
6. Same form for stack, workspace, and service templates.
7. Inputs can be supplied from a Copier answers file, so a stack or
   workspace can be re-created from a recorded `.copier-answers*.yml`
   (or a hand-written YAML) without retyping, and the form then shows
   those values pre-filled for review.
8. Updating an existing stack (and workspace or service) can open the same
   form pre-filled with the recorded answers, so the user sees what was
   answered last time, changes some, and re-renders.

## 3. Library choice

Research done 2026-09-05 (library survey, prior-art survey, and a read of
copier-go's own prompter). Sources are linked inline.

### 3a. Candidates

| Library | Verdict | Why |
|---|---|---|
| `charmbracelet/huh` | **Use.** Already in the module graph (indirect via copier-go) at v0.6.0 with bubbletea v1.1.0, bubbles, lipgloss. | Covers help (`Description`), choices (`Select`, `MultiSelect` with filtering), confirm with left/right, masked input (`EchoModePassword`), `Validate`, `Placeholder`, `Suggestions`, `Group.WithHideFunc` for conditional groups, `shift+tab` back, `ErrUserAborted`, `Form` is a `tea.Model` so it can be embedded in a custom loop. |
| plain bubbletea + bubbles | Fallback only | Full control but re-implements every widget huh already ships. |
| `AlecAivazis/survey` | No | Archived read-only since 2024-04-19. |
| `manifoldco/promptui`, `cqroot/prompt` | No | No help/review model, low activity. |
| `erikgeiser/promptkit` | No | Windows input dropped between sequential prompts. |
| `rivo/tview`, `pterm`, `gum` | No | Full TUI toolkit, basic prompts, or a separate binary. |

huh facts that shape the design (verified in the v0.6.0 source):

- Accessible mode auto-enables only for `TERM=dumb`; it reads `os.Stdin`
  directly (`accessibility/accessibility.go`), so it cannot be scripted
  through `cmd.InOrStdin()`. Non-TTY detection and the scripted fallback
  must be angee's own code.
- Ctrl-C yields `ErrUserAborted` from `Form.Run()`; when the form is
  embedded in a parent bubbletea model the parent must handle Ctrl-C
  itself ([huh discussion #273](https://github.com/charmbracelet/huh/discussions/273)).
- A `Group` renders all its fields in one scrollable viewport and moves
  focus with tab/shift+tab, so a single-screen form needs no jump-to-field
  and no separate review step.
- Versions: v0.8.0 (Oct 2025) is the last v0 line; v2.0.3 (Mar 2026)
  moved to `charm.land/huh/v2` on Bubble Tea v2
  ([releases](https://github.com/charmbracelet/huh/releases)). copier-go
  pins v0.6.0 transitively, so start there and defer the v2 jump until
  copier-go moves, to avoid two bubbletea majors in one binary.

### 3b. Prior art

- **copier** (Python) is the closest model for the input layer: `help`,
  `choices`, `secret`, `multiselect`, `validator`, `when`, and
  `--defaults`/`--data` for non-TTY. It has no review or back step
  ([docs](https://copier.readthedocs.io/en/stable/configuring/)).
- **gh pr create** is the closest model for the ending: after the draft it
  shows a "What's next?" menu (Submit / Add metadata / Continue in browser /
  Cancel) that re-enters until you submit, and errors instead of hanging
  when flags make it non-interactive.
- **npm init** ends with "Is this OK?" but offers no per-field edit;
  cookiecutter, inquirer/yeoman, aws configure, docker init, cargo-generate,
  create-vite, terraform, kubebuilder all ask sequentially with no way
  back. None of the surveyed tools has "pick any answer from the summary
  and change it"; that loop is ours to build.
- **copier-go** carries the full question set in `QuestionDef` (`Help`,
  `Choices` list/dict/Jinja, `Multiselect`, `Secret`, `Placeholder`,
  `Validator`, `When`) and exports `ShouldAsk`, `ValidateAnswer`,
  `ResolveDefault`, `ParseAnswer`, and its renderer. Its `Prompter` is
  not injectable (no `WithPrompter` option, worker hard-codes the huh
  prompter), which does not matter: angee never reaches it because
  `copierx.LocalRenderer.Copy` passes `WithData` + `WithDefaults(true)`.
  Note its dict-choice handling iterates a Go map and loses order; our
  `yaml.Node` parse must not.

### 3c. Decision

Build the form on **huh v0.6.0** driven by the enriched descriptor, with
angee-owned mode selection (interactive / scripted / defaults) and an
single scrollable screen that doubles as the review, and keep handing the
final answers to copier-go through `WithData`. Rejected: a custom
bubbletea model (re-implements huh) and upgraded line prompts alone
(cannot satisfy navigation or review). `when:`/`validator:` stay
informational in v1 and move server-side in v2 by reusing copier-go's
exported evaluation in a `TemplateValidate` platform method, so remote
operators evaluate Jinja with the template on hand.

## 4. Design

### 4.1 Descriptor enrichment (platform + API)

Extend `copierx.Input` and `api.TemplateInputDescriptor` with the Copier
question fields the form needs, keeping matching `yaml`/`json` tags:

```go
type TemplateInputDescriptor struct {
    Name        string   `json:"name"`
    Type        string   `json:"type,omitempty"`
    Required    bool     `json:"required"`
    Immutable   bool     `json:"immutable"`
    Generated   bool     `json:"generated"`
    Default     string   `json:"default,omitempty"`
    Question    bool     `json:"question"`
    Order       int      `json:"order"`                 // copier.yml position
    Help        string   `json:"help,omitempty"`
    Placeholder string   `json:"placeholder,omitempty"`
    Secret      bool     `json:"secret,omitempty"`
    Multiselect bool     `json:"multiselect,omitempty"`
    Choices     []Choice `json:"choices,omitempty"`     // {value,label}
    When        string   `json:"when,omitempty"`        // raw Jinja, informational in v1
    Validator   string   `json:"validator,omitempty"`   // raw Jinja, informational in v1
}
```

- Parse `copier.yml` with `yaml.Node` so question order survives; sort the
  descriptor by `Order`, then name for `_angee.inputs`-only entries.
- `choices` accepts the list form, the map form (label → value), and the
  Jinja string form (kept raw; the form falls back to free text).
- `templateKindFromRef` learns `services/` so `Template("services/x")`
  resolves; `resolveTemplate` already handles the `service` kind.
- Mirror in `internal/operator/schema.graphql`, regenerate gqlgen, extend
  the REST/GraphQL parity and Hasura contract tests, regenerate
  `docs/reference/graphql/` and update `docs/reference/operator-api.md`.
- Immediate, UI-free win: the existing line prompt can print help and
  `(choices: a, b)` and reject values outside the choice list.

### 4.2 `angee template describe` (and `list`)

`angee template list [--json]` and `angee template describe <ref> [--json]`
over the existing `Templates()`/`Template()` platform methods. The text
form prints one block per question:

```text
runtime_mode  (str, default: process)   choices: process, docker
  Run framework application services as local processes or Docker containers.
```

This is the non-interactive answer to "get help and see all available
options" and is what agents and scripts will use.

### 4.3 The form package: `internal/cli/inputform`

A CLI-only package (no business logic) with one entry point:

```go
type Request struct {
    Title     string                          // "Initialize stack dev"
    Inputs    []api.TemplateInputDescriptor   // ordered
    Provided  map[string]string               // --input and stack defaults
    Mode      Mode                            // Interactive | Scripted | Defaults
    In        io.Reader                       // cmd.InOrStdin()
    Out, Err  io.Writer
}
func Run(ctx context.Context, req Request) (map[string]string, error)
```

Mode selection lives in the command, not the package:

| Situation | Mode | Behaviour |
|---|---|---|
| `--yes` | Defaults | No prompts. Missing required → error listing `--input k=v` flags. |
| stdin is a TTY (`stdinIsTerminal()`) and `TERM != dumb` | Interactive | Single scrollable form with final confirm (§4.4). |
| stdin is not a TTY, or `ANGEE_ACCESSIBLE=1` | Scripted | Line prompts (today's reader, upgraded with help + choices), one line per question, EOF → error naming the input. Used by tests through `NewRootWithIO`. |

Field mapping (Interactive):

| Descriptor | Widget | Keys |
|---|---|---|
| `bool` | confirm (Yes/No) | left/right, enter |
| `choices`, single | select, filtering on when > 8 options | up/down, type to filter, enter |
| `choices`, `multiselect` | multi-select | up/down, space, enter |
| `secret` | masked text | |
| `int` | text with integer validation | |
| `path` | text with placeholder = default | |
| `str` (default) | text, placeholder = default, help as description | |
| `generated` or `immutable` or `!question` | not shown as fields; listed read-only above the confirm | |

Every field shows `help` as its description and the key hints in the
footer; layout and navigation are described in §4.4.

### 4.4 One scrollable screen: the form is the review

All questions live in a single huh `Group`, so the whole form is one
scrollable screen (huh v0.6.0 renders a group in a `bubbles/viewport`
sized from `tea.WindowSizeMsg` and scrolls to keep the focused field in
view, `group.go`). Nothing is hidden behind "next", so a separate review
screen is unnecessary:

```text
Initialize stack stacks/dev                                   3/35

  project_name   notes
    Machine name of the project host; also the chained project's name.
  runtime_mode   ▸ process   docker
    Run framework application services as local processes or containers.
    (changed)
  ingress_domain localhost
    Public DNS name … localhost keeps plain local HTTP on edge_port.
    (default)
  api_key        ********
  …
  Render the template?   ▸ Yes    No
tab next · shift+tab back · ↑↓ options · ←→ toggle · ctrl+c abort
```

- `tab`/`enter` moves to the next field, `shift+tab` back, without losing
  typed values; the viewport follows focus. Up/down navigate inside a
  choice list, left/right toggle a yes/no.
- Each field's description is its `help` plus an origin marker
  (`default`, `answers`, `flag`, `recorded`, `changed`) kept current with
  `DescriptionFunc`.
- The last field is a confirm, "Render the template?". `Yes` submits;
  `No` or ctrl+c aborts with "aborted, nothing rendered" and exit code
  130. Read-only inputs (`generated`, `immutable`, `!question`) appear as
  a note above the confirm, not as fields.
- Per-field validation runs when focus leaves the field (`Validate`), so
  an invalid choice or integer is shown inline before submit.
- After submit the CLI prints the final answers as a plain summary to
  stderr (secrets masked) so the record survives in scrollback and in
  logs, then renders.
- Fallback for very small terminals: `WithHeight` from the window size
  and the same group; if the height is below a threshold, split into
  groups of five fields, which huh pages with the same keys.

### 4.5 Conditional questions and validators

No current template uses `when:` or `validator:`. v1 carries them in the
descriptor and the form ignores them, documented as a limitation. v2 adds
`POST /templates/{ref}/validate` (platform method `TemplateValidate`) that
runs copier-go's `when`/`validator` evaluation against candidate answers;
the form calls it when focus leaves a field and before the final confirm, showing per-field errors.
Workspaces already have `WorkspaceCreatePreflight`, which the form uses
today for `missing_required` / `invalid_inputs`.

### 4.6 Command wiring

- `angee init`, `angee stack init`: replace `resolveStackTemplateInputs`
  with the form. Path templates (absolute or `..`) resolve locally through
  `Template()` now that the guard is only for remote transports; keep the
  guard for `--operator`.
- `angee workspace create`: run preflight first (gives `StackDefaults`),
  feed `EffectiveInputs` as `Provided`, run the form only when the template
  has questions the user has not answered, re-run preflight on the result.
- `angee service create`: describe `services/<ref>`, form, create.
- Update commands (`stack update --template`, `workspace update`,
  `service update --template`): `--interactive` / `-i` opens the form
  pre-filled with the recorded answers on the single scrollable screen
  (§4.8). Without `-i` they stay non-prompting, as today.
- `--yes` keeps its meaning; `--input` pre-fills and the form still shows
  (so the review step is always available); `ANGEE_ACCESSIBLE=1` forces
  the scripted line mode.

### 4.7 Inputs from an answers file

Add `--answers <file>` (repeatable) to `init`, `stack init`,
`workspace create`, `service create`, and later the update commands. The
file is any YAML mapping, including a Copier answers file as written by a
previous render (`.copier-answers.stack.yml`, `.copier-answers.yml`):

- Keys starting with `_` (`_src_path`, `_commit`, `_angee…`) are ignored.
- Scalars become strings the same way defaults do (`fmt.Sprint`); lists
  (multiselect) become a JSON array string; nested maps are rejected with
  the key name.
- The file is read by the CLI, so it works unchanged against `--operator`
  and needs no API change: the merged map goes out as `Inputs`.
- Layering, lowest to highest: template defaults → stack
  `workspace_defaults` → `--answers` files in order given → `--input` →
  edits made in the form. Each field marks its value's origin
  (`default`, `answers`, `flag`, `edited`).
- Keys the descriptor does not know are passed through with a warning
  at `-v`, matching `--input` today.
- `secret: true` answers are never written by Copier, so they stay
  prompted (or must come from `--input`); with `--yes` a missing secret is
  reported like any other missing required input.
- Under `--yes`, values from `--answers` are validated (type, choices)
  exactly like `--input`.

Follow-up (v2): a "Save answers to file…" choice beside the final confirm and
`angee template describe --answers-template` that prints a ready-to-edit
YAML with every question, its help as a comment, and its default, so the
file can be authored before the machine exists.

### 4.8 Interactive update with recorded answers

`angee stack update --template -i` (and `workspace update -i`,
`service update --template -i`) shows the previous answers and lets the
user change them before re-rendering:

- The recorded answers live on the stack root (`.copier-answers.stack.yml`
  for stacks, the answers file the render state names for workspaces and
  services), so over `--operator` they are not on the client. Add one
  platform method per target, served over REST and GraphQL like
  `Template()`:

  ```go
  // TemplateInputsResponse pairs a template descriptor with the answers a
  // previous render recorded for one target.
  type TemplateInputsResponse struct {
      Target     string             `json:"target"`      // stack | workspace/<name> | service/<name>
      Template   TemplateDescriptor `json:"template"`
      Recorded   map[string]string  `json:"recorded"`    // from the answers file, `_`-keys dropped
      Unrecorded []string           `json:"unrecorded"`  // secret answers Copier never writes
  }
  ```

  `StackTemplateInputs(ctx)`, `WorkspaceTemplateInputs(ctx, name)`,
  `ServiceTemplateInputs(ctx, name)`. Each reuses the origin resolution
  the update path already has (`serviceTemplateOrigin`, the stack and
  workspace equivalents), so the template ref and answers path are the
  same ones the re-render will use.
- The CLI opens the same single screen (§4.4) pre-filled, every field
  marked `recorded`, `immutable` inputs listed read-only, `unrecorded`
  secrets shown empty with `(not recorded)`. The confirm reads
  "Re-render the stack?".
- Submission sends the full edited map as `Inputs` to the existing
  update call, so `--dry-run` and `--overwrite` keep their meaning and the
  conflict report is unchanged. The platform keeps enforcing `immutable`
  (a changed immutable value is rejected with the key named), and the
  form does not offer to edit those rows.
- `-i` combined with `--input`/`--answers`: those pre-fill over the
  recorded values and show as `flag`/`answers` origin; `-i` without a TTY
  is an error, not a hang.
- After a successful re-render the answers file is rewritten by Copier as
  today, so the next `-i` shows the new values.

### 4.9 Tests

- `copierx`: order preserved, choices in all three forms, help/secret/
  placeholder parsed, `_angee.inputs` vs top-level precedence unchanged.
- `service`: descriptor for `stacks/`, `workspaces/`, `services/`; parity
  and Hasura contract tests updated.
- `inputform`: Scripted mode end-to-end through `NewRootWithIO`
  (help printed, choice rejected, EOF error names the input, `--yes`
  skips). Interactive mode: drive the bubbletea model with
  `Form.WithInput`/`WithOutput` and scripted key messages for one
  text, one select, one confirm, shift+tab back to change an answer, and
  the final confirm.
  A `-short`-skipped golden test renders the full screen.
- `cli`: init/workspace/service commands under `--yes`, `--input`, piped
  stdin, and interactive stub.

## 5. Delivery checklist (Codex-sized chunks)

- [x] **A. Descriptor enrichment.** `copierx` ordered parse + new fields;
      `api` + GraphQL schema + gqlgen regen; `services/` kind; line
      prompt prints help + choices and rejects bad choices; docs
      (`operator-api.md`, `graphql/`), CHANGELOG.
- [x] **B. `angee template list|describe`.** CLI over existing platform
      methods; `--json`; docs `commands.md`.
- [ ] **C. `inputform` package.** Modes, field mapping, single-screen
      group with origin markers and final confirm, small-terminal paging,
      abort semantics, tests via scripted mode and driven bubbletea model.
- [ ] **D. Wire init / stack init.** Replace `resolveStackTemplateInputs`;
      keep `--yes`; add `--answers <file>` with the layering and
      conversion rules of §4.7; tests including a round trip from a
      rendered `.copier-answers.stack.yml`.
- [ ] **E. Wire workspace create and service create.** Preflight first;
      stack defaults as pre-fill; `--answers`; tests.
- [ ] **F. Interactive update.** `TemplateInputsResponse` and the three
      platform methods over REST/GraphQL (parity + Hasura contract tests);
      `-i` on `stack update --template`, `workspace update`,
      `service update --template`; review-first form mode; tests including
      a stack rendered, updated with one changed answer, and re-rendered.
- [ ] **G. Docs + release.** `commands.md` (interactive section, key
      map, env vars), `templates.md` (author guidance: write `help`,
      `choices`, `placeholder`; keep questions in the order to ask),
      CHANGELOG `v0.13.0`, tag.
- [ ] **H. (later)** "Save answers to file…" beside the final confirm;
      `describe --answers-template`; `TemplateValidate` for
      `when`/`validator`.

## 6. Open questions

- Should `--input` for a choice question with an invalid value fail fast
  (proposed: yes, same validation as the form) even under `--yes`?
- Answers-file lists: JSON array string versus comma-separated for
  multiselect values in `map[string]string` inputs. Proposed: JSON, since
  values may contain commas.
- Whether `angee dev` should surface the same form when it bootstraps a
  missing stack, or keep delegating to `angee init`.

package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/fyltr/angee/internal/projmode"
	"github.com/fyltr/angee/internal/projmode/django"
	"github.com/fyltr/angee/internal/tmpl"
)

// runInitRuntimeOnly is the project-mode counterpart to the compose-mode
// flow in runInit. Triggered when `.angee-template.yaml` declares a
// language `runtime:` and an empty `services:` list — see R-15 / R-16.
//
// Steps (mirroring docs/ARCHITECTURE.md §12.7):
//
//  1. Resolve secrets (e.g. django-secret-key) just like compose-mode.
//  2. Create `.angee/data/{staticfiles,mediafiles,logs,tmp}/`.
//  3. Render `project.yaml.tmpl` → `.angee/project.yaml`.
//  4. Render `gitignore.tmpl` → `<project>/.gitignore` (if absent).
//  5. Merge `pyproject.angee.tmpl` into the consumer's `pyproject.toml`.
//  6. Write `.angee/.env` with secrets (gitignored).
//  7. Run the runtime adapter's `migrate` Dispatch (best-effort).
//  8. Run `loaddata` for each `fixtures:` entry (best-effort).
//
// Everything after step 5 is best-effort — a fresh project may not be
// fully wired yet (e.g. settings.py importing a missing module). The
// user can re-run `angee migrate` / `angee fixtures load` after fixing.
func runInitRuntimeOnly(
	projectRoot, templateDir string,
	meta *tmpl.TemplateMeta,
	params tmpl.TemplateParams,
	supplied map[string]string,
	promptFn tmpl.PromptFunc,
) error {
	angeeDir := filepath.Join(projectRoot, ".angee")
	dataDir := filepath.Join(angeeDir, "data")

	// 1. Secrets.
	secrets, err := tmpl.ResolveSecrets(meta, supplied, params.ProjectName, promptFn)
	if err != nil {
		return err
	}

	// 2. Create .angee/data subdirs. Idempotent.
	for _, sub := range []string{
		"", // .angee/ itself
		"data",
		filepath.Join("data", "staticfiles"),
		filepath.Join("data", "mediafiles"),
		filepath.Join("data", "logs"),
		filepath.Join("data", "tmp"),
	} {
		if err := os.MkdirAll(filepath.Join(angeeDir, sub), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	printSuccess(fmt.Sprintf(
		"Created .angee/data/{staticfiles,mediafiles,logs,tmp} at %s",
		dataDir,
	))

	// 3. Render project.yaml.tmpl → .angee/project.yaml. The template is
	// optional; if absent we synthesize a minimal manifest from the meta.
	projectYAML, err := renderTemplateFile(
		filepath.Join(templateDir, "project.yaml.tmpl"), params,
	)
	if err == nil {
		dst := filepath.Join(angeeDir, "project.yaml")
		if err := os.WriteFile(dst, []byte(projectYAML), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", dst, err)
		}
		printSuccess(fmt.Sprintf("Wrote .angee/project.yaml (runtime: %s)", meta.Runtime))
	} else if os.IsNotExist(err) {
		// Synthesize a minimal manifest pointing at src/manage.py for
		// django-angee. New runtimes can extend this.
		if meta.Runtime == django.Name {
			body := fmt.Sprintf(`version: 1
runtime: %s
django:
  manage_py: src/manage.py
  invoker:   uv
  uv:
    project: .
`, meta.Runtime)
			dst := filepath.Join(angeeDir, "project.yaml")
			if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
				return fmt.Errorf("writing synthetic %s: %w", dst, err)
			}
			printSuccess("Wrote synthetic .angee/project.yaml (no project.yaml.tmpl)")
		}
	} else {
		return fmt.Errorf("rendering project.yaml.tmpl: %w", err)
	}

	// 4. .gitignore in project root, only if absent (don't clobber).
	gitignoreDst := filepath.Join(projectRoot, ".gitignore")
	if _, err := os.Stat(gitignoreDst); os.IsNotExist(err) {
		body, gerr := renderTemplateFile(
			filepath.Join(templateDir, "gitignore.tmpl"), params,
		)
		if gerr == nil {
			if err := os.WriteFile(gitignoreDst, []byte(body), 0o644); err != nil {
				return fmt.Errorf("writing .gitignore: %w", err)
			}
			printSuccess("Wrote .gitignore")
		}
	}

	// 5. Merge pyproject.angee.tmpl into pyproject.toml (idempotent).
	if err := mergePyProjectAngee(projectRoot, templateDir, params); err != nil {
		return err
	}

	// 6. .angee/.env with secrets (gitignored). Mode 0600.
	envBody := tmpl.FormatEnvFile(secrets)
	envPath := filepath.Join(angeeDir, ".env")
	if err := os.WriteFile(envPath, []byte(envBody), 0o600); err != nil {
		return fmt.Errorf("writing .angee/.env: %w", err)
	}
	printSuccess(fmt.Sprintf(
		"Wrote .angee/.env (%d secret(s) — gitignored, never committed)",
		len(secrets),
	))

	// 7-8. Best-effort migrate + loaddata. Surface failures as warnings;
	// the user can run `angee migrate` / `angee fixtures load` after.
	if err := runFrameworkPostInit(projectRoot, meta); err != nil {
		fmt.Printf("  \033[33m!\033[0m  %s\n", err)
		fmt.Printf("  \033[33m!\033[0m  re-run with: angee migrate && angee fixtures load\n")
	}

	fmt.Printf("\n\033[1m  Next steps:\033[0m\n\n")
	printInfo("angee dev    Start build watcher + runserver + frontend")
	printInfo("angee build  Re-compose runtime/")
	printInfo("angee migrate")
	fmt.Println()
	return nil
}

// renderTemplateFile is a tiny helper for the few `*.tmpl` files
// project-mode init renders directly (project.yaml.tmpl, gitignore.tmpl,
// pyproject.angee.tmpl). The compose-mode flow uses tmpl.Render on
// angee.yaml.tmpl specifically; this is a sibling for any other file.
func renderTemplateFile(path string, params tmpl.TemplateParams) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t, err := template.New(filepath.Base(path)).Parse(string(body))
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, params); err != nil {
		return "", fmt.Errorf("rendering %s: %w", path, err)
	}
	return sb.String(), nil
}

// mergePyProjectAngee appends the template's `pyproject.angee.tmpl`
// fragment to the consumer's `pyproject.toml` — but only if the
// `[tool.angee.dev.runtime` section is not already present. This keeps
// init idempotent on re-runs.
//
// We do a textual append rather than a structured TOML merge because
// the fragment is hand-authored and the rule "append blocks if the
// header is absent" is simpler to reason about than a deep merge.
func mergePyProjectAngee(
	projectRoot, templateDir string,
	params tmpl.TemplateParams,
) error {
	frag, err := renderTemplateFile(
		filepath.Join(templateDir, "pyproject.angee.tmpl"), params,
	)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no fragment to merge — fine
		}
		return err
	}
	dst := filepath.Join(projectRoot, "pyproject.toml")
	existing, err := os.ReadFile(dst)
	if err != nil {
		return fmt.Errorf("reading %s: %w", dst, err)
	}
	if strings.Contains(string(existing), "[tool.angee.dev") {
		// Already merged — leave alone.
		return nil
	}
	out := strings.TrimRight(string(existing), "\n") + "\n\n" +
		strings.TrimSpace(frag) + "\n"
	if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	printSuccess("Merged [tool.angee.dev.*] blocks into pyproject.toml")
	return nil
}

// runFrameworkPostInit invokes the runtime adapter's `migrate` and
// `fixtures load <each>` after init scaffolding. Best-effort — failures
// are reported but don't abort init.
func runFrameworkPostInit(projectRoot string, meta *tmpl.TemplateMeta) error {
	adapter, err := pickAdapter(meta.Runtime)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(projectRoot, ".angee", "project.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		return fmt.Errorf("no .angee/project.yaml — skipping post-init")
	}
	manifest, err := projmode.LoadManifest(projectRoot)
	if err != nil {
		return err
	}
	uvProject := ""
	if manifest.Django != nil && manifest.Django.UV != nil {
		uvProject = manifest.Django.UV.Project
	}
	py, err := projmode.ResolvePython(projectRoot, uvProject)
	if err != nil {
		return err
	}
	pyProject, _ := projmode.LoadPyProjectAngeeDev(projectRoot)
	ctx := projmode.Ctx{
		ProjectRoot: projectRoot,
		Manifest:    manifest,
		PyProject:   pyProject,
		Python:      py,
	}

	// migrate
	if err := runFrameworkSubcmd(ctx, adapter, "migrate", nil); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	printSuccess("Ran migrate")

	// fixtures: declared template fixtures land first (the
	// .angee-template/fixtures/*.json the scaffold ships) so the
	// users / groups they create are present when each package's
	// seed.apply() runs after.
	for _, f := range meta.Fixtures {
		fixturePath := f
		if !filepath.IsAbs(fixturePath) {
			fixturePath = filepath.Join(projectRoot, ".angee-template", f)
		}
		if _, err := os.Stat(fixturePath); err != nil {
			fmt.Printf("  \033[33m!\033[0m  fixture %q: %v\n", f, err)
			continue
		}
		// Use Django's `manage.py loaddata <path>` directly (not
		// `angee fixtures load`, which walks installed packages).
		if err := runDjangoSubcmd(ctx, "loaddata", []string{fixturePath}); err != nil {
			return fmt.Errorf("loaddata %s: %w", f, err)
		}
		printSuccess(fmt.Sprintf("Loaded fixture: %s", f))
	}

	// `manage.py angee fixtures load` walks every installed angee
	// package's `fixtures/` directory, applies the JSON files there,
	// AND invokes each package's `fixtures.seed.apply()` post-hook
	// (the idempotent helper that creates demo records, REBAC grants,
	// etc.). Skipping this leaves a freshly-init'd project with the
	// scaffold's user fixture loaded but no domain seed data — the
	// user clicks "Notes" and lands on an empty list. The hook is
	// idempotent by package contract (get_or_create everywhere) so
	// re-running init is safe.
	if err := runFrameworkSubcmd(ctx, adapter, "fixtures", []string{"load"}); err != nil {
		// Best-effort — a package without a seed.py is normal. Surface
		// any genuine failure as a warning so the user can re-run
		// `angee fixtures load` manually after diagnosing.
		fmt.Printf("  \033[33m!\033[0m  seed: %v\n", err)
	} else {
		printSuccess("Ran package seed hooks")
	}
	return nil
}

// runFrameworkSubcmd builds the *Process for `manage.py angee <sub>` via
// the adapter and runs it as a child (NOT syscall.Exec — we want to keep
// running after).
func runFrameworkSubcmd(
	ctx projmode.Ctx,
	adapter projmode.Adapter,
	sub string,
	args []string,
) error {
	p, err := adapter.Dispatch(ctx, sub, args)
	if err != nil {
		return err
	}
	return runChildSync(p)
}

// runDjangoSubcmd runs a stock-Django subcommand (e.g. `loaddata`) —
// bypasses the `angee` insertion that adapter.Dispatch does.
func runDjangoSubcmd(ctx projmode.Ctx, sub string, args []string) error {
	dj := ctx.Manifest.Django
	if dj == nil {
		return fmt.Errorf("no django manifest")
	}
	managePy := dj.ManagePy
	if !filepath.IsAbs(managePy) {
		managePy = filepath.Join(ctx.ProjectRoot, managePy)
	}
	argv := append([]string{}, ctx.Python.Args...)
	argv = append(argv, managePy, sub)
	argv = append(argv, args...)
	p := &projmode.Process{
		Name:    sub,
		Cwd:     ctx.ProjectRoot,
		Command: ctx.Python.Cmd,
		Args:    argv,
	}
	return runChildSync(p)
}

// runChildSync runs a *Process to completion synchronously, with stdio
// inherited from the parent so the user sees output in real time.
func runChildSync(p *projmode.Process) error {
	cmd := exec.Command(p.Command, p.Args...)
	cmd.Dir = p.Cwd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = mergeEnv(p.Env)
	return cmd.Run()
}

// secretPromptFnFor returns the same prompt function runInit's compose
// flow uses, factored out so runtime-only init shares it. nil when --yes.
func secretPromptFnFor(yes bool) tmpl.PromptFunc {
	if yes {
		return nil
	}
	reader := bufio.NewReader(os.Stdin)
	return func(def tmpl.SecretDef) (string, error) {
		desc := def.Description
		if desc == "" {
			desc = def.Name
		}
		fmt.Printf("  \033[1m%s\033[0m (%s): ", def.Name, desc)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
}

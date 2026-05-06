# Example Templates

Status: target after template refactor

This directory contains example Copier templates for the target Angee model described in `docs/USAGE.md`.

These templates are intentionally concrete enough to bootstrap real local and Docker workflows, but they are still examples. They assume the future init/update pipeline can resolve `_angee` metadata, render Copier templates, write `$ANGEE_ROOT/angee.yaml`, and start an ad-hoc `angee operator` for `angee dev`.

## Layout

```text
examples/templates/
  stacks/
    dev/
    staging-docker/
  workspaces/
    feature-dev/
  agents/
    personal-assistant/
    angee-developer/
```

## Local Dev Target

Target workflow for `django-angee/examples/angee-notes`:

```sh
angee init stack dev ../django-angee/examples/angee-notes \
  --template ./examples/templates/stacks/dev \
  --root ../django-angee/examples/angee-notes/.angee \
  --yes

cd ../django-angee/examples/angee-notes
angee dev
```

Equivalent planned sugar:

```sh
cd ../django-angee/examples/angee-notes
angee init --dev --template ../../../../angee-go/examples/templates/stacks/dev --yes
angee dev
```

`angee dev` should start or reuse a local ad-hoc `angee operator` that reconciles from `.angee/angee.yaml`.

The same target is captured as data in:

```text
examples/templates/targets/angee-notes-dev.yaml
```

## Docker Staging Target

```sh
angee init stack staging-docker ./staging-root \
  --template ./examples/templates/stacks/staging-docker \
  --set domain=staging.example.com \
  --secret anthropic-api-key=env:ANTHROPIC_API_KEY \
  --yes

angee up --root ./staging-root/.angee
```

The staging template is based on the shape of `~/Work/fyltr/fyltr-django/docker-compose.dev.yaml`, `docker-compose.yaml.staging`, and `.angee-template/`.

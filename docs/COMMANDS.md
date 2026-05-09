# Commands

This page documents the CLI surface implemented in this repository.

Global flags:

```sh
--root string       ANGEE_ROOT containing angee.yaml (default ".")
--operator string   operator URL for HTTP mode
--json              write JSON output
```

## Stack

```sh
angee doctor
angee init --dev [path] [--input key=value ...] [--yes] [--force]
angee stack init <template> [path] [--input key=value ...] [--yes] [--force]
angee stack update
angee stack destroy [--purge]
angee status
```

`angee init --dev` is shorthand for the `dev` stack template. The template must
be available through the local or remote template resolver.

## Runtime

```sh
angee build [service...]
angee up [service...] [--build]
angee dev [--build]
angee down
angee start <service>...
angee stop <service>...
angee restart <service>...
angee logs [service...] [--follow]
```

`angee up` starts container services only. `angee dev` starts container services
and local-process services. Runtime actions are routed by each service's
`runtime` value.

## Services

```sh
angee service init <name> [flags]
angee service update <name> [flags]
angee service destroy <name> [--stop=false]
angee service list
angee service start <service>...
angee service stop <service>...
angee service restart <service>...
angee service logs <name> [--follow]
```

Service flags:

```sh
--runtime container|local
--image image
--command arg
--env key=value
--mount uri
--port spec
--workdir uri-or-path
--start
```

If `--runtime` is omitted, `--image` creates a container service and
`--command` creates a local service.

## Jobs

```sh
angee job list
angee job run <name> [--input key=value ...]
```

`job run` executes the declared job command and writes the job output to stdout.

## Sources

```sh
angee source list
angee source fetch <name>
angee source status <name>
angee source pull <name>
angee source push <name> [--ref ref]
```

Implemented source materialization is `git` and `local`.

## Workspaces

```sh
angee workspace create <template> [--name name] [--ttl duration] [--input key=value ...] [--start]
angee workspace update <name> [--ttl duration] [--input key=value ...]
angee workspace list
angee workspace get <name>
angee workspace status [name]
angee workspace logs <name> [--follow]
angee workspace start <name>
angee workspace stop <name>
angee workspace restart <name>
angee workspace git <name>
angee workspace push <name> [--ref ref]
angee workspace open <name> [--editor vscode|idea|gh-desktop]
angee workspace destroy <name> [--purge]
```

Workspaces are rendered from Copier templates with `_angee` metadata. A
workspace can also render and control an inner stack when the template declares
a chain root. When run from inside `$ANGEE_ROOT/workspaces/<name>/...`,
`angee workspace status` may omit the name.

## Operator

```sh
angee operator [--root root] [--bind address] [--port port] [--token token]
angee --operator http://127.0.0.1:9000 status
```

Non-loopback binds require `--token`. Remote CLI mode uses the REST operator
API for supported operations.

# XFlow Runner Cobra CLI Design

Date: 2026-06-29

## Goal

Upgrade `cmd/runner` from a single `flag`-based entrypoint to a `spf13/cobra`
CLI that can grow into an operational runner tool while preserving today's
server-runner MVP behavior.

The first implementation should keep the execution-plane boundary unchanged:
runner connects only to xflow-server through the Runner Protocol, registers
capabilities, polls work, executes handlers through `execution.Runner`, and
reports results. It must not connect to Redis, Asynq, MySQL, or any StateStore.

## Scope

Implement these commands first:

```text
xflow-runner
  run              start the runner loop
  verify           validate local settings and check server/protocol reachability
  config validate  validate the resolved runner configuration
  config sample    print a sample runner configuration
```

Preserve compatibility with the current direct invocation flags:

```bash
xflow-runner run --server http://localhost:8080 --id runner-1 --concurrency 2 --cap xflow.function
```

Register/list/unregister and service management commands are intentionally out
of scope for the first implementation. The server does not yet have a durable
runner account, token, or unregister API, so implementing those commands now
would expose placeholder behavior.

## GitLab Runner-Inspired Choices

Adopt the parts that fit xflow's current architecture:

- A single binary with subcommands.
- A global `--config` flag.
- Command-specific `--help`.
- Environment variable overrides.
- A debug/log-level switch.
- A `verify` command for operational checks.
- Future room for service commands without adding them now.

Do not adopt GitLab Runner concepts that depend on CI/CD executor semantics,
runner tokens, multi-runner configuration, or OS service installers in this
first pass.

## Command Model

### `run`

`run` is the default operational command. It resolves configuration, builds the
Runner Protocol client, registers builtin node handlers, and starts
`service/runner.Runner`.

Flags:

```text
--server string              xflow-server base URL
--id string                  runner ID
--concurrency int            max in-flight tasks for this runner
--cap string                 comma-separated node type capabilities
--heartbeat-interval duration
--poll-wait duration
```

The existing defaults remain:

- `server`: `http://localhost:8080`
- `id`: `runner-<pid>`
- `concurrency`: `1`
- `cap`: `xflow.function`

`SIGINT` and `SIGTERM` keep the current graceful cancellation behavior.
`SIGQUIT` drain and hot reload can be added later after the runner service has
explicit in-flight task tracking.

### `verify`

`verify` checks that the resolved configuration is usable. First implementation
behavior:

- Validate server URL, runner ID, positive concurrency, and non-empty
  capabilities.
- Build a protocol client.
- Call the server's runner registration and heartbeat endpoints using the
  resolved configuration.

This changes the in-memory runner pool on the server, but that matches current
protocol capabilities. A later server health or dry-run endpoint can make
`verify` side-effect-free.

### `config validate`

`config validate` resolves configuration from file, environment, and flags, then
prints an error if any required value is invalid. It does not contact the server.

### `config sample`

`config sample` prints a minimal YAML configuration:

```yaml
runner:
  id: "runner-1"
  concurrency: 2
  capabilities:
    - "xflow.function"

server:
  url: "http://localhost:8080"

poll:
  wait: "1s"

heartbeat:
  interval: "5s"

logging:
  level: "info"
  format: "text"
```

## Configuration

Use YAML because existing design documentation already describes runner
configuration in YAML.

Resolution order:

```text
CLI flags > XFLOW_RUNNER_* environment variables > config file > defaults
```

Environment variables:

```text
XFLOW_RUNNER_CONFIG
XFLOW_RUNNER_SERVER
XFLOW_RUNNER_ID
XFLOW_RUNNER_CONCURRENCY
XFLOW_RUNNER_CAP
XFLOW_RUNNER_HEARTBEAT_INTERVAL
XFLOW_RUNNER_POLL_WAIT
XFLOW_RUNNER_LOG_LEVEL
XFLOW_RUNNER_LOG_FORMAT
```

The `--config` global flag defaults to `XFLOW_RUNNER_CONFIG` when set. If neither
is set, the runner uses defaults and CLI/env overrides without requiring a file.

## Package Layout

Keep cobra and config wiring inside `cmd/runner`:

```text
cmd/runner/main.go
cmd/runner/command.go
cmd/runner/config.go
cmd/runner/run.go
cmd/runner/verify.go
```

`main.go` should only execute the root command. The existing `runRunner(ctx,
cfg)` behavior can remain behind the `run` command so service-layer code does
not depend on cobra.

## Validation

Configuration validation rules:

- `server.url` must be an absolute `http` or `https` URL.
- `runner.id` must not be empty.
- `runner.concurrency` must be greater than zero.
- Capabilities must contain at least one non-empty node type.
- Durations must parse with `time.ParseDuration` and be positive when set.
- Log level must be one of `debug`, `info`, `warn`, or `error`.
- Log format must be `text` or `json`.

## Testing

Add focused tests under `cmd/runner`:

- `run` command accepts existing flags and resolves the same config as today.
- Env vars override config file values.
- CLI flags override env vars.
- `config validate` rejects invalid server URL, empty capabilities, and invalid
  durations.
- `config sample` emits parseable YAML.
- `verify` uses a fake protocol client or httptest server to prove it calls
  register and heartbeat with resolved config.

Then run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner ./service/runner ./service/protocol
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache make test
```

The existing Podman Redis/MySQL smoke tests should be rerun after the CLI change
because they exercise the real server and runner process wiring.

## Future Commands

The cobra command tree should leave room for:

```text
register      create local runner config after a server registration flow exists
list          list local configured runners
unregister    remove local runner config and call server unregister
run-single    run until one task completes or the wait timeout expires
install       install OS service
uninstall     remove OS service
start         start OS service
stop          stop OS service
restart       restart OS service
status        inspect OS service status
```

These should wait until the server owns durable runner identity, auth, and
service lifecycle requirements are clear.

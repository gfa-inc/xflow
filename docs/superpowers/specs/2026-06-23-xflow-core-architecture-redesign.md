# XFlow Core Architecture Redesign

## Goal

Reshape xflow from an embedded SDK-first DAG runner into a clean workflow
runtime foundation that can grow into a server/runner platform comparable in
shape to mature workflow systems.

The project is still early. Breaking public APIs is acceptable when it makes
the core easier to use, easier to extend, and easier to operate later.

## Design Principles

- Keep the user-facing SDK small and semantic.
- Keep engine pure: graph state machine, scheduling, lease creation, result
  commit, signal/suspend transitions.
- Move handler execution out of engine into runtime.
- Model everything on the canvas as a node, but separate action and trigger
  runtime interfaces.
- Prefer explicit state and protocol types over stringly typed status values.
- Keep backend implementations reusable, but do not expose backend/provider
  concepts through the primary SDK.

## Target Package Boundaries

```text
types/        DSL, statuses, handler contracts, descriptors, handler IO, protocol-neutral IDs
node/         builtin action node constructors and implementations
engine/       pure workflow state machine and DAG scheduler
runtime/      handler registry, dispatcher, executor, suspend-aware execution
backend/      provider assembly contract and backend contract tests
backend/memory
backend/asynq or backend/redis
sdk/xflow     minimal user-facing SDK and workflow builder
service/      future server control plane
runner/       future runner process/runtime
cmd/          thin binaries only
```

`backend/` remains the package for concrete backend implementations and
assembly. The engine-level state interface must not be named `Backend`, because
that conflicts with the existing package meaning.

## Engine State Interfaces

Replace the current fat legacy state backend with `engine.StateStore`, a composed
interface made from smaller capabilities:

```go
type StateStore interface {
    Executions
    Graphs
    Nodes
    Outputs
    Scheduling
    Signals
    SubExecutions
    Events
}
```

Capability names:

- `Executions`: create, read, and update execution lifecycle.
- `Graphs`: save and load compiled graph IR.
- `Nodes`: start and complete node state with idempotency.
- `Outputs`: persist and retrieve node outputs.
- `Scheduling`: initialize indegree counters, record arrivals, and check
  completion.
- `Signals`: suspend, deliver, revoke, resume-lock, and resuspend semantics.
- `SubExecutions`: loop/split child execution tracking.
- `Events`: execution events for waiters, hooks, and future notifications.

`engine.New` should accept:

```go
func New(state StateStore, queue TaskQueue, opts ...Option) *Engine
```

`TaskQueue` stays as the engine task delivery interface, but methods should be
node-task-specific:

```go
type TaskQueue interface {
    EnqueueNode(ctx context.Context, task *Task) error
    EnqueueNodeAfter(ctx context.Context, task *Task, delay time.Duration) error
}
```

## Status Types

Split execution and node lifecycle terms:

```go
type ExecutionStatus string
type NodeStatus string
```

Execution statuses include `pending`, `running`, `success`, `failed`,
`canceling`, `canceled`, and `timeout`.

Node statuses include `pending`, `running`, `success`, `failed`, `skipped`,
`suspended`, `continued`, and `canceled`.

Engine and backend code must use these typed constants. Literal status strings
are allowed only inside JSON/database serialization boundaries and tests that
assert wire values.

## Engine Runtime Boundary

Engine must not execute handlers. Remove the long-term dependency from engine
to handler registry and direct handler execution.

Engine exposes:

```go
func (e *Engine) Submit(ctx context.Context, g *graph.Graph, params map[string]any) (types.ExecutionID, error)
func (e *Engine) AcquireTaskLease(ctx context.Context, task *Task) (*TaskLease, error)
func (e *Engine) CommitTaskResult(ctx context.Context, token LeaseToken, result TaskResult) error
func (e *Engine) DeliverSignal(ctx context.Context, id types.ExecutionID, name string, data map[string]any) error
func (e *Engine) Cancel(ctx context.Context, id types.ExecutionID) error
```

`TaskLease` carries routing metadata, input data, node type, version, attempt,
and a fencing token. `CommitTaskResult` must reject stale or duplicate tokens.

## Runtime Package

`runtime/` owns handler execution:

```go
type Executor interface {
    Execute(ctx context.Context, lease *engine.TaskLease) (engine.TaskResult, error)
}

type Registry interface {
    Handler(nodeType string, version int) (node.Handler, error)
}
```

`runtime.Dispatcher` performs:

1. Acquire task lease from engine.
2. Execute via embedded or remote executor.
3. Commit result back to engine.

Embedded execution remains the first implementation. Remote execution is
enabled by the same lease/result boundary, not by adding handler execution back
into engine.

## Action and Trigger Nodes

XFlow follows the n8n-style conceptual model: a workflow canvas contains nodes,
and trigger nodes are nodes that start executions. However, action nodes and
trigger nodes have different runtime capabilities.

DSL/UI layer:

```go
type NodeKind string

const (
    NodeAction  NodeKind = "action"
    NodeTrigger NodeKind = "trigger"
)

type NodeDef struct {
    Name       string
    Type       string
    Kind       NodeKind
    Version    int
    Parameters map[string]any
}
```

Runtime layer:

```go
type Handler interface {
    Descriptor() Descriptor
}

type ActionHandler interface {
    Handler
    Execute(ctx context.Context, input *Input) (*Output, error)
}

type TriggerHandler interface {
    Handler
    Activate(ctx context.Context, input *TriggerInput, emit TriggerEmitter) (*TriggerSubscription, error)
    Deactivate(ctx context.Context, sub *TriggerSubscription) error
}
```

Optional trigger capabilities:

```go
type PollingTrigger interface {
    Poll(ctx context.Context, input *TriggerInput) ([]*Output, error)
}

type WebhookTrigger interface {
    WebhookSpec(ctx context.Context, input *TriggerInput) (*WebhookSpec, error)
    HandleWebhook(ctx context.Context, req *WebhookRequest) (*Output, error)
}

type ScheduleTrigger interface {
    Schedule(ctx context.Context, input *TriggerInput) (*ScheduleSpec, error)
}
```

Graph rules:

- A trigger node is still a node in `nodes`.
- Trigger nodes may have outgoing edges.
- Trigger nodes must not have incoming edges.
- Action nodes may be roots in SDK/local/testing mode.
- Platform/server mode may require at least one trigger node.
- Trigger emit creates a new execution whose initial input is the trigger
  output.

## SDK Builder

The primary SDK should expose few methods and hide engine/runtime/backend
concepts.

Primary user API:

```go
xflow.NewLocal(...)
xflow.NewCluster(...)
xflow.NewRemote(...) // future

xflow.Workflow(name)

engine.Submit(ctx, workflow, params...)
engine.Wait(ctx, id)
engine.Signal(ctx, id, name, data)
engine.Cancel(ctx, id)
engine.Status(ctx, id)
engine.Stop()
```

Workflow builder:

```go
wf := xflow.Workflow("purchase-approval")

start := wf.Node("manual", node.ManualTrigger())
risk := wf.Node("risk-check", node.Function(checkRisk))
approve := wf.Node("approval", node.Approval(...))
notify := wf.Node("notify", node.HTTP(...))

wf.Connect(start, risk)
wf.Connect(risk.Output("review"), approve)
wf.Connect(risk.Output("approved"), notify)
wf.Connect(approve, notify)
```

Named input ports:

```go
wf.Connect(fetchUser.Output("main"), merge.Input("user"))
wf.Connect(fetchOrder.Output("main"), merge.Input("order"))
```

Local-only direct handlers must be explicit:

```go
wf.LocalNode("mock", handler)
```

Do not keep panic-prone generic node addition API or `Connect(src, dst any)` as the
main API. If generic connection handling remains internally, wrap it behind
typed public methods.

SDK must not expose:

- `StateStore`
- `TaskQueue`
- `Provider`
- `Registry`
- `Dispatcher`
- `TaskLease`
- `LeaseToken`
- `TaskResult`

Advanced extension hooks can live outside the primary SDK surface after there
is real demand.

## Backend Provider

`backend.Provider` remains the assembly contract:

```go
type Provider interface {
    State() engine.StateStore
    Queue() engine.TaskQueue
    Registry() execution.Registry
    Bind(rt *runtime.Runtime) func()
}
```

The exact `Bind` signature may change if `runtime.Runtime` is unnecessary, but
the provider should assemble state, queue, registry, and lifecycle. It should
not become the user-facing abstraction.

## Implementation Batches

### Batch 1: Status and Naming Cleanup

- Introduce typed execution and node statuses.
- Replace node status string literals across engine and backend code.
- Update docs that still reference `sdk/internal/adapter`, `.Codex`, or old
  adapter names.
- Keep current behavior.

### Batch 2: StateStore Split

- Replace fat legacy state backend with `StateStore` and capability interfaces.
- Update memory and Redis/Asynq implementations.
- Add backend contract tests for memory and Redis when available.

Contract coverage:

- execution lifecycle
- graph save/load
- node start and terminal idempotency
- indegree arrivals
- completion check
- signal pre-delivery
- suspend/consume race behavior
- resume lock
- resuspend
- output storage
- event watch/publish

### Batch 3: Runtime Separation

- Move handler execution out of engine.
- Replace engine-side handler execution fallback with suspend-aware runtime result types.
- Ensure suspending handlers work through lease/result commit.

### Batch 4: SDK Builder Redesign

- Replace legacy workflow builder API with `Workflow`, `Node`,
  `Connect`, `Input`, and `Output`.
- Remove generic node addition API from the public path.
- Update examples and docs.

### Batch 5: Runner Protocol MVP Types

- Define `LeaseID`, `LeaseToken`, `TaskLease`, `TaskResult`,
  `RunnerHeartbeat`, and `RunnerCapability`.
- Add stale-token and duplicate-commit tests.
- Do not implement full server/gateway in this batch.

## Verification Strategy

Each batch must use test-first changes where behavior changes.

Minimum commands:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./engine ./engine/graph ./backend ./backend/memory ./store ./store/memstore ./store/sqlstore ./types
GOCACHE=/private/tmp/xflow-gocache go test ./sdk/...
```

After the `runtime/` package is introduced, include it in the first command:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./runtime
```

Full repository verification remains:

```bash
go test ./...
```

Some tests may require unsandboxed local networking because existing `node`
tests use `httptest`. If sandboxed `go test ./...` fails due to local listen or
Go cache permissions, rerun with a writable `GOCACHE` and document the
remaining environment limitation.

## Acceptance Criteria

- `engine` no longer resolves or executes node handlers.
- `engine` depends on `StateStore` and `TaskQueue`, not a fat backend
  interface.
- Runtime owns handler registry and action/trigger execution.
- Trigger is a node kind, not a separate SDK object.
- The primary SDK builder uses `Workflow`, `Node`, `Connect`, `Input`, and
  `Output`.
- `sdk/xflow` does not expose provider/state/queue/runtime internals.
- Core tests and SDK examples use the redesigned APIs.
- Current architecture docs match the code.

# XFlow Core Architecture Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor xflow toward a platform-ready workflow runtime with typed state, smaller engine state boundaries, runtime-owned execution, and a simpler SDK builder.

**Architecture:** Implement the redesign in five batches. Start with typed statuses and naming cleanup, then split state capabilities, then move handler execution into runtime, then redesign SDK builder, and finally add runner protocol MVP types. Keep behavior green after every batch.

**Tech Stack:** Go 1.25+, standard library, Redis/Asynq backend, existing `types`, `node`, `engine`, `execution`, `backend`, `sdk/xflow`, and tests.

## Global Constraints

- Work directly in the current checkout because the user declined an isolated worktree.
- Do not revert unrelated existing workspace changes.
- Use TDD for behavior changes: write or update the failing test, verify failure, implement, verify pass.
- Do not expose `StateStore`, `TaskQueue`, `Provider`, `Registry`, `Dispatcher`, `TaskLease`, `LeaseToken`, or `TaskResult` through the primary `sdk/xflow` API.
- Keep `backend/` as the concrete backend implementation and assembly package.
- Name the engine state composition `engine.StateStore`, not `Backend`.
- SDK builder public naming must use `Workflow`, `Node`, `Connect`, `Input`, and `Output`; do not use `When/Then`.
- Trigger is a node kind, not a separate SDK object. Runtime interfaces for action and trigger nodes are separate.
- Because git commit approval is currently blocked, each task ends with a verification checkpoint rather than a commit.

---

### Task 1: Typed Execution and Node Statuses

**Files:**
- Modify: `types/execution.go`
- Modify: `engine/types.go`
- Modify: `engine/errorpolicy.go`
- Modify: `engine/engine.go`
- Modify: `engine/scheduler.go`
- Modify: `backend/memory/memory_state.go`
- Modify: `backend/asynq/redis_state.go`
- Modify: `sdk/xflow/xflow.go`
- Test: `engine/errorpolicy_test.go`
- Test: `engine/scheduler_test.go`
- Test: `engine/suspend_test.go`

**Interfaces:**
- Produces: `types.ExecutionStatus`, `types.NodeStatus`, `types.IsTerminalExecutionStatus`, `types.IsTerminalNodeStatus`.
- Consumes: existing `types.ExecutionStatus` use sites, migrated to `types.ExecutionStatus` where practical.

- [ ] **Step 1: Write/update failing tests for typed node status**

Add assertions in `engine/errorpolicy_test.go` that `ApplyOnError` returns `types.NodeStatusFailed`, `types.NodeStatusSuccess`, and `types.NodeStatusContinued`, not raw strings.

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./engine -run TestApplyOnError -count=1
```

Expected: fail or compile fail because `types.NodeStatus*` and typed outcome are not implemented yet.

- [ ] **Step 3: Implement typed statuses**

In `types/execution.go`, define:

```go
type ExecutionStatus string
type NodeStatus string

const (
    ExecutionPending   ExecutionStatus = "pending"
    ExecutionRunning   ExecutionStatus = "running"
    ExecutionSuccess   ExecutionStatus = "success"
    ExecutionFailed    ExecutionStatus = "failed"
    ExecutionCanceling ExecutionStatus = "canceling"
    ExecutionCanceled  ExecutionStatus = "canceled"
    ExecutionTimeout   ExecutionStatus = "timeout"
)

const (
    NodePending   NodeStatus = "pending"
    NodeRunning   NodeStatus = "running"
    NodeSuccess   NodeStatus = "success"
    NodeFailed    NodeStatus = "failed"
    NodeSkipped   NodeStatus = "skipped"
    NodeSuspended NodeStatus = "suspended"
    NodeContinued NodeStatus = "continued"
    NodeCanceled  NodeStatus = "canceled"
)
```

Keep compatibility aliases only where needed to keep the current tree compiling during this batch.

- [ ] **Step 4: Migrate engine/backend status fields**

Change `engine.ExecutionSnapshot.Status` to `types.ExecutionStatus`, `engine.NodeSnapshot.Status` to `types.NodeStatus`, `OnErrorOutcome.NodeStatus` to `types.NodeStatus`, and migrate direct string comparisons to typed constants.

- [ ] **Step 5: Verify task**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./engine ./engine/graph ./backend ./backend/memory ./store ./store/memstore ./store/sqlstore ./types
```

Expected: all selected packages pass.

---

### Task 2: StateStore Capability Interfaces

**Files:**
- Modify: `engine/interfaces.go`
- Modify: `backend/backend.go`
- Modify: `backend/memory/memory_state.go`
- Modify: `backend/asynq/redis_state.go`
- Test: `backend/contract/state_store_test.go`

**Interfaces:**
- Consumes: typed status constants from Task 1.
- Produces: `engine.StateStore` composed from `Executions`, `Graphs`, `Nodes`, `Outputs`, `Scheduling`, `Signals`, `SubExecutions`, and `Events`.

- [ ] **Step 1: Write backend contract tests**

Create contract tests covering execution lifecycle, graph load, node terminal idempotency, indegree arrival, completion, output storage, signal pre-delivery, suspend-consume, resume lock, and event watch/publish against memory backend.

- [ ] **Step 2: Run contract tests to verify failure**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./backend/contract -count=1
```

Expected: fail because capability interfaces and memory event implementation are incomplete.

- [ ] **Step 3: Split interfaces**

Replace the monolithic legacy state backend definition with small capability interfaces and `type StateStore interface { ... }`.

- [ ] **Step 4: Update providers and state implementations**

Update `backend.Provider.State()` to return `engine.StateStore`. Update memory and asynq state implementations to satisfy the split interfaces.

- [ ] **Step 5: Verify task**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./backend/contract ./engine ./backend ./backend/memory ./types
```

Expected: all listed packages pass.

---

### Task 3: Runtime-Owned Handler Execution

**Files:**
- Create: `runtime/dispatcher.go`
- Create: `runtime/registry.go`
- Create: `runtime/runner.go`
- Delete the old legacy execution package compatibility package after migrating to `runtime/`
- Modify: `engine/engine.go`
- Modify: `engine/types.go`
- Test: `runtime/dispatcher_test.go`
- Test: `engine/runner_commit_test.go`
- Test: `engine/suspend_test.go`

**Interfaces:**
- Consumes: `engine.AcquireTaskLease`, `engine.CommitTaskResult`, typed statuses, and `types.ActionHandler`/`types.TriggerHandler`.
- Produces: runtime dispatcher and embedded executor path.

- [ ] **Step 1: Write failing runtime dispatcher tests**

Test that dispatcher acquires a lease, executes an action handler, and commits a result through `CommitTaskResult`.

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./runtime -count=1
```

Expected: fail because `runtime/` does not exist or dispatcher is incomplete.

- [ ] **Step 3: Introduce runtime package**

Move the execution boundary into `runtime/`, with names aligned to the spec.

- [ ] **Step 4: Remove long-term engine handler execution path**

Replace engine-side handler execution with lease/result paths. Suspension is represented as a runtime result committed back to engine.

- [ ] **Step 5: Verify task**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./runtime ./engine ./backend/memory
```

Expected: all listed packages pass.

---

### Task 4: SDK Builder Redesign

**Files:**
- Modify: `sdk/xflow/builder.go`
- Modify: `sdk/xflow/xflow.go`
- Modify: `sdk/examples/basic_test.go`
- Modify: `sdk/examples/cluster_test.go`
- Test: `sdk/xflow/builder_test.go`

**Interfaces:**
- Consumes: graph compile and runtime registry behavior.
- Produces: `xflow.Workflow`, `(*Workflow).Node`, `(*Workflow).LocalNode`, `(*Workflow).Connect`, `(*Node).Input`, `(*Node).Output`, `(*Node).Body`.

- [ ] **Step 1: Write failing builder tests**

Test that `Workflow("wf").Node(...).Connect(...)` builds the expected `types.WorkflowDef` nodes and connections, including named input ports.

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./sdk/xflow -run TestWorkflowBuilder -count=1
```

Expected: fail because the new builder API does not exist.

- [ ] **Step 3: Implement typed SDK builder API**

Implement `Workflow`, `Node`, `LocalNode`, `Connect`, `Input`, `Output`, and `Body`. Keep old API only if needed as an internal shim during migration.

- [ ] **Step 4: Update examples**

Rewrite SDK examples to use `Workflow`, `Node`, `Connect`, `Input`, and `Output`.

- [ ] **Step 5: Verify task**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./sdk/...
```

Expected: SDK packages pass.

---

### Task 5: Runner Protocol MVP Types

**Files:**
- Modify: `engine/types.go`
- Create or modify: `runtime/protocol.go`
- Test: `engine/task_wire_test.go`
- Test: `engine/runner_commit_test.go`

**Interfaces:**
- Consumes: runtime lease/result boundary from Task 3.
- Produces: `LeaseID`, `LeaseToken`, `TaskLease`, `TaskResult`, `RunnerHeartbeat`, and `RunnerCapability`.

- [ ] **Step 1: Write stale token tests**

Add tests proving duplicate and stale commit tokens do not advance the DAG twice.

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./engine -run 'Test.*(Lease|Commit|Token)' -count=1
```

Expected: fail because token semantics are incomplete.

- [ ] **Step 3: Add protocol-neutral types**

Define lease/result/heartbeat/capability types and wire JSON tags.

- [ ] **Step 4: Enforce commit fencing**

Persist or derive commit token state so duplicate/stale results are ignored.

- [ ] **Step 5: Verify task**

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./engine ./runtime ./backend/memory ./sdk/...
```

Expected: all listed packages pass.

---

## Final Verification

Run:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./engine ./engine/graph ./runtime ./backend ./backend/memory ./store ./store/memstore ./store/sqlstore ./types ./sdk/...
```

Then attempt:

```bash
GOCACHE=/private/tmp/xflow-gocache go test ./...
```

If full repository tests fail due sandbox-local networking in `httptest`, report the exact environment failure and the narrower passing verification command.

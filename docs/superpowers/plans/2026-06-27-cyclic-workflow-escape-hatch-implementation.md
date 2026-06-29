# Cyclic Workflow Escape Hatch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add opt-in cyclic workflow support while preserving DAG behavior by default.

**Architecture:** `types.WorkflowOptions` configures compile and runtime behavior. Compiled graphs carry cyclic metadata, the engine starts cyclic workflows from one `xflow.start`, and cyclic scheduling follows the active output edge directly instead of using in-degree counters. Runtime stores latest node state/output only, with activation fencing to let newer cyclic entries overwrite older terminal node state without allowing stale commits.

**Tech Stack:** Go, existing `engine`, `engine/graph`, `types`, `nodes/node`, `backend/memory`, and existing tests.

## Global Constraints

- `engine/` must not import redis/asynq/mysql/sql.
- Core packages must not import `service/` or `cmd/`.
- Default workflows remain DAGs and still reject cycles.
- Cyclic mode requires `options.allow_cycles: true`.
- Cyclic v1 requires exactly one `xflow.start`.
- `xflow.trigger` is rejected for now.
- `max_auto_depth <= 0` defaults to `100`.
- Runtime history is latest-state only; external systems own audit/history.

---

### Task 1: Compile Options And Validation

**Files:**
- Modify: `types/workflow.go`
- Modify: `engine/graph/graph.go`
- Modify: `engine/graph/compile.go`
- Test: `engine/graph/compile_test.go`

**Interfaces:**
- Produces: `types.WorkflowOptions`
- Produces: `graph.Graph.AllowCycles`, `graph.Graph.MaxAutoDepth`, `graph.Graph.StartIdx`
- Produces: compile validation for cyclic workflows

- [x] **Step 1: Write failing compile tests**

Add tests covering default cycle rejection, opt-in cycle allowance, missing start, multiple starts, trigger rejection, and wait-all merge rejection.

- [x] **Step 2: Run compile tests and verify failure**

Run: `go test ./engine/graph`
Expected: fail because `WorkflowDef.Options` and graph cyclic metadata do not exist.

- [x] **Step 3: Implement options and validation**

Add `WorkflowOptions`, copy options into graph metadata, keep default `detectCycle`, skip only when `AllowCycles` is true, and validate start/trigger/merge constraints.

- [x] **Step 4: Run compile tests**

Run: `go test ./engine/graph`
Expected: pass.

### Task 2: Start Node

**Files:**
- Create: `nodes/node/start.go`
- Test: `nodes/node/start_test.go`

**Interfaces:**
- Produces: built-in `xflow.start` action handler.
- Produces: `node.Start() Builder`.

- [x] **Step 1: Write failing node tests**

Verify descriptor type and that execution returns submission input data.

- [x] **Step 2: Run node tests and verify failure**

Run: `go test ./nodes/node`
Expected: fail because `Start` is undefined.

- [x] **Step 3: Implement `xflow.start`**

Add a small handler that returns `input.Data` on `main` and registers itself in `init`.

- [x] **Step 4: Run node tests**

Run: `go test ./nodes/node`
Expected: pass.

### Task 3: Runtime Cyclic Scheduling

**Files:**
- Modify: `engine/types.go`
- Modify: `engine/engine.go`
- Modify: `engine/scheduler.go`
- Modify: `engine/fake_test.go`
- Modify: `backend/memory/memory_state.go`
- Test: `engine/scheduler_test.go`
- Test: `engine/runner_commit_test.go`

**Interfaces:**
- Produces: `Task.AutoDepth`, `Task.ActivationID`
- Produces: `NodeSnapshot.ActivationID`
- Produces: cyclic submit from `StartIdx`
- Produces: edge-driven cyclic scheduling and max-auto-depth failure

- [x] **Step 1: Write failing runtime tests**

Verify cyclic submit starts at `xflow.start` even with incoming edges, a return edge re-enters a terminal node and overwrites latest output, max auto depth fails automatic cycles, and stale activation tasks cannot overwrite newer activation state.

- [x] **Step 2: Run engine tests and verify failure**

Run: `go test ./engine`
Expected: fail because cyclic runtime fields and scheduling do not exist.

- [x] **Step 3: Implement task activation and submit behavior**

Add activation/depth fields, start cyclic workflows from `StartIdx`, reset resume auto-depth to zero, and carry activation ids through leases and node snapshots.

- [x] **Step 4: Implement cyclic scheduler branch**

When `g.AllowCycles`, enqueue active-port downstream nodes directly, increment auto depth, fail execution when the next depth exceeds `MaxAutoDepth`, and mark quiescent cyclic executions successful.

- [x] **Step 5: Update in-memory stores**

Allow newer activation ids to overwrite older terminal snapshots, reject stale leases, and keep DAG zero-activation behavior unchanged.

- [x] **Step 6: Run engine tests**

Run: `go test ./engine`
Expected: pass.

### Task 4: SDK Builder And Docs

**Files:**
- Modify: `sdk/xflow/builder.go`
- Modify: `sdk/xflow/builder_test.go`
- Modify: `docs/design/DSL-SPECIFICATION.md`

**Interfaces:**
- Produces: builder method to set cyclic options.
- Produces: DSL documentation for `options.allow_cycles`, `max_auto_depth`, and `xflow.start`.

- [x] **Step 1: Write failing SDK test**

Verify builder can emit `WorkflowDef.Options` and allows cycles only after explicit opt-in.

- [x] **Step 2: Run SDK tests and verify failure**

Run: `go test ./sdk/xflow`
Expected: fail because builder cyclic options do not exist.

- [x] **Step 3: Implement builder option**

Add `AllowCycles(maxAutoDepth int)` on `WorkflowBuilder`, pass options to `WorkflowDef`, and skip builder-level cycle detection only in that mode.

- [x] **Step 4: Update DSL docs**

Document options and start node semantics.

- [x] **Step 5: Run relevant tests**

Run: `go test ./engine/graph ./nodes/node ./engine ./sdk/xflow`
Expected: pass.

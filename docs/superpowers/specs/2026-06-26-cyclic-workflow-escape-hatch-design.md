# Cyclic Workflow Escape Hatch Design

## Problem

xflow currently treats every workflow as a DAG. The graph compiler rejects
cycles, and the engine scheduler advances nodes by decrementing downstream
in-degree until a node becomes ready.

That model fits task orchestration, but it does not fit UI-authored approval
and lifecycle flows that intentionally return to an earlier state. A
vulnerability lifecycle is a typical example:

```text
start -> submit -> review -> assign -> fix -> retest -> publish -> verify -> fixed
                                      ^         |
                                      |         |
                                      +---------+
```

The return edge can represent approval rejection, failed retest, failed
acceptance, delayed remediation, or risk-report expiry. Users expect the UI to
let them draw these edges directly.

A full stateflow or BPMN-style runtime would model this cleanly, but it is a
large product and engine expansion. The near-term goal is smaller: allow
explicitly opted-in cyclic workflows while preserving the existing DAG behavior
by default.

## Goals

- Keep default workflow behavior unchanged: DAGs still reject cycles.
- Add an explicit cyclic escape hatch for UI-authored approval, retest, and
  acceptance return flows.
- Require a clear start point for cyclic workflows through `xflow.start` and,
  later, `xflow.trigger`.
- In cyclic mode, advance along active outgoing edges instead of waiting for
  destination in-degree to reach zero.
- Allow nodes to execute more than once in one execution.
- Store only the latest node status and output in xflow runtime state.
- Let external systems or hooks own full history, audit, and business-level
  lifecycle records.
- Prevent automatic infinite loops with a per-trigger automatic depth limit.
- Keep implementation materially smaller than a full `stateflow` workflow kind.

## Non-goals

- Do not build a complete stateflow, BPMN, Petri-net, or multi-token process
  engine.
- Do not provide complete per-attempt node history inside the core engine.
- Do not guarantee cross-round merge, join, or wait-all semantics.
- Do not compensate or roll back side effects from earlier loop iterations.
- Do not make cyclic behavior the default.
- Do not weaken existing DAG validation for users that do not opt in.

## Proposed User-Facing Contract

Workflow definitions can opt in through workflow options:

```yaml
options:
  allow_cycles: true
  max_auto_depth: 100
  cyclic_history: external
```

The exact Go shape can be added to `types.WorkflowDef` as a new optional field:

```go
type WorkflowOptions struct {
	AllowCycles   bool   `json:"allow_cycles,omitempty"`
	MaxAutoDepth  int    `json:"max_auto_depth,omitempty"`
	CyclicHistory string `json:"cyclic_history,omitempty"`
}
```

`cyclic_history` is declarative metadata for documentation, UI warnings, and
validation. The first supported value is `external`, meaning xflow does not
store complete per-transition history.

When `allow_cycles` is true, the workflow must declare exactly one start node:

```yaml
nodes:
  - name: start
    type: xflow.start
```

Future server/UI flows can introduce trigger nodes:

```yaml
nodes:
  - name: create_vuln
    type: xflow.trigger
    parameters:
      trigger_type: manual
```

v1 should implement `xflow.start` first and reserve `xflow.trigger` for later
webhook, cron, manual, and event sources.

## Cyclic Mode Positioning

`allow_cycles` is an opt-in beta capability for human-driven return flows such
as approval rejection, failed retest, failed acceptance, and manual rework. It
is not a general-purpose cyclic concurrent workflow runtime.

v1 is a single-token execution mode:

- One active path is expected at a time.
- Nodes may be re-entered.
- The latest node state and output overwrite prior runtime state.
- Full transition history belongs outside the engine.
- Parallel fan-out and join behavior inside cycles is not guaranteed.

This covers the common UI flow where a vulnerability or ticket moves from one
current state to another, possibly returning to an earlier state after a manual
decision.

## Compile-Time Rules

`engine/graph.Compile` should preserve the current path by default:

- If `allow_cycles` is false or omitted, run cycle detection exactly as today.
- If `allow_cycles` is true, skip cycle detection and enable cyclic graph
  metadata.

Additional cyclic validation:

- There must be exactly one `xflow.start` node in v1.
- The start node may have incoming edges; those edges do not affect initial
  execution.
- `max_auto_depth <= 0` should use a default, recommended as `100`.
- `xflow.trigger` should be rejected until trigger runtime semantics exist.
- `xflow.merge` with `mode: wait_all`, parallel joins, and other fan-in nodes
  on cyclic paths should be rejected in v1, not merely warned.

The fan-in restriction is deliberate. Without round or attempt isolation,
wait-all can mix inputs from different loop iterations and produce silent
incorrect behavior.

## Start Node Semantics

`xflow.start` is a minimal built-in action node:

- It has no required input.
- It returns the execution submission input and context as its output.
- It exists to give UI-authored cyclic graphs one explicit entry point.
- It is valid in normal DAG workflows too, but only cyclic workflows require
  it.

In a DAG workflow, the existing "all zero in-degree nodes are roots" behavior
can remain valid. In a cyclic workflow, submission starts only the configured
start node.

## Runtime Scheduling

Non-cyclic workflows keep the current in-degree scheduler.

Cyclic workflows use edge-driven scheduling:

```text
completed node + active output port -> matching outgoing edges -> enqueue targets
```

Rules:

- Do not call `DecrementInDegree` to decide readiness in cyclic mode.
- Do not block a target because it has already reached a terminal node status.
- Re-entering a node overwrites that node's current runtime state.
- `$nodes['name']` resolves to the node's latest output.
- The engine does not preserve `$nodes['name'].history` or
  `$nodes['name'].attempts`.

This is intentionally simpler than stateflow. It lets implementers build UI
flows with return edges while accepting that business history and audit records
are managed elsewhere.

## Activation Versioning

Even though the engine does not keep full history, cyclic mode should keep a
small monotonic activation identifier per node entry.

Recommended fields:

```text
activation_id: monotonically increasing execution-local number or opaque id
auto_depth: automatic edge depth since the latest manual/signal boundary
```

The activation id is not user-facing history. It is runtime safety metadata
used to reject stale task commits, stale resume attempts, and delayed results
from a prior node activation. Without it, an old task or signal can overwrite a
newer cycle after the same node has been re-entered.

The implementation can introduce this narrowly in task payloads and state
commit paths without exposing full attempt history.

## Automatic Depth Limit

Cyclic mode needs a hard guard against accidental automatic infinite loops:

```yaml
options:
  max_auto_depth: 100
```

Semantics:

- The start node begins with `auto_depth = 0`.
- Every automatic edge-driven enqueue increments depth by one.
- If the next depth would exceed `max_auto_depth`, the execution fails with a
  clear error such as `max auto execution depth exceeded`.
- When a node suspends and later resumes through a signal/manual event, the
  resumed path resets `auto_depth` to `0`.
- Manual triggers are not limited by total lifetime count; each manual resume
  gets its own automatic depth budget.

This matches approval and lifecycle flows: a user can send a vulnerability
through many manual repair/retest rounds, but one unattended automatic cycle
cannot run forever.

## Suspend and Signal Semantics

Cyclic mode should reuse the existing `SuspendingHandler` mechanism. Approval,
wait, retest, and acceptance nodes can suspend and later resume from external
signals.

Additional cyclic requirements:

- Resume must be conditional on the current suspend still being active.
- Resume should verify activation metadata so stale signals do not resume an
  obsolete node entry.
- Duplicate signals must not enqueue duplicate resume paths for the same active
  suspension.
- Successful signal resume resets `auto_depth` to `0`.

Full business idempotency remains the responsibility of the node handler or the
external business system.

## Completion Semantics

The existing DAG completion rule, "all nodes have reached terminal state", does
not fit cyclic mode. A node can be terminal for its latest activation and still
be entered again later.

v1 should use explicit end nodes for cyclic workflows:

```yaml
nodes:
  - name: fixed
    type: xflow.end
    parameters:
      status: success

  - name: rejected
    type: xflow.end
    parameters:
      status: success
```

`xflow.end` semantics:

- When reached, it completes the execution.
- It can set final execution status and output.
- Downstream edges from an end node should be rejected.

If `xflow.end` is not implemented in the first slice, cyclic completion should
be defined as quiescence: no queued/running task and no suspended node. However,
explicit end nodes are more predictable for UI-authored lifecycle flows and
should be preferred.

## UI Authoring Contract

The UI can keep one visual canvas for both DAG and cyclic workflows.

For cyclic workflows, the UI should:

- Require one start node.
- Allow return edges.
- Expose `allow_cycles` as an advanced workflow setting.
- Expose `max_auto_depth` with a default of `100`.
- Display a warning that runtime state keeps only latest node output.
- Display a warning that history, audit, and side-effect consistency are owned
  by the integrating system.
- Reject publish if cyclic v1 contains merge, wait-all, or parallel joins on
  cyclic paths.

This allows users to draw the vulnerability lifecycle shape they expect without
forcing the engine into a full stateflow model.

## Data and Expression Semantics

In cyclic mode:

```text
$input          = current upstream active edge input
$nodes['x']     = latest successful output of node x
$nodes['x'].foo = field foo from latest output of node x
```

Not supported by core runtime:

```text
$nodes['x'].history
$nodes['x'].attempts
$nodes['x'][3]
```

If a workflow needs historical decisions, retest attempts, or full audit trails,
the workflow should write them to an external store from handlers or hooks.

## Persistence and History

`cyclic_history: external` means:

- xflow persists enough runtime state to route the current execution.
- xflow does not persist every state transition as a durable audit ledger.
- Node status and output represent the latest activation.
- External systems can subscribe through hooks or node handlers to store full
  lifecycle history.

This keeps the core engine small and matches the user's stated preference that
history be handled by the workflow implementer.

## Compatibility

Existing workflows are unaffected because `allow_cycles` defaults to false.

Existing DAG tests should continue to pass. Existing compile errors for cycles
should remain unless the workflow explicitly opts in.

The cyclic feature should be documented as beta or advanced because enabling it
trades strict DAG guarantees for UI flexibility and low engine complexity.

## Testing Strategy

Add tests before implementation.

Compile tests:

- Default workflows still reject cycles.
- `allow_cycles: true` allows a simple cycle.
- Cyclic workflows without a start node fail validation.
- Cyclic workflows with multiple start nodes fail validation.
- Cyclic workflows reject `xflow.trigger` until implemented.
- Cyclic workflows reject wait-all merge or unsupported fan-in on cyclic paths.

Start/runtime tests:

- A cyclic workflow starts at `xflow.start` even if the start node has an
  incoming edge.
- A simple return flow re-enters a node and overwrites its latest output.
- `$nodes['x']` returns the latest output after re-entry.
- A pure automatic cycle fails when `max_auto_depth` is exceeded.
- A suspend/resume cycle resets `auto_depth` after signal resume.
- A stale activation result cannot overwrite a newer activation.

Compatibility tests:

- Existing DAG scheduler behavior remains unchanged for non-cyclic workflows.
- Existing approval, wait, merge, split, and loop tests keep passing outside
  cyclic mode.

## Implementation Slices

1. Add `WorkflowOptions`, graph cyclic metadata, and `xflow.start` descriptor.
2. Update compile validation for `allow_cycles`, unique start, and unsupported
   cyclic fan-in.
3. Update submission to start from `StartIdx` in cyclic mode.
4. Add cyclic edge-driven scheduling branch while preserving DAG scheduling.
5. Add `auto_depth` propagation and max-depth failure.
6. Add minimal activation metadata to prevent stale commit/resume corruption.
7. Add `xflow.end` or define quiescent completion for cyclic mode.
8. Update DSL documentation and UI validation guidance.

## Open Decisions

- Whether `xflow.end` is required in the first implementation slice or can be
  added immediately after cyclic scheduling.
- Whether activation ids are stored only in volatile task payloads or also in
  node snapshots for stronger stale-commit protection.
- Whether cyclic fan-in is rejected globally in cyclic workflows or only when a
  fan-in node is proven to be on a cycle.
- Whether `cyclic_history` should be a string enum or a nested policy object in
  the public DSL.

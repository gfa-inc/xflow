# XFlow Server Runner MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first working control-plane / execution-plane split: `cmd/server` accepts workflow control requests, `cmd/runner` executes tasks through a Runner Protocol, and engine scheduling remains centralized on the server side.

**Architecture:** Keep `engine/` IO-free and reuse `execution.Dispatcher`, `execution.Runner`, `engine.BuildTaskLease`, and `engine.CommitTaskResult`. Add a small protocol package plus service-level server/runner components under `service/`, while `cmd/server` and `cmd/runner` only wire configuration and lifecycle.

**Tech Stack:** Go, `net/http`, JSON, existing `engine`, `execution`, `backend/asynq`, `backend/memory`, `sdk/xflow`, `nodes/node`.

## Global Constraints

- `engine/` must NOT import redis/asynq/mysql/sql.
- `execution/` and `backend/memory/` must remain free of Redis/Asynq/MySQL/network dependencies.
- Future server/runner code goes under `service/`; core packages must NEVER import `service/` or `cmd/`.
- Runner must not connect to Redis, Asynq, or StateStore directly.
- Server owns StateStore, TaskQueue, scheduling, signal delivery, timeout sweep, and final execution state.
- Start with HTTP+JSON long polling for Runner Protocol; keep the protocol interface narrow enough to replace with gRPC/streaming later.
- Do not implement Relay Gateway, remote SDK, auth, UI, or advanced placement in this MVP.

---

## File Structure

- Create `service/protocol/types.go`: wire DTOs for runner registration, polling, lease assignment, task result, heartbeat, and cancel.
- Create `service/protocol/client.go`: HTTP client used by `cmd/runner`.
- Create `service/protocol/server.go`: HTTP route registration helpers used by `cmd/server`.
- Create `service/control/server.go`: control-plane API for submit, inspect, signal, cancel, and runner polling.
- Create `service/control/dispatcher.go`: server-side bridge from queued engine tasks to runner assignments.
- Create `service/control/runner_pool.go`: in-memory runner registry, heartbeat tracking, capacity, and pending assignment queues.
- Create `service/runner/runner.go`: execution-plane loop that polls server, executes assigned leases through `execution.Runner`, and reports results.
- Modify `cmd/server/main.go`: parse config, create backend provider, engine, control server, dispatcher, and HTTP listener.
- Modify `cmd/runner/main.go`: parse config, register node capabilities, start runner loop.
- Add tests in `service/protocol`, `service/control`, and `service/runner`.
- Add an integration test under `sdk/examples` or `service/control` proving a server process and runner loop complete a simple workflow.

---

### Task 1: Runner Protocol DTOs

**Files:**
- Create: `service/protocol/types.go`
- Test: `service/protocol/types_test.go`

**Interfaces:**
- Produces: `RegisterRunnerRequest`, `RegisterRunnerResponse`, `HeartbeatRequest`, `PollTaskRequest`, `PollTaskResponse`, `ReportResultRequest`, `ReportResultResponse`.
- Consumes: existing `engine.TaskLease`, `engine.TaskResult`, `types.ExecutionID`.

- [ ] **Step 1: Write failing JSON round-trip tests**

Create `service/protocol/types_test.go` with tests that marshal and unmarshal `PollTaskResponse` containing a lease, and `ReportResultRequest` containing a successful task result.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/protocol`

Expected: fail because package `service/protocol` does not exist.

- [ ] **Step 3: Implement DTOs**

Create `service/protocol/types.go` with JSON-tagged structs:

```go
package protocol

import (
	"time"

	"github.com/gfa-inc/xflow/engine"
)

type Capability struct {
	NodeType    string `json:"node_type"`
	NodeVersion int    `json:"node_version,omitempty"`
}

type RegisterRunnerRequest struct {
	RunnerID     string       `json:"runner_id"`
	Concurrency  int          `json:"concurrency"`
	Capabilities []Capability `json:"capabilities"`
}

type RegisterRunnerResponse struct {
	RunnerID string `json:"runner_id"`
}

type HeartbeatRequest struct {
	RunnerID  string `json:"runner_id"`
	Capacity  int    `json:"capacity"`
	InFlight  int    `json:"in_flight"`
	Timestamp int64  `json:"timestamp"`
}

type HeartbeatResponse struct {
	ServerTime int64 `json:"server_time"`
}

type PollTaskRequest struct {
	RunnerID     string       `json:"runner_id"`
	Capacity     int          `json:"capacity"`
	Capabilities []Capability `json:"capabilities"`
}

type PollTaskResponse struct {
	Lease *engine.TaskLease `json:"lease,omitempty"`
	Wait  time.Duration     `json:"wait"`
}

type ReportResultRequest struct {
	RunnerID string            `json:"runner_id"`
	Lease    *engine.TaskLease `json:"lease"`
	Result   engine.TaskResult `json:"result"`
}

type ReportResultResponse struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}
```

- [ ] **Step 4: Run tests**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/protocol`

Expected: pass.

- [ ] **Step 5: Commit**

Commit message: `feat: add runner protocol DTOs`

---

### Task 2: Runner Pool and Assignment Queue

**Files:**
- Create: `service/control/runner_pool.go`
- Test: `service/control/runner_pool_test.go`

**Interfaces:**
- Consumes: `protocol.Capability`, `engine.TaskLease`.
- Produces: `RunnerPool.Register`, `RunnerPool.Heartbeat`, `RunnerPool.Assign`, `RunnerPool.Poll`.

- [ ] **Step 1: Write failing tests**

Cover:
- registering a runner with `xflow.function` capability;
- assigning a matching lease;
- polling returns the assigned lease;
- polling a non-matching capability returns no lease;
- heartbeat updates capacity and timestamp.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/control -run TestRunnerPool`

Expected: fail because `RunnerPool` does not exist.

- [ ] **Step 3: Implement `RunnerPool`**

Use a mutex-protected in-memory registry. Keep assignment simple in MVP: exact `NodeType` match, FIFO per runner, no persistent pending pool. Return `false` from `Assign` when no registered runner can execute the lease.

- [ ] **Step 4: Run tests**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/control -run TestRunnerPool`

Expected: pass.

- [ ] **Step 5: Commit**

Commit message: `feat: add control runner pool`

---

### Task 3: Control Server HTTP API

**Files:**
- Create: `service/control/server.go`
- Create: `service/protocol/server.go`
- Test: `service/control/server_test.go`

**Interfaces:**
- Consumes: `*engine.Engine`, `engine.StateStore`, `RunnerPool`.
- Produces HTTP routes:
  - `POST /v1/runners/register`
  - `POST /v1/runners/heartbeat`
  - `POST /v1/runners/poll`
  - `POST /v1/runners/result`
  - `POST /v1/executions/{id}/signal`
  - `POST /v1/executions/{id}/cancel`
  - `GET /v1/executions/{id}`

- [ ] **Step 1: Write failing HTTP handler tests**

Use `httptest.Server`. Test runner register/poll/result endpoints first, then inspect/signal/cancel with a fake engine facade if needed.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/control -run TestHTTP`

Expected: fail because HTTP server does not exist.

- [ ] **Step 3: Implement route registration**

Use only `net/http` and `encoding/json`. Return JSON errors with HTTP status codes:
- `400` invalid JSON or missing required fields;
- `404` unknown execution;
- `409` stale lease token or rejected result;
- `500` unexpected server error.

- [ ] **Step 4: Run tests**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/control -run TestHTTP`

Expected: pass.

- [ ] **Step 5: Commit**

Commit message: `feat: add control server api`

---

### Task 4: Server-Side Task Dispatcher

**Files:**
- Create: `service/control/dispatcher.go`
- Test: `service/control/dispatcher_test.go`
- Modify: `backend/asynq/backend.go` only if a clean hook is needed to bind a non-embedded executor.

**Interfaces:**
- Consumes: queued `engine.Task`, `engine.Engine.BuildTaskLease`, `RunnerPool.Assign`.
- Produces: dispatcher component that converts queue tasks into runner assignments.

- [ ] **Step 1: Write failing dispatcher test**

Use `backend/memory` or a fake task source to enqueue one task. Assert dispatcher builds a lease and assigns it to a runner with matching capability without executing the handler locally.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/control -run TestDispatcher`

Expected: fail because dispatcher component does not exist.

- [ ] **Step 3: Implement dispatcher**

Keep this MVP narrow:
- no placement routing;
- no persistent pending state;
- no gateway;
- when no runner is available, return an error so the queue can retry.

- [ ] **Step 4: Run tests**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/control -run TestDispatcher`

Expected: pass.

- [ ] **Step 5: Commit**

Commit message: `feat: dispatch tasks to runners`

---

### Task 5: Runner Protocol Client

**Files:**
- Create: `service/protocol/client.go`
- Test: `service/protocol/client_test.go`

**Interfaces:**
- Consumes: protocol DTOs and server HTTP endpoints.
- Produces: `Client.Register`, `Client.Heartbeat`, `Client.Poll`, `Client.ReportResult`.

- [ ] **Step 1: Write failing client tests**

Use `httptest.Server` and verify request paths, methods, JSON bodies, and response decoding.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/protocol -run TestClient`

Expected: fail because `Client` does not exist.

- [ ] **Step 3: Implement HTTP client**

Use `http.Client` with context-aware requests. Treat non-2xx responses as errors including response body text.

- [ ] **Step 4: Run tests**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/protocol -run TestClient`

Expected: pass.

- [ ] **Step 5: Commit**

Commit message: `feat: add runner protocol client`

---

### Task 6: Runner Execution Loop

**Files:**
- Create: `service/runner/runner.go`
- Test: `service/runner/runner_test.go`

**Interfaces:**
- Consumes: `protocol.Client`, `execution.Runner`, `execution.Registry`, node definitions.
- Produces: runner loop that registers, polls leases, executes through existing embedded runner, and reports results.

- [ ] **Step 1: Write failing runner tests**

Use a fake protocol client that returns one lease. Register a fake node handler in `execution.Registry`. Assert the runner reports a successful `engine.TaskResult`.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/runner`

Expected: fail because package `service/runner` does not exist.

- [ ] **Step 3: Implement runner loop**

Implement:
- `Register` once on startup;
- heartbeat ticker;
- poll loop respecting context cancellation;
- execute one lease at a time in MVP;
- report success or failure through protocol client.

- [ ] **Step 4: Run tests**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/runner`

Expected: pass.

- [ ] **Step 5: Commit**

Commit message: `feat: add remote runner loop`

---

### Task 7: Command Wiring

**Files:**
- Modify: `cmd/server/main.go`
- Modify: `cmd/runner/main.go`
- Test: `cmd/server` and `cmd/runner` build tests via `go test`.

**Interfaces:**
- Consumes: `service/control`, `service/runner`, `service/protocol`.
- Produces:
  - `xflow-server -addr :8080 -redis localhost:6379`
  - `xflow-runner -server http://localhost:8080 -id runner-1`

- [ ] **Step 1: Write command wiring tests or build checks**

Add tests only for config parsing if parsing is extracted. Otherwise rely on package build and integration test in Task 8.

- [ ] **Step 2: Run build checks**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/server ./cmd/runner`

Expected: current packages pass but do nothing.

- [ ] **Step 3: Implement command wiring**

Use `flag` for MVP. Server starts backend/asynq when `-redis` is set; allow `-memory` for local integration tests. Runner starts HTTP protocol client and execution registry.

- [ ] **Step 4: Run build checks**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/server ./cmd/runner`

Expected: pass.

- [ ] **Step 5: Commit**

Commit message: `feat: wire server and runner commands`

---

### Task 8: End-to-End MVP Test

**Files:**
- Create: `service/control/e2e_test.go`
- Modify: `docs/README.md` or `.claude/docs/deployment-topologies.md` with MVP status.

**Interfaces:**
- Consumes: all previous tasks.
- Produces: one automated proof that a submitted workflow is executed by a runner, not by the server.

- [ ] **Step 1: Write failing E2E test**

Use in-memory backend to avoid Redis in unit tests. Start control server with `httptest.Server`, start runner loop with a registered fake node, submit a one-node workflow, wait for completion, and assert output.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/control -run TestServerRunnerE2E`

Expected: fail until command/control/runner wiring is complete.

- [ ] **Step 3: Complete missing integration seams**

Fix only the seams needed for the E2E path:
- workflow submission path;
- dispatcher startup;
- runner polling;
- result commit;
- completion wait or polling.

- [ ] **Step 4: Run full focused test suite**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./service/... ./cmd/server ./cmd/runner ./engine ./execution ./backend/memory ./sdk/xflow
```

Expected: pass when dependencies are available in the module cache or network is available.

- [ ] **Step 5: Update docs**

Change deployment docs from “server/runner 规划” to “MVP implemented” with limitations:
- HTTP+JSON long polling;
- no Relay Gateway;
- no remote SDK;
- no auth;
- simple node-type capability matching.

- [ ] **Step 6: Commit**

Commit message: `test: add server runner e2e coverage`

---

## Follow-Up Plans

After this MVP lands, split remaining work into separate plans:

1. **Remote SDK Plan:** `xflow.NewRemote`, remote `Submit/Wait/Signal/Cancel/Inspect`, client retries, compatibility tests.
2. **Durable Dispatcher Plan:** persistent runner pending/inflight state, lease timeout recovery, idempotent result handling across server restarts.
3. **Runner Placement Plan:** tags, env, region, user scope, weighted runner selection, capacity-aware scheduling.
4. **Relay Gateway Plan:** gateway registration, protocol relay, local pending buffer, reconnect behavior.
5. **Production Hardening Plan:** auth, mTLS/token, metrics, tracing, structured logs, health checks, graceful shutdown.

## Self-Review

- Spec coverage: covers the first required milestone, `server + runner + Runner Protocol` MVP. Remote SDK and Relay Gateway are intentionally split into follow-up plans because they are separate subsystems.
- Placeholder scan: no open-ended implementation placeholder is required for task acceptance; each task has concrete files, interfaces, tests, commands, and deliverables.
- Type consistency: protocol DTOs flow from `service/protocol` into `service/control` and `service/runner`; engine execution remains via existing `engine.TaskLease` and `engine.TaskResult`.

package types

// ExecutionID uniquely identifies a single workflow execution instance.
type ExecutionID string

// Status represents the lifecycle state of a workflow execution.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusCanceling Status = "canceling"
	StatusCanceled  Status = "canceled"
	StatusTimeout   Status = "timeout"
	StatusSkipped   Status = "skipped"
)

// Result holds the outcome of a completed (or failed) workflow execution.
type Result struct {
	ExecutionID ExecutionID    `json:"execution_id,omitempty"`
	Status      Status         `json:"status,omitempty"`
	Output      map[string]any `json:"output,omitempty"`
	Error       string         `json:"error,omitempty"`
}

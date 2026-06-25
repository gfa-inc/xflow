package backend

import (
	"context"

	"github.com/gfa-inc/xflow/engine"
	"github.com/gfa-inc/xflow/types"
)

// Provider supplies the engine backends and lifecycle binding for an embedded deployment.
type Provider interface {
	State() engine.StateStore
	Queue() engine.TaskQueue
	Registry() engine.HandlerRegistry
	Bind(eng *engine.Engine) func()
}

// Waiter is an optional backend capability for event-driven completion waits.
type Waiter interface {
	WaitDone(ctx context.Context, id types.ExecutionID) (types.Result, error)
}

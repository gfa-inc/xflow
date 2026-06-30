package backend

import (
	"context"
	"time"

	"github.com/gfa-inc/xflow/types"
)

type TriggerPrimitives interface {
	Dedup(ctx context.Context, key string, ttl time.Duration) (bool, error)
	TryLock(ctx context.Context, key string, ttl time.Duration) (types.TriggerLock, bool, error)
	State(ctx context.Context, scope string) types.TriggerState
}

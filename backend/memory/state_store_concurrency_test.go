//go:build concurrency

// Spec: .claude/specs/lua-concurrency-tests.md
package memory

import (
	"testing"

	"github.com/gfa-inc/xflow/backend/internal/statestoretest"
	"github.com/gfa-inc/xflow/engine"
)

func TestMemoryStateStore_Concurrency(t *testing.T) {
	statestoretest.RunStateStoreConcurrencySuite(t, func(t *testing.T) engine.StateStore {
		return New().State()
	})
}

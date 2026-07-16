package contract

import (
	"testing"

	"github.com/gfa-inc/xflow/backend/memory"
)

func TestMemoryStateStoreContract(t *testing.T) {
	state := memory.New().State()
	RunStateStoreContract(t, state)
}

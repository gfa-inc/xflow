package transform_test

import (
	"context"
	"testing"

	"github.com/gfa-inc/xflow/node/registry"
	"github.com/gfa-inc/xflow/node"
	"github.com/gfa-inc/xflow/types"
)

func TestPick_KeepsOnlyNamedFields(t *testing.T) {
	b := node.Pick("id", "name")

	h, ok := registry.Lookup("xflow.transform.pick")
	if !ok {
		t.Fatal("pick handler not registered")
	}
	out, err := h.Execute(context.Background(), &types.Input{
		Params: b.RawParams().(map[string]any),
		Data:   map[string]any{"id": "u1", "name": "Ada", "password": "secret"},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(out.Data) != 2 || out.Data["id"] != "u1" || out.Data["name"] != "Ada" {
		t.Fatalf("data = %#v, want only id and name", out.Data)
	}
}

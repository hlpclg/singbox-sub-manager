package remote

import (
	"context"
	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"testing"
)

func TestNodeCheck_Run(t *testing.T) {
	node := nodes.Node{Name: "test-node"}

	oldCheck := runCheckNode
	defer func() { runCheckNode = oldCheck }()

	runCheckNode = func(ctx context.Context, n nodes.Node) error {
		return nil // Success
	}

	c := NewNodeCheck(node)
	res := c.Run(context.Background(), health.Config{})
	if res.Status != health.StatusPass {
		t.Errorf("expected pass, got %v", res.Status)
	}
	if c.ID() != "remote.test-node" {
		t.Errorf("expected remote.test-node, got %v", c.ID())
	}
}

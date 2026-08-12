package remote

import (
	"context"
	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

var runCheckNode = CheckNode

type NodeCheck struct {
	node nodes.Node
}

func NewNodeCheck(n nodes.Node) *NodeCheck {
	return &NodeCheck{node: n}
}

func (c *NodeCheck) ID() string {
	return "remote." + c.node.Name
}

func (c *NodeCheck) Name() string {
	return "remote " + c.node.Name
}

func (c *NodeCheck) Run(ctx context.Context, cfg health.Config) health.Result {
	err := runCheckNode(ctx, c.node)
	if err != nil {
		return health.Result{
			Status:  health.StatusFail,
			Message: err.Error(),
		}
	}
	return health.Result{
		Status:  health.StatusPass,
		Message: "available",
	}
}

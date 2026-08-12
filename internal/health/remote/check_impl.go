package remote

import (
	"context"
	"github.com/hlpclg/singbox-sub-manager/internal/health"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"time"
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
	nodeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := runCheckNode(nodeCtx, c.node)
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

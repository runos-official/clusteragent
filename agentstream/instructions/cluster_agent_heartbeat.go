package instructions

import (
	"github.com/runos-official/clusteragent/agentstream/l2sec"
)

func ClusterAgentHeartbeatToServer() *l2sec.FromClusterAgent {
	return &l2sec.FromClusterAgent{
		Type: "ClusterAgentHeartbeatToServer",
	}
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
)

// YugaClusterController is a struct that facilitates the manipulation of a DB cluster,
// with its nodes running in Docker containers.
//
// To create the cluster, we are using Yugabyte's preconfigured tool, which is set up as follows:
// 1. If the cluster size is greater than or equal to 3,
// the replication factor (RF) is set to 3; otherwise, RF is set to 1.
// 2. The cluster supports a maximum of 3 master nodes.
// Therefore, a cluster with more than 3 master nodes cannot be created.
// Any additional node will act as a tserver node.
//
// In addition, RF=3 supports the failure of one master node, while RF=1 does not provide resilience to node failures.
// There are some bugs in YugaDB that prevent the cluster behavior from functioning as expected:
// 1. After a master-node is deleted, a new master-node cannot be added to the cluster as a replacement.
// 2. If the leader node is failing, the whole db connectivity will stop functioning.
type YugaClusterController struct {
	DBClusterController
}

// StartYugaCluster creates a Yugabyte cluster in a Docker environment
// and returns its connection properties.
func StartYugaCluster(ctx context.Context, t *testing.T, clusterSize uint) (
	*YugaClusterController, *dbtest.Connection,
) {
	t.Helper()

	if runtime.GOOS != linuxOS {
		t.Skip("Container IP access not supported on non-linux Docker")
	}

	cluster := &YugaClusterController{}

	t.Logf("starting yuga cluster of size: %d", clusterSize)
	for range clusterSize {
		cluster.AddNode(ctx, t)
	}

	t.Cleanup(func() {
		cluster.stopAndRemoveCluster(t)
	})

	clusterConnection := cluster.getNodesConnections(ctx, t)
	clusterConnection.LoadBalance = true

	return cluster, clusterConnection
}

// AddNode creates, starts and add a YugabyteDB node to the cluster.
func (cc *YugaClusterController) AddNode(ctx context.Context, t *testing.T) *dbtest.DatabaseContainer {
	t.Helper()

	node := &dbtest.DatabaseContainer{
		Name:         fmt.Sprintf("yuga-%s", uuid.New()),
		Role:         LeaderNode,
		DatabaseType: dbtest.YugaDBType,
		Cmd:          dbtest.YugabyteCMD,
	}

	if cc.GetClusterSize() != 0 {
		// node.Role refers to the node's Raft role.
		node.Role = FollowerNode
		node.Cmd = append(node.Cmd, fmt.Sprintf("--join=%s", cc.getLeaderHost(ctx, t)))
	}

	cc.nodes = append(cc.nodes, node)

	require.NoError(t, nodeStartupRetry.Execute(ctx, func() error {
		t.Logf("starting db node %v with role: %v", node.Name, node.Role)
		node.StartContainer(ctx, t)
		return node.EnsureNodeReadiness(t, "Data placement constraint successfully verified")
	}))

	return node
}

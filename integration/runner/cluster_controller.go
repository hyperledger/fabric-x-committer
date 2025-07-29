/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// DBClusterController is a class that facilitates the manipulation of a DB cluster,
// with its nodes running in Docker containers.
type DBClusterController struct {
	nodes []*dbtest.DatabaseContainer
}

const (
	//nolint:revive // LeaderNode and FollowerNode represent db nodes role.
	LeaderNode   = "leader"
	FollowerNode = "follower"
	TabletNode   = "tablet"
	MasterNode   = "master"

	linuxOS = "linux"
)

var nodeStartupRetry = &connection.RetryProfile{
	// MaxElapsedTime is the duration allocated for the retry mechanism during the database initialization process.
	MaxElapsedTime: 5 * time.Minute,
	// InitialInterval is the starting wait time interval that increases every retry attempt.
	InitialInterval: 1 * time.Second,
}

// StopAndRemoveNodeWithRole stops and removes a node given a role.
func (cc *DBClusterController) StopAndRemoveNodeWithRole(t *testing.T, role string) {
	t.Helper()
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster.")
	for idx, node := range cc.nodes {
		if node.Role == role {
			node.StopAndRemoveContainer(t)
			cc.nodes = append(cc.nodes[:idx], cc.nodes[idx+1:]...)
			return
		}
	}
}

// StopAndRemoveNodeWithName stops and removes a node given a name.
func (cc *DBClusterController) StopAndRemoveNodeWithName(t *testing.T, name string) {
	t.Helper()
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster.")
	for idx, node := range cc.nodes {
		if node.Name == name {
			t.Logf("found ")
			node.StopAndRemoveContainer(t)
			cc.nodes = append(cc.nodes[:idx], cc.nodes[idx+1:]...)
			return
		}
	}
}

// GetClusterSize returns the number of active nodes in the cluster.
func (cc *DBClusterController) GetClusterSize() int {
	return len(cc.nodes)
}

// GetNodesContainerID returns the container IDs of the current nodes.
func (cc *DBClusterController) GetNodesContainerID(t *testing.T) []string {
	t.Helper()
	containersIDs := make([]string, cc.GetClusterSize())
	for _, node := range cc.nodes {
		containersIDs = append(containersIDs, node.ContainerID())
	}
	return containersIDs
}

func (cc *DBClusterController) getLeaderHost(ctx context.Context, t *testing.T) string {
	t.Helper()

	require.NotEmpty(t, cc.nodes, "no nodes available in cluster")

	for _, node := range cc.nodes {
		if node.Role == LeaderNode {
			return node.GetContainerConnectionDetails(ctx, t).GetHost()
		}
	}

	t.Fatal("no leader node found in cluster")
	return "" // unreachable, but required for compiler
}

func (cc *DBClusterController) getNodesConnections(ctx context.Context, t *testing.T) *dbtest.Connection {
	t.Helper()
	endpoints := make([]*connection.Endpoint, cc.GetClusterSize())
	for i, node := range cc.nodes {
		endpoints[i] = node.GetContainerConnectionDetails(ctx, t)
	}

	return dbtest.NewConnection(endpoints...)
}

func (cc *DBClusterController) stopAndRemoveCluster(t *testing.T) {
	t.Helper()
	for _, node := range cc.nodes {
		t.Logf("stopping and removing node: %v", node.Name)
		node.StopAndRemoveContainer(t)
	}
	cc.nodes = nil
}

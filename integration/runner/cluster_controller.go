/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// DBClusterController is a class that facilitates the manipulation of a DB cluster,
// with its nodes running in Docker containers.
type DBClusterController struct {
	nodes []*dbtest.DatabaseContainer
}

const linuxOS = "linux"

// StopAndRemoveSingleNodeByRole stops and removes a node given a role.
func (cc *DBClusterController) StopAndRemoveSingleNodeByRole(t *testing.T, role string) {
	t.Helper()
	nodeToBeRemoved, itsIndex := cc.GetSingleNodeByRole(role)
	require.NotNil(t, nodeToBeRemoved, "not found nodes to remove of requested role.")
	cc.StopAndRemoveNodeByIndex(t, itsIndex)

}

// StopAndRemoveNodeByIndex stop and remove the node in the provided index.
func (cc *DBClusterController) StopAndRemoveNodeByIndex(t *testing.T, index int) {
	t.Helper()
	require.Greater(t, len(cc.nodes), index)
	node := cc.nodes[index]
	t.Logf("Removing node: %s", node.Name)
	node.StopAndRemoveContainer(t)
	cc.nodes = append(cc.nodes[:index], cc.nodes[index+1:]...)
}

func (cc *DBClusterController) getNodesConnectionsByRole(
	t *testing.T,
	role string,
) *dbtest.Connection {
	t.Helper()
	endpoints := make([]*connection.Endpoint, 0, len(cc.nodes))
	for _, node := range cc.IterNodesByRole(role) {
		endpoints = append(endpoints, node.GetContainerConnectionDetails(t))
	}
	return dbtest.NewConnection(endpoints...)
}

// GetSingleNodeByRole returns the first node that matches the requested role in the cluster.
func (cc *DBClusterController) GetSingleNodeByRole(role string) (*dbtest.DatabaseContainer, int) {
	for idx, node := range cc.IterNodesByRole(role) {
		return node, idx
	}
	return nil, 0
}

func (cc *DBClusterController) getNodesConnections(t *testing.T) *dbtest.Connection {
	t.Helper()
	endpoints := make([]*connection.Endpoint, len(cc.nodes))
	for i, node := range cc.nodes {
		endpoints[i] = node.GetContainerConnectionDetails(t)
	}

	return dbtest.NewConnection(endpoints...)
}

// IterNodesByRole returns an iterator over the cluster's nodes that match the given role.
func (cc *DBClusterController) IterNodesByRole(role string) iter.Seq2[int, *dbtest.DatabaseContainer] {
	return func(yield func(int, *dbtest.DatabaseContainer) bool) {
		for idx, node := range cc.nodes {
			if node.Role == role {
				if !yield(idx, node) {
					return
				}
			}
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

func (cc *DBClusterController) stopAndRemoveCluster(t *testing.T) {
	t.Helper()
	for _, node := range cc.nodes {
		t.Logf("stopping and removing node: %v", node.Name)
		node.StopAndRemoveContainer(t)
	}
	cc.nodes = nil
}

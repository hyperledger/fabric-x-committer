/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"iter"
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

const linuxOS = "linux"

var nodeStartupRetry = &connection.RetryProfile{
	// MaxElapsedTime is the duration allocated for the retry mechanism during the database initialization process.
	MaxElapsedTime: 5 * time.Minute,
	// InitialInterval is the starting wait time interval that increases every retry attempt.
	InitialInterval: 1 * time.Second,
}

// StopAndRemoveSingleNodeWithRole stops and removes a node given a role.
func (cc *DBClusterController) StopAndRemoveSingleNodeWithRole(t *testing.T, role string) {
	t.Helper()
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster.")
	for idx, node := range cc.IterNodesByRole(role) {
		node.StopAndRemoveContainer(t)
		cc.nodes = append(cc.nodes[:idx], cc.nodes[idx+1:]...)
		return
	}
}

// IterNodesByRole stops and removes a node given a role.
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

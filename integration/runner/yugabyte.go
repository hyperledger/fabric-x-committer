/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	"fmt"
	"maps"
	"net"
	"path"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// YugaClusterController is a struct that facilitates the manipulation of a DB cluster,
// with nodes running in Docker containers.
// It allows configuring the number of master and tablet nodes.
// The cluster's replication factor (RF) is determined as follows:
//   - If the number of tablet nodes is greater than or equal to 3,
//     RF is set to 3; otherwise, RF is set to 1.
type YugaClusterController struct {
	DBClusterController

	networkName string
}

const (
	// latest LTS.
	defaultImage = "yugabytedb/yugabyte:2024.2.4.0-b89"

	// leaderRegexPattern is the raw regular expression pattern.
	// It is used for matching lines in the master status output
	// and extracting the RPC Host/Port of the master node whose Role is LEADER.
	leaderRegexPattern = `(?m)^[^\n]*[ \t]+(\S+):\d+[ \t]+[^\n]+[ \t]+LEADER[ \t]+[^\n]*$`

	//nolint:revive // MasterNode and TabletNode represents yugabyte db nodes role.
	MasterNode = "master"
	TabletNode = "tablet"
)

// leaderRegex is the compiled regular expression.
// to efficiently extract the leader master's RPC Host/Port.
var leaderRegex = regexp.MustCompile(leaderRegexPattern)

// StartYugaCluster creates a Yugabyte cluster in a Docker environment
// and returns its connection properties.
func StartYugaCluster(ctx context.Context, t *testing.T, numberOfMasters, numberOfTablets uint) (
	*YugaClusterController, *dbtest.Connection,
) {
	t.Helper()

	if runtime.GOOS != linuxOS {
		t.Skip("Container IP access not supported on non-linux Docker")
	}

	t.Logf("starting yuga cluster with (%d) masters and (%d) tablets ", numberOfMasters, numberOfTablets)

	cluster := &YugaClusterController{
		networkName: uuid.NewString(),
	}
	dbtest.CreateDockerNetwork(t, cluster.networkName)
	t.Cleanup(func() {
		dbtest.RemoveDockerNetwork(t, cluster.networkName)
	})

	// We create the nodes before startup to ensure
	// we have the names of the master nodes.
	// This information is required ahead of time to start each node.
	for range numberOfMasters {
		cluster.createNode(MasterNode)
	}
	for range numberOfTablets {
		cluster.createNode(TabletNode)
	}
	cluster.startNodes(ctx, t)

	t.Cleanup(func() {
		cluster.stopAndRemoveCluster(t)
	})

	// The master nodes are not involved in DB communication;
	// the application connects to the tablet servers.
	clusterConnection := cluster.getConnectionsOfGivenRole(ctx, t, TabletNode)
	clusterConnection.LoadBalance = true

	return cluster, clusterConnection
}

func (cc *YugaClusterController) createNode(role string) {
	node := &dbtest.DatabaseContainer{
		Name:         fmt.Sprintf("yuga-%s-%s", role, uuid.New().String()),
		Image:        defaultImage,
		Role:         role,
		DatabaseType: dbtest.YugaDBType,
		Network:      cc.networkName,
	}
	cc.nodes = append(cc.nodes, node)
}

func (cc *YugaClusterController) startNodes(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, n := range cc.IterNodesByRole(MasterNode) {
		n.Cmd = nodeConfig(n.Role, n.Name, cc.getMasterAddresses(), cc.desiredRF())
		n.StartContainer(ctx, t)
	}

	expectedAlive := len(maps.Collect(cc.IterNodesByRole(MasterNode)))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, actualAlive := cc.getLeaderMaster(t)
		require.Equal(ct, expectedAlive, actualAlive)
	}, time.Minute, time.Millisecond*100)

	for _, n := range cc.IterNodesByRole(TabletNode) {
		n.Cmd = nodeConfig(n.Role, n.Name, cc.getMasterAddresses(), cc.desiredRF())
		n.StartContainer(ctx, t)
		n.EnsureNodeReadiness(t, "syncing data to disk ... ok")
	}
}

func (cc *YugaClusterController) getMasterAddresses() string {
	var masterAddresses []string //nolint:prealloc
	for _, n := range cc.IterNodesByRole(MasterNode) {
		masterAddresses = append(masterAddresses, fmt.Sprintf("%s:7100", n.Name))
	}
	return strings.Join(masterAddresses, ",")
}

// RF=3 when number of tablets >=3, else RF=1.
func (cc *YugaClusterController) desiredRF() int {
	numberOfTablets := len(maps.Collect(cc.IterNodesByRole(TabletNode)))
	if numberOfTablets >= 3 {
		return 3
	}
	return 1
}

func (cc *YugaClusterController) getLeaderMaster(t *testing.T) (string, int) {
	t.Helper()
	var output string
	masterAddresses := cc.getMasterAddresses()
	for _, n := range cc.nodes {
		if out := n.ExecuteCommand(t, []string{
			"/home/yugabyte/bin/yb-admin",
			"-master_addresses", masterAddresses,
			"list_all_masters",
		}); out != "" {
			output = out
			break
		}
	}

	if output == "" {
		t.Fatal("Could not get yb-admin output from any master")
	}

	found := leaderRegex.FindStringSubmatch(output)
	require.NotEmpty(t, found)
	return found[1], strings.Count(output, "ALIVE")
}

func (cc *DBClusterController) getConnectionsOfGivenRole(
	ctx context.Context,
	t *testing.T,
	role string,
) *dbtest.Connection {
	t.Helper()
	var endpoints []*connection.Endpoint //nolint:prealloc
	for _, node := range cc.IterNodesByRole(role) {
		endpoints = append(endpoints, node.GetContainerConnectionDetails(ctx, t))
	}
	return dbtest.NewConnection(endpoints...)
}

// RemoveLeaderMasterNode finds the leader of the master nodes, retrieve its container name and removes it.
func (cc *YugaClusterController) RemoveLeaderMasterNode(t *testing.T) {
	t.Helper()
	cc.removeMasterNode(t, true)
}

// RemoveNonLeaderMasterNode finds a master node, which is not a leader, retrieve its container name and removes it.
func (cc *YugaClusterController) RemoveNonLeaderMasterNode(t *testing.T) {
	t.Helper()
	cc.removeMasterNode(t, false)
}

// removeMaster removes a master node from the cluster.
// If isLeader is true, it removes the current leader master.
// Otherwise, it removes any non-leader master.
//
//nolint:revive // flag-parameter is required here to avoid code duplication.
func (cc *YugaClusterController) removeMasterNode(t *testing.T, isLeader bool) {
	t.Helper()
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster")

	leaderName, _ := cc.getLeaderMaster(t)
	targetIdx := -1

	for idx, node := range cc.nodes {
		if node.Role != MasterNode {
			continue
		}
		if isLeader && node.Name == leaderName || !isLeader && node.Name != leaderName {
			targetIdx = idx
			break
		}
	}

	require.NotEqual(t, -1, targetIdx, "no suitable master node found for removal")

	node := cc.nodes[targetIdx]
	t.Logf("Removing master node: %s", node.Name)
	node.StopAndRemoveContainer(t)
	cc.nodes = append(cc.nodes[:targetIdx], cc.nodes[targetIdx+1:]...)
}

func nodeConfig(role, nodeName, masterAddresses string, replicationFactor int) []string {
	switch role {
	case MasterNode:
		return append(baseConfig("yb-master", nodeName, "7100"),
			"--master_addresses", masterAddresses,
			fmt.Sprintf("--replication_factor=%d", replicationFactor),
		)
	case TabletNode:
		return append(baseConfig("yb-tserver", nodeName, "9100"),
			"--start_pgsql_proxy",
			"--tserver_master_addrs", masterAddresses,
		)
	default:
		return nil
	}
}

func baseConfig(binary, nodeName, bindPort string) []string {
	return []string{
		path.Join("/home/yugabyte/bin", binary),
		"--fs_data_dirs=/mnt/disk0",
		"--rpc_bind_addresses", net.JoinHostPort(nodeName, bindPort),
	}
}

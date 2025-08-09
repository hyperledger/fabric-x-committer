/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
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
)

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
	for _, n := range cc.nodes {
		n.Cmd = nodeConfig(n.Role, n.Name, cc.getMasterAddresses(), cc.desiredRF())
		n.StartContainer(ctx, t)
	}
}

func (cc *YugaClusterController) getMasterAddresses() string {
	addrs := make([]string, 0, len(cc.nodes))
	for _, n := range cc.nodes {
		if n.Role == MasterNode {
			addrs = append(addrs, fmt.Sprintf("%s:7100", n.Name))
		}
	}
	return strings.Join(addrs, ",")
}

// RF=3 when number of tablets >=3, else RF=1.
func (cc *YugaClusterController) desiredRF() int {
	numberOfTablets := 0
	for _, n := range cc.nodes {
		if n.Role == TabletNode {
			numberOfTablets++
		}
	}
	if numberOfTablets >= 3 {
		return 3
	}
	return 1
}

func (cc *YugaClusterController) getLeaderMaster(t *testing.T) string {
	t.Helper()
	var output string
	for _, n := range cc.nodes {
		if out := n.ExecuteCommand(t, cc.getLeaderMasterCommand()); out != "" {
			output = out
			break
		}
	}

	if output == "" {
		t.Fatal("Could not get yb-admin output from any master")
	}

	// split by lines.
	for _, line := range strings.Split(output, "\n") {
		// if the line has a "LEADER" role mentioned, keep on.
		if strings.Contains(line, "LEADER") {
			// separate into fields.
			// fields[1] holds the host:port information.
			if fields := strings.Fields(line); len(fields) >= 2 {
				// split into host and port.
				host, _, err := net.SplitHostPort(fields[1])
				require.NoError(t, err)
				if host != "" {
					t.Logf("master leader is %s", host)
					return host
				}
			}
		}
	}
	t.Fatal("Could not find a LEADER in yb-admin output")
	return "" // unreachable but required for compiler
}

func (cc *DBClusterController) getConnectionsOfGivenRole(
	ctx context.Context,
	t *testing.T,
	role string,
) *dbtest.Connection {
	t.Helper()
	var endpoints []*connection.Endpoint
	for _, node := range cc.nodes {
		if node.Role == role {
			endpoints = append(endpoints, node.GetContainerConnectionDetails(ctx, t))
		}
	}
	return dbtest.NewConnection(endpoints...)
}

func (cc *YugaClusterController) getLeaderMasterCommand() []string {
	return []string{
		"/home/yugabyte/bin/yb-admin",
		"-init_master_addrs", cc.getMasterAddresses(),
		"list_all_masters",
	}
}

// RemoveLeaderMasterNode finds the leader of the master nodes, retrieve its container name and removes it.
func (cc *YugaClusterController) RemoveLeaderMasterNode(t *testing.T) {
	t.Helper()
	cc.removeMasterNode(t, true)
}

// RemoveNotLeaderMasterNode finds a master node, which is not a leader, retrieve its container name and removes it.
func (cc *YugaClusterController) RemoveNotLeaderMasterNode(t *testing.T) {
	t.Helper()
	cc.removeMasterNode(t, false)
}

// removeMaster removes a master node from the cluster.
// If removeLeader is true, it removes the current leader master.
// Otherwise, it removes any non-leader master.
//
//nolint:revive // flag-parameter is required here to avoid code duplication.
func (cc *YugaClusterController) removeMasterNode(t *testing.T, removeLeader bool) {
	t.Helper()
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster")

	leaderName := cc.getLeaderMaster(t)
	targetIdx := -1

	for idx, node := range cc.nodes {
		if node.Role != MasterNode {
			continue
		}
		if removeLeader && node.Name == leaderName {
			targetIdx = idx
			break
		}
		if !removeLeader && node.Name != leaderName {
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
			"--master_addresses="+masterAddresses,
			fmt.Sprintf("--replication_factor=%d", replicationFactor),
		)
	case TabletNode:
		return append(baseConfig("yb-tserver", nodeName, "9100"),
			"--start_pgsql_proxy",
			"--tserver_master_addrs="+masterAddresses,
		)
	default:
		return nil
	}
}

func baseConfig(binary, nodeName, bindPort string) []string {
	return []string{
		"/home/yugabyte/bin/" + binary,
		"--fs_data_dirs=/mnt/disk0",
		"--rpc_bind_addresses=" + nodeName + ":" + bindPort,
	}
}

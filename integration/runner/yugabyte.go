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
)

type (
	// YugaClusterController is a struct that facilitates the manipulation of a DB cluster,
	// with nodes running in Docker containers.
	// It allows configuring the number of master and tablet nodes.
	// The cluster's replication factor (RF) is determined as follows:
	//   - If the number of tablet nodes is greater than or equal to 3,
	//     RF is set to 3; otherwise, RF is set to 1.
	YugaClusterController struct {
		DBClusterController

		replicationFactor int
		networkName       string
	}

	nodeConfigParameters struct {
		role              string
		nodeName          string
		masterAddresses   string
		replicationFactor int
	}
)

const (
	// latest LTS.
	defaultImage = "yugabytedb/yugabyte:2024.2.4.0-b89"

	networkPrefix = "sc_yuga_net_"
	masterPort    = "7100"
	tabletPort    = "9100"

	// MasterNode represents yugabyte master db node.
	MasterNode = "master"
	// TabletNode represents yugabyte tablet db node.
	TabletNode = "tablet"

	// Tablet is a tablet (data node) in the cluster.
	Tablet = 1 << iota
	// NonLeaderMaster is a master node that does not hold the leader role.
	NonLeaderMaster
	// LeaderMaster is the master node currently acting as the leader.
	LeaderMaster
)

// leaderRegex is the compiled regular expression.
// to efficiently extract the leader master's RPC Host/Port.
var leaderRegex = regexp.MustCompile(`(?m)^[^\n]*[ \t]+(\S+):\d+[ \t]+[^\n]+[ \t]+LEADER[ \t]+[^\n]*$`)

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

	rf := 1
	if numberOfTablets >= 3 {
		rf = 3
	}

	cluster := &YugaClusterController{
		replicationFactor: rf,
		networkName:       fmt.Sprintf("%s%s", networkPrefix, uuid.NewString()),
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
	clusterConnection := cluster.getNodesConnectionsByRole(t, TabletNode)
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
	masterAddresses := cc.getMasterAddresses()
	for _, n := range cc.IterNodesByRole(MasterNode) {
		n.Cmd = nodeConfig(t, nodeConfigParameters{
			n.Role,
			n.Name,
			masterAddresses,
			cc.replicationFactor,
		})
		n.StartContainer(ctx, t)
	}

	expectedAlive := len(maps.Collect(cc.IterNodesByRole(MasterNode)))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, actualAlive := cc.getLeaderMaster(t)
		require.Equal(ct, expectedAlive, actualAlive)
	}, time.Minute, time.Millisecond*100)

	for _, n := range cc.IterNodesByRole(TabletNode) {
		n.Cmd = nodeConfig(t, nodeConfigParameters{
			n.Role,
			n.Name,
			masterAddresses,
			cc.replicationFactor,
		})
		n.StartContainer(ctx, t)
	}

	for _, n := range cc.IterNodesByRole(TabletNode) {
		n.EnsureNodeReadiness(t, "syncing data to disk ... ok")
	}
}

func (cc *YugaClusterController) getMasterAddresses() string {
	masterAddresses := make([]string, 0, len(cc.nodes))
	for _, n := range cc.IterNodesByRole(MasterNode) {
		masterAddresses = append(masterAddresses, net.JoinHostPort(n.Name, masterPort))
	}
	return strings.Join(masterAddresses, ",")
}

func (cc *YugaClusterController) getLeaderMaster(t *testing.T) (string, int) {
	t.Helper()
	var output string
	cmd := []string{
		"/home/yugabyte/bin/yb-admin",
		"-master_addresses", cc.getMasterAddresses(),
		"list_all_masters",
	}
	for _, n := range cc.nodes {
		if output = n.ExecuteCommand(t, cmd); output != "" {
			break
		}
	}
	require.NotEmpty(t, output, "Could not get yb-admin output from any node")

	found := leaderRegex.FindStringSubmatch(output)
	require.Greater(t, len(found), 1)
	return found[1], strings.Count(output, "ALIVE")
}

// StopAndRemoveMasterNode stops and removes a single master node from the cluster, based on the provided bitmask.
// The masterKind bitmask can include LeaderMaster, NonLeaderMaster, or both.
// * If both are set, the function stops and removes the first matching master it finds (leader or non-leader).
// * If only one is set, it stops and removes a matching master accordingly.
func (cc *YugaClusterController) StopAndRemoveMasterNode(t *testing.T, masterKind int) {
	t.Helper()
	require.NotZero(t, masterKind&(LeaderMaster|NonLeaderMaster),
		"mask should be set to LeaderMaster or NonLeaderMaster")
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster")

	leaderName, _ := cc.getLeaderMaster(t)
	targetIdx := -1

	for idx, node := range cc.IterNodesByRole(MasterNode) {
		isLeader := node.Name == leaderName
		if masterKind&LeaderMaster != 0 && isLeader || masterKind&NonLeaderMaster != 0 && !isLeader {
			targetIdx = idx
			break
		}
	}
	require.NotEqual(t, -1, targetIdx, "no suitable master node found for removal")

	cc.StopAndRemoveNodeByIndex(t, targetIdx)
}

func nodeConfig(t *testing.T, params nodeConfigParameters) []string {
	t.Helper()
	switch params.role {
	case MasterNode:
		return append(nodeCommonConfig("yb-master", params.nodeName, masterPort),
			"--master_addresses", params.masterAddresses,
			fmt.Sprintf("--replication_factor=%d", params.replicationFactor),
		)
	case TabletNode:
		return append(nodeCommonConfig("yb-tserver", params.nodeName, tabletPort),
			"--start_pgsql_proxy",
			"--tserver_master_addrs", params.masterAddresses,
		)
	default:
		t.Fatalf("unknown role provided: %s", params.role)
		return nil
	}
}

func nodeCommonConfig(binary, nodeName, bindPort string) []string {
	return []string{
		path.Join("/home/yugabyte/bin", binary),
		"--fs_data_dirs=/mnt/disk0",
		"--rpc_bind_addresses", net.JoinHostPort(nodeName, bindPort),
	}
}

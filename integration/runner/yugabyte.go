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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
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
	defaultImage = "yugabytedb/yugabyte:2025.2.0.1-b1"

	networkPrefix = "sc_yuga_net_"
	masterPort    = "7100"
	tabletPort    = "9100"

	// MasterNode represents yugabyte master db node.
	MasterNode = "master"
	// TabletNode represents yugabyte tablet db node.
	TabletNode = "tablet"
	// LeaderMasterNode represents the yugabyte master node currently serving as the Raft leader.
	LeaderMasterNode = "leader"
	// FollowerMasterNode represents a yugabyte master node that is not the leader (a follower).
	FollowerMasterNode = "follower"
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

	t.Logf("starting yuga cluster with (%d) masters and (%d) tablets ", numberOfMasters, numberOfTablets)

	// YugabyteDB uses Raft consensus for data replication across tablet
	// servers. With RF=3, each Raft group tolerates 1 node failure (majority
	// quorum = 2 of 3). With fewer than 3 tablets, RF=3 is impossible, so we
	// fall back to RF=1 (no fault tolerance, single-node mode).
	rf := 1
	if numberOfTablets >= 3 {
		rf = 3
	}

	cluster := &YugaClusterController{
		replicationFactor: rf,
		networkName:       fmt.Sprintf("%s%s", networkPrefix, uuid.NewString()),
	}
	// A dedicated Docker network enables container-to-container communication
	// via container names (Docker DNS). Masters and tservers use each other's
	// container names as RPC addresses for Raft consensus and heartbeats.
	test.CreateDockerNetwork(t, cluster.networkName)
	t.Cleanup(func() {
		test.RemoveDockerNetwork(t, cluster.networkName)
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

	// In YugabyteDB, masters manage cluster metadata (tablet locations, schema,
	// Raft leadership) while tservers handle all SQL read/write operations.
	// The application only connects to tservers for data access.
	clusterConnection := cluster.getNodesConnectionsByRole(t, TabletNode)
	// LoadBalance distributes new connections across all tservers in the
	// cluster instead of sending all traffic to a single node.
	clusterConnection.LoadBalance = true

	return cluster, clusterConnection
}

func (cc *YugaClusterController) createNode(role string) {
	node := &dbtest.DatabaseContainer{
		Name:         fmt.Sprintf("%s_yuga_%s_%s", test.DockerNamesPrefix, role, uuid.New().String()),
		Image:        defaultImage,
		Role:         role,
		DatabaseType: dbtest.YugaDBType,
		// All nodes join the same Docker network so they can resolve each
		// other by container name for inter-node RPC (Raft, heartbeats).
		Network: cc.networkName,
	}
	// Expose the YSQL port via host port mapping so the test client can reach
	// the tablet servers through a host-accessible address on all platforms.
	// Only tablets need this because masters are not involved in DB communication.
	if role == TabletNode {
		node.ExposePort("5433")
	}
	cc.nodes = append(cc.nodes, node)
}

func (cc *YugaClusterController) startNodes(ctx context.Context, t *testing.T) {
	t.Helper()
	masterAddresses := cc.getMasterAddresses()

	// Configure all nodes before starting any containers. Each node needs
	// the full list of master addresses so it can join the Raft consensus
	// group (masters) or discover where to register (tservers).
	for _, n := range cc.IterNodesByRole(MasterNode) {
		n.Cmd = nodeConfig(t, nodeConfigParameters{
			n.Role,
			n.Name,
			masterAddresses,
			cc.replicationFactor,
		})
	}
	for _, n := range cc.IterNodesByRole(TabletNode) {
		n.Cmd = nodeConfig(t, nodeConfigParameters{
			n.Role,
			n.Name,
			masterAddresses,
			cc.replicationFactor,
		})
	}

	// Start tservers BEFORE masters.
	//
	// Observation: when masters started first, the DB resiliency tests that
	// remove a single tablet server would permanently stall all transactions.
	// Inspecting the cluster with `yb-admin list_tablets system transactions`
	// showed the global transaction table (system.transactions) was created
	// with RF=2 instead of the expected RF=3.
	//
	// Root cause: the master leader creates system.transactions during its
	// initialization, using RF = min(num_registered_tservers, cluster_RF).
	// When masters start first, the leader initializes before all tservers
	// have sent their first heartbeat, so it sees only 2 registered tservers
	// and creates the table with RF=2. Note: the flag
	// `txn_table_wait_min_ts_count` does NOT help here — it only controls
	// a background task code path, not the leader initialization code path.
	//
	// Consequence: with RF=2, each Raft group for the transaction table has
	// only 2 replicas. Removing 1 tablet server drops some groups to 1/2
	// replicas, which is below majority quorum (need 2/2 alive). This
	// permanently stalls all distributed transactions. The table's RF
	// cannot be fixed after creation — modify_table_placement_info is
	// explicitly blocked for system.transactions by YugabyteDB.
	//
	// Fix: by starting tservers first, they are already retrying master
	// connections when the master comes up. All tservers register within
	// the first heartbeat window, so the leader sees 3 registered tservers
	// and creates the table with the full RF=3.
	for _, n := range cc.IterNodesByRole(TabletNode) {
		n.StartContainer(ctx, t)
	}
	for _, n := range cc.IterNodesByRole(MasterNode) {
		n.StartContainer(ctx, t)
	}

	// Wait for all masters to form a Raft quorum and elect a leader.
	// Until a leader is elected, the cluster cannot accept tserver
	// registrations, create system tables, or serve any metadata requests.
	expectedMasters := len(maps.Collect(cc.IterNodesByRole(MasterNode)))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, expectedMasters, strings.Count(cc.listAllMasters(t), "ALIVE"))
	}, time.Minute, time.Millisecond*100)

	// Wait for each tserver to finish bootstrapping (syncing data to disk).
	// Until this completes, the tserver is not ready to serve SQL queries.
	for _, n := range cc.IterNodesByRole(TabletNode) {
		n.EnsureNodeReadinessByLogs(t, dbtest.YugabyteTabletNodeReadinessOutput)
	}

	// Wait for all tservers to register with the master via heartbeats.
	// The master must know about all tservers before the cluster is ready
	// to serve queries reliably.
	expectedTablets := len(maps.Collect(cc.IterNodesByRole(TabletNode)))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, expectedTablets, strings.Count(cc.runYBAdmin(t, "list_all_tablet_servers"), "ALIVE"))
	}, time.Minute, time.Millisecond*500)

	// Verify the global transaction table was created with the expected RF.
	// This catches the race condition early instead of letting the test
	// stall for minutes when a tserver is later removed.
	cc.verifyTransactionTableRF(t)
}

func (cc *YugaClusterController) getMasterAddresses() string {
	masterAddresses := make([]string, 0, len(cc.nodes))
	for _, n := range cc.IterNodesByRole(MasterNode) {
		masterAddresses = append(masterAddresses, net.JoinHostPort(n.Name, masterPort))
	}
	return strings.Join(masterAddresses, ",")
}

func (cc *YugaClusterController) getLeaderMasterName(t *testing.T) string {
	t.Helper()
	found := leaderRegex.FindStringSubmatch(cc.listAllMasters(t))
	require.Greater(t, len(found), 1)
	return found[1]
}

func (cc *YugaClusterController) verifyTransactionTableRF(t *testing.T) {
	t.Helper()
	if cc.replicationFactor <= 1 {
		return
	}
	output := cc.runYBAdmin(t, "list_tablets", "system", "transactions", "0", "include_followers")
	for _, n := range cc.IterNodesByRole(TabletNode) {
		require.Contains(t, output, n.Name)
	}
}

func (cc *YugaClusterController) listAllMasters(t *testing.T) string {
	t.Helper()
	output := cc.runYBAdmin(t, "list_all_masters")
	require.NotEmpty(t, output, "Could not get yb-admin output from any node")
	return output
}

func (cc *YugaClusterController) runYBAdmin(t *testing.T, args ...string) string {
	t.Helper()
	cmd := append([]string{
		"/home/yugabyte/bin/yb-admin",
		"-master_addresses", cc.getMasterAddresses(),
	}, args...)
	var output string
	for _, n := range cc.nodes {
		if output = n.ExecuteCommand(t, cmd); output != "" {
			break
		}
	}
	return output
}

// StopAndRemoveSingleMasterNodeByRaftRole stops and removes a single
// master node from the cluster based on the provided role.
func (cc *YugaClusterController) StopAndRemoveSingleMasterNodeByRaftRole(t *testing.T, raftRole string) {
	t.Helper()
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster")

	leaderName := cc.getLeaderMasterName(t)
	targetIdx := -1

	for idx, node := range cc.IterNodesByRole(MasterNode) {
		isLeader := node.Name == leaderName
		if (raftRole == LeaderMasterNode && isLeader) || (raftRole == FollowerMasterNode && !isLeader) {
			targetIdx = idx
			break
		}
	}
	require.NotEqual(t, -1, targetIdx, "no suitable master node found for removal")

	cc.StopAndRemoveSingleNodeByIndex(t, targetIdx)
}

func nodeConfig(t *testing.T, params nodeConfigParameters) []string {
	t.Helper()
	switch params.role {
	case MasterNode:
		return append(nodeCommonConfig("yb-master", params.nodeName, masterPort),
			// List of all master nodes so they can form a Raft group.
			"--master_addresses", params.masterAddresses,
			// Desired number of replicas for system and user tables.
			fmt.Sprintf("--replication_factor=%d", params.replicationFactor),
			// Tell the master to wait for at least RF tservers before
			// creating the transaction status table. This controls
			// the background task code path; the leader initialization
			// path is covered by starting tservers before masters.
			fmt.Sprintf("--txn_table_wait_min_ts_count=%d", params.replicationFactor),
		)
	case TabletNode:
		return append(nodeCommonConfig("yb-tserver", params.nodeName, tabletPort),
			// Enable the YSQL (PostgreSQL-compatible) query layer.
			"--start_pgsql_proxy",
			// Master addresses so the tserver can register and discover tablets.
			"--tserver_master_addrs", params.masterAddresses,
			// Limit shards per tserver to reduce resource usage in tests.
			"--yb_num_shards_per_tserver=1",
			// By default YSQL binds to the container hostname only. With host
			// port forwarding the traffic arrives on 0.0.0.0 inside the
			// container, so we must listen on all interfaces.
			"--pgsql_proxy_bind_address=0.0.0.0",
			// glog (used by yb-tserver) defaults to writing logs to files.
			// EnsureNodeReadinessByLogs reads container stdout, so we redirect
			// logs to stderr which Docker captures.
			"--logtostderr",
			// Enable read committed isolation level. Without this,
			// YugabyteDB falls back to repeatable read for read committed
			// transactions, which changes concurrency behavior.
			"--yb_enable_read_committed_isolation=true",
			// Reduce heartbeat interval from the default 1000ms to 200ms.
			// The master leader creates system.transactions shortly after
			// leader election, using RF = min(registered_tservers, cluster_RF).
			// A faster heartbeat gives each tserver ~5x more registration
			// attempts in the narrow window between master leader election
			// and table creation, preventing the RF from being set lower
			// than expected.
			"--heartbeat_interval_ms=200",
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

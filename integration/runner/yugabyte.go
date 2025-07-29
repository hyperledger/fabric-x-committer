package runner

import (
	"bufio"
	"context"
	"fmt"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
)

const (
	defaultNetName = "cluster_net"
	defaultImage   = "yugabytedb/yugabyte:latest"
)

type YugaClusterControllerWithDifferentiations struct {
	DBClusterController
}

func StartYugaClusterWithTabletsAndMasters(ctx context.Context, t *testing.T, numberOfMasters, numberOfTablets uint) (
	*YugaClusterControllerWithDifferentiations, *dbtest.Connection,
) {
	t.Helper()

	//if runtime.GOOS != linuxOS {
	//	t.Skip("Container IP access not supported on non-linux Docker")
	//}

	dbtest.CreateDockerNetwork(t, defaultNetName)
	t.Cleanup(func() {
		dbtest.RemoveDockerNetwork(t, defaultNetName)
	})

	cluster := &YugaClusterControllerWithDifferentiations{}

	t.Logf("starting yuga cluster with (%d) masters and (%d) tablets ", numberOfMasters, numberOfTablets)

	// we create the nodes before startup, so we can make sure
	// that we hold the names/container IDs of the master nodes.
	// to start each node, we need to know this information from a head.
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

	cluster.getLeaderMaster(t)

	// the master nodes aren't relevant for the db communication, the application,
	// connects to the tablet servers.
	cluster.removeNodesWithRole(t, MasterNode)

	clusterConnection := cluster.getNodesConnections(ctx, t)
	clusterConnection.LoadBalance = true

	return cluster, clusterConnection
}

// createNode needed to create the nodes from a head to create a proper leader selection.
func (cc *YugaClusterControllerWithDifferentiations) createNode(role string) {
	node := &dbtest.DatabaseContainer{
		Name:         fmt.Sprintf("yuga-%s-%s", role, uuid.New().String()[:8]),
		Image:        defaultImage,
		Role:         role,
		DatabaseType: dbtest.YugaDBType,
		Network:      defaultNetName,
	}
	cc.nodes = append(cc.nodes, node)
}

func (cc *YugaClusterControllerWithDifferentiations) startNodes(ctx context.Context, t *testing.T) {
	t.Helper()

	for _, n := range cc.nodes {
		masterAddresses := cc.getMasterAddresses()
		t.Logf("master_addresses: %v", masterAddresses)
		switch n.Role {
		case MasterNode:
			n.Cmd = []string{
				"/home/yugabyte/bin/yb-master",
				"--fs_data_dirs=/mnt/disk0",
				"--rpc_bind_addresses=" + n.Name + ":7100",
				"--master_addresses=" + masterAddresses,
				fmt.Sprintf("--replication_factor=%d", cc.desiredRF()),
			}
		case TabletNode:
			n.Cmd = []string{
				"/home/yugabyte/bin/yb-tserver",
				"--fs_data_dirs=/mnt/disk0",
				"--start_pgsql_proxy",
				"--rpc_bind_addresses=" + n.Name + ":9100",
				"--tserver_master_addrs=" + masterAddresses,
			}
		}
		n.StartContainer(ctx, t)
	}
}

func (cc *YugaClusterControllerWithDifferentiations) getMasterAddresses() string {
	addrs := make([]string, 0, len(cc.nodes)+1)
	for _, n := range cc.nodes {
		if n.Role == MasterNode {
			addrs = append(addrs, fmt.Sprintf("%s:7100", n.Name))
		}
	}
	return strings.Join(addrs, ",")
}

func (cc *YugaClusterControllerWithDifferentiations) desiredRF() int {
	// RF=3 when number of masters >=3, else RF=1
	numberOfMasters := 0
	for _, n := range cc.nodes {
		if n.Role == MasterNode {
			numberOfMasters++
		}
	}
	if numberOfMasters >= 3 {
		return 3
	}
	return 1
}

func (cc *YugaClusterControllerWithDifferentiations) removeNodesWithRole(t *testing.T, role string) {
	t.Helper()
	require.NotEmpty(t, cc.nodes, "trying to remove nodes of an empty cluster.")
	for idx, node := range cc.nodes {
		if node.Role == role {
			cc.nodes = append(cc.nodes[:idx], cc.nodes[idx+1:]...)
		}
	}
}

func (cc *YugaClusterControllerWithDifferentiations) RemoveLeaderMasterNode(t *testing.T) {
	cc.StopAndRemoveNodeWithName(t, cc.getLeaderMaster(t))
}

func (cc *YugaClusterControllerWithDifferentiations) getLeaderMaster(t *testing.T) string {
	client := dbtest.GetDockerClient(t)

	var output, res string
	for _, n := range cc.nodes {
		exec, err := client.CreateExec(docker.CreateExecOptions{
			Container:    n.ContainerID(),
			Cmd:          []string{"/home/yugabyte/bin/yb-admin", "-init_master_addrs", cc.getMasterAddresses(), "list_all_masters"},
			AttachStdout: true,
			AttachStderr: true,
		})
		require.NoError(t, err)

		var stdout, stderr strings.Builder
		require.NoError(t, client.StartExec(exec.ID, docker.StartExecOptions{
			OutputStream: &stdout,
			ErrorStream:  &stderr,
			RawTerminal:  false,
		}))

		inspect, err := client.InspectExec(exec.ID)
		require.NoError(t, err)
		if inspect.ExitCode != 0 {
			t.Errorf("Exec in %s failed (code %d): %s", n.ContainerID(), inspect.ExitCode, stderr.String())
			continue
		}

		output = stdout.String()
		break // one successful output is enough
	}

	if output == "" {
		t.Fatal("Could not get yb-admin output from any master")
	}

	// Parse the output
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "LEADER") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				res = strings.Split(fields[1], ":")[0]
				fmt.Printf("🚀 Master leader is: %s\n", res)
				return res
			}
		}
	}

	t.Fatal("Could not find LEADER in yb-admin output")

	return "" //unreachable but required for compiler
}

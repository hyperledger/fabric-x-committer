/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	defaultPostgresDBName = "postgres"

	// PrimaryNode represents a primary postgres db node.
	PrimaryNode = "primary"
	// SecondaryNode represents a secondary postgres db node.
	SecondaryNode = "secondary"
)

var (
	// postgresSecondaryPromotionCommand promotes a standby to read-write primary.
	postgresSecondaryPromotionCommand = []string{
		"psql", "-U", "yugabyte", "-c", "SELECT pg_promote();",
	}

	// primaryReplicationCommands configures the primary for streaming replication:
	// create a dedicated replication role, allow replication in pg_hba.conf, and reload.
	// $PGDATA is /var/lib/postgresql/data in the official image.
	primaryReplicationCommands = [][]string{
		{"psql", "-U", "yugabyte", "-c", "CREATE ROLE repl_user WITH REPLICATION LOGIN PASSWORD 'repl_password';"},
		{"sh", "-c", `echo "host replication repl_user all md5" >> "$PGDATA/pg_hba.conf"`},
		{"psql", "-U", "yugabyte", "-c", "SELECT pg_reload_conf();"},
	}

	// secondaryStartupScript is passed to bash -c with the primary host as $1.
	//go:embed postgres_secondary_startup.sh
	secondaryStartupScript string
)

// PostgresClusterController is a struct that facilitates the manipulation of postgres cluster,
// with its nodes running in Docker containers.
// We create a cluster of size 2; one primary and one secondary.
type PostgresClusterController struct {
	DBClusterController
	networkName string
}

// StartPostgresCluster creates a postgres cluster with WAL streaming replication
// using the official postgres image. The setup follows the EDB streaming replication guide
// (https://www.enterprisedb.com/blog/how-set-streaming-replication-keep-your-postgresql-database-performant-and-date
// accessed 2026-02-13), omitting production concerns unnecessary for short-lived test clusters:
// WAL archiving (archive_mode/archive_command), replication slots, synchronous replication,
// and restore_command — since we use pg_basebackup -Xs for gapless WAL streaming.
func StartPostgresCluster(ctx context.Context, t *testing.T) (*PostgresClusterController, *dbtest.Connection) {
	t.Helper()

	networkName := fmt.Sprintf("sc_pg_net_%s", uuid.NewString())
	test.CreateDockerNetwork(t, networkName)
	t.Cleanup(func() {
		test.RemoveDockerNetwork(t, networkName)
	})

	cluster := &PostgresClusterController{
		networkName: networkName,
	}

	t.Log("starting postgres cluster")
	cluster.addPrimaryNode(ctx, t)
	cluster.addSecondaryNode(ctx, t)

	t.Cleanup(func() {
		cluster.stopAndRemoveCluster(t)
	})

	clusterConnections := cluster.getNodesConnections(t)
	// After the initial connection, we create a testing-db that will be used by the vc-service.
	clusterConnections.Database = defaultPostgresDBName

	return cluster, clusterConnections
}

func (cc *PostgresClusterController) addPrimaryNode(ctx context.Context, t *testing.T) {
	t.Helper()

	node := &dbtest.DatabaseContainer{
		Name:         fmt.Sprintf("%s_postgres_primary_%s", test.DockerNamesPrefix, uuid.New()),
		Role:         PrimaryNode,
		Image:        dbtest.DefaultPostgresImage,
		DatabaseType: dbtest.PostgresDBType,
		Network:      cc.networkName,
		// We use "yugabyte" as the superuser to match dbtest.NewConnection(),
		// which hardcodes this username for both YugabyteDB and PostgreSQL.
		Env: []string{
			"POSTGRES_USER=yugabyte",
			"POSTGRES_PASSWORD=yugabyte",
		},
		// Enable WAL streaming replication so secondaries can connect.
		Cmd: []string{
			"-c", "wal_level=replica",
			"-c", "max_wal_senders=10",
			"-c", "hot_standby=on",
		},
	}
	// Expose the PostgreSQL port via host port mapping so the test client
	// can connect through a host-accessible address on all platforms.
	node.ExposePort("5432")
	cc.nodes = append(cc.nodes, node)

	node.StartContainer(ctx, t)
	// The postgres image has a two-phase startup: a temporary server for
	// initdb (logs "ready" once), then the real server (logs "ready" again). We run
	// commands (psql, shell) inside the container immediately after this check, so
	// both phases must be complete. In unit tests this isn't needed because the
	// external TCP connection pool retries through the brief gap automatically.
	ensurePostgresFullyReady(t, node)

	for _, cmd := range primaryReplicationCommands {
		node.ExecuteCommand(t, cmd)
	}
}

func (cc *PostgresClusterController) addSecondaryNode(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NotEmpty(t, cc.nodes)

	primary, _ := cc.GetSingleNodeByRole(PrimaryNode)
	require.NotNil(t, primary)

	node := &dbtest.DatabaseContainer{
		Name:         fmt.Sprintf("%s_postgres_secondary_%s", test.DockerNamesPrefix, uuid.New()),
		Role:         SecondaryNode,
		Image:        dbtest.DefaultPostgresImage,
		DatabaseType: dbtest.PostgresDBType,
		Network:      cc.networkName,
		Entrypoint:   []string{"bash", "-c", secondaryStartupScript, "bash", primary.Name},
	}
	node.ExposePort("5432")
	cc.nodes = append(cc.nodes, node)

	node.StartContainer(ctx, t)
	node.EnsureNodeReadinessByLogs(t, dbtest.SecondaryPostgresNodeReadinessOutput)
}

func ensurePostgresFullyReady(t *testing.T, node *dbtest.DatabaseContainer) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		output := node.GetContainerLogs(t)
		count := strings.Count(output, dbtest.PostgresReadinesssOutput)
		require.GreaterOrEqual(ct, count, 2)
	}, 45*time.Second, 250*time.Millisecond)
}

// PromoteSecondaryNode runs a script that promotes the first follower db node
// it finds, from read-only to read-write node.
func (cc *PostgresClusterController) PromoteSecondaryNode(t *testing.T) {
	t.Helper()
	secondaryNode, _ := cc.GetSingleNodeByRole(SecondaryNode)
	require.NotNil(t, secondaryNode)
	secondaryNode.ExecuteCommand(t, postgresSecondaryPromotionCommand)
}

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
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const (
	// We're using the Bitnami PostgreSQL image because it simplifies the setup
	// and management of WAL replication between primary and secondary instances.
	// The image includes built-in scripts and environment variables that handle replication configuration
	// out of the box, which reduces complexity and potential
	// misconfigurations compared to setting it up manually with the official image.
	postgresImage             = "bitnami/postgresql"
	defaultPostgresDBName     = "postgres"
	defaultBitnamiPostgresTag = "latest"
)

var postgresSecondaryPromotionCommand = []string{
	"psql",
	"-U",
	"postgres",
	"-c",
	"SELECT pg_promote();",
}

type (
	// PostgresClusterController is a struct that facilitates the manipulation of postgres cluster,
	// with its nodes running in Docker containers.
	// We create a cluster of size 2; one primary and one secondary.
	PostgresClusterController struct {
		DBClusterController
	}

	postgresNodeCreationBundle struct {
		name           string
		requiredOutput string
		role           string
		additionalEnvs []string
	}
)

// StartPostgresCluster creates a postgres cluster in a Docker environment
// and returns its connection properties.
func StartPostgresCluster(ctx context.Context, t *testing.T) (*PostgresClusterController, *dbtest.Connection) {
	t.Helper()

	if runtime.GOOS != linuxOS {
		t.Skip("Container IP access not supported on non-linux Docker")
	}

	cluster := &PostgresClusterController{}

	t.Log("starting postgres cluster")
	cluster.addPrimaryNode(ctx, t)
	cluster.addSecondaryNode(ctx, t)

	t.Cleanup(func() {
		cluster.stopAndRemoveCluster(t)
	})

	clusterConnections := cluster.getNodesConnections(ctx, t)
	// bitnami/postgres requires explicit db name mentioning for initial connection.
	// After the initial connection, we create a testing-db that will be used by the vc-service.
	clusterConnections.Database = defaultPostgresDBName

	return cluster, clusterConnections
}

func (cc *PostgresClusterController) addPrimaryNode(ctx context.Context, t *testing.T) *dbtest.DatabaseContainer {
	t.Helper()
	return cc.createAndStartNode(
		ctx, t,
		postgresNodeCreationBundle{
			name:           fmt.Sprintf("postgres-primary-%s", uuid.New()),
			requiredOutput: "database system is ready to accept connections",
			role:           MasterNode,
			additionalEnvs: []string{
				"POSTGRESQL_REPLICATION_MODE=master",
				"POSTGRESQL_USERNAME=yugabyte",
				"POSTGRESQL_PASSWORD=yugabyte",
			},
		},
	)
}

func (cc *PostgresClusterController) addSecondaryNode(ctx context.Context, t *testing.T) *dbtest.DatabaseContainer {
	t.Helper()
	// the cluster has to contain a primary node.
	require.NotEmpty(t, cc.nodes)
	return cc.createAndStartNode(
		ctx, t,
		postgresNodeCreationBundle{
			name:           fmt.Sprintf("postgres-secondary-%s", uuid.New()),
			requiredOutput: "started streaming WAL from primary",
			role:           FollowerNode,
			additionalEnvs: []string{
				"POSTGRESQL_REPLICATION_MODE=slave",
				fmt.Sprintf("POSTGRESQL_MASTER_HOST=%s", cc.getLeaderHost(ctx, t)),
			},
		},
	)
}

func (cc *PostgresClusterController) createAndStartNode(
	ctx context.Context,
	t *testing.T,
	nodeCreationOpts postgresNodeCreationBundle,
) *dbtest.DatabaseContainer {
	t.Helper()
	node := &dbtest.DatabaseContainer{
		Name:         nodeCreationOpts.name,
		Role:         nodeCreationOpts.role,
		Image:        postgresImage,
		Tag:          defaultBitnamiPostgresTag,
		DatabaseType: dbtest.PostgresDBType,
		Env: append([]string{
			"POSTGRESQL_REPLICATION_USER=repl_user",
			"POSTGRESQL_REPLICATION_PASSWORD=repl_password",
			"ALLOW_EMPTY_PASSWORD=yes",
		}, nodeCreationOpts.additionalEnvs...),
	}

	cc.nodes = append(cc.nodes, node)
	node.StartContainer(ctx, t)

	require.NoError(t, nodeStartupRetry.Execute(ctx, func() error {
		t.Logf("starting db node %v with role: %v", node.Name, node.Role)
		node.StartContainer(ctx, t)
		return node.EnsureNodeReadiness(t, nodeCreationOpts.requiredOutput)
	}))

	return node
}

func (cc *PostgresClusterController) getNodesConnections(ctx context.Context, t *testing.T) *dbtest.Connection {
	t.Helper()
	endpoints := make([]*connection.Endpoint, cc.GetClusterSize())
	for i, node := range cc.nodes {
		endpoints[i] = node.GetContainerConnectionDetails(ctx, t)
	}

	return dbtest.NewConnection(endpoints...)
}

func (cc *PostgresClusterController) getLeaderHost(ctx context.Context, t *testing.T) string {
	t.Helper()

	require.NotEmpty(t, cc.nodes, "no nodes available in cluster")

	for _, node := range cc.nodes {
		if node.Role == MasterNode {
			return node.GetContainerConnectionDetails(ctx, t).GetHost()
		}
	}

	t.Fatal("no leader node found in cluster")
	return "" // unreachable, but required for compiler
}

// PromoteFollowerNode runs a script that promotes the first follower db node
// it finds, from read-only to read-write node.
func (cc *PostgresClusterController) PromoteFollowerNode(t *testing.T) {
	t.Helper()
	require.NotEmpty(t, cc.nodes)
	for _, node := range cc.nodes {
		if node.Role == FollowerNode {
			node.ExecuteCommand(t, postgresSecondaryPromotionCommand)
		}
	}
}

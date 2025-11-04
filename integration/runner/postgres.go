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
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	// We're using the Bitnami PostgreSQL image because it simplifies the setup
	// and management of WAL replication between primary and secondary instances.
	// The image includes built-in scripts and environment variables that handle replication configuration
	// out of the box, which reduces complexity and potential
	// misconfigurations compared to setting it up manually with the official image.
	// Bitnami has migrated the bitnami/postgresql image to bitnamilegacy/postgresql,
	// where it no longer receives updates or security patches.
	// Since this image is used only for testing purposes, we can retain it for now.
	postgresImage             = "bitnamilegacy/postgresql"
	defaultPostgresDBName     = "postgres"
	defaultBitnamiPostgresTag = "latest"

	// PrimaryNode represents a primary postgres db node.
	PrimaryNode = "primary"
	// SecondaryNode represents a secondary postgres db node.
	SecondaryNode = "secondary"
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

	postgresNodeCreationParameters struct {
		name           string
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

	clusterConnections := cluster.getNodesConnections(t)
	// bitnami/postgres requires explicit db name mentioning for initial connection.
	// After the initial connection, we create a testing-db that will be used by the vc-service.
	clusterConnections.Database = defaultPostgresDBName

	return cluster, clusterConnections
}

func (cc *PostgresClusterController) addPrimaryNode(ctx context.Context, t *testing.T) *dbtest.DatabaseContainer {
	t.Helper()
	node := cc.createNode(postgresNodeCreationParameters{
		name: fmt.Sprintf("postgres-primary-%s", uuid.New()),
		role: PrimaryNode,
		additionalEnvs: []string{
			"POSTGRESQL_REPLICATION_MODE=master",
			"POSTGRESQL_USERNAME=yugabyte",
			"POSTGRESQL_PASSWORD=yugabyte",
		},
	})
	node.StartContainer(ctx, t)
	node.EnsurePostgresNodeReadiness(t, "5432")
	return node
}

func (cc *PostgresClusterController) addSecondaryNode(ctx context.Context, t *testing.T) *dbtest.DatabaseContainer {
	t.Helper()
	// the cluster has to contain a primary node.
	require.NotEmpty(t, cc.nodes)
	t.Helper()
	primary, _ := cc.GetSingleNodeByRole(PrimaryNode)
	require.NotNil(t, primary)

	node := cc.createNode(postgresNodeCreationParameters{
		name: fmt.Sprintf("postgres-secondary-%s", uuid.New()),
		role: SecondaryNode,
		additionalEnvs: []string{
			"POSTGRESQL_REPLICATION_MODE=slave",
			fmt.Sprintf("POSTGRESQL_MASTER_HOST=%s", primary.GetContainerConnectionDetails(t).GetHost()),
		},
	})
	node.StartContainer(ctx, t)
	node.EnsurePostgresNodeReadiness(t, "5432")
	return node
}

func (cc *PostgresClusterController) createNode(
	nodeCreationOpts postgresNodeCreationParameters,
) *dbtest.DatabaseContainer {
	node := &dbtest.DatabaseContainer{
		Name:         nodeCreationOpts.name,
		Role:         nodeCreationOpts.role,
		Image:        postgresImage,
		Tag:          defaultBitnamiPostgresTag,
		DatabaseType: test.PostgresDBType,
		Env: append([]string{
			"POSTGRESQL_REPLICATION_USER=repl_user",
			"POSTGRESQL_REPLICATION_PASSWORD=repl_password",
			"ALLOW_EMPTY_PASSWORD=yes",
		}, nodeCreationOpts.additionalEnvs...),
	}
	cc.nodes = append(cc.nodes, node)

	return node
}

// PromoteSecondaryNode runs a script that promotes the first follower db node
// it finds, from read-only to read-write node.
func (cc *PostgresClusterController) PromoteSecondaryNode(t *testing.T) {
	t.Helper()
	secondaryNode, _ := cc.GetSingleNodeByRole(SecondaryNode)
	require.NotNil(t, secondaryNode)
	_, exitCode := secondaryNode.ExecuteCommand(t, postgresSecondaryPromotionCommand)
	require.Zero(t, exitCode)
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/dbconn"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	committerReleaseImage = "docker.io/hyperledger/fabric-x-committer:latest"
	loadgenReleaseImage   = "docker.io/hyperledger/fabric-x-loadgen:latest"
	networkPrefixName     = test.DockerNamesPrefix + "_network"
	genBlockFile          = "sc-genesis-block.proto.bin"
	// containerConfigPath is the path to the config directory inside the container.
	containerConfigPath = "/root/config"
	// localConfigPath is the path to the sample YAML configuration of each service.
	localConfigPath = "../../cmd/config/samples"

	// containerPathForYugabytePassword holds the path to the database credentials inside the docker container.
	// This work-around is needed due to a Yugabyte behavior that prevents using default passwords in secure mode.
	// Instead, Yugabyte generates a random password, and this path points to the output file containing it.
	containerPathForYugabytePassword = "/root/var/data/yugabyted_credentials.txt" //nolint:gosec

	defaultDBPort = "5433"
)

// enforcePostgresSSLAndReloadConfigScript enforces SSL-only client connections to a PostgreSQL
// instance by updating pg_hba.conf and reloads its server configuration without restarting the instance.
var enforcePostgresSSLAndReloadConfigScript = []string{
	"sh", "-c",
	`sed -i 's/^host all all all scram-sha-256$/hostssl all all 0.0.0.0\/0 scram-sha-256/' ` +
		`/var/lib/postgresql/data/pg_hba.conf`,
	`psql -U yugabyte -c "SELECT pg_reload_conf();"`,
}

// TestCommitterReleaseImagesWithTLS runs the committer components in different Docker containers with different TLS
// modes and verifies it starts and connect successfully.
// This test uses the release images for all the components but 'db' and 'orderer'.
func TestCommitterReleaseImagesWithTLS(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Log("creating config-block")
	configBlockPath := filepath.Join(t.TempDir(), genBlockFile)
	v := config.NewViperWithLoadGenDefaults()
	c, err := config.ReadLoadGenYamlAndSetupLogging(v, filepath.Join(localConfigPath, "loadgen.yaml"))
	require.NoError(t, err)
	c.LoadProfile.Policy.CryptoMaterialPath = t.TempDir()
	configBlock, err := workload.CreateConfigBlock(&c.LoadProfile.Policy)
	require.NoError(t, err)
	require.NoError(t, configtxgen.WriteOutputBlock(configBlock, configBlockPath))

	dbNode := "db"
	ordererNode := "orderer"
	loadgenNode := "loadgen"
	committerNodes := []string{"verifier", "vc", "query", "coordinator", "sidecar"}

	credsFactory := test.NewCredentialsFactory(t)
	for _, dbType := range []string{dbtest.YugaDBType, dbtest.PostgresDBType} {
		t.Run(fmt.Sprintf("database:%s", dbType), func(t *testing.T) {
			t.Parallel()
			for _, mode := range test.ServerModes {
				t.Run(fmt.Sprintf("tls-mode:%s", mode), func(t *testing.T) {
					t.Parallel()
					// Create an isolated network for each test with different tls mode.
					networkName := fmt.Sprintf("%s_%s", networkPrefixName, uuid.NewString())
					test.CreateDockerNetwork(t, networkName)
					t.Cleanup(func() {
						test.RemoveDockerNetwork(t, networkName)
					})

					params := startNodeParameters{
						credsFactory:    credsFactory,
						networkName:     networkName,
						tlsMode:         mode,
						configBlockPath: configBlockPath,
						dbType:          dbType,
					}

					for _, node := range append(committerNodes, dbNode, ordererNode, loadgenNode) {
						// stop and remove the container if it already exists.
						stopAndRemoveContainersByName(
							ctx, t, createDockerClient(t), assembleContainerName(node, mode, dbType),
						)
					}

					// start a secured database node and return the db password.
					params.dbPassword = startSecuredDatabaseNode(ctx, t, params.asNode(dbNode))
					// start the orderer node.
					startCommitterNodeWithTestImage(ctx, t, params.asNode(ordererNode))
					// start the committer nodes.
					for _, node := range committerNodes {
						startCommitterNodeWithReleaseImage(ctx, t, params.asNode(node))
					}
					// start the load generator node.
					startLoadgenNodeWithReleaseImage(ctx, t, params.asNode(loadgenNode))

					monitorMetric(t,
						getContainerMappedHostPort(
							ctx, t, assembleContainerName("loadgen", mode, dbType), loadGenMetricsPort,
						),
					)
				})
			}
		})
	}
}

// CreateAndStartSecuredDatabaseNode creates a containerized YugabyteDB or PostgreSQL
// database instance in a secure mode.
func startSecuredDatabaseNode(ctx context.Context, t *testing.T, params startNodeParameters) string {
	t.Helper()

	tlsConfig, _ := params.credsFactory.CreateServerCredentials(t, params.tlsMode, params.node)

	node := &dbtest.DatabaseContainer{
		DatabaseType: params.dbType,
		Network:      params.networkName,
		Hostname:     params.node,
		DbPort:       defaultDBPort,
		TLSConfig:    &tlsConfig,
	}

	node.StartContainer(ctx, t)
	t.Cleanup(func() {
		node.StopAndRemoveContainer(t)
	})
	conn := node.GetConnectionOptions(ctx, t)

	// This is relevant if a different CA was used to issue the DB's TLS certificates.
	require.NotEmpty(t, tlsConfig.CACertPaths)
	conn.TLS = dbconn.DatabaseTLSConfig{
		Mode:       connection.OneSideTLSMode,
		CACertPath: tlsConfig.CACertPaths[0],
	}

	// post start container tweaking
	switch node.DatabaseType {
	case dbtest.YugaDBType:
		// Must run after node startup to ensure proper root ownership and permissions for the TLS certificate files.
		node.ExecuteCommand(t, []string{"bash", "-c", "chown root:root /creds/*"})
		node.EnsureNodeReadinessByLogs(t, dbtest.YugabytedReadinessOutput)
		conn.Password = node.ReadPasswordFromContainer(t, containerPathForYugabytePassword)
	case dbtest.PostgresDBType:
		// Must run after node startup to ensure proper root ownership and permissions for the TLS certificate files.
		node.ExecuteCommand(t, []string{"bash", "-c", "chown postgres:postgres /creds/*"})
		node.EnsureNodeReadinessByLogs(t, dbtest.PostgresReadinesssOutput)
		node.ExecuteCommand(t, enforcePostgresSSLAndReloadConfigScript)
	default:
		t.Fatalf("Unsupported database type: %s", node.DatabaseType)
	}

	return conn.Password
}

// startCommitterNodeWithReleaseImage starts a committer node using the release image.
func startCommitterNodeWithReleaseImage(ctx context.Context, t *testing.T, params startNodeParameters) {
	t.Helper()

	configPath := filepath.Join(containerConfigPath, params.node)
	containerUser := "0:0"
	t.Logf("Starting %s as container with user %s.\n", committerReleaseImage, containerUser)
	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image: committerReleaseImage,
			Cmd: []string{
				fmt.Sprintf("start-%s", params.node),
				"--config",
				fmt.Sprintf("%s.yaml", configPath),
			},
			Hostname: params.node,
			User:     containerUser,
			Env: []string{
				"SC_COORDINATOR_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_COORDINATOR_VERIFIER_TLS_MODE=" + params.tlsMode,
				"SC_COORDINATOR_VALIDATOR_COMMITTER_TLS_MODE=" + params.tlsMode,
				"SC_QUERY_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_COMMITTER_TLS_MODE=" + params.tlsMode,
				"SC_VC_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_VERIFIER_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_ORDERER_TLS_MODE=" + params.tlsMode,
				"SC_VC_DATABASE_PASSWORD=" + params.dbPassword,
				"SC_QUERY_DATABASE_PASSWORD=" + params.dbPassword,
			},
			Tty: true,
		},
		hostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(params.networkName),
			Binds: assembleBinds(t, params,
				fmt.Sprintf("%s.yaml:/%s.yaml",
					filepath.Join(mustGetWD(t), localConfigPath, params.node), configPath,
				),
			),
		},
		name: assembleContainerName(params.node, params.tlsMode, params.dbType),
	})
}

// startLoadgenNodeWithReleaseImage starts a load generator container using the release image.
func startLoadgenNodeWithReleaseImage(
	ctx context.Context,
	t *testing.T,
	params startNodeParameters,
) {
	t.Helper()

	configPath := filepath.Join(containerConfigPath, params.node)
	containerUser := "0:0"
	t.Logf("Starting %s as container with user %s.\n", loadgenReleaseImage, containerUser)
	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image: loadgenReleaseImage,
			Cmd: []string{
				"start",
				"--config",
				fmt.Sprintf("%s.yaml", configPath),
			},
			Hostname: params.node,
			User:     containerUser,
			ExposedPorts: nat.PortSet{
				loadGenMetricsPort + "/tcp": {},
			},
			Tty: true,
			Env: []string{
				"SC_LOADGEN_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_LOADGEN_ORDERER_CLIENT_SIDECAR_CLIENT_TLS_MODE=" + params.tlsMode,
				"SC_LOADGEN_ORDERER_CLIENT_ORDERER_TLS_MODE=" + params.tlsMode,
			},
		},
		hostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(params.networkName),
			PortBindings: nat.PortMap{
				loadGenMetricsPort + "/tcp": []nat.PortBinding{{
					HostIP:   "localhost",
					HostPort: "0", // auto port assign
				}},
			},
			Binds: assembleBinds(t, params,
				fmt.Sprintf("%s.yaml:/%s.yaml",
					filepath.Join(mustGetWD(t), localConfigPath, params.node), configPath,
				),
			),
		},
		name: assembleContainerName(params.node, params.tlsMode, params.dbType),
	})
}

// startCommitterNodeWithTestImage starts a committer node using the test image (used for: DB, orderer).
func startCommitterNodeWithTestImage(
	ctx context.Context,
	t *testing.T,
	params startNodeParameters,
) {
	t.Helper()

	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image:    testNodeImage,
			Cmd:      []string{"run", params.node},
			Tty:      true,
			Hostname: params.node,
			Env: []string{
				"SC_ORDERER_SERVER_TLS_MODE=" + params.tlsMode,
			},
		},
		hostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(params.networkName),
			Binds: assembleBinds(t, params,
				fmt.Sprintf("%s:/%s", params.configBlockPath, filepath.Join(containerConfigPath, genBlockFile)),
			),
		},
		name: assembleContainerName(params.node, params.tlsMode, params.dbType),
	})
}

// mustGetWD returns the current working directory.
func mustGetWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return wd
}

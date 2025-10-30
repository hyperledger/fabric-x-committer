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
	"github.com/hyperledger/fabric-x-common/internaltools/configtxgen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	testutils "github.com/hyperledger/fabric-x-committer/utils/test"
)

type startNodeParameters struct {
	credsFactory    *testutils.CredentialsFactory
	node            string
	networkName     string
	tlsMode         string
	configBlockPath string
	dbType          string
	dbPassword      string
}

const (
	committerReleaseImage = "icr.io/cbdc/committer:0.0.2"
	loadgenReleaseImage   = "icr.io/cbdc/loadgen:0.0.2"
	containerPrefixName   = "sc_test"
	networkPrefixName     = containerPrefixName + "_network"
	genBlockFile          = "sc-genesis-block.proto.bin"
	// containerConfigPath is the path to the config directory inside the container.
	containerConfigPath = "/root/config"
	// localConfigPath is the path to the sample YAML configuration of each service.
	localConfigPath = "../../cmd/config/samples"

	// containerPathForYugabytePassword holds the path to the database credentials inside the docker container.
	// This work-around is needed due to a Yugabyte behavior that prevents using default passwords in secure mode.
	// Instead, Yugabyte generates a random password, and this path points to the output file containing it.
	containerPathForYugabytePassword = "/root/var/data/yugabyted_credentials.txt" //nolint:gosec
)

var (
	// enforcePostgresSSLScript enforces SSL-only client connections to a PostgreSQL instance by updating pg_hba.conf.
	enforcePostgresSSLScript = []string{
		"sh", "-c",
		`sed -i 's/^host all all all scram-sha-256$/hostssl all all 0.0.0.0\/0 scram-sha-256/' ` +
			`/var/lib/postgresql/data/pg_hba.conf`,
	}

	// reloadPostgresConfigScript reloads the PostgreSQL server configuration without restarting the instance.
	reloadPostgresConfigScript = []string{
		"psql", "-U", "yugabyte", "-c", "SELECT pg_reload_conf();",
	}
)

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
	configBlock, err := workload.CreateConfigBlock(c.LoadProfile.Transaction.Policy)
	require.NoError(t, err)
	require.NoError(t, configtxgen.WriteOutputBlock(configBlock, configBlockPath))

	credsFactory := testutils.NewCredentialsFactory(t)
	for _, mode := range testutils.ServerModes {
		t.Run(fmt.Sprintf("tls-mode:%s", mode), func(t *testing.T) {
			t.Parallel()
			// Create an isolated network for each test with different tls mode.
			networkName := fmt.Sprintf("%s_%s", networkPrefixName, uuid.NewString())
			testutils.CreateDockerNetwork(t, networkName)
			t.Cleanup(func() {
				testutils.RemoveDockerNetwork(t, networkName)
			})

			params := startNodeParameters{
				credsFactory:    credsFactory,
				networkName:     networkName,
				tlsMode:         mode,
				configBlockPath: configBlockPath,
				dbType:          testutils.YugaDBType,
			}

			for _, node := range []string{
				"db", "verifier", "vc", "query", "coordinator", "sidecar", "orderer", "loadgen",
			} {
				params.node = node
				// stop and remove the container if it already exists.
				stopAndRemoveContainersByName(ctx, t, createDockerClient(t), assembleContainerName(node, mode))

				switch node {
				case "db":
					params.dbPassword = startSecuredDatabaseNode(ctx, t, params).Password
				case "orderer":
					startCommitterNodeWithTestImage(ctx, t, params)
				case "loadgen":
					startLoadgenNodeWithReleaseImage(ctx, t, params)
				default:
					startCommitterNodeWithReleaseImage(ctx, t, params)
				}
			}
			monitorMetric(t,
				getContainerMappedHostPort(ctx, t, assembleContainerName("loadgen", mode), loadGenMetricsPort),
			)
		})
	}
}

// CreateAndStartSecuredDatabaseNode creates a containerized YugabyteDB or PostgreSQL
// database instance in a secure mode.
func startSecuredDatabaseNode(ctx context.Context, t *testing.T, params startNodeParameters) *dbtest.Connection {
	t.Helper()

	tlsConfig, credsPath := params.credsFactory.CreateServerCredentials(t, params.tlsMode, params.dbType, params.node)

	node := &dbtest.DatabaseContainer{
		DatabaseType: params.dbType,
		Network:      params.networkName,
		Hostname:     params.node,
		TLSConfig:    tlsConfig,
		CredsPathDir: credsPath,
		UseTLS:       true,
	}

	node.StartContainer(ctx, t)
	conn := node.GetConnectionOptions(ctx, t)

	// This is relevant if a different CA was used to issue the DB's TLS certificates.
	require.NotEmpty(t, node.TLSConfig.CACertPaths)
	conn.TLS = connection.DatabaseTLSConfig{
		Mode:       connection.OneSideTLSMode,
		CACertPath: node.TLSConfig.CACertPaths[0],
	}

	// post start container tweaking
	switch node.DatabaseType {
	case testutils.YugaDBType:
		// Ensure proper root ownership and permissions for the TLS certificate files.
		node.ExecuteCommand(t, []string{
			"chown", "root:root",
			fmt.Sprintf("/creds/node.%s.crt", node.Hostname),
			fmt.Sprintf("/creds/node.%s.key", node.Hostname),
		})
		node.EnsureNodeReadiness(t, "Data placement constraint successfully verified")
		conn.Password = node.ReadPasswordFromContainer(t, containerPathForYugabytePassword)
	case testutils.PostgresDBType:
		// Ensure proper root ownership and permissions for the TLS certificate files.
		node.ExecuteCommand(t, []string{
			"chown", "postgres:postgres",
			"/creds/server.crt",
			"/creds/server.key",
		})
		node.EnsureNodeReadiness(t, dbtest.PostgresReadinessOutput)
		node.ExecuteCommand(t, enforcePostgresSSLScript)
		node.ExecuteCommand(t, reloadPostgresConfigScript)
	default:
		t.Fatalf("Unsupported database type: %s", node.DatabaseType)
	}

	t.Cleanup(
		func() {
			node.StopAndRemoveContainer(t)
		})

	return conn
}

// startCommitterNodeWithReleaseImage starts a committer node using the release image.
func startCommitterNodeWithReleaseImage(ctx context.Context, t *testing.T, params startNodeParameters) {
	t.Helper()

	configPath := filepath.Join(containerConfigPath, params.node)
	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image: committerReleaseImage,
			Cmd: []string{
				"committer",
				fmt.Sprintf("start-%s", params.node),
				"--config",
				fmt.Sprintf("%s.yaml", configPath),
			},
			Hostname: params.node,
			Env: []string{
				"SC_COORDINATOR_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_COORDINATOR_VERIFIER_TLS_MODE=" + params.tlsMode,
				"SC_COORDINATOR_VALIDATOR_COMMITTER_TLS_MODE=" + params.tlsMode,
				"SC_QUERY_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_COMMITTER_TLS_MODE=" + params.tlsMode,
				"SC_VC_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_VERIFIER_SERVER_TLS_MODE=" + params.tlsMode,
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
		name: assembleContainerName(params.node, params.tlsMode),
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
	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image: loadgenReleaseImage,
			Cmd: []string{
				params.node,
				"start",
				"--config",
				fmt.Sprintf("%s.yaml", configPath),
			},
			Hostname: params.node,
			ExposedPorts: nat.PortSet{
				loadGenMetricsPort + "/tcp": {},
			},
			Tty: true,
			Env: []string{
				"SC_LOADGEN_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_LOADGEN_ORDERER_CLIENT_SIDECAR_CLIENT_TLS_MODE=" + params.tlsMode,
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
		name: assembleContainerName(params.node, params.tlsMode),
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
		},
		hostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(params.networkName),
			Binds: assembleBinds(t, params,
				fmt.Sprintf("%s:/%s", params.configBlockPath, filepath.Join(containerConfigPath, genBlockFile)),
			),
		},
		name: assembleContainerName(params.node, params.tlsMode),
	})
}

func assembleContainerName(node, tlsMode string) string {
	return fmt.Sprintf("%s_%s_%s", containerPrefixName, node, tlsMode)
}

func assembleBinds(t *testing.T, params startNodeParameters, additionalBinds ...string) []string {
	t.Helper()

	_, serverCredsPath := params.credsFactory.CreateServerCredentials(
		t, params.tlsMode, testutils.DefaultCertStyle, params.node,
	)
	require.NotEmpty(t, serverCredsPath)
	_, clientCredsPath := params.credsFactory.CreateClientCredentials(t, params.tlsMode)
	require.NotEmpty(t, clientCredsPath)

	return append([]string{
		fmt.Sprintf("%s:/server-certs", serverCredsPath),
		fmt.Sprintf("%s:/client-certs", clientCredsPath),
	}, additionalBinds...)
}

// mustGetWD returns the current working directory.
func mustGetWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return wd
}

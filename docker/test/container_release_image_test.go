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
	testutils "github.com/hyperledger/fabric-x-committer/utils/test"
)

type startNodeParameters struct {
	credsFactory    *testutils.CredentialsFactory
	node            string
	networkName     string
	tlsMode         string
	configBlockPath string
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

			for _, node := range []string{
				"db", "verifier", "vc", "query", "coordinator", "sidecar", "orderer", "loadgen",
			} {
				params := startNodeParameters{
					credsFactory:    credsFactory,
					node:            node,
					networkName:     networkName,
					tlsMode:         mode,
					configBlockPath: configBlockPath,
				}

				// stop and remove the container if it already exists.
				stopAndRemoveContainersByName(ctx, t, createDockerClient(t), assembleContainerName(node, mode))

				switch node {
				case "db", "orderer":
					startCommitterNodeWithTestImage(ctx, t, params)
				case "loadgen":
					startLoadgenNodeWithReleaseImage(ctx, t, params)
				default:
					startCommitterNodeWithReleaseImage(ctx, t, params)
				}
			}
			monitorMetric(t,
				containerMappedHostPort(ctx, t, assembleContainerName("loadgen", mode), loadGenMetricsPort),
			)
		})
	}
}

// startCommitterNodeWithReleaseImage starts a committer node using the release image.
func startCommitterNodeWithReleaseImage(
	ctx context.Context,
	t *testing.T,
	params startNodeParameters,
) {
	t.Helper()
	_, serverCredsPath := params.credsFactory.CreateServerCredentials(t, params.tlsMode, params.node)
	require.NotEmpty(t, serverCredsPath)
	_, clientCredsPath := params.credsFactory.CreateClientCredentials(t, params.tlsMode)
	require.NotEmpty(t, clientCredsPath)

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
			},
			Tty: true,
		},
		hostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(params.networkName),
			Binds: assembleBinds(t,
				serverCredsPath,
				clientCredsPath,
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
	_, serverCredsPath := params.credsFactory.CreateServerCredentials(t, params.tlsMode, params.node)
	require.NotEmpty(t, serverCredsPath)
	_, clientCredsPath := params.credsFactory.CreateClientCredentials(t, params.tlsMode)
	require.NotEmpty(t, clientCredsPath)

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
			Binds: assembleBinds(t,
				serverCredsPath,
				clientCredsPath,
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
	_, serverCredsPath := params.credsFactory.CreateServerCredentials(t, params.tlsMode, params.node)
	require.NotEmpty(t, serverCredsPath)
	_, clientCredsPath := params.credsFactory.CreateClientCredentials(t, params.tlsMode)
	require.NotEmpty(t, clientCredsPath)

	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image:    testNodeImage,
			Cmd:      []string{"run", params.node},
			Tty:      true,
			Hostname: params.node,
		},
		hostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(params.networkName),
			Binds: assembleBinds(t,
				serverCredsPath,
				clientCredsPath,
				fmt.Sprintf("%s:/%s", params.configBlockPath, filepath.Join(containerConfigPath, genBlockFile)),
			),
		},
		name: assembleContainerName(params.node, params.tlsMode),
	})
}

func assembleContainerName(node, tlsMode string) string {
	return fmt.Sprintf("%s_%s_%s", containerPrefixName, node, tlsMode)
}

func assembleBinds(t *testing.T, serverCredsPath, clientCredsPath string, additionalBinds ...string) []string {
	t.Helper()
	return append([]string{
		fmt.Sprintf("%s:/certs", serverCredsPath),
		fmt.Sprintf("%s:/client_certs", clientCredsPath),
	}, additionalBinds...)
}

// mustGetWD returns the current working directory.
func mustGetWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return wd
}

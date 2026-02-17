/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	_ "embed"
	"fmt"
	"math/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/service/sidecar/sidecarclient"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	sidecarPort            = "4001"
	loadGenMetricsPort     = "2118"
	mockOrdererPort        = "7050"
	queryServicePort       = "7001"
	coordinatorServicePort = "9001"
	databasePort           = "5433"

	committerContainerName = "committer"
)

var commonTestNodeCMD = []string{"run", "db", "committer", "orderer"}

// TestStartTestNodeWithTLSModesAndRemoteConnection launches the committer’s
// all-in-one Docker image under each TLS mode, verifies that remote (non-internal)
// clients can connect to all exposed services, and runs a full transaction cycle and querying it.
func TestStartTestNodeWithTLSModesAndRemoteConnection(t *testing.T) {
	t.Parallel()

	credsFactory := test.NewCredentialsFactory(t)

	for _, mode := range test.ServerModes {
		t.Run(fmt.Sprintf("tls-mode:%s", mode), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			containerName := assembleContainerName(committerContainerName, mode, dbtest.PostgresDBType)
			stopAndRemoveContainersByName(ctx, t, createDockerClient(t), containerName)
			startCommitter(ctx, t, startNodeParameters{
				node:         containerName,
				credsFactory: credsFactory,
				tlsMode:      mode,
				cmd:          commonTestNodeCMD,
			})

			// Retrieve the policy from the loadgen configuration that matches the config-block policy.
			// We do this in each subtest to avoid race conditions.
			v := config.NewViperWithLoadGenDefaults()
			c, err := config.ReadLoadGenYamlAndSetupLogging(v, filepath.Join(localConfigPath, "loadgen.yaml"))
			require.NoError(t, err)
			ordererEp := mustGetEndpoint(ctx, t, containerName, mockOrdererPort)
			c.LoadProfile.Policy.OrdererEndpoints = []*commontypes.OrdererEndpoint{
				{
					Host: ordererEp.Host, Port: ordererEp.Port, ID: 0, MspID: "org",
					API: []string{commontypes.Broadcast, commontypes.Deliver},
				},
			}
			runtime := runner.CommitterRuntime{
				CredFactory:      credsFactory,
				SeedForCryptoGen: rand.New(rand.NewSource(10)),
				Config:           &runner.Config{},
				SystemConfig: config.SystemConfig{
					Endpoints: config.SystemEndpoints{
						Sidecar: config.ServiceEndpoints{
							Server: mustGetEndpoint(ctx, t, containerName, sidecarPort),
						},
						Query: config.ServiceEndpoints{
							Server: mustGetEndpoint(ctx, t, containerName, queryServicePort),
						},
						Coordinator: config.ServiceEndpoints{
							Server: mustGetEndpoint(ctx, t, containerName, coordinatorServicePort),
						},
					},
					Policy: &c.LoadProfile.Policy,
				},
				DBEnv: vc.NewDatabaseTestEnvFromConnection(
					t,
					dbtest.NewConnection(mustGetEndpoint(ctx, t, containerName, databasePort)),
					false,
				),
			}
			runtime.SystemConfig.ClientTLS, _ = credsFactory.CreateClientCredentials(t, mode)
			runtime.CreateRuntimeClients(ctx, t)
			runtime.OpenNotificationStream(ctx, t)

			// Adding namespace policy and creating transaction builder
			runtime.AddOrUpdateNamespaces(t, "1")

			runtime.CommittedBlock = sidecarclient.StartSidecarClient(ctx, t, &sidecarclient.Parameters{
				ChannelID: channelName,
				Client: test.NewTLSClientConfig(
					runtime.SystemConfig.ClientTLS, runtime.SystemConfig.Endpoints.Sidecar.Server,
				),
			}, 0)

			t.Log("Try to fetch the first block")
			b, ok := channel.NewReader(ctx, runtime.CommittedBlock).Read()
			require.True(t, ok)
			t.Logf("Received block #%d with %d TXs", b.Header.Number, len(b.Data.Data))

			// Committing namespace transaction
			runtime.CreateNamespacesAndCommit(t, "1")

			t.Log("Insert TXs")
			txIDs := runtime.MakeAndSendTransactionsToOrderer(t, [][]*applicationpb.TxNamespace{
				{{
					NsId:      "1",
					NsVersion: 0,
					BlindWrites: []*applicationpb.Write{
						{
							Key:   []byte("k1"),
							Value: []byte("v1"),
						},
						{
							Key:   []byte("k2"),
							Value: []byte("v2"),
						},
					},
				}},
			}, []committerpb.Status{committerpb.Status_COMMITTED})
			require.Len(t, txIDs, 1)

			t.Log("Query Rows")
			timeoutContext, cancel := context.WithTimeout(ctx, time.Minute)
			t.Cleanup(cancel)

			ret, err := runtime.QueryServiceClient.GetRows(
				timeoutContext,
				&committerpb.Query{
					Namespaces: []*committerpb.QueryNamespace{
						{
							NsId: "1",
							Keys: [][]byte{
								[]byte("k1"), []byte("k2"),
							},
						},
					},
				},
			)
			require.NoError(t, err)
			t.Logf("read rows from namespace: %v", ret)

			requiredItems := []*committerpb.RowsNamespace{
				{
					NsId: "1",
					Rows: []*committerpb.Row{
						{
							Key:     []byte("k1"),
							Value:   []byte("v1"),
							Version: 0,
						},
						{
							Key:     []byte("k2"),
							Value:   []byte("v2"),
							Version: 0,
						},
					},
				},
			}
			requireQueryResults(t, requiredItems, ret.Namespaces)
		})
	}
}

// TestStartTestNode spawns an all-in-one instance of the committer using docker
// to verify that the committer container starts as expected.
func TestStartTestNode(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	containerName := fmt.Sprintf("%s_%s", test.DockerNamesPrefix, committerContainerName)
	stopAndRemoveContainersByName(ctx, t, createDockerClient(t), containerName)
	startCommitter(ctx, t, startNodeParameters{
		node:         containerName,
		credsFactory: test.NewCredentialsFactory(t),
		tlsMode:      connection.NoneTLSMode,
		cmd:          append(commonTestNodeCMD, "loadgen"),
	})

	t.Log("Try to fetch the first block")
	sidecarEndpoint, err := connection.NewEndpoint(
		net.JoinHostPort(localhost, getContainerMappedHostPort(ctx, t, containerName, sidecarPort)),
	)
	require.NoError(t, err)
	committedBlock := sidecarclient.StartSidecarClient(ctx, t, &sidecarclient.Parameters{
		ChannelID: channelName,
		Client:    test.NewInsecureClientConfig(sidecarEndpoint),
	}, 0)
	b, ok := channel.NewReader(ctx, committedBlock).Read()
	require.True(t, ok)
	t.Logf("Received block #%d with %d TXs", b.Header.Number, len(b.Data.Data))

	monitorMetric(t, getContainerMappedHostPort(ctx, t, containerName, loadGenMetricsPort), nil)
}

func startCommitter(ctx context.Context, t *testing.T, params startNodeParameters) {
	t.Helper()
	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image: testNodeImage,
			Cmd:   params.cmd,
			ExposedPorts: nat.PortSet{
				sidecarPort + "/tcp":            struct{}{},
				mockOrdererPort + "/tcp":        struct{}{},
				loadGenMetricsPort + "/tcp":     struct{}{},
				queryServicePort + "/tcp":       struct{}{},
				coordinatorServicePort + "/tcp": struct{}{},
				databasePort + "/tcp":           struct{}{},
			},
			Env: []string{
				"SC_COORDINATOR_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_COORDINATOR_VERIFIER_TLS_MODE=" + params.tlsMode,
				"SC_COORDINATOR_VALIDATOR_COMMITTER_TLS_MODE=" + params.tlsMode,
				"SC_COORDINATOR_MONITORING_TLS_MODE=" + params.tlsMode,
				"SC_QUERY_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_QUERY_MONITORING_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_MONITORING_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_COMMITTER_TLS_MODE=" + params.tlsMode,
				"SC_VC_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_VC_MONITORING_TLS_MODE=" + params.tlsMode,
				"SC_VERIFIER_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_ORDERER_TLS_MODE=" + params.tlsMode,
				"SC_VERIFIER_MONITORING_TLS_MODE=" + params.tlsMode,
				"SC_SIDECAR_ORDERER_CONNECTION_TLS_MODE=" + params.tlsMode,
				"SC_ORDERER_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_LOADGEN_SERVER_TLS_MODE=" + params.tlsMode,
				"SC_LOADGEN_MONITORING_TLS_MODE=" + params.tlsMode,
				"SC_LOADGEN_ORDERER_CLIENT_SIDECAR_CLIENT_TLS_MODE=" + params.tlsMode,
				"SC_LOADGEN_ORDERER_CLIENT_ORDERER_TLS_MODE=" + params.tlsMode,
			},
			Tty: true,
		},
		hostConfig: &container.HostConfig{
			NetworkMode: network.NetworkDefault,
			PortBindings: nat.PortMap{
				// sidecar port binding
				sidecarPort + "/tcp": []nat.PortBinding{{
					HostIP:   localhostIP,
					HostPort: "0", // auto port assign
				}},
				mockOrdererPort + "/tcp": []nat.PortBinding{{
					HostIP:   localhostIP,
					HostPort: "0", // auto port assign
				}},
				loadGenMetricsPort + "/tcp": []nat.PortBinding{{
					HostIP:   localhostIP,
					HostPort: "0", // auto port assign
				}},
				queryServicePort + "/tcp": []nat.PortBinding{{
					HostIP:   localhostIP,
					HostPort: "0", // auto port assign
				}},
				coordinatorServicePort + "/tcp": []nat.PortBinding{{
					HostIP:   localhostIP,
					HostPort: "0", // auto port assign
				}},
				databasePort + "/tcp": []nat.PortBinding{{
					HostIP:   localhostIP,
					HostPort: "0", // auto port assign
				}},
			},
			// we create the credentials for the servers with "localhost" SNI.
			Binds: assembleBinds(t, params.asNode(localhost)),
		},
		name: params.node,
	})
}

func mustGetEndpoint(ctx context.Context, t *testing.T, containerName, servicePort string) *connection.Endpoint {
	t.Helper()
	ep, err := connection.NewEndpoint(
		net.JoinHostPort(localhost, getContainerMappedHostPort(ctx, t, containerName, servicePort)),
	)
	require.NoError(t, err)
	return ep
}

// requireQueryResults checks that the QueryService returned the expected rows.
func requireQueryResults(
	t *testing.T,
	requiredItems []*committerpb.RowsNamespace,
	retNamespaces []*committerpb.RowsNamespace,
) {
	t.Helper()
	require.Len(t, retNamespaces, len(requiredItems))
	for idx := range retNamespaces {
		test.RequireProtoElementsMatch(t, requiredItems[idx].Rows, retNamespaces[idx].Rows)
	}
}

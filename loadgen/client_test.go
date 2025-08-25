/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package loadgen

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/broadcastdeliver"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// We expect at least 3 blocks for a valid test run.
const defaultExpectedTXs = defaultBlockSize * 3

// We can enforce exact limits only for the sidecar and the coordinator.
// The other adapters runs concurrent workers that might overshoot.
// So we test both requirements together and enforce that the result is greater.
var defaultLimits = []*adapters.GenerateLimit{
	nil, {}, {Blocks: 5, Transactions: 5 * defaultBlockSize},
}

func TestLoadGenForLoadGen(t *testing.T) {
	t.Parallel()

	for _, limit := range defaultLimits {
		clientConf := DefaultClientConf()
		clientConf.Limit = limit
		// Ensure the client doesn't generate load, but only receives it from the sub client.
		clientConf.LoadProfile.Workers = 0
		t.Run(limitToString(limit), func(t *testing.T) {
			t.Parallel()
			clientConf.Adapter.VerifierClient = startVerifiers(t)
			_, err := clientConf.Server.PreAllocateListener()
			require.NoError(t, err)

			subClientConf := DefaultClientConf()
			subClientConf.Adapter.LoadGenClient = test.MakeInsecureClientConfig(&clientConf.Server.Endpoint)
			subClient, err := NewLoadGenClient(subClientConf)
			require.NoError(t, err)

			t.Log("Start distributed loadgen")
			test.RunServiceAndGrpcForTest(t.Context(), t, subClient, subClientConf.Server)
			testLoadGenerator(t, clientConf)
		})
	}
}

func TestLoadGenForVCService(t *testing.T) {
	t.Parallel()
	for _, limit := range defaultLimits {
		clientConf := DefaultClientConf()
		clientConf.Limit = limit
		t.Run(limitToString(limit), func(t *testing.T) {
			t.Parallel()
			env := vc.NewValidatorAndCommitServiceTestEnv(t, 2)
			clientConf.Adapter.VCClient = test.MakeInsecureMultiClientConfig(env.Endpoints...)
			testLoadGenerator(t, clientConf)
		})
	}
}

func TestLoadGenForSigVerifier(t *testing.T) {
	t.Parallel()
	for _, limit := range defaultLimits {
		clientConf := DefaultClientConf()
		clientConf.Limit = limit
		t.Run(limitToString(limit), func(t *testing.T) {
			t.Parallel()
			clientConf.Adapter.VerifierClient = startVerifiers(t)
			// Start client
			testLoadGenerator(t, clientConf)
		})
	}
}

func startVerifiers(t *testing.T) *connection.MultiClientConfig {
	t.Helper()
	endpoints := make([]*connection.Endpoint, 2)
	for i := range endpoints {
		sConf := &verifier.Config{
			Server: connection.NewLocalHostServer(),
			ParallelExecutor: verifier.ExecutorConfig{
				BatchSizeCutoff:   50,
				BatchTimeCutoff:   10 * time.Millisecond,
				ChannelBufferSize: 50,
				Parallelism:       40,
			},
		}

		service := verifier.New(sConf)
		test.RunGrpcServerForTest(t.Context(), t, sConf.Server, service.RegisterService)
		endpoints[i] = &sConf.Server.Endpoint
	}
	return test.MakeInsecureMultiClientConfig(endpoints...)
}

func TestLoadGenForCoordinator(t *testing.T) {
	t.Parallel()
	for _, limit := range append(
		defaultLimits,
		&adapters.GenerateLimit{Blocks: 5},
		&adapters.GenerateLimit{Transactions: 5*defaultBlockSize + 2}, // +2 for the config and meta namespace TXs.
	) {
		clientConf := DefaultClientConf()
		clientConf.Limit = limit
		t.Run(limitToString(limit), func(t *testing.T) {
			t.Parallel()
			_, sigVerServer := mock.StartMockSVService(t, 1)
			_, vcServer := mock.StartMockVCService(t, 1)

			cConf := &coordinator.Config{
				Server:             connection.NewLocalHostServer(),
				Monitoring:         defaultMonitoring(),
				Verifier:           *test.ServerToMultiClientConfig(sigVerServer.Configs...),
				ValidatorCommitter: *test.ServerToMultiClientConfig(vcServer.Configs...),
				DependencyGraph: &coordinator.DependencyGraphConfig{
					NumOfLocalDepConstructors: 1,
					WaitingTxsLimit:           100_000,
				},
				ChannelBufferSizePerGoroutine: 10,
			}

			service := coordinator.NewCoordinatorService(cConf)
			test.RunServiceAndGrpcForTest(t.Context(), t, service, cConf.Server)

			// Start client
			clientConf.Adapter.CoordinatorClient = test.MakeInsecureClientConfig(&cConf.Server.Endpoint)
			testLoadGenerator(t, clientConf)
		})
	}
}

func TestLoadGenForSidecar(t *testing.T) {
	t.Parallel()

	for _, limit := range append(
		defaultLimits,
		&adapters.GenerateLimit{Blocks: 5},
		&adapters.GenerateLimit{Transactions: 5*defaultBlockSize + 1}, // +1 for the meta namespace TX.
	) {
		clientConf := DefaultClientConf()
		clientConf.Limit = limit
		t.Run(limitToString(limit), func(t *testing.T) {
			t.Parallel()
			_, coordinatorServer := mock.StartMockCoordinatorService(t)

			// When using the sidecar adapter, the load generator and the sidecar
			// should have each other's endpoints.
			// To avoid manually pre-choosing ports that might conflict with other tests,
			// we pre allocate them by starting a listener that picks a port automatically and bind to it.
			// In real evaluation scenario, the ports will be selected by the deployment infrastructure.
			sidecarServerConf := preAllocatePorts(t)
			ordererServers := make([]*connection.ServerConfig, 3)
			for i := range ordererServers {
				ordererServers[i] = preAllocatePorts(t)
			}

			// Start server under test
			chanID := "channel"
			sidecarConf := &sidecar.Config{
				Server: sidecarServerConf,
				Orderer: broadcastdeliver.Config{
					Connection: broadcastdeliver.ConnectionConfig{
						Endpoints: connection.NewOrdererEndpoints(0, "org", ordererServers...),
					},
					ChannelID:     chanID,
					ConsensusType: broadcastdeliver.Bft,
				},
				LastCommittedBlockSetInterval: 100 * time.Millisecond,
				WaitingTxsLimit:               5000,
				Committer:                     test.MakeInsecureClientConfig(&coordinatorServer.Configs[0].Endpoint),
				Monitoring:                    defaultMonitoring(),
				Ledger: sidecar.LedgerConfig{
					Path: t.TempDir(),
				},
			}
			service, err := sidecar.New(sidecarConf)
			require.NoError(t, err)
			t.Cleanup(service.Close)
			test.RunServiceAndGrpcForTest(t.Context(), t, service, sidecarConf.Server)

			// Start client
			clientConf.Adapter.SidecarClient = &adapters.SidecarClientConfig{
				ChannelID:      chanID,
				OrdererServers: ordererServers,
				Client:         test.MakeInsecureClientConfig(&sidecarServerConf.Endpoint),
			}
			testLoadGenerator(t, clientConf)
		})
	}
}

func TestLoadGenForOrderer(t *testing.T) {
	t.Parallel()
	for _, limit := range defaultLimits {
		clientConf := DefaultClientConf()
		clientConf.Limit = limit
		t.Run(limitToString(limit), func(t *testing.T) {
			t.Parallel()
			// Start dependencies
			orderer, ordererServer := mock.StartMockOrderingServices(
				t, &mock.OrdererConfig{NumService: 3, BlockSize: 100},
			)
			_, coordinatorServer := mock.StartMockCoordinatorService(t)

			endpoints := connection.NewOrdererEndpoints(0, "msp", ordererServer.Configs...)
			sidecarConf := &sidecar.Config{
				Server: connection.NewLocalHostServer(),
				Orderer: broadcastdeliver.Config{
					Connection: broadcastdeliver.ConnectionConfig{
						Endpoints: endpoints,
					},
					ChannelID:     "mychannel",
					ConsensusType: broadcastdeliver.Bft,
				},
				LastCommittedBlockSetInterval: 100 * time.Millisecond,
				WaitingTxsLimit:               5000,
				Committer:                     test.MakeInsecureClientConfig(&coordinatorServer.Configs[0].Endpoint),
				Monitoring:                    defaultMonitoring(),
				Ledger: sidecar.LedgerConfig{
					Path: t.TempDir(),
				},
			}

			// Start sidecar.
			service, err := sidecar.New(sidecarConf)
			require.NoError(t, err)
			t.Cleanup(service.Close)
			test.RunServiceAndGrpcForTest(t.Context(), t, service, sidecarConf.Server)

			// Submit default config block.
			require.NotNil(t, clientConf.LoadProfile)
			clientConf.LoadProfile.Transaction.Policy.OrdererEndpoints = endpoints
			configBlock, err := workload.CreateConfigBlock(clientConf.LoadProfile.Transaction.Policy)
			require.NoError(t, err)
			orderer.SubmitBlock(t.Context(), configBlock)

			// Start client
			clientConf.Adapter.OrdererClient = &adapters.OrdererClientConfig{
				SidecarClient:        test.MakeInsecureClientConfig(&sidecarConf.Server.Endpoint),
				Orderer:              sidecarConf.Orderer,
				BroadcastParallelism: 5,
			}
			testLoadGenerator(t, clientConf)
		})
	}
}

func TestLoadGenForOnlyOrderer(t *testing.T) {
	t.Parallel()
	for _, limit := range defaultLimits {
		clientConf := DefaultClientConf()
		clientConf.Limit = limit
		t.Run(limitToString(limit), func(t *testing.T) {
			t.Parallel()
			// Start dependencies
			orderer, ordererServer := mock.StartMockOrderingServices(
				t, &mock.OrdererConfig{
					NumService: 3,
					BlockSize:  int(clientConf.LoadProfile.Block.Size), //nolint:gosec // uint64 -> int.
				},
			)

			endpoints := connection.NewOrdererEndpoints(0, "msp", ordererServer.Configs...)

			// Submit default config block.
			// This is ignored when sidecar isn't used.
			// We validate the test doesn't break when config block is delivered.
			require.NotNil(t, clientConf.LoadProfile)
			clientConf.LoadProfile.Transaction.Policy.OrdererEndpoints = endpoints
			configBlock, err := workload.CreateConfigBlock(clientConf.LoadProfile.Transaction.Policy)
			require.NoError(t, err)
			orderer.SubmitBlock(t.Context(), configBlock)

			// Start client
			clientConf.Adapter.OrdererClient = &adapters.OrdererClientConfig{
				Orderer: broadcastdeliver.Config{
					Connection: broadcastdeliver.ConnectionConfig{
						Endpoints: endpoints,
					},
					ChannelID:     "mychannel",
					ConsensusType: broadcastdeliver.Bft,
				},
				BroadcastParallelism: 5,
			}
			testLoadGenerator(t, clientConf)
		})
	}
}

func preAllocatePorts(t *testing.T) *connection.ServerConfig {
	t.Helper()
	server := connection.NewLocalHostServer()
	listener, err := server.PreAllocateListener()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = listener.Close()
	})
	return server
}

func testLoadGenerator(t *testing.T, c *ClientConfig) {
	t.Helper()
	client, err := NewLoadGenClient(c)
	require.NoError(t, err)

	ready := test.RunServiceAndGrpcForTest(t.Context(), t, client, client.conf.Server)
	eventuallyMetrics(t, client.resources.Metrics, func(m metrics.MetricState) bool {
		return m.TransactionsSent > 0 &&
			m.TransactionsReceived > 0 &&
			m.TransactionsCommitted > 0 &&
			m.TransactionsAborted == 0
	})

	if !c.Limit.HasLimit() {
		// If we have a limit, the Prometheus server might stop before we can fetch the metrics.
		test.CheckMetrics(t, client.resources.Metrics.URL(),
			"loadgen_block_sent_total",
			"loadgen_transaction_sent_total",
			"loadgen_transaction_received_total",
			"loadgen_valid_transaction_latency_seconds",
			"loadgen_invalid_transaction_latency_seconds",
		)
	}

	eventuallyMetrics(t, client.resources.Metrics, func(m metrics.MetricState) bool {
		return m.TransactionsSent > defaultExpectedTXs &&
			m.TransactionsReceived > defaultExpectedTXs &&
			m.TransactionsCommitted > defaultExpectedTXs &&
			m.TransactionsAborted == 0
	})

	if !c.Limit.HasLimit() {
		return
	}

	// If there is a limit, we expect the load generator to terminate.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	if !assert.True(t, ready.WaitForReady(ctx)) {
		t.Fatalf("Did not finish. State: %+v", client.resources.Metrics.GetState())
	}

	m := client.resources.Metrics.GetState()

	if c.Limit.Blocks == 0 || c.Limit.Transactions == 0 {
		if c.Limit.Blocks > 0 {
			require.Equal(t, c.Limit.Blocks, m.BlocksSent)
			require.Equal(t, c.Limit.Blocks, m.BlocksReceived)
		}
		if c.Limit.Transactions > 0 {
			require.Equal(t, c.Limit.Transactions, m.TransactionsSent)
			require.Equal(t, c.Limit.Transactions, m.TransactionsReceived)
		}
	} else {
		// We cant enforce exact limits for both requirements.
		if c.Adapter.OrdererClient == nil {
			// The orderer does not track sent blocks.
			require.GreaterOrEqual(t, m.BlocksSent, c.Limit.Blocks)
		}
		require.GreaterOrEqual(t, m.BlocksReceived, c.Limit.Blocks)
		require.GreaterOrEqual(t, m.TransactionsSent, c.Limit.Transactions)
		require.GreaterOrEqual(t, m.TransactionsReceived, c.Limit.Transactions)
	}
}

func limitToString(m *adapters.GenerateLimit) string {
	if m == nil {
		return "<nil>"
	}
	var out []string
	if m.Blocks > 0 {
		out = append(out, fmt.Sprintf("block=%d", m.Blocks))
	}
	if m.Transactions > 0 {
		out = append(out, fmt.Sprintf("tx=%d", m.Transactions))
	}
	if len(out) == 0 {
		return "<empty>"
	}
	return strings.Join(out, ",")
}

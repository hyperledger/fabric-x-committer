/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package loadgen

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gavv/httpexpect/v2"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

const (
	// We expect at least 3 blocks for a valid test run.
	defaultExpectedTXs = defaultBlockSize * 3
)

type loadGenTestCase struct {
	serverTLSMode    string
	limit            *adapters.GenerateLimit
	useMSPIdentities bool
}

// We can enforce exact limits only for the sidecar and the coordinator.
// The other adapters runs concurrent workers that might overshoot.
// So we test both requirements together and enforce that the result is greater.
var (
	testCases = []loadGenTestCase{
		{serverTLSMode: connection.MutualTLSMode},
		{serverTLSMode: connection.OneSideTLSMode},
		{serverTLSMode: connection.NoneTLSMode},
		{serverTLSMode: connection.NoneTLSMode, limit: &adapters.GenerateLimit{}},
		{serverTLSMode: connection.NoneTLSMode, limit: &adapters.GenerateLimit{
			Blocks: 5, Transactions: 5 * defaultBlockSize,
		}},
	}
	ordererTestCases = append(testCases, loadGenTestCase{serverTLSMode: connection.NoneTLSMode, useMSPIdentities: true})
)

func TestLoadGenForLoadGen(t *testing.T) {
	t.Parallel()
	for _, tc := range testCases {
		t.Run(loadGenTestCaseName(tc), func(t *testing.T) {
			t.Parallel()
			e := newLoadGenClientTestEnv(t, tc)
			// Ensure the client doesn't generate load, but only receives it from the sub client.
			e.clientConf.LoadProfile.Workers = 0

			e.clientConf.Adapter.VerifierClient = startVerifiers(t, e.serverTLSConfig, e.clientTLSConfig)
			serve.PreAllocateListener(t, &e.serverConf.GRPC)

			subEnv := newLoadGenClientTestEnv(t, tc)
			// We ensure the sub client uses the same crypto artifacts as
			// the main load generator and the entire system.
			subEnv.clientConf.LoadProfile.Policy.ArtifactsPath = e.clientConf.LoadProfile.Policy.ArtifactsPath

			subEnv.clientConf.Adapter.LoadGenClient = test.NewTLSClientConfig(
				e.clientTLSConfig, &e.serverConf.GRPC.Endpoint,
			)
			subClient, err := NewLoadGenClient(subEnv.clientConf)
			require.NoError(t, err)

			t.Log("Start distributed loadgen")
			subCtx, subCancel := context.WithCancel(t.Context())
			t.Cleanup(subCancel)
			subDone := test.RunServiceAndServeForTest(subCtx, t, subClient, subEnv.serverConf)
			// Stop the sub-client before test cleanup tears down the main server.
			// Without this, the main server's gRPC Stop runs first (LIFO cleanup order),
			// causing the sub-client to hit "connection reset by peer" on in-flight RPCs.
			t.Cleanup(func() {
				subCancel()
				subDone.WaitForReady(t.Context())
			})
			e.testLoadGenerator(t)
		})
	}
}

func TestLoadGenForVCService(t *testing.T) {
	t.Parallel()
	for _, tc := range testCases {
		t.Run(loadGenTestCaseName(tc), func(t *testing.T) {
			t.Parallel()
			e := newLoadGenClientTestEnv(t, tc)
			env := vc.NewValidatorAndCommitServiceTestEnv(t, &vc.TestEnvOpts{
				NumServices: 2, ServerCreds: e.serverTLSConfig,
			})
			e.clientConf.Adapter.VCClient = test.NewTLSMultiClientConfig(e.clientTLSConfig, env.Endpoints...)
			e.testLoadGenerator(t)
		})
	}
}

func TestLoadGenForVerifier(t *testing.T) {
	t.Parallel()
	for _, tc := range testCases {
		t.Run(loadGenTestCaseName(tc), func(t *testing.T) {
			t.Parallel()
			e := newLoadGenClientTestEnv(t, tc)
			e.clientConf.Adapter.VerifierClient = startVerifiers(t, e.serverTLSConfig, e.clientTLSConfig)
			e.testLoadGenerator(t)
		})
	}
}

func startVerifiers(t *testing.T, serverTLS, clientTLS connection.TLSConfig) *connection.MultiClientConfig {
	t.Helper()
	endpoints := make([]*connection.Endpoint, 2)
	for i := range endpoints {
		service := verifier.New(&verifier.Config{
			ParallelExecutor: verifier.ExecutorConfig{
				BatchSizeCutoff:   50,
				BatchTimeCutoff:   10 * time.Millisecond,
				ChannelBufferSize: 50,
				Parallelism:       40,
			},
		})

		serverConfig := test.NewLocalHostServiceConfig(serverTLS)
		test.ServeForTest(t.Context(), t, serverConfig, service)
		endpoints[i] = &serverConfig.GRPC.Endpoint
	}
	return test.NewTLSMultiClientConfig(clientTLS, endpoints...)
}

func TestLoadGenForCoordinator(t *testing.T) {
	t.Parallel()
	for _, tc := range append(
		testCases,
		loadGenTestCase{limit: &adapters.GenerateLimit{Blocks: 5}},
		loadGenTestCase{limit: &adapters.GenerateLimit{
			Transactions: 5*defaultBlockSize + 2, // +2 for the config and meta namespace TXs.
		}},
	) {
		t.Run(loadGenTestCaseName(tc), func(t *testing.T) {
			t.Parallel()
			e := newLoadGenClientTestEnv(t, tc)

			mockSettings := test.StartServerParameters{
				NumService: 1,
				TLSConfig:  e.serverTLSConfig,
			}
			_, sigVerServer := mock.StartMockVerifierService(t, mockSettings)
			_, vcServer := mock.StartMockVCService(t, mockSettings)

			cConf := &coordinator.Config{
				Verifier:           *test.ServerToMultiClientConfig(e.clientTLSConfig, sigVerServer.Configs...),
				ValidatorCommitter: *test.ServerToMultiClientConfig(e.clientTLSConfig, vcServer.Configs...),
				DependencyGraph: &coordinator.DependencyGraphConfig{
					NumOfLocalDepConstructors: 1,
					WaitingTxsLimit:           100_000,
				},
				ChannelBufferSizePerGoroutine: 10,
			}

			service := coordinator.NewCoordinatorService(cConf)
			serverConfig := test.NewLocalHostServiceConfig(e.serverTLSConfig)
			test.RunServiceAndServeForTest(t.Context(), t, service, serverConfig)

			// Start client
			coordEp := &serverConfig.GRPC.Endpoint
			e.clientConf.Adapter.CoordinatorClient = test.NewTLSClientConfig(e.clientTLSConfig, coordEp)
			e.testLoadGenerator(t)
		})
	}
}

func TestLoadGenForSidecar(t *testing.T) {
	t.Parallel()
	for _, tc := range append(
		ordererTestCases,
		loadGenTestCase{limit: &adapters.GenerateLimit{Blocks: 5}},
		loadGenTestCase{limit: &adapters.GenerateLimit{
			Transactions: 5*defaultBlockSize + 1, // +1 for the meta namespace TX.
		}},
	) {
		t.Run(loadGenTestCaseName(tc), func(t *testing.T) {
			t.Parallel()
			e, lgEnv := clientConfigWithOrdererForTestCase(t, tc)
			_, coordinatorServer := mock.StartMockCoordinatorService(t, test.StartServerParameters{
				TLSConfig: e.ServerTLSConfig,
			})
			// When using the sidecar adapter, the load generator and the sidecar
			// should have each other's endpoints.
			// To avoid manually pre-choosing ports that might conflict with other tests,
			// we pre allocate them by starting a listener that picks a port automatically and bind to it.
			// In real evaluation scenario, the ports will be selected by the deployment infrastructure.
			sidecarServerConf := test.NewPreAllocatedLocalHostServerConfig(t, e.ServerTLSConfig)

			// The sidecar adapter runs its own orderer.
			e.StopServers()

			// Start server under test
			sidecarConf := &sidecar.Config{
				LastCommittedBlockSetInterval: 100 * time.Millisecond,
				WaitingTxsLimit:               5000,
				ChannelBufferSize:             sidecar.DefaultBufferSize,
				Committer: test.NewTLSClientConfig(
					e.ClientTLSConfig,
					&coordinatorServer.Configs[0].GRPC.Endpoint,
				),
				Notification: sidecar.NotificationServiceConfig{
					MaxTimeout:         sidecar.DefaultNotificationMaxTimeout,
					MaxActiveTxIDs:     sidecar.DefaultMaxActiveTxIDs,
					MaxTxIDsPerRequest: sidecar.DefaultMaxTxIDsPerRequest,
				},
				Ledger: sidecar.LedgerConfig{
					Path: t.TempDir(),
				},
				Orderer: e.OrdererConnConfig,
			}
			service, err := sidecar.New(sidecarConf)
			require.NoError(t, err)
			t.Cleanup(service.Close)
			test.RunServiceAndServeForTest(t.Context(), t, service, sidecarServerConf)
			// Start client
			lgEnv.clientConf.Adapter.SidecarClient = &adapters.SidecarClientConfig{
				OrdererServers: test.GrpcServiceToConnectionServerConfigs(e.AllServerConfig...),
				SidecarClient:  test.NewTLSClientConfig(e.ClientTLSConfig, &sidecarServerConf.GRPC.Endpoint),
			}
			lgEnv.testLoadGenerator(t)
		})
	}
}

func TestLoadGenForOrderer(t *testing.T) {
	t.Parallel()
	for _, tc := range ordererTestCases {
		t.Run(loadGenTestCaseName(tc), func(t *testing.T) {
			t.Parallel()
			e, lgEnv := clientConfigWithOrdererForTestCase(t, tc)

			// Start dependencies
			_, coordinatorServer := mock.StartMockCoordinatorService(t, test.StartServerParameters{
				TLSConfig: e.ServerTLSConfig,
			})

			sidecarConf := &sidecar.Config{
				LastCommittedBlockSetInterval: 100 * time.Millisecond,
				WaitingTxsLimit:               5000,
				ChannelBufferSize:             sidecar.DefaultBufferSize,
				Committer: test.NewTLSClientConfig(
					e.ClientTLSConfig,
					&coordinatorServer.Configs[0].GRPC.Endpoint,
				),
				Notification: sidecar.NotificationServiceConfig{
					MaxTimeout:         sidecar.DefaultNotificationMaxTimeout,
					MaxActiveTxIDs:     sidecar.DefaultMaxActiveTxIDs,
					MaxTxIDsPerRequest: sidecar.DefaultMaxTxIDsPerRequest,
				},
				Ledger: sidecar.LedgerConfig{
					Path: t.TempDir(),
				},
				Orderer: e.OrdererConnConfig,
			}
			serverConfig := test.NewLocalHostServiceConfig(e.ServerTLSConfig)

			// Start sidecar.
			service, err := sidecar.New(sidecarConf)
			require.NoError(t, err)
			t.Cleanup(service.Close)
			test.RunServiceAndServeForTest(t.Context(), t, service, serverConfig)

			// Start client
			lgEnv.clientConf.Adapter.OrdererClient = &adapters.OrdererClientConfig{
				SidecarClient:        test.NewTLSClientConfig(e.ClientTLSConfig, &serverConfig.GRPC.Endpoint),
				Orderer:              e.OrdererConnConfig,
				BroadcastParallelism: 5,
			}
			lgEnv.testLoadGenerator(t)
		})
	}
}

func TestLoadGenForOnlyOrderer(t *testing.T) {
	t.Parallel()
	for _, tc := range ordererTestCases {
		t.Run(loadGenTestCaseName(tc), func(t *testing.T) {
			t.Parallel()
			e, lgEnv := clientConfigWithOrdererForTestCase(t, tc)

			// Start client
			lgEnv.clientConf.Adapter.OrdererClient = &adapters.OrdererClientConfig{
				Orderer:              e.OrdererConnConfig,
				BroadcastParallelism: 5,
			}
			lgEnv.testLoadGenerator(t)
		})
	}
}

func (e *loadGenClientTestEnv) testLoadGenerator(t *testing.T) {
	t.Helper()
	client, err := NewLoadGenClient(e.clientConf)
	require.NoError(t, err)

	ready := test.RunServiceAndServeForTest(t.Context(), t, client, e.serverConf)
	metricsURL, err := monitoring.MakeMetricsURL(e.serverConf.Monitoring.Endpoint.Address(), &e.clientTLSConfig)
	require.NoError(t, err)
	eventuallyMetrics(t, client.resources.Metrics, func(m metrics.MetricState) bool {
		return m.TransactionsSent > 0 &&
			m.TransactionsReceived > 0 &&
			m.TransactionsCommitted > 0 &&
			m.TransactionsAborted == 0
	})

	limitConf := e.clientConf.Limit

	if !limitConf.HasLimit() {
		// If we have a limit, the Prometheus server might stop before we can fetch the metrics.
		test.CheckMetrics(t, metricsURL, test.MustGetTLSConfig(t, &e.clientTLSConfig),
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
			m.TransactionsCommitted > defaultExpectedTXs
	})

	// Check after throughput is reached, not as part of the polling condition.
	// Including this in eventuallyMetrics would cause the condition to never
	// satisfy if even a single transaction is aborted during the entire run.
	require.Zero(t, client.resources.Metrics.GetState().TransactionsAborted, "unexpected aborted transactions")

	if !limitConf.HasLimit() {
		return
	}

	// If there is a limit, we expect the load generator to terminate.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	if !assert.True(t, ready.WaitForReady(ctx)) {
		t.Fatalf("Did not finish. State: %+v", client.resources.Metrics.GetState())
	}

	m := client.resources.Metrics.GetState()

	if limitConf.Blocks == 0 || limitConf.Transactions == 0 {
		if limitConf.Blocks > 0 {
			require.Equal(t, limitConf.Blocks, m.BlocksSent)
			require.Equal(t, limitConf.Blocks, m.BlocksReceived)
		}
		if limitConf.Transactions > 0 {
			require.Equal(t, limitConf.Transactions, m.TransactionsSent)
			require.Equal(t, limitConf.Transactions, m.TransactionsReceived)
		}
	} else {
		// We cant enforce exact limits for both requirements.
		if e.clientConf.Adapter.OrdererClient == nil {
			// The orderer does not track sent blocks.
			require.GreaterOrEqual(t, m.BlocksSent, limitConf.Blocks)
		}
		require.GreaterOrEqual(t, m.BlocksReceived, limitConf.Blocks)
		require.GreaterOrEqual(t, m.TransactionsSent, limitConf.Transactions)
		require.GreaterOrEqual(t, m.TransactionsReceived, limitConf.Transactions)
	}
}

func TestLoadGenRateLimiterServer(t *testing.T) {
	t.Parallel()
	lgEnv := newLoadGenClientTestEnv(t, loadGenTestCase{})
	lgEnv.clientConf.Adapter.VerifierClient = startVerifiers(t, test.InsecureTLSConfig, test.InsecureTLSConfig)
	curRate := uint64(10)
	lgEnv.clientConf.Stream.RateLimit = curRate
	// We use small wait to ensure the rate limiter serves in low granularity.
	lgEnv.clientConf.LoadProfile.Block.PreferredRate = 10 * time.Millisecond
	lgEnv.clientConf.LoadProfile.Block.MaxSize = 10
	lgEnv.clientConf.LoadProfile.Block.MinSize = 1

	client, err := NewLoadGenClient(lgEnv.clientConf)
	require.NoError(t, err)

	test.RunServiceAndServeForTest(t.Context(), t, client, lgEnv.serverConf)
	rlEndpoint := &lgEnv.serverConf.HTTP.Endpoint
	require.NotZero(t, rlEndpoint.Port)
	t.Logf("limiter endpoint: %v", rlEndpoint)

	e := httpexpect.Default(t, fmt.Sprintf("http://%s", rlEndpoint))
	requireGetRate := func(rate uint64) {
		e.GET("/getRateLimit").
			Expect().
			Status(http.StatusOK).
			JSON().Object().ContainsKey("rate").HasValue("rate", strconv.FormatUint(rate, 10))
	}
	setRate := func(rate uint64) {
		e.POST("/setRateLimit").WithJSON(servicepb.RateLimit{
			Rate: rate,
		}).Expect().Status(http.StatusOK)
	}

	requireGetRate(curRate)
	requireEventuallyMeasuredRate(t, client.resources.Metrics, curRate)

	curRate = uint64(50)
	setRate(curRate)
	requireGetRate(curRate)
	requireEventuallyMeasuredRate(t, client.resources.Metrics, curRate)
}

func requireEventuallyMeasuredRate(t *testing.T, m *metrics.PerfMetrics, expectedRate uint64) {
	t.Helper()
	prevTime := time.Now()
	prevState := m.GetState()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		newTime := time.Now()
		newState := m.GetState()
		elapsed := newTime.Sub(prevTime).Seconds()
		rate := float64(newState.TransactionsSent-prevState.TransactionsSent) / elapsed
		prevState = newState
		prevTime = newTime
		require.InDelta(ct, expectedRate, rate, 0.2*float64(expectedRate))
	}, 20*time.Second, 2*time.Second)
}

type loadGenClientTestEnv struct {
	clientConf      *ClientConfig
	serverConf      *serve.Config
	serverTLSConfig connection.TLSConfig
	clientTLSConfig connection.TLSConfig
}

func newLoadGenClientTestEnv(t *testing.T, tc loadGenTestCase) *loadGenClientTestEnv {
	t.Helper()
	clientConf := DefaultClientConf(t)
	clientConf.Limit = tc.limit

	serverTLSConfig, clientTLSConfig := test.CreateServerAndClientTLSConfig(t, tc.serverTLSMode)
	serverConfig := test.NewLocalHostServiceConfig(serverTLSConfig)
	serverConfig.HTTP = *test.NewLocalHostServer(serverTLSConfig)
	return &loadGenClientTestEnv{
		clientConf:      clientConf,
		serverConf:      serverConfig,
		serverTLSConfig: serverTLSConfig,
		clientTLSConfig: clientTLSConfig,
	}
}

func clientConfigWithOrdererForTestCase(t *testing.T, tc loadGenTestCase) (
	e *mock.OrdererTestEnv, lgEnv *loadGenClientTestEnv,
) {
	t.Helper()
	lgEnv = newLoadGenClientTestEnv(t, tc)
	policy := &lgEnv.clientConf.LoadProfile.Policy
	e = mock.NewOrdererTestEnv(t, &mock.OrdererTestParameters{
		NumIDs:                3,
		PeerOrganizationCount: policy.PeerOrganizationCount,
		ChanID:                policy.ChannelID,
		OrdererConfig: &mock.OrdererConfig{
			//nolint:gosec // uint64 -> int.
			BlockSize:        int(lgEnv.clientConf.LoadProfile.Block.MaxSize),
			SendGenesisBlock: true,
		},
		ArtifactsPath:   policy.ArtifactsPath,
		ServerTLSConfig: lgEnv.serverTLSConfig,
		ClientTLSConfig: lgEnv.clientTLSConfig,
	})

	policy.OrdererEndpoints = e.AllEndpoints
	if tc.useMSPIdentities {
		peerIdentities := getMSPIdentities(t, e.ArtifactsPath)
		for _, nsID := range []string{workload.DefaultGeneratedNamespaceID, committerpb.MetaNamespaceID} {
			policy.NamespacePolicies[nsID] = &workload.Policy{
				Scheme:        workload.PolicySchemeMSP,
				MSPIdentities: peerIdentities,
			}
		}
	}
	return e, lgEnv
}

func loadGenTestCaseName(tc loadGenTestCase) string {
	limit := "<nil>"
	if tc.limit != nil {
		limit = "<empty>"
		var out []string
		if tc.limit.Blocks > 0 {
			out = append(out, fmt.Sprintf("block=%d", tc.limit.Blocks))
		}
		if tc.limit.Transactions > 0 {
			out = append(out, fmt.Sprintf("tx=%d", tc.limit.Transactions))
		}
		if len(out) > 0 {
			limit = strings.Join(out, ",")
		}
	}
	return fmt.Sprintf("tls:%s limit:%s msp-ids:%v", tc.serverTLSMode, limit, tc.useMSPIdentities)
}

func getMSPIdentities(t *testing.T, artifactsPath string) []ordererdial.IdentityConfig {
	t.Helper()

	// Get MSP directories from the artifacts path.
	mspDirs := testcrypto.GetPeersMspDirs(artifactsPath)
	require.NotEmpty(t, mspDirs, "MSP directories should not be empty")

	// Convert to IdentityConfig format.
	mspIdentities := make([]ordererdial.IdentityConfig, len(mspDirs))
	for i, mspDir := range mspDirs {
		mspIdentities[i] = ordererdial.IdentityConfig{
			MspID:  mspDir.MspName,
			MSPDir: mspDir.MspDir,
			BCCSP:  mspDir.CspConf,
		}
	}
	return mspIdentities
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"crypto/rand"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/ledger/blkstorage"
	"github.com/hyperledger/fabric-x-common/internaltools/configtxgen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/api/protonotify"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/sidecar/sidecarclient"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type sidecarTestEnv struct {
	config            Config
	coordinator       *mock.Coordinator
	coordinatorServer *test.GrpcServers
	ordererEnv        *mock.OrdererTestEnv

	sidecar        *Service
	committedBlock chan *common.Block
	configBlock    *common.Block
	notifyStream   protonotify.Notifier_OpenNotificationStreamClient
}

type sidecarTestConfig struct {
	NumService      int
	NumFakeService  int
	NumHolders      int
	WithConfigBlock bool
}

const (
	blockSize              = 100
	expectedProcessingTime = 30 * time.Second
)

func (c *sidecarTestConfig) String() string {
	var b []string
	if c.NumService > 1 {
		b = append(b, fmt.Sprintf("services:%d", c.NumService))
	}
	if c.NumFakeService > 0 {
		b = append(b, fmt.Sprintf("fake:%d", c.NumFakeService))
	}
	if c.WithConfigBlock {
		b = append(b, "config-block")
	}
	if len(b) > 0 {
		return strings.Join(b, ",")
	}
	return "default"
}

func TestSidecarSecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(t,
		func(t *testing.T, tlsCfg connection.TLSConfig) test.RPCAttempt {
			t.Helper()
			env := newSidecarTestEnvWithTLS(
				t,
				sidecarTestConfig{NumService: 1},
				tlsCfg,
			)
			env.startSidecarService(t.Context(), t)
			return func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error {
				t.Helper()
				env.startSidecarClient(ctx, t, 0, cfg)
				committerBlock := channel.NewReader(ctx, env.committedBlock)
				if _, ok := committerBlock.Read(); !ok {
					return errors.New("failed to read committed block")
				}
				return nil
			}
		},
	)
}

func newSidecarTestEnvWithTLS(
	t *testing.T,
	conf sidecarTestConfig,
	serverCreds connection.TLSConfig,
) *sidecarTestEnv {
	t.Helper()
	coordinator, coordinatorServer := mock.StartMockCoordinatorService(t)
	ordererEnv := mock.NewOrdererTestEnv(t, &mock.OrdererTestConfig{
		ChanID: "ch1",
		Config: &mock.OrdererConfig{
			NumService: conf.NumService,
			BlockSize:  blockSize,
			// We want each block to contain exactly <blockSize> transactions.
			// Therefore, we set a higher block timeout so that we have enough time to send all the
			// transactions to the orderer and create a block.
			BlockTimeout:    5 * time.Minute,
			SendConfigBlock: false,
		},
		NumFake:    conf.NumFakeService,
		NumHolders: conf.NumHolders,
	})

	ordererEndpoints := ordererEnv.AllEndpoints()
	configBlock := ordererEnv.SubmitConfigBlock(t, &workload.ConfigBlock{
		OrdererEndpoints: ordererEndpoints,
		ChannelID:        ordererEnv.TestConfig.ChanID,
	})

	var genesisBlockFilePath string
	initOrdererEndpoints := ordererEndpoints
	if conf.WithConfigBlock {
		genesisBlockFilePath = filepath.Join(t.TempDir(), "config.block")
		require.NoError(t, configtxgen.WriteOutputBlock(configBlock, genesisBlockFilePath))
		initOrdererEndpoints = nil
	}
	sidecarConf := &Config{
		Server: connection.NewLocalHostServerWithTLS(serverCreds),
		Orderer: ordererconn.Config{
			ChannelID: ordererEnv.TestConfig.ChanID,
			Connection: ordererconn.ConnectionConfig{
				Endpoints: initOrdererEndpoints,
			},
		},
		Committer: test.NewInsecureClientConfig(&coordinatorServer.Configs[0].Endpoint),
		Ledger: LedgerConfig{
			Path: t.TempDir(),
		},
		LastCommittedBlockSetInterval: 100 * time.Millisecond,
		WaitingTxsLimit:               1000,
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServerWithTLS(test.InsecureTLSConfig),
		},
		Bootstrap: Bootstrap{
			GenesisBlockFilePath: genesisBlockFilePath,
		},
	}
	sidecar, err := New(sidecarConf)
	require.NoError(t, err)
	t.Cleanup(sidecar.Close)

	return &sidecarTestEnv{
		sidecar:           sidecar,
		coordinator:       coordinator,
		coordinatorServer: coordinatorServer,
		ordererEnv:        ordererEnv,
		config:            *sidecarConf,
		configBlock:       configBlock,
	}
}

func (env *sidecarTestEnv) startSidecarServiceAndClientAndNotificationStream(
	ctx context.Context,
	t *testing.T,
	startBlkNum int64,
	sidecarClientCreds connection.TLSConfig,
) {
	t.Helper()
	env.startSidecarService(ctx, t)
	env.startSidecarClient(ctx, t, startBlkNum, sidecarClientCreds)
	env.startNotificationStream(ctx, t, sidecarClientCreds)
}

func (env *sidecarTestEnv) startSidecarService(ctx context.Context, t *testing.T) {
	t.Helper()
	test.RunServiceAndGrpcForTest(ctx, t, env.sidecar, env.config.Server)
}

func (env *sidecarTestEnv) startSidecarClient(
	ctx context.Context,
	t *testing.T,
	startBlkNum int64,
	sidecarClientCreds connection.TLSConfig,
) {
	t.Helper()
	env.committedBlock = sidecarclient.StartSidecarClient(ctx, t, &sidecarclient.Parameters{
		ChannelID: env.config.Orderer.ChannelID,
		Client:    test.NewTLSClientConfig(sidecarClientCreds, &env.config.Server.Endpoint),
	}, startBlkNum)
}

func (env *sidecarTestEnv) startNotificationStream(
	ctx context.Context,
	t *testing.T,
	sidecarClientCreds connection.TLSConfig,
) {
	t.Helper()
	conn := test.NewSecuredConnection(t, &env.config.Server.Endpoint, sidecarClientCreds)
	var err error
	env.notifyStream, err = protonotify.NewNotifierClient(conn).OpenNotificationStream(ctx)
	require.NoError(t, err)
}

func TestSidecar(t *testing.T) {
	t.Parallel()
	for _, mode := range test.ServerModes {
		t.Run(fmt.Sprintf("tls-mode:%s", mode), func(t *testing.T) {
			t.Parallel()
			serverTLSConfig, clientTLSConfig := test.CreateServerAndClientTLSConfig(t, mode)
			for _, conf := range []sidecarTestConfig{
				{WithConfigBlock: false},
				{WithConfigBlock: true},
				{
					WithConfigBlock: true,
					NumFakeService:  3,
				},
			} {
				t.Run(conf.String(), func(t *testing.T) {
					t.Parallel()
					env := newSidecarTestEnvWithTLS(t, conf, serverTLSConfig)
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
					t.Cleanup(cancel)
					env.startSidecarServiceAndClientAndNotificationStream(ctx, t, 0, clientTLSConfig)
					env.requireBlock(ctx, t, 0)
					env.sendTransactionsAndEnsureCommitted(ctx, t, 1)
				})
			}
		})
	}
}

func TestSidecarConfigUpdate(t *testing.T) {
	t.Parallel()
	env := newSidecarTestEnvWithTLS(t, sidecarTestConfig{NumService: 3, NumHolders: 3}, test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startSidecarServiceAndClientAndNotificationStream(ctx, t, 0, test.InsecureTLSConfig)
	env.requireBlock(ctx, t, 0)

	t.Log("Sanity check")
	expectedBlock := uint64(1)
	env.sendTransactionsAndEnsureCommitted(ctx, t, expectedBlock)
	expectedBlock++

	submitConfigBlock := func(endpoints []*commontypes.OrdererEndpoint) {
		env.ordererEnv.SubmitConfigBlock(t, &workload.ConfigBlock{
			OrdererEndpoints: endpoints,
		})
	}

	t.Log("Update the sidecar to use a second orderer group")
	env.ordererEnv.Holder.HoldFromBlock.Store(expectedBlock + 2)
	submitConfigBlock(env.ordererEnv.AllHolderEndpoints())
	env.requireBlock(ctx, t, expectedBlock)
	expectedBlock++

	t.Log("Sanity check")
	env.sendTransactionsAndEnsureCommitted(ctx, t, expectedBlock)
	expectedBlock++

	t.Log("Submit new config block, and ensure it was not received")
	// We submit the config that returns to the non-holding orderer.
	// But it should not be processed as the sidecar should have switched to the holding
	// orderer.
	submitConfigBlock(env.ordererEnv.AllRealOrdererEndpoints())
	select {
	case <-ctx.Done():
		t.Fatal("context deadline exceeded")
	case <-env.committedBlock:
		t.Fatal("the sidecar cannot receive blocks since its orderer holds them")
	case <-time.After(expectedProcessingTime):
		t.Log("Fantastic")
	}

	t.Log("We expect the block to be held")
	nextBlock, err := env.coordinator.GetNextBlockNumberToCommit(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, nextBlock)
	require.Equal(t, expectedBlock, nextBlock.Number)

	t.Log("We advance the holder by one to allow the config block to pass through, but not other blocks")
	env.ordererEnv.Holder.HoldFromBlock.Add(1)
	env.requireBlock(ctx, t, expectedBlock)
	expectedBlock++

	t.Log("The sidecar should use the non-holding orderer, so the holding should not affect the processing")
	env.sendTransactionsAndEnsureCommitted(ctx, t, expectedBlock)
}

func TestSidecarConfigRecovery(t *testing.T) {
	t.Parallel()
	env := newSidecarTestEnvWithTLS(t, sidecarTestConfig{NumService: 3}, test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startSidecarServiceAndClientAndNotificationStream(ctx, t, 0, test.InsecureTLSConfig)
	env.requireBlock(ctx, t, 0)

	t.Log("Stop the sidecar service and ledger service")
	cancel()
	require.Eventually(t, func() bool {
		return test.CheckServerStopped(t, env.config.Server.Endpoint.Address())
	}, 4*time.Second, 500*time.Millisecond)
	require.Eventually(t, func() bool {
		return !env.coordinator.IsStreamActive()
	}, 2*time.Second, 250*time.Millisecond)

	// Important: Close the sidecar explicitly to release LevelDB resources
	require.NotNil(t, env.sidecar)
	env.sidecar.Close()

	// Create a new context for the remaining operations
	newCtx, newCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(newCancel)

	t.Log("Modify the Sidecar config, use illegal host endpoint")
	// We need to use ilegalEndpoints instead of an empty Endpoints struct,
	// as the sidecar expects the Endpoints to be non-empty.
	env.config.Orderer.Connection.Endpoints = []*commontypes.OrdererEndpoint{
		{Host: "localhost", Port: 9999},
	}

	var err error
	t.Log("Create a new sidecar with the new configuration")
	env.sidecar, err = New(&env.config)
	require.NoError(t, err)
	t.Cleanup(env.sidecar.Close)

	// The Genesis block path is empty since the test didn't set WithConfigBlock:
	t.Log("Set the coordinator config block to use orderer AllEndpoints.")
	env.coordinator.SetConfigTransaction(env.configBlock.Data.Data[0])

	t.Log("Start the new sidecar")
	env.startSidecarServiceAndClientAndNotificationStream(newCtx, t, 0, test.InsecureTLSConfig)

	env.requireBlock(newCtx, t, 0)

	// Now we can send transactions with the new configuration
	env.sendTransactionsAndEnsureCommitted(newCtx, t, 1)
}

func TestSidecarRecovery(t *testing.T) {
	t.Parallel()
	env := newSidecarTestEnvWithTLS(t, sidecarTestConfig{}, test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startSidecarServiceAndClientAndNotificationStream(ctx, t, 0, test.InsecureTLSConfig)
	env.requireBlock(ctx, t, 0)

	t.Log("1. Commit block 1 to 10")
	for i := range 10 {
		env.sendTransactionsAndEnsureCommitted(ctx, t, uint64(i+1)) //nolint:gosec
	}

	t.Log("2. Stop the sidecar service and ledger service")
	cancel()
	require.Eventually(t, func() bool {
		return test.CheckServerStopped(t, env.config.Server.Endpoint.Address())
	}, 4*time.Second, 500*time.Millisecond)
	require.Eventually(t, func() bool {
		return !env.coordinator.IsStreamActive()
	}, 2*time.Second, 250*time.Millisecond)

	// NOTE: It is challenging to make the block store lag behind the state
	//       database because we record the last committed block number in
	//       the database at regular intervals (e.g., every five seconds).
	//       In most cases, this interval allows the sidecar enough time
	//       to commit the block to the block store. However, if the sidecar
	//       fails immediately after recording the last committed block
	//       number—i.e., before the ledger server empties its input queues—
	//       then the block store could end up behind the state database.
	//       Directly testing this scenario could lead to flakiness.
	//       Therefore, to simulate it, we use certain Fabric utilities
	//       to artificially create a situation where the block store
	//       falls behind the state database by removing blocks.
	t.Log("3. Remove all blocks from the ledger except block 0")
	require.NoError(t, blkstorage.ResetBlockStore(env.config.Ledger.Path))
	ctx2, cancel2 := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel2)
	checkNextBlockNumberToCommit(ctx2, t, env.coordinator, 11)

	// NOTE: The Fabric block store implementation used by the sidecar
	//       maintains the ledger height in memory and updates it whenever
	//       a new block is committed. Utilities such as ResetBlockStore
	//       are expected to be executed only when the process is fully
	//       shut down. Consequently, when the block store object is
	//       recreated, it reads the current height from disk. Therefore,
	//       we reset the ledger service object so that its in-memory data
	//       structure reflects the correct height.
	var err error
	env.sidecar.ledgerService, err = newLedgerService(
		env.config.Orderer.ChannelID,
		env.config.Ledger.Path,
		newPerformanceMetrics(),
	)
	require.NoError(t, err)
	ensureAtLeastHeight(t, env.sidecar.ledgerService, 1) // back to block 0

	t.Log("4. Make coordinator not idle to ensure sidecar is waiting")
	env.coordinator.SetWaitingTxsCount(10)

	t.Log("5. Restart the sidecar service and ledger service")
	env.startSidecarServiceAndClientAndNotificationStream(ctx2, t, 11, test.InsecureTLSConfig)

	t.Log("6. Recovery should not happen when coordinator is not idle.")
	require.Never(t, func() bool {
		return env.sidecar.ledgerService.GetBlockHeight() > 1
	}, 3*time.Second, 1*time.Second)

	t.Log("7. Make coordinator idle")
	env.coordinator.SetWaitingTxsCount(0)

	t.Log("8. Blockstore would recover lost blocks")
	ensureAtLeastHeight(t, env.sidecar.ledgerService, 11)

	t.Log("9. Send the next expected block by the coordinator.")
	env.sendTransactionsAndEnsureCommitted(ctx2, t, 11)

	checkNextBlockNumberToCommit(ctx2, t, env.coordinator, 12)
	ensureAtLeastHeight(t, env.sidecar.ledgerService, 12)
	cancel2()
}

func TestSidecarRecoveryAfterCoordinatorFailure(t *testing.T) {
	t.Parallel()
	env := newSidecarTestEnvWithTLS(t, sidecarTestConfig{}, test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startSidecarServiceAndClientAndNotificationStream(ctx, t, 0, test.InsecureTLSConfig)
	env.requireBlock(ctx, t, 0)

	coordLabel := env.getCoordinatorLabel(t)
	monitoring.RequireConnectionMetrics(
		t, coordLabel,
		env.sidecar.metrics.coordConnection,
		monitoring.ExpectedConn{Status: connection.Connected},
	)

	t.Log("1. Commit block 1 to 10")
	for i := range 10 {
		env.sendTransactionsAndEnsureCommitted(ctx, t, uint64(i+1)) //nolint:gosec
	}

	t.Log("2. Stop the coordinator")
	env.coordinatorServer.Servers[0].Stop()

	monitoring.RequireConnectionMetrics(
		t, coordLabel,
		env.sidecar.metrics.coordConnection,
		monitoring.ExpectedConn{Status: connection.Disconnected, FailureTotal: 1},
	)

	t.Log("3. Send transactions to ordering service to create block 11 after stopping the coordinator")
	txs := env.sendGeneratedTransactionsForBlock(ctx, t)

	t.Log("4. Restart the coordinator and validate processing block 11")
	env.coordinatorServer = mock.StartMockCoordinatorServiceFromListWithConfig(t, env.coordinator,
		env.coordinatorServer.Configs[0])

	monitoring.RequireConnectionMetrics(
		t, coordLabel,
		env.sidecar.metrics.coordConnection,
		monitoring.ExpectedConn{Status: connection.Connected, FailureTotal: 1},
	)

	env.requireBlockWithTXs(ctx, t, 11, txs)
}

func TestSidecarStartWithoutCoordinator(t *testing.T) {
	t.Parallel()
	env := newSidecarTestEnvWithTLS(t, sidecarTestConfig{}, test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	t.Log("Stop the coordinator")
	env.coordinatorServer.Servers[0].Stop()
	coordLabel := env.getCoordinatorLabel(t)
	test.CheckServerStopped(t, coordLabel)

	t.Log("Start the service")
	env.startSidecarServiceAndClientAndNotificationStream(ctx, t, 0, test.InsecureTLSConfig)
	monitoring.RequireConnectionMetrics(
		t, coordLabel,
		env.sidecar.metrics.coordConnection,
		monitoring.ExpectedConn{Status: connection.Disconnected},
	)

	t.Log("Restart the coordinator")
	env.coordinatorServer = mock.StartMockCoordinatorServiceFromListWithConfig(t, env.coordinator,
		env.coordinatorServer.Configs[0])
	monitoring.RequireConnectionMetrics(
		t, coordLabel,
		env.sidecar.metrics.coordConnection,
		monitoring.ExpectedConn{Status: connection.Connected},
	)

	t.Log("Wait for block 0")
	env.requireBlock(ctx, t, 0)

	t.Log("Commit blocks")
	for i := range 10 {
		env.sendTransactionsAndEnsureCommitted(ctx, t, uint64(i+1)) //nolint:gosec
	}
}

func TestSidecarVerifyBadTxForm(t *testing.T) {
	t.Parallel()
	env := newSidecarTestEnvWithTLS(t, sidecarTestConfig{WithConfigBlock: true}, test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env.startSidecarServiceAndClientAndNotificationStream(ctx, t, 0, test.InsecureTLSConfig)
	env.requireBlock(ctx, t, 0)
	txs, expected := MalformedTxTestCases(&workload.TxBuilder{ChannelID: env.ordererEnv.TestConfig.ChanID})
	testSize := len(expected)
	t.Logf("sending %d malformed txs", testSize)
	env.submitTXs(ctx, t, txs)
	t.Logf("sending %d good txs", blockSize-testSize)
	extraTxs := env.sendGeneratedTransactions(ctx, t, blockSize-testSize)
	txs = append(txs, extraTxs...)
	for range extraTxs {
		expected = append(expected, protoblocktx.Status_COMMITTED)
	}
	env.requireBlockWithTXsAndStatus(ctx, t, 1, txs, expected)
}

func (env *sidecarTestEnv) getCoordinatorLabel(t *testing.T) string {
	t.Helper()
	conn, err := connection.NewSingleConnection(env.config.Committer)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	return conn.CanonicalTarget()
}

func (env *sidecarTestEnv) sendTransactionsAndEnsureCommitted(
	ctx context.Context,
	t *testing.T,
	expectedBlockNumber uint64,
) {
	t.Helper()
	txs := env.sendGeneratedTransactionsForBlock(ctx, t)
	env.requireBlockWithTXs(ctx, t, expectedBlockNumber, txs)
}

func (env *sidecarTestEnv) sendGeneratedTransactionsForBlock(
	ctx context.Context,
	t *testing.T,
) []*protoloadgen.TX {
	t.Helper()
	// mock-orderer expects <blockSize> txs to create the next block.
	return env.sendGeneratedTransactions(ctx, t, blockSize)
}

func (env *sidecarTestEnv) sendGeneratedTransactions(
	ctx context.Context,
	t *testing.T,
	count int,
) []*protoloadgen.TX {
	t.Helper()
	txs := make([]*protoloadgen.TX, count)
	for i := range txs {
		txs[i] = makeValidTx(t, env.ordererEnv.TestConfig.ChanID)
	}
	env.submitTXs(ctx, t, txs)
	return txs
}

// submitTXs submits the given TXs and register them in the notification service.
func (env *sidecarTestEnv) submitTXs(ctx context.Context, t *testing.T, txs []*protoloadgen.TX) {
	t.Helper()
	txIDs := make([]string, len(txs))
	for i, tx := range txs {
		txIDs[i] = tx.Id
	}
	err := env.notifyStream.Send(&protonotify.NotificationRequest{
		TxStatusRequest: &protonotify.TxStatusRequest{
			TxIds: txIDs,
		},
		Timeout: durationpb.New(3 * time.Minute),
	})
	require.NoError(t, err)

	// Allows processing the request before submitting the payload.
	time.Sleep(1 * time.Second)

	for i, tx := range workload.MapToEnvelopeBatch(0, txs) {
		ok := env.ordererEnv.Orderer.SubmitEnv(ctx, tx)
		require.True(t, ok, "tx %d", i)
	}
}

func (env *sidecarTestEnv) requireBlockWithTXs(
	ctx context.Context,
	t *testing.T,
	expectedBlockNumber uint64,
	txs []*protoloadgen.TX,
) {
	t.Helper()
	allValid := make([]protoblocktx.Status, len(txs))
	for i := range allValid {
		allValid[i] = protoblocktx.Status_COMMITTED
	}
	env.requireBlockWithTXsAndStatus(ctx, t, expectedBlockNumber, txs, allValid)
}

//nolint:revive // 5 arguments.
func (env *sidecarTestEnv) requireBlockWithTXsAndStatus(
	ctx context.Context,
	t *testing.T,
	expectedBlockNumber uint64,
	txs []*protoloadgen.TX,
	status []protoblocktx.Status,
) {
	t.Helper()
	require.Len(t, status, len(txs))
	block := env.requireBlock(ctx, t, expectedBlockNumber)
	require.NotNil(t, block.Data)
	require.Len(t, block.Data.Data, blockSize)
	for i, msg := range block.Data.Data {
		payload, hdr, err := serialization.UnwrapEnvelope(msg)
		require.NoError(t, err)
		require.Equal(t, txs[i].Id, hdr.TxId)
		tx, err := serialization.UnmarshalTx(payload)
		require.NoError(t, err)
		require.True(t, proto.Equal(txs[i].Tx, tx))
	}
	require.NotNil(t, block.Metadata)
	require.GreaterOrEqual(t, len(block.Metadata.Metadata), 3)
	require.Len(t, block.Metadata.Metadata[2], blockSize)
	for i, actualStatus := range block.Metadata.Metadata[2] {
		require.Equalf(t, status[i].String(), protoblocktx.Status(actualStatus).String(), "tx index: %d", i)
	}

	txIDs := make([]string, len(txs))
	for i := range txs {
		txIDs[i] = txs[i].Id
	}
	RequireNotifications(t, env.notifyStream, expectedBlockNumber, txIDs, status)
}

func (env *sidecarTestEnv) requireBlock(
	ctx context.Context,
	t *testing.T,
	expectedBlockNumber uint64,
) *common.Block {
	t.Helper()
	checkNextBlockNumberToCommit(ctx, t, env.coordinator, expectedBlockNumber+1)
	ensureAtLeastHeight(t, env.sidecar.ledgerService, expectedBlockNumber+1)

	block, ok := channel.NewReader(ctx, env.committedBlock).Read()
	require.True(t, ok)
	require.NotNil(t, block)
	require.NotNil(t, block.Header)
	require.Equal(t, expectedBlockNumber, block.Header.Number)
	return block
}

func TestConstructStatuses(t *testing.T) {
	t.Parallel()
	statuses := map[string]*protoblocktx.StatusWithHeight{
		"tx1": {
			Code:        protoblocktx.Status_COMMITTED,
			BlockNumber: 1,
			TxNumber:    1,
		},
		"tx2": {
			Code:        protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
			BlockNumber: 1,
			TxNumber:    3,
		},
		"tx3": {
			Code:        protoblocktx.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED,
			BlockNumber: 2,
			TxNumber:    3,
		},
		"tx4": {
			Code:        protoblocktx.Status_COMMITTED,
			BlockNumber: 1,
			TxNumber:    6,
		},
	}
	expectedHeight := map[string]*types.Height{
		"tx1": types.NewHeight(1, 1),
		"tx2": types.NewHeight(1, 3),
		"tx3": types.NewHeight(1, 5),
		"tx4": types.NewHeight(1, 6),
	}

	expectedFinalStatuses := []protoblocktx.Status{
		protoblocktx.Status_COMMITTED,
		protoblocktx.Status_COMMITTED,
		protoblocktx.Status_COMMITTED,
		protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
		protoblocktx.Status_COMMITTED,
		protoblocktx.Status_REJECTED_DUPLICATE_TX_ID,
		protoblocktx.Status_COMMITTED,
	}
	actualFinalStatuses := make([]protoblocktx.Status, 7)
	for _, skippedIdx := range []int{0, 2, 4} {
		actualFinalStatuses[skippedIdx] = protoblocktx.Status_COMMITTED
	}
	require.NoError(t, fillStatuses(actualFinalStatuses, statuses, expectedHeight))
	require.Equal(t, expectedFinalStatuses, actualFinalStatuses)
}

func checkNextBlockNumberToCommit(
	ctx context.Context,
	t *testing.T,
	coordinator *mock.Coordinator,
	expectedBlockNumber uint64,
) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		nextBlock, err := coordinator.GetNextBlockNumberToCommit(ctx, nil)
		require.NoError(ct, err)
		require.NotNil(ct, nextBlock)
		require.Equal(ct, expectedBlockNumber, nextBlock.Number)
	}, expectedProcessingTime, 50*time.Millisecond)
}

func makeValidTx(t *testing.T, chanID string) *protoloadgen.TX {
	t.Helper()
	txb := workload.TxBuilder{ChannelID: chanID}
	return txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:        strings.ReplaceAll(uuid.New().String(), "-", "")[:32],
			NsVersion:   0,
			BlindWrites: []*protoblocktx.Write{{Key: utils.MustRead(rand.Reader, 32)}},
		}},
	})
}

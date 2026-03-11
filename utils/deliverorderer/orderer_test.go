/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/deliverorderer"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

const channelForTest = "mychannel"

func TestLoadOrdererDeliveryParametersFromConfig(t *testing.T) {
	t.Parallel()

	// Create test environment
	serverTLSConfig, clientTLSConfig := test.CreateServerAndClientTLSConfig(t, connection.MutualTLSMode)
	e := mock.NewOrdererTestEnv(t, &mock.OrdererTestParameters{
		ChanID:      channelForTest,
		NumIDs:      1,
		ServerPerID: 1,
		OrdererConfig: &mock.OrdererConfig{
			BlockSize:        10,
			SendGenesisBlock: true,
		},
		ServerTLSConfig: serverTLSConfig,
		ClientTLSConfig: clientTLSConfig,
	})

	tmpDir := t.TempDir()
	invalidBlockPath := filepath.Join(tmpDir, "invalid-block.pb")
	require.NoError(t, os.WriteFile(invalidBlockPath, []byte("invalid block content"), 0o600))

	for _, tc := range []struct {
		name                   string
		faultToleranceLevel    string
		configBlockPath        string
		tlsConfig              ordererconn.OrdererTLSConfig
		expectError            bool
		expectedChannelID      string
		expectedFaultTolerance string
		expectedErrorContains  string
	}{
		{
			name:                   "valid config with all parameters",
			faultToleranceLevel:    ordererconn.BFT,
			configBlockPath:        e.OrdererConnConfig.LastKnownConfigBlockPath,
			tlsConfig:              e.OrdererConnConfig.TLS,
			expectedChannelID:      channelForTest,
			expectedFaultTolerance: ordererconn.BFT,
		},
		{
			name:                   "valid config with CFT fault tolerance",
			faultToleranceLevel:    ordererconn.CFT,
			configBlockPath:        e.OrdererConnConfig.LastKnownConfigBlockPath,
			tlsConfig:              e.OrdererConnConfig.TLS,
			expectedChannelID:      channelForTest,
			expectedFaultTolerance: ordererconn.CFT,
		},
		{
			name:                  "error when config block path is empty",
			faultToleranceLevel:   ordererconn.BFT,
			configBlockPath:       "",
			expectError:           true,
			expectedErrorContains: "config block path is empty",
		},
		{
			name:                "error when config block file does not exist",
			faultToleranceLevel: ordererconn.BFT,
			configBlockPath:     "/nonexistent/path/config-block.pb",
			expectError:         true,
		},
		{
			name:                "error when config block file is invalid",
			faultToleranceLevel: ordererconn.BFT,
			configBlockPath:     invalidBlockPath,
			expectError:         true,
		},
		{
			name:                "error with invalid TLS configuration",
			faultToleranceLevel: ordererconn.BFT,
			configBlockPath:     e.OrdererConnConfig.LastKnownConfigBlockPath,
			tlsConfig: ordererconn.OrdererTLSConfig{
				Mode:              connection.MutualTLSMode,
				CertPath:          "/nonexistent/cert.pem",
				KeyPath:           "/nonexistent/key.pem",
				CommonCACertPaths: []string{"/nonexistent/ca.pem"},
			},
			expectError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Setup config based on test parameters
			config := &ordererconn.Config{
				FaultToleranceLevel:      tc.faultToleranceLevel,
				LastKnownConfigBlockPath: tc.configBlockPath,
				TLS:                      tc.tlsConfig,
				Retry:                    e.OrdererConnConfig.Retry,
				Identity:                 e.OrdererConnConfig.Identity,
			}

			// Call the function under test
			params, err := deliverorderer.LoadParametersFromConfig(config)

			// Verify results
			if tc.expectError {
				require.Error(t, err)
				assert.Nil(t, params)
				if tc.expectedErrorContains != "" {
					assert.Contains(t, err.Error(), tc.expectedErrorContains)
				}
				return
			}

			// Success case validations
			require.NoError(t, err)
			require.NotNil(t, params)
			assert.Equal(t, tc.expectedFaultTolerance, params.FaultToleranceLevel)

			assert.Equal(t, config.BlockWithholdingTimeout, params.BlockWithholdingTimeout)
			assert.Equal(t, config.LivenessCheckInterval, params.LivenessCheckInterval)
			assert.Equal(t, config.SuspicionGracePeriod, params.SuspicionGracePeriod)
			assert.Equal(t, config.MaxBlocksAhead, params.MaxBlocksAhead)

			assert.NotNil(t, params.Signer)
			assert.NotNil(t, params.Session)
			assert.NotNil(t, params.Session.LastestKnownConfig)
		})
	}
}

func TestNewOrdererDeliverEdgeCases(t *testing.T) {
	t.Parallel()

	configBlock, confErr := testcrypto.CreateOrExtendConfigBlockWithCrypto(t.TempDir(), &testcrypto.ConfigBlock{
		ChannelID:             "test",
		OrdererEndpoints:      []*types.OrdererEndpoint{{ID: 1, Host: "localhost", Port: 7050}},
		PeerOrganizationCount: 1,
	})
	require.NoError(t, confErr)

	serverTLSConfig, _ := test.CreateServerAndClientTLSConfig(t, connection.MutualTLSMode)
	tls, tlsErr := connection.NewTLSMaterials(serverTLSConfig)
	require.NoError(t, tlsErr)

	invalidBlock := &common.Block{
		Header: &common.BlockHeader{Number: 0},
		Data: &common.BlockData{
			Data: [][]byte{[]byte("invalid data")},
		},
	}

	cancelledContext, cancel := context.WithCancel(t.Context())
	cancel()

	t.Run("invalid LastBlock", func(t *testing.T) {
		t.Parallel()
		err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel: ordererconn.BFT,
			TLS:                 *tls,
			OutputBlock:         make(chan *common.Block, 10),
			Session: &deliverorderer.SessionInfo{
				LastestKnownConfig:          configBlock,
				NextBlockVerificationConfig: configBlock,
				LastBlock:                   invalidBlock, // Invalid block
			},
		})
		require.ErrorContains(t, err, "error loading last block")
	})

	t.Run("invalid LastestKnownConfig", func(t *testing.T) {
		t.Parallel()
		err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel: ordererconn.BFT,
			TLS:                 *tls,
			OutputBlock:         make(chan *common.Block, 10),
			Session: &deliverorderer.SessionInfo{
				LastestKnownConfig:          invalidBlock, // Invalid config
				NextBlockVerificationConfig: configBlock,
			},
		})
		require.ErrorContains(t, err, "error loading last known config")
	})

	t.Run("invalid NextBlockVerificationConfig", func(t *testing.T) {
		t.Parallel()
		err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel: ordererconn.BFT,
			TLS:                 *tls,
			OutputBlock:         make(chan *common.Block, 10),
			Session: &deliverorderer.SessionInfo{
				LastestKnownConfig:          configBlock,
				NextBlockVerificationConfig: invalidBlock, // Invalid config
			},
		})
		require.ErrorContains(t, err, "error loading next block verification config")
	})

	t.Run("NextBlockVerificationConfig is ahead of LastBlock", func(t *testing.T) {
		t.Parallel()

		txs := workload.GenerateTransactions(t, nil, 10)
		lastBlock := workload.MapToOrdererBlock(0, txs)
		testcrypto.PrepareBlockHeaderAndMetadata(lastBlock, testcrypto.BlockPrepareParameters{
			PrevBlock: configBlock,
		})
		require.EqualValues(t, 1, lastBlock.Header.Number)

		configBlock3 := proto.CloneOf(configBlock)
		configBlock3.Header.Number = 3

		err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel: ordererconn.BFT,
			TLS:                 *tls,
			OutputBlock:         make(chan *common.Block, 10),
			Session: &deliverorderer.SessionInfo{
				NextBlockVerificationConfig: configBlock3,
				LastBlock:                   lastBlock,
			},
		})
		require.ErrorContains(t, err, "config block number [3] is ahead of the next expected block [2]")
	})
}

func TestOrdererDeliverCFT(t *testing.T) {
	t.Parallel()
	e := newDeliverOrdererTestEnv(t, deliverOrdererTestEnvParams{
		ftMode:  ordererconn.CFT,
		tlsMode: connection.NoneTLSMode,
	})
	e.startDelivery(t)
	e.waitForDeliveryOfConfigBlock(t)

	t.Log("Sanity check - submit blocks")
	b := e.submit(t)
	require.EqualValues(t, 0, b.SourceID)

	t.Log("CFT mode: blocks with bad signatures should be rejected")
	expectedBlockNum := e.PrevBlock.Header.Number + 1
	txs := workload.GenerateTransactions(t, nil, 10)
	badSigBlock := workload.MapToOrdererBlock(0, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(badSigBlock, testcrypto.BlockPrepareParameters{
		PrevBlock: e.PrevBlock,
	})
	for _, p := range e.PartyStates {
		p.ReplaceBlock.Store(expectedBlockNum, badSigBlock)
	}

	goodBlock := workload.MapToOrdererBlock(0, txs)
	err := e.Orderer.SubmitBlock(t.Context(), goodBlock)
	require.NoError(t, err)

	_, ok := channel.NewReader(t.Context(), e.output).ReadWithTimeout(3 * time.Second)
	require.False(t, ok)

	t.Log("Releasing the real block - eventually should be received")
	for _, p := range e.PartyStates {
		p.ReplaceBlock.Clear()
	}
	gottenBlock := e.WaitForBlock(t, e.output)
	require.Len(t, gottenBlock.Data.Data, 10)
	b = e.getBlockWithSource(t, gottenBlock)
	require.EqualValues(t, 0, b.SourceID)
}

func TestOrdererDeliverNoFT(t *testing.T) {
	t.Parallel()
	e := newDeliverOrdererTestEnv(t, deliverOrdererTestEnvParams{
		tlsMode: connection.NoneTLSMode,
	})
	e.startDeliveryNoFt(t)
	configBlock := e.WaitForBlock(t, e.output)
	require.Len(t, configBlock.Data.Data, 1)

	t.Log("Sanity check - submit blocks")
	e.Submit(t, e.output)

	t.Log("NoFT mode: even blocks with bad signatures are accepted")
	expectedBlockNum := e.PrevBlock.Header.Number + 1
	txs := workload.GenerateTransactions(t, nil, 10)
	badSigBlock := workload.MapToOrdererBlock(0, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(badSigBlock, testcrypto.BlockPrepareParameters{
		PrevBlock: e.PrevBlock,
	})
	for _, p := range e.PartyStates {
		p.ReplaceBlock.Store(expectedBlockNum, badSigBlock)
	}
	e.Submit(t, e.output)
}

func TestOrdererDeliverBFT(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		tlsMode            string
		withholdingTimeout time.Duration
		maxBlocksAhead     uint64
	}{
		{name: connection.NoneTLSMode, tlsMode: connection.NoneTLSMode},
		{name: connection.OneSideTLSMode, tlsMode: connection.OneSideTLSMode},
		{name: connection.MutualTLSMode, tlsMode: connection.MutualTLSMode},
		{name: "max blocks ahead", withholdingTimeout: time.Hour, maxBlocksAhead: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newDeliverOrdererTestEnv(t, deliverOrdererTestEnvParams{
				ftMode:             ordererconn.BFT,
				tlsMode:            tc.tlsMode,
				withholdingTimeout: tc.withholdingTimeout,
				maxBlocksAhead:     tc.maxBlocksAhead,
			})
			stopDelivery := e.startDelivery(t)
			e.waitForDeliveryOfConfigBlock(t)

			t.Log("All good")
			e.submit(t)

			require.Len(t, e.PartyStates, 3)
			p0 := e.PartyStates[0]
			p1 := e.PartyStates[1]
			p2 := e.PartyStates[2]

			t.Log("Switch data source to party 2 (holding)")
			expectedBlockNum := e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum)
			p1.HoldFromBlock.Store(expectedBlockNum)
			p2.HoldFromBlock.Store(expectedBlockNum + 1)
			b := e.submit(t)
			require.EqualValues(t, 2, b.SourceID)

			t.Log("Switch data source to party 1 (holding)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum)
			p1.HoldFromBlock.Store(expectedBlockNum + 1)
			p2.HoldFromBlock.Store(expectedBlockNum)
			b = e.submit(t)
			require.EqualValues(t, 1, b.SourceID)

			t.Log("Switch data source to party 0 (holding)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum + 1)
			p1.HoldFromBlock.Store(expectedBlockNum)
			p2.HoldFromBlock.Store(expectedBlockNum)
			b = e.submit(t)
			require.EqualValues(t, 0, b.SourceID)

			t.Log("Switch data source to party 2 (malformed block)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum + 1)
			p1.HoldFromBlock.Store(expectedBlockNum)
			p2.HoldFromBlock.Store(expectedBlockNum + 1)
			p0.ReplaceBlock.Store(expectedBlockNum, &common.Block{
				Header: &common.BlockHeader{Number: expectedBlockNum},
			})
			b = e.submit(t)
			require.EqualValues(t, 2, b.SourceID)

			t.Log("Switch data source to party 1 (bad signatures)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum)
			p1.HoldFromBlock.Store(expectedBlockNum + 1)
			p2.HoldFromBlock.Store(expectedBlockNum + 1)
			badSigBlock := &common.Block{}
			testcrypto.PrepareBlockHeaderAndMetadata(badSigBlock, testcrypto.BlockPrepareParameters{
				PrevBlock: e.PrevBlock,
			})
			p2.ReplaceBlock.Store(expectedBlockNum, badSigBlock)
			b = e.submit(t)
			require.EqualValues(t, 1, b.SourceID)

			t.Log("Recover from last delivery params")
			stopDelivery()

			p1.HoldFromBlock.Store(0)
			e.startDelivery(t)
			b = e.submit(t)
			require.EqualValues(t, 1, b.SourceID)

			t.Log("New config block")
			e.SubmitConfigBlock(t, &testcrypto.ConfigBlock{})
			e.getBlockWithSource(t, e.WaitForBlock(t, e.output))

			t.Log("Sanity check with new config block")
			e.submit(t)
		})
	}
}

type deliverOrdererTestEnvParams struct {
	tlsMode            string
	ftMode             string
	withholdingTimeout time.Duration
	maxBlocksAhead     uint64
}

type deliverOrdererTestEnv struct {
	*mock.OrdererTestEnv
	deliveryParams   deliverorderer.Parameters
	output           chan *common.Block
	outputWithSource chan *deliver.BlockWithSourceID
}

func newDeliverOrdererTestEnv(t *testing.T, p deliverOrdererTestEnvParams) *deliverOrdererTestEnv {
	t.Helper()
	// Create the credentials for both server and client using the same CA.
	// In this test, we are using the same server TLS config for all the orderer instances for simplicity.
	serverTLSConfig, clientTLSConfig := test.CreateServerAndClientTLSConfig(t, p.tlsMode)

	// We use a short retry grpc-config to shorten the test time.
	e := &deliverOrdererTestEnv{
		OrdererTestEnv: mock.NewOrdererTestEnv(t, &mock.OrdererTestParameters{
			ChanID:      channelForTest,
			NumIDs:      3,
			ServerPerID: 4,
			OrdererConfig: &mock.OrdererConfig{
				BlockSize:        10,
				SendGenesisBlock: true,
			},
			ServerTLSConfig: serverTLSConfig,
			ClientTLSConfig: clientTLSConfig,
		}),
		output:           make(chan *common.Block, 100),
		outputWithSource: make(chan *deliver.BlockWithSourceID, 100),
	}
	params, err := deliverorderer.LoadParametersFromConfig(&e.OrdererConnConfig)
	require.NoError(t, err)
	params.FaultToleranceLevel = p.ftMode
	params.BlockWithholdingTimeout = p.withholdingTimeout
	params.MaxBlocksAhead = p.maxBlocksAhead
	params.Retry.MaxElapsedTime = time.Minute
	params.OutputBlock = e.output
	params.OutputBlockWithSourceID = e.outputWithSource
	params.Session.NextBlockVerificationConfig = params.Session.LastestKnownConfig
	e.deliveryParams = *params
	return e
}

func (e *deliverOrdererTestEnv) startDelivery(t *testing.T) context.CancelFunc {
	t.Helper()
	wg := sync.WaitGroup{}
	t.Cleanup(wg.Wait)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	wg.Go(func() {
		deliverErr := deliverorderer.ToQueue(ctx, e.deliveryParams)
		assert.ErrorIs(t, deliverErr, context.Canceled)
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

func (e *deliverOrdererTestEnv) startDeliveryNoFt(t *testing.T) context.CancelFunc {
	t.Helper()
	wg := sync.WaitGroup{}
	t.Cleanup(wg.Wait)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	wg.Go(func() {
		deliverErr := deliverorderer.ToQueueWithNoFT(ctx, deliverorderer.NoFTParameters{
			ClientConfig: &e.OrdererConnConfig,
			NextBlockNum: 0,
			OutputBlock:  e.output,
		})
		assert.ErrorIs(t, deliverErr, context.Canceled)
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

func (e *deliverOrdererTestEnv) waitForDeliveryOfConfigBlock(t *testing.T) {
	t.Helper()
	configBlock := e.WaitForBlock(t, e.output)
	require.Len(t, configBlock.Data.Data, 1)
	e.getBlockWithSource(t, configBlock)
	e.PrevBlock = configBlock
}

func (e *deliverOrdererTestEnv) getBlockWithSource(
	t *testing.T, expectedBlock *common.Block,
) *deliver.BlockWithSourceID {
	t.Helper()
	blk2, blkOK := channel.NewReader(t.Context(), e.outputWithSource).ReadWithTimeout(3 * time.Second)
	require.True(t, blkOK)
	require.NotNil(t, blk2)
	test.RequireProtoEqual(t, blk2.Block, expectedBlock)
	return blk2
}

func (e *deliverOrdererTestEnv) submit(t *testing.T) *deliver.BlockWithSourceID {
	t.Helper()
	blk := e.Submit(t, e.output)
	return e.getBlockWithSource(t, blk)
}

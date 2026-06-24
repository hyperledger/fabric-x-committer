/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/deliverorderer"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/test"
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
		tlsConfig              connection.TLSConfig
		expectError            bool
		expectedChannelID      string
		expectedFaultTolerance string
		expectedErrorContains  string
	}{
		{
			name:                   "valid config with all parameters",
			faultToleranceLevel:    ordererdial.BFT,
			configBlockPath:        e.OrdererConnConfig.LatestKnownConfigBlockPath,
			tlsConfig:              e.OrdererConnConfig.TLS,
			expectedChannelID:      channelForTest,
			expectedFaultTolerance: ordererdial.BFT,
		},
		{
			name:                   "valid config with CFT fault tolerance",
			faultToleranceLevel:    ordererdial.CFT,
			configBlockPath:        e.OrdererConnConfig.LatestKnownConfigBlockPath,
			tlsConfig:              e.OrdererConnConfig.TLS,
			expectedChannelID:      channelForTest,
			expectedFaultTolerance: ordererdial.CFT,
		},
		{
			name:                  "error when config block path is empty",
			faultToleranceLevel:   ordererdial.BFT,
			configBlockPath:       "",
			expectError:           true,
			expectedErrorContains: "config block path is empty",
		},
		{
			name:                "error when config block file does not exist",
			faultToleranceLevel: ordererdial.BFT,
			configBlockPath:     "/nonexistent/path/config-block.pb",
			expectError:         true,
		},
		{
			name:                "error when config block file is invalid",
			faultToleranceLevel: ordererdial.BFT,
			configBlockPath:     invalidBlockPath,
			expectError:         true,
		},
		{
			name:                "error with invalid TLS configuration",
			faultToleranceLevel: ordererdial.BFT,
			configBlockPath:     e.OrdererConnConfig.LatestKnownConfigBlockPath,
			tlsConfig: connection.TLSConfig{
				Mode:        connection.MutualTLSMode,
				CertPath:    "/nonexistent/cert.pem",
				KeyPath:     "/nonexistent/key.pem",
				CACertPaths: []string{"/nonexistent/ca.pem"},
			},
			expectError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Setup config based on test parameters
			config := &ordererdial.Config{
				FaultToleranceLevel:        tc.faultToleranceLevel,
				LatestKnownConfigBlockPath: tc.configBlockPath,
				TLS:                        tc.tlsConfig,
				Retry:                      e.OrdererConnConfig.Retry,
				Identity:                   e.OrdererConnConfig.Identity,
			}

			// Call the function under test
			params, err := deliverorderer.LoadParametersFromConfig(config)

			// Verify results
			if tc.expectError {
				require.Error(t, err)
				if tc.expectedErrorContains != "" {
					assert.Contains(t, err.Error(), tc.expectedErrorContains)
				}
				return
			}

			// Success case validations
			require.NoError(t, err)
			require.NotNil(t, params)
			assert.Equal(t, tc.expectedFaultTolerance, params.FaultToleranceLevel)

			assert.Equal(t, config.SuspicionGracePeriodPerBlock, params.SuspicionGracePeriodPerBlock)

			assert.NotNil(t, params.Signer)
			assert.NotNil(t, params.LatestKnownConfig)
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
	tls, tlsErr := connection.NewClientTLSCredentials(serverTLSConfig)
	require.NoError(t, tlsErr)

	invalidBlock := &common.Block{
		Header: &common.BlockHeader{Number: 0},
		Data: &common.BlockData{
			Data: [][]byte{[]byte("invalid data")},
		},
	}

	cancelledContext, cancel := context.WithCancel(t.Context())
	cancel()

	t.Run("invalid FaultToleranceLevel", func(t *testing.T) {
		t.Parallel()
		_, err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel: "invalid",
			TLS:                 *tls,
			OutputBlock:         make(chan *common.Block, 10),
		})
		require.ErrorContains(t, err, "invalid fault tolerance level")
	})

	t.Run("invalid LastBlock", func(t *testing.T) {
		t.Parallel()
		_, err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel:          ordererdial.BFT,
			TLS:                          *tls,
			OutputBlock:                  make(chan *common.Block, 10),
			SuspicionGracePeriodPerBlock: time.Second,
			LatestKnownConfig:            configBlock,
			NextBlockVerificationConfig:  configBlock,
			LastBlock:                    invalidBlock, // Invalid block
		})
		require.ErrorContains(t, err, "error loading last block")
	})

	t.Run("invalid LatestKnownConfig", func(t *testing.T) {
		t.Parallel()
		_, err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel:          ordererdial.BFT,
			TLS:                          *tls,
			OutputBlock:                  make(chan *common.Block, 10),
			SuspicionGracePeriodPerBlock: time.Second,
			LatestKnownConfig:            invalidBlock, // Invalid config
			NextBlockVerificationConfig:  configBlock,
		})
		require.ErrorContains(t, err, "error loading last known config")
	})

	t.Run("invalid NextBlockVerificationConfig", func(t *testing.T) {
		t.Parallel()
		_, err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel:          ordererdial.BFT,
			TLS:                          *tls,
			OutputBlock:                  make(chan *common.Block, 10),
			SuspicionGracePeriodPerBlock: time.Second,
			LatestKnownConfig:            configBlock,
			NextBlockVerificationConfig:  invalidBlock, // Invalid config
		})
		require.ErrorContains(t, err, "error loading next block verification config")
	})

	t.Run("NextBlockVerificationConfig is ahead of LastBlock", func(t *testing.T) {
		t.Parallel()

		txs := workload.GenerateTransactions(t, nil, 10)
		lastBlock := testcrypto.PrepareBlockHeaderAndMetadata(
			workload.MapToOrdererBlock(0, txs),
			testcrypto.BlockPrepareParameters{
				PrevBlock: configBlock,
			},
		)
		require.EqualValues(t, 1, lastBlock.Header.Number)

		configBlock3 := proto.CloneOf(configBlock)
		configBlock3.Header.Number = 3

		_, err := deliverorderer.ToQueue(cancelledContext, deliverorderer.Parameters{
			FaultToleranceLevel:          ordererdial.BFT,
			TLS:                          *tls,
			OutputBlock:                  make(chan *common.Block, 10),
			SuspicionGracePeriodPerBlock: time.Second,
			NextBlockVerificationConfig:  configBlock3,
			LastBlock:                    lastBlock,
		})
		require.ErrorContains(t, err, "config block number [3] is ahead of the next expected block [2]")
	})
}

func TestOrdererDeliverCFT(t *testing.T) {
	t.Parallel()
	e := newDeliverOrdererTestEnv(t, deliverOrdererTestEnvParams{
		ftMode:  ordererdial.CFT,
		tlsMode: connection.NoneTLSMode,
	})
	e.startDelivery(t)
	e.waitForDeliveryOfConfigBlock(t)

	m := e.deliveryParams.Metrics

	// Verify initial metrics state
	test.RequireIntMetricValue(t, 0, m.CurrentDataSourceID)
	test.RequireIntMetricValue(t, 0, m.StreamBlockNumber.WithLabelValues("data"))

	t.Log("Sanity check - submit blocks")
	b := e.submit(t)
	require.EqualValues(t, 0, b.SourceID)

	// Verify stream progress metrics after first block
	test.EventuallyIntMetric(t, 1, m.StreamBlockNumber.WithLabelValues("data"),
		5*time.Second, 100*time.Millisecond, "data stream should reach block 1")

	// Verify blocks delivered counter incremented
	blocksDelivered := test.GetIntMetricValue(t, m.BlocksDeliveredTotal.WithLabelValues("data", "0"))
	require.GreaterOrEqual(t, blocksDelivered, 1, "at least one block should be delivered")

	// Verify stream lifecycle metrics
	streamStarts := test.GetIntMetricValue(t, m.StreamStartsTotal.WithLabelValues("data", "0"))
	require.GreaterOrEqual(t, streamStarts, 1, "stream should have started")

	activeStreams := test.GetIntMetricValue(t, m.ActiveStreams.WithLabelValues("data", "0"))
	require.Equal(t, 1, activeStreams, "one data stream should be active")

	// Config updates: Genesis block (block 0) is not counted as an update
	configUpdates := test.GetIntMetricValue(t, m.ConfigUpdatesTotal)
	require.Equal(t, 0, configUpdates, "no config updates yet")

	t.Log("CFT mode: blocks with bad signatures should be rejected")
	expectedBlockNum := e.PrevBlock.Header.Number + 1
	txs := workload.GenerateTransactions(t, nil, 10)
	badSigBlock := testcrypto.PrepareBlockHeaderAndMetadata(
		workload.MapToOrdererBlock(0, txs),
		testcrypto.BlockPrepareParameters{
			PrevBlock: e.PrevBlock,
		},
	)
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

	// Verify final stream progress
	test.EventuallyIntMetric(t, 2, m.StreamBlockNumber.WithLabelValues("data"),
		5*time.Second, 100*time.Millisecond, "data stream should reach block 2")

	// Verify stream restart metrics
	streamRestarts := test.GetIntMetricValue(t, m.StreamRestartsTotal.WithLabelValues("data_block_error"))
	require.GreaterOrEqual(t, streamRestarts, 1, "stream should have restarted due to bad signature")

	// Verify error metrics for signature verification failure
	sigErrors := test.GetIntMetricValue(t, m.StreamErrorsTotal.WithLabelValues("data", "0", "signature_verification"))
	require.GreaterOrEqual(t, sigErrors, 1, "signature verification error should be recorded")
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

	badSigBlock := testcrypto.PrepareBlockHeaderAndMetadata(
		workload.MapToOrdererBlock(0, txs),
		testcrypto.BlockPrepareParameters{
			PrevBlock: e.PrevBlock,
		},
	)
	for _, p := range e.PartyStates {
		p.ReplaceBlock.Store(expectedBlockNum, badSigBlock)
	}
	e.Submit(t, e.output)
}

func TestOrdererDeliverBFT(t *testing.T) {
	t.Parallel()
	for _, tlsMode := range test.ServerModes {
		t.Run(tlsMode, func(t *testing.T) {
			t.Parallel()
			e := newDeliverOrdererTestEnv(t, deliverOrdererTestEnvParams{
				ftMode:  ordererdial.BFT,
				tlsMode: tlsMode,
			})
			stopDelivery := e.startDelivery(t)
			e.waitForDeliveryOfConfigBlock(t)

			m := e.deliveryParams.Metrics

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

			// Verify current data source metric is being tracked
			currentSource := test.GetIntMetricValue(t, m.CurrentDataSourceID)
			require.Equal(t, 2, currentSource, "current data source should be party 2")

			// Verify block gap gauge is being tracked (negative gap means header streams are ahead)
			blockGap := test.GetIntMetricValue(t, m.BlockGapGauge.WithLabelValues("2"))
			require.GreaterOrEqual(t, blockGap, 0)

			p2.HoldFromBlock.Store(expectedBlockNum + 2)
			b = e.submit(t)
			require.EqualValues(t, 2, b.SourceID)

			blockGap = test.GetIntMetricValue(t, m.BlockGapGauge.WithLabelValues("2"))
			require.GreaterOrEqual(t, blockGap, 1, "block gap should be >= 1 when data is ahead")

			t.Log("Switch data source to party 1 (holding)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum)
			p1.HoldFromBlock.Store(expectedBlockNum + 1)
			p2.HoldFromBlock.Store(expectedBlockNum)
			b = e.submit(t)
			require.EqualValues(t, 1, b.SourceID)

			// Verify suspicion metrics for party 2
			suspicionCount2 := test.GetIntMetricValue(t, m.SuspicionRaisedTotal.WithLabelValues("2"))
			require.GreaterOrEqual(t, suspicionCount2, 1, "suspicion should be raised for party 2")
			suspicionConfirmCount2 := test.GetIntMetricValue(t, m.SuspicionConfirmedTotal.WithLabelValues("2"))
			require.GreaterOrEqual(t, suspicionConfirmCount2, 1, "suspicion should be confirmed for party 2")

			currentSource = test.GetIntMetricValue(t, m.CurrentDataSourceID)
			require.Equal(t, 1, currentSource, "current data source should be party 1")

			t.Log("Switch data source to party 0 (holding)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum + 1)
			p1.HoldFromBlock.Store(expectedBlockNum)
			p2.HoldFromBlock.Store(expectedBlockNum)
			b = e.submit(t)
			require.EqualValues(t, 0, b.SourceID)

			// Verify suspicion for party 1
			suspicionCount1 := test.GetIntMetricValue(t, m.SuspicionRaisedTotal.WithLabelValues("1"))
			require.GreaterOrEqual(t, suspicionCount1, 1, "suspicion should be raised for party 1")
			currentSource = test.GetIntMetricValue(t, m.CurrentDataSourceID)
			require.Equal(t, 0, currentSource, "current data source should be party 0")

			t.Log("Switch data source to party 2 (malformed block)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum + 1)
			p1.HoldFromBlock.Store(expectedBlockNum)
			p2.HoldFromBlock.Store(expectedBlockNum + 1)
			p0.ReplaceBlock.Store(expectedBlockNum, &common.Block{
				Header:   &common.BlockHeader{Number: expectedBlockNum},
				Data:     &common.BlockData{},
				Metadata: &common.BlockMetadata{},
			})
			b = e.submit(t)
			require.EqualValues(t, 2, b.SourceID)

			// Verify stream errors are being tracked
			// The malformed block comes from party 0 (the data stream source)
			malformedBlockErrors := test.GetIntMetricValue(t,
				m.StreamErrorsTotal.WithLabelValues("data", "0", "malformed_block"))
			require.GreaterOrEqual(t, malformedBlockErrors, 1,
				"at least one malformed block error should be recorded from party 0")

			t.Log("Switch data source to party 1 (bad signatures)")
			expectedBlockNum = e.PrevBlock.Header.Number + 1
			p0.HoldFromBlock.Store(expectedBlockNum)
			p1.HoldFromBlock.Store(expectedBlockNum + 1)
			p2.HoldFromBlock.Store(expectedBlockNum + 1)
			badSigBlock := testcrypto.PrepareBlockHeaderAndMetadata(&common.Block{}, testcrypto.BlockPrepareParameters{
				PrevBlock: e.PrevBlock,
			})
			p2.ReplaceBlock.Store(expectedBlockNum, badSigBlock)
			b = e.submit(t)
			require.EqualValues(t, 1, b.SourceID)

			// Verify signature verification errors are tracked
			// Party 1's data stream has the bad signature block.
			signatureErrors := test.GetIntMetricValue(t,
				m.StreamErrorsTotal.WithLabelValues("data", "2", "signature_verification"))
			require.GreaterOrEqual(t, signatureErrors, 1,
				"at least one signature verification error should be recorded from party 2's dats stream")

			t.Log("Recover from last delivery params")
			stopDelivery()

			p1.HoldFromBlock.Store(0)
			stopDelivery = e.startDelivery(t)
			b = e.submit(t)
			require.EqualValues(t, 1, b.SourceID)

			t.Log("New config block")
			e.SubmitConfigBlock(t, &testcrypto.ConfigBlock{})
			e.getBlockWithSource(t, e.WaitForBlock(t, e.output))

			t.Log("Sanity check with new config block")
			e.submit(t)

			// Verify config update metrics
			configUpdates := test.GetIntMetricValue(t, m.ConfigUpdatesTotal)
			require.GreaterOrEqual(t, configUpdates, 1, "should have at least 1 config update")

			// Verify config block number is tracked
			configBlockNum := test.GetIntMetricValue(t, m.ConfigBlockNumber)
			require.GreaterOrEqual(t, configBlockNum, 1, "config block number should be >= 1")

			finalBlockNum := test.GetIntMetricValue(t, m.StreamBlockNumber.WithLabelValues("data"))
			require.GreaterOrEqual(t, finalBlockNum, 5, "should have delivered multiple blocks")

			t.Log("Restart stream with longer suspecion deadline")
			stopDelivery()
			p0.HoldFromBlock.Store(0)
			p0.ReplaceBlock.Clear()
			p1.HoldFromBlock.Store(0)
			p1.ReplaceBlock.Clear()
			p2.HoldFromBlock.Store(0)
			p2.ReplaceBlock.Clear()
			e.deliveryParams.SuspicionGracePeriodPerBlock = time.Hour

			stopDelivery = e.startDelivery(t)
			b = e.submit(t)
			curSource := b.SourceID
			curSourceLabel := strconv.FormatUint(uint64(curSource), 10)

			t.Logf("Hold blocks from current source: %s", curSourceLabel)
			e.PartyStates[curSource].HoldFromBlock.Store(b.Block.Header.Number)

			curSusCount := test.GetIntMetricValue(t, m.SuspicionRaisedTotal.WithLabelValues(curSourceLabel))
			curConfirmedSusCount := test.GetIntMetricValue(t, m.SuspicionConfirmedTotal.WithLabelValues(curSourceLabel))
			curClearedSusCount := test.GetIntMetricValue(t, m.SuspicionClearedTotal.WithLabelValues(curSourceLabel))

			expectedDeadline := float64(time.Now().Add(time.Hour - time.Millisecond).UnixMilli())
			t.Log("Submit another block without receiving it")
			txs := workload.GenerateTransactions(t, nil, 10)
			block := workload.MapToOrdererBlock(0, txs)
			err := e.Orderer.SubmitBlock(t.Context(), block)
			require.NoError(t, err)
			b, ok := channel.NewReader(t.Context(), e.outputWithSource).ReadWithTimeout(3 * time.Second)
			require.False(t, ok)
			require.Nil(t, b)

			suspicions := test.GetIntMetricValue(t, m.SuspicionRaisedTotal.WithLabelValues(curSourceLabel))
			require.Greater(t, suspicions, curSusCount)
			suspicionConfirmed := test.GetIntMetricValue(t, m.SuspicionConfirmedTotal.WithLabelValues(curSourceLabel))
			require.Equal(t, curConfirmedSusCount, suspicionConfirmed)
			suspicionCleared := test.GetIntMetricValue(t, m.SuspicionClearedTotal.WithLabelValues(curSourceLabel))
			require.Equal(t, curClearedSusCount, suspicionCleared)

			// Verify target arrival deadline was set when suspicion was raised.
			targetDeadline := test.GetMetricValue(t, m.TargetArrivalDeadline.WithLabelValues(curSourceLabel))
			require.Greater(t, targetDeadline, expectedDeadline)

			// Verify gap.
			gap := test.GetIntMetricValue(t, m.BlockGapGauge.WithLabelValues(curSourceLabel))
			require.Equal(t, -1, gap)

			t.Log("Release data block")
			e.PartyStates[curSource].HoldFromBlock.Store(0)
			blk := e.WaitForBlock(t, e.output)
			require.NotNil(t, blk)

			// Suspecion cleared.
			suspicionCleared = test.GetIntMetricValue(t, m.SuspicionClearedTotal.WithLabelValues(curSourceLabel))
			require.Greater(t, suspicionCleared, curClearedSusCount)
			targetDeadline = test.GetMetricValue(t, m.TargetArrivalDeadline.WithLabelValues(curSourceLabel))
			require.Zero(t, targetDeadline)

			stopDelivery()
		})
	}
}

type deliverOrdererTestEnvParams struct {
	tlsMode string
	ftMode  string
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

	e := mock.NewOrdererTestEnv(t, &mock.OrdererTestParameters{
		ChanID:      channelForTest,
		NumIDs:      3,
		ServerPerID: 4,
		OrdererConfig: &mock.OrdererConfig{
			BlockSize:        10,
			SendGenesisBlock: true,
		},
		ServerTLSConfig: serverTLSConfig,
		ClientTLSConfig: clientTLSConfig,
	})
	output := make(chan *common.Block, 100)
	outputWithSource := make(chan *deliver.BlockWithSourceID, 100)
	params, err := deliverorderer.LoadParametersFromConfig(&e.OrdererConnConfig)
	require.NoError(t, err)
	params.FaultToleranceLevel = p.ftMode
	params.Retry.MaxElapsedTime = time.Minute
	params.OutputBlock = output
	params.OutputBlockWithSourceID = outputWithSource
	params.NextBlockVerificationConfig = params.LatestKnownConfig
	params.Metrics = deliverorderer.NewMetrics(monitoring.NewProvider(), monitoring.MetricsParameters{})
	return &deliverOrdererTestEnv{
		OrdererTestEnv:   e,
		deliveryParams:   params,
		output:           output,
		outputWithSource: outputWithSource,
	}
}

func (e *deliverOrdererTestEnv) startDelivery(t *testing.T) context.CancelFunc {
	t.Helper()
	wg := sync.WaitGroup{}
	t.Cleanup(wg.Wait)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	wg.Go(func() {
		sessionInfo, deliverErr := deliverorderer.ToQueue(ctx, e.deliveryParams)
		assert.ErrorIs(t, deliverErr, context.Canceled)
		e.deliveryParams.LastBlock = sessionInfo.LastBlock
		e.deliveryParams.NextBlockVerificationConfig = sessionInfo.NextBlockVerificationConfig
		e.deliveryParams.LatestKnownConfig = sessionInfo.LatestKnownConfig
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

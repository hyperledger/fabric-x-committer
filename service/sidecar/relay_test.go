/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type relayTestEnv struct {
	relay                      *relay
	coordinator                *mock.Coordinator
	incomingBlockToBeCommitted chan *common.Block
	committedBlock             chan *common.Block
	configBlocks               []*common.Block
	metrics                    *perfMetrics
	waitingTxsLimit            int
}

const (
	valid     = byte(protoblocktx.Status_COMMITTED)
	duplicate = byte(protoblocktx.Status_REJECTED_DUPLICATE_TX_ID)
)

func newRelayTestEnv(t *testing.T) *relayTestEnv {
	t.Helper()
	coord, coordinatorServer := mock.StartMockCoordinatorService(t)
	coordinatorEndpoint := coordinatorServer.Configs[0].Endpoint

	metrics := newPerformanceMetrics()
	relayService := newRelay(
		time.Second,
		metrics,
	)

	conn, err := connection.Connect(connection.NewInsecureDialConfig(&coordinatorEndpoint))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })

	logger.Infof("sidecar connected to coordinator at %s", &coordinatorEndpoint)

	env := &relayTestEnv{
		relay:                      relayService,
		coordinator:                coord,
		incomingBlockToBeCommitted: make(chan *common.Block, 10),
		committedBlock:             make(chan *common.Block, 10),
		metrics:                    metrics,
		waitingTxsLimit:            100,
	}

	client := protocoordinatorservice.NewCoordinatorClient(conn)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(relayService.run(ctx, &relayRunConfig{
			coordClient:                    client,
			nextExpectedBlockByCoordinator: 0,
			configUpdater: func(block *common.Block) {
				env.configBlocks = append(env.configBlocks, block)
			},
			incomingBlockToBeCommitted: env.incomingBlockToBeCommitted,
			outgoingCommittedBlock:     env.committedBlock,
			waitingTxsLimit:            env.waitingTxsLimit,
		}))
	}, nil)
	return env
}

func TestRelayNormalBlock(t *testing.T) {
	t.Parallel()
	relayEnv := newRelayTestEnv(t)
	relayEnv.coordinator.SetDelay(10 * time.Second)

	blk0 := createBlockForTest(t, 0, nil, [3]string{"tx1", "tx2", "tx3"})
	require.Nil(t, blk0.Metadata)
	relayEnv.incomingBlockToBeCommitted <- blk0

	test.EventuallyIntMetric(t, 3, relayEnv.metrics.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	test.EventuallyIntMetric(t, 3, relayEnv.metrics.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(relayEnv.waitingTxsLimit-3), relayEnv.relay.waitingTxsSlots.Load(t))

	committedBlock0 := <-relayEnv.committedBlock
	test.RequireIntMetricValue(t, 3, relayEnv.metrics.transactionsStatusReceivedTotal.WithLabelValues(
		protoblocktx.Status_COMMITTED.String(),
	))
	test.EventuallyIntMetric(t, 0, relayEnv.metrics.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(relayEnv.waitingTxsLimit), relayEnv.relay.waitingTxsSlots.Load(t))

	expectedMetadata := &common.BlockMetadata{
		Metadata: [][]byte{nil, nil, {valid, valid, valid}},
	}
	require.Equal(t, expectedMetadata, committedBlock0.Metadata)
	require.Equal(t, blk0, committedBlock0)

	m := relayEnv.metrics
	require.Greater(t, test.GetMetricValue(t, m.blockMappingInRelaySeconds), float64(0))
	require.Greater(t, test.GetMetricValue(t, m.mappedBlockProcessingInRelaySeconds), float64(0))
	require.Greater(t, test.GetMetricValue(t, m.transactionStatusesProcessingInRelaySeconds), float64(0))

	relayEnv.relay.waitingTxsSlots.Store(t, int64(0))
	blk1 := createBlockForTest(t, 1, nil, [3]string{"tx1", "tx2", "tx3"})
	relayEnv.incomingBlockToBeCommitted <- blk1
	require.Never(t, func() bool {
		return test.GetMetricValue(t, m.transactionsSentTotal) > 3
	}, 3*time.Second, 1*time.Second)
	relayEnv.relay.waitingTxsSlots.Store(t, int64(3))
	relayEnv.relay.waitingTxsSlots.Broadcast()
	test.EventuallyIntMetric(t, 6, relayEnv.metrics.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
}

func TestBlockWithDuplicateTransactions(t *testing.T) {
	t.Parallel()
	relayEnv := newRelayTestEnv(t)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	incoming := channel.NewWriter(ctx, relayEnv.incomingBlockToBeCommitted)
	committed := channel.NewReader(ctx, relayEnv.committedBlock)

	t.Log("Submitting block 0")
	blk0 := createBlockForTest(t, 0, nil, [3]string{"tx1", "tx1", "tx1"})
	require.Nil(t, blk0.Metadata)
	incoming.Write(blk0)

	t.Log("Waiting for block 0 result")
	committedBlock0, ok := committed.Read()
	require.True(t, ok)
	expectedMetadata := &common.BlockMetadata{
		Metadata: [][]byte{nil, nil, {valid, duplicate, duplicate}},
	}
	require.Equal(t, expectedMetadata, committedBlock0.Metadata)
	require.Equal(t, blk0, committedBlock0)

	t.Log("Submitting block 1")
	blk1 := createBlockForTest(t, 1, nil, [3]string{"tx2", "tx3", "tx2"})
	require.Nil(t, blk1.Metadata)
	incoming.Write(blk1)

	t.Log("Waiting for block 1 result")
	committedBlock1, ok := committed.Read()
	require.True(t, ok)
	expectedMetadata = &common.BlockMetadata{
		Metadata: [][]byte{nil, nil, {valid, valid, duplicate}},
	}
	require.Equal(t, expectedMetadata, committedBlock1.Metadata)
	require.Equal(t, blk1, committedBlock1)
}

func TestRelayConfigBlock(t *testing.T) {
	t.Parallel()
	relayEnv := newRelayTestEnv(t)
	configBlk := createConfigBlockForTest(t)
	relayEnv.incomingBlockToBeCommitted <- configBlk
	committedBlock := <-relayEnv.committedBlock
	require.Equal(t, configBlk, committedBlock)
	require.NotNil(t, committedBlock.Metadata)
	require.Greater(t, len(committedBlock.Metadata.Metadata), statusIdx)
	require.Equal(t, []byte{valid}, committedBlock.Metadata.Metadata[statusIdx])
	require.Len(t, relayEnv.configBlocks, 1)
	require.Equal(t, configBlk, relayEnv.configBlocks[0])
}

func createConfigBlockForTest(t *testing.T) *common.Block {
	t.Helper()
	block, err := workload.CreateConfigBlock(&workload.PolicyProfile{})
	require.NoError(t, err)
	return block
}

// createBlockForTest creates sample block with three txIDs.
func createBlockForTest(t *testing.T, number uint64, preBlockHash []byte, txIDs [3]string) *common.Block {
	t.Helper()
	return &common.Block{
		Header: &common.BlockHeader{
			Number:       number,
			PreviousHash: preBlockHash,
		},
		Data: &common.BlockData{
			Data: [][]byte{
				createEnvelopeBytesForTest(t, txIDs[0]),
				createEnvelopeBytesForTest(t, txIDs[1]),
				createEnvelopeBytesForTest(t, txIDs[2]),
			},
		},
	}
}

func createEnvelopeBytesForTest(t *testing.T, txID string) []byte {
	t.Helper()
	header := &common.Header{
		ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
			ChannelId: "ch1",
			TxId:      txID,
			Type:      int32(common.HeaderType_MESSAGE),
		}),
	}
	return protoutil.MarshalOrPanic(&common.Envelope{
		Payload: protoutil.MarshalOrPanic(&common.Payload{
			Header: header,
			Data:   protoutil.MarshalOrPanic(makeValidTx(t, txID)),
		}),
	})
}

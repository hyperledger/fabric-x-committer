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
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
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
	statusQueue                chan []*committerpb.TxStatus
	configBlocks               []*common.Block
	metrics                    *perfMetrics
	waitingTxsLimit            int
}

const (
	valid     = byte(committerpb.Status_COMMITTED)
	duplicate = byte(committerpb.Status_REJECTED_DUPLICATE_TX_ID)
)

func newRelayTestEnv(t *testing.T) *relayTestEnv {
	t.Helper()
	coord, coordinatorServer := mock.StartMockCoordinatorService(t, test.InsecureTLSConfig)
	coordinatorEndpoint := coordinatorServer.Configs[0].Endpoint

	metrics := newPerformanceMetrics()
	relayService := newRelay(
		time.Second,
		metrics,
	)

	conn := test.NewInsecureConnection(t, &coordinatorEndpoint)

	logger.Infof("sidecar connected to coordinator at %s", &coordinatorEndpoint)

	env := &relayTestEnv{
		relay:                      relayService,
		coordinator:                coord,
		incomingBlockToBeCommitted: make(chan *common.Block, 10),
		committedBlock:             make(chan *common.Block, 10),
		statusQueue:                make(chan []*committerpb.TxStatus, 10),
		metrics:                    metrics,
		waitingTxsLimit:            100,
	}

	client := servicepb.NewCoordinatorClient(conn)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(relayService.run(ctx, &relayRunConfig{
			coordClient:                    client,
			nextExpectedBlockByCoordinator: 0,
			configUpdater: func(block *common.Block) {
				env.configBlocks = append(env.configBlocks, block)
			},
			incomingBlockToBeCommitted: env.incomingBlockToBeCommitted,
			outgoingCommittedBlock:     env.committedBlock,
			outgoingStatusUpdates:      env.statusQueue,
			waitingTxsLimit:            env.waitingTxsLimit,
		}))
	}, nil)
	return env
}

func TestRelayNormalBlock(t *testing.T) {
	t.Parallel()
	relayEnv := newRelayTestEnv(t)
	m := relayEnv.metrics
	relayEnv.coordinator.SetDelay(10 * time.Second)

	t.Log("Block #0: Submit")
	txCount := 3
	blk0, txIDs0 := createBlockForTest(t, 0, nil)
	require.Nil(t, blk0.Metadata)
	relayEnv.incomingBlockToBeCommitted <- blk0

	t.Log("Block #0: Check submit metrics")
	test.EventuallyIntMetric(t, txCount, m.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	test.EventuallyIntMetric(t, txCount, m.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(relayEnv.waitingTxsLimit-txCount), relayEnv.relay.waitingTxsSlots.Load(t))

	t.Log("Block #0: Check block in the queue")
	committedBlock0 := <-relayEnv.committedBlock
	require.NotNil(t, committedBlock0)
	require.Equal(t, &common.BlockMetadata{
		Metadata: [][]byte{nil, nil, {valid, valid, valid}},
	}, committedBlock0.Metadata)
	require.Equal(t, blk0, committedBlock0)

	t.Log("Block #0: Check status in the queue")
	status0 := relayEnv.readAllStatusQueue(t)
	test.RequireProtoElementsMatch(t, []*committerpb.TxStatus{
		{
			Ref:    committerpb.NewTxRef(txIDs0[0], 0, 0),
			Status: committerpb.Status_COMMITTED,
		},
		{
			Ref:    committerpb.NewTxRef(txIDs0[1], 0, 1),
			Status: committerpb.Status_COMMITTED,
		},
		{
			Ref:    committerpb.NewTxRef(txIDs0[2], 0, 2),
			Status: committerpb.Status_COMMITTED,
		},
	}, status0)

	t.Log("Block #0: Check receive metrics")
	test.RequireIntMetricValue(t, txCount, m.transactionsStatusReceivedTotal.WithLabelValues(
		committerpb.Status_COMMITTED.String(),
	))
	test.EventuallyIntMetric(t, 0, m.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Greater(t, test.GetMetricValue(t, m.blockMappingInRelaySeconds), float64(0))
	require.Greater(t, test.GetMetricValue(t, m.mappedBlockProcessingInRelaySeconds), float64(0))
	require.Greater(t, test.GetMetricValue(t, m.transactionStatusesProcessingInRelaySeconds), float64(0))
	require.Equal(t, int64(relayEnv.waitingTxsLimit), relayEnv.relay.waitingTxsSlots.Load(t))

	t.Log("Block #1: Submit without available slots")
	blk1, _ := createBlockForTest(t, 1, nil)
	require.Nil(t, blk1.Metadata)
	relayEnv.relay.waitingTxsSlots.Store(t, int64(0))
	relayEnv.incomingBlockToBeCommitted <- blk1

	t.Log("Block #1: Verify not processed")
	require.Never(t, func() bool {
		return test.GetMetricValue(t, m.transactionsSentTotal) > 3
	}, 3*time.Second, 1*time.Second)

	t.Log("Block #1: Release slots and verify processing")
	relayEnv.relay.waitingTxsSlots.Store(t, int64(txCount))
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

	t.Log("Block #0: Submit")
	blk0, txIDs0 := createBlockForTest(t, 0, nil)
	require.Nil(t, blk0.Metadata)
	blk0.Data.Data[1] = blk0.Data.Data[0]
	blk0.Data.Data[2] = blk0.Data.Data[0]
	incoming.Write(blk0)

	t.Log("Block #0: Check block in the queue")
	committedBlock0, ok := committed.Read()
	require.True(t, ok)
	expectedMetadata := &common.BlockMetadata{
		Metadata: [][]byte{nil, nil, {valid, duplicate, duplicate}},
	}
	require.Equal(t, expectedMetadata, committedBlock0.Metadata)
	require.Equal(t, blk0, committedBlock0)

	t.Log("Block #0: Check status in the queue")
	status0 := relayEnv.readAllStatusQueue(t)
	test.RequireProtoElementsMatch(t, []*committerpb.TxStatus{
		{
			Ref:    committerpb.NewTxRef(txIDs0[0], 0, 0),
			Status: committerpb.Status_COMMITTED,
		},
	}, status0)

	t.Log("Block #1: Submit")
	blk1, txIDs1 := createBlockForTest(t, 1, nil)
	blk1.Data.Data[2] = blk1.Data.Data[0]
	require.Nil(t, blk1.Metadata)
	incoming.Write(blk1)

	t.Log("Block #1: Check block in the queue")
	committedBlock1, ok := committed.Read()
	require.True(t, ok)
	expectedMetadata = &common.BlockMetadata{
		Metadata: [][]byte{nil, nil, {valid, valid, duplicate}},
	}
	require.Equal(t, expectedMetadata, committedBlock1.Metadata)
	require.Equal(t, blk1, committedBlock1)

	t.Log("Block #1: Check status in the queue")
	status1 := relayEnv.readAllStatusQueue(t)
	test.RequireProtoElementsMatch(t, []*committerpb.TxStatus{
		{
			Ref:    committerpb.NewTxRef(txIDs1[0], 1, 0),
			Status: committerpb.Status_COMMITTED,
		},
		{
			Ref:    committerpb.NewTxRef(txIDs1[1], 1, 1),
			Status: committerpb.Status_COMMITTED,
		},
	}, status1)
}

func TestRelayConfigBlock(t *testing.T) {
	t.Parallel()
	relayEnv := newRelayTestEnv(t)
	m := relayEnv.metrics
	coordinatorDelay := 10 * time.Second
	relayEnv.coordinator.SetDelay(coordinatorDelay)

	t.Log("Block #0 (data tx): Submit")
	txCount := 3
	blk0, _ := createBlockForTest(t, 0, nil)
	relayEnv.incomingBlockToBeCommitted <- blk0

	t.Log("Block #1 (config tx): Submit.")
	configBlk := createConfigBlockForTest(t)
	configBlk.Header.Number = 1
	relayEnv.incomingBlockToBeCommitted <- configBlk

	t.Log("Block #2 (data tx): Submit.")
	blk2, _ := createBlockForTest(t, 2, nil)
	relayEnv.incomingBlockToBeCommitted <- blk2

	t.Log("Block #0 (data tx): Check submit metrics. Block 1 and 2 would not have been queued yet.")
	test.EventuallyIntMetric(t, txCount, m.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	test.EventuallyIntMetric(t, txCount, m.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(relayEnv.waitingTxsLimit-txCount), relayEnv.relay.waitingTxsSlots.Load(t))

	t.Log("Block #1 (config tx): Will not be queued till all previously submitted transactions are processed")
	require.Never(t, func() bool {
		return relayEnv.relay.waitingTxsSlots.Load(t) < int64(relayEnv.waitingTxsLimit-txCount)
	}, coordinatorDelay/2, 1*time.Second)

	t.Log("Block #0 (data tx): Committed.")
	committedBlock0 := <-relayEnv.committedBlock
	require.Equal(t, blk0, committedBlock0)

	t.Log("Block #1 (config tx): Check submit metrics. Block 1 would have been queued but Block 2.")
	test.EventuallyIntMetric(t, txCount+1, m.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	test.EventuallyIntMetric(t, 1, m.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(relayEnv.waitingTxsLimit-1), relayEnv.relay.waitingTxsSlots.Load(t))
	require.Never(t, func() bool {
		return relayEnv.relay.waitingTxsSlots.Load(t) < int64(relayEnv.waitingTxsLimit-1)
	}, coordinatorDelay/2, 1*time.Second)

	t.Log("Block #1 (config tx): Committed.")
	committedBlock1 := <-relayEnv.committedBlock

	select {
	case <-relayEnv.committedBlock:
		t.Fatal("Block #2 should not have been committed by now.")
	case <-time.After(coordinatorDelay / 2):
	}

	require.Equal(t, configBlk, committedBlock1)
	require.NotNil(t, committedBlock1.Metadata)
	require.Greater(t, len(committedBlock1.Metadata.Metadata), statusIdx)
	require.Equal(t, []byte{valid}, committedBlock1.Metadata.Metadata[statusIdx])
	require.Len(t, relayEnv.configBlocks, 1)
	require.Equal(t, configBlk, relayEnv.configBlocks[0])

	committedBlock2 := <-relayEnv.committedBlock
	require.Equal(t, blk2, committedBlock2)
}

func (e *relayTestEnv) readAllStatusQueue(t *testing.T) []*committerpb.TxStatus {
	t.Helper()
	var status []*committerpb.TxStatus
	statusQueue := channel.NewReader(t.Context(), e.statusQueue)
	// We have to read multiple times from the queue because it might split the status report into batches according
	// to the processing logic.
	for {
		s, ok := statusQueue.ReadWithTimeout(5 * time.Second)
		if !ok {
			break
		}
		status = append(status, s...)
	}
	return status
}

func createConfigBlockForTest(t *testing.T) *common.Block {
	t.Helper()
	block, err := workload.CreateConfigBlock(&workload.PolicyProfile{})
	require.NoError(t, err)
	return block
}

// createBlockForTest creates sample block with three txIDs.
func createBlockForTest(t *testing.T, number uint64, preBlockHash []byte) (*common.Block, [3]string) {
	t.Helper()
	tx1 := makeValidTx(t, "ch1")
	tx2 := makeValidTx(t, "ch1")
	tx3 := makeValidTx(t, "ch1")
	return &common.Block{
		Header: &common.BlockHeader{
			Number:       number,
			PreviousHash: preBlockHash,
		},
		Data: &common.BlockData{
			Data: [][]byte{
				tx1.SerializedEnvelope,
				tx2.SerializedEnvelope,
				tx3.SerializedEnvelope,
			},
		},
	}, [3]string{tx1.Id, tx2.Id, tx3.Id}
}

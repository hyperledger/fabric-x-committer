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
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils"
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
	metrics                    *perfMetrics
	waitingTxsLimit            int
}

const (
	valid     = byte(committerpb.Status_COMMITTED)
	duplicate = byte(committerpb.Status_REJECTED_DUPLICATE_TX_ID)
)

func newRelayTestEnv(t *testing.T) *relayTestEnv {
	t.Helper()
	coord, coordinatorServer := mock.StartMockCoordinatorService(t, test.StartServerParameters{})
	coordinatorEndpoint := coordinatorServer.Configs[0].GRPC.Endpoint

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
			incomingBlockToBeCommitted:     env.incomingBlockToBeCommitted,
			outgoingCommittedBlock:         env.committedBlock,
			outgoingStatusUpdates:          env.statusQueue,
			waitingTxsLimit:                env.waitingTxsLimit,
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
	test.EventuallyIntMetric(t, txCount, m.transactionInThroughput, 5*time.Second, 10*time.Millisecond)
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
	test.RequireIntMetricValue(t, txCount, m.transactionOutThroughput)
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

	committedBlock2 := <-relayEnv.committedBlock
	require.Equal(t, blk2, committedBlock2)
}

func TestRelaySnapshotBlockSplitAndDrain(t *testing.T) {
	t.Parallel()
	relayEnv := newRelayTestEnv(t)
	m := relayEnv.metrics
	coordinatorDelay := 2 * time.Second
	relayEnv.coordinator.SetDelay(coordinatorDelay)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	incoming := channel.NewWriter(ctx, relayEnv.incomingBlockToBeCommitted)
	committed := channel.NewReader(ctx, relayEnv.committedBlock)

	t.Log("Block #0 (regulars + snapshot): Submit")
	regular1 := makeValidTx(t, "ch1")
	regular2 := makeValidTx(t, "ch1")
	snapshot := makeSnapshotTxForTest(t, "ch1")
	blk0 := &common.Block{
		Header: &common.BlockHeader{Number: 0},
		Data: &common.BlockData{Data: [][]byte{
			regular1.SerializedEnvelope,
			regular2.SerializedEnvelope,
			snapshot.SerializedEnvelope,
		}},
	}
	require.Nil(t, blk0.Metadata)
	require.True(t, incoming.Write(blk0))

	t.Log("Block #0: Check regular transactions submitted first")
	test.EventuallyIntMetric(t, 2, m.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	test.EventuallyIntMetric(t, 2, m.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(relayEnv.waitingTxsLimit-2), relayEnv.relay.waitingTxsSlots.Load(t))

	t.Log("Block #0: Snapshot not submitted before regular drain")
	require.Never(t, func() bool {
		return test.GetIntMetricValue(t, m.transactionsSentTotal) > 2
	}, coordinatorDelay/2, 10*time.Millisecond)

	t.Log("Block #0: Snapshot submitted after regular drain")
	test.EventuallyIntMetric(t, 3, m.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	test.EventuallyIntMetric(t, 1, m.waitingTransactionsQueueSize, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(relayEnv.waitingTxsLimit-1), relayEnv.relay.waitingTxsSlots.Load(t))

	t.Log("Block #1: Enqueue while snapshot path drains")
	blk1, _ := createBlockForTest(t, 1, nil)
	require.Nil(t, blk1.Metadata)
	require.True(t, incoming.Write(blk1))

	t.Log("Block #1: Not submitted before snapshot drain")
	require.Never(t, func() bool {
		return test.GetIntMetricValue(t, m.transactionsSentTotal) > 3
	}, coordinatorDelay/2, 10*time.Millisecond)

	t.Log("Block #0: Committed")
	committedBlock0, ok := committed.Read()
	require.True(t, ok)
	require.Equal(t, blk0, committedBlock0)
	require.NotNil(t, committedBlock0.Metadata)
	require.Greater(t, len(committedBlock0.Metadata.Metadata), statusIdx)
	require.Equal(t, []byte{valid, valid, valid}, committedBlock0.Metadata.Metadata[statusIdx])

	t.Log("Block #1: Eventually submitted and committed")
	test.EventuallyIntMetric(t, 6, m.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	committedBlock1, ok := committed.Read()
	require.True(t, ok)
	require.Equal(t, blk1, committedBlock1)
	require.NotNil(t, committedBlock1.Metadata)
	require.Greater(t, len(committedBlock1.Metadata.Metadata), statusIdx)
	require.Equal(t, []byte{valid, valid, valid}, committedBlock1.Metadata.Metadata[statusIdx])
}

// TestSplitSnapshotMappedBlockPartitionsRejected verifies that rejected statuses are
// partitioned around the snapshot by their original block position: rejects before the
// snapshot ride the pre-snapshot segment and rejects after it ride the post-snapshot
// segment. This keeps a post-snapshot reject's stored tx_status commit after the snapshot
// barrier, so it does not leak into the snapshot clone's state.
func TestSplitSnapshotMappedBlockPartitionsRejected(t *testing.T) {
	t.Parallel()

	snapshotTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID}},
			Endorsements: dummyEndorsements(1),
		}
	}
	regularTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:        "ns",
				BlindWrites: []*applicationpb.Write{{Key: []byte("key")}},
			}},
			Endorsements: dummyEndorsements(1),
		}
	}
	// malformedTx has no namespaces, so it is rejected (MALFORMED_EMPTY_NAMESPACES) with a
	// stored status and no tx body, exercising the rejectedBefore partition branch.
	malformedTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{Endorsements: dummyEndorsements(1)}
	}

	txb := &workload.TxBuilder{ChannelID: testChannelID}
	// Block layout by original TxNum:
	//   0: malformed (rejected; empty namespaces; original TxNum 0 < snapshot TxNum 2)
	//   1: regular
	//   2: snapshot (accepted; the barrier)
	//   3: regular
	//   4: snapshot (rejected as duplicate; original TxNum 4 > snapshot TxNum 2)
	block := workload.MapToOrdererBlock(7, []*servicepb.LoadGenTx{
		txb.MakeTx(malformedTx()),
		txb.MakeTx(regularTx()),
		txb.MakeTx(snapshotTx()),
		txb.MakeTx(regularTx()),
		txb.MakeTx(snapshotTx()),
	})

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	require.NoError(t, err)
	require.True(t, mappedBlock.hasSnapshot)
	require.Len(t, mappedBlock.block.Rejected, 2)

	segments := splitSnapshotMappedBlock(mappedBlock)
	// Pre-snapshot regular segment, snapshot segment, post-snapshot regular segment.
	require.Len(t, segments, 3)

	pre, snap, post := segments[0], segments[1], segments[2]

	// Pre-snapshot segment: the leading regular TX plus the malformed reject (original TxNum 0),
	// which must land here (before the barrier), not on the post-snapshot segment.
	require.False(t, pre.hasSnapshot)
	require.Len(t, pre.block.Txs, 1)
	require.Len(t, pre.block.Rejected, 1)
	require.Equal(
		t,
		committerpb.Status_MALFORMED_EMPTY_NAMESPACES,
		pre.block.Rejected[0].Status,
	)
	require.Equal(t, uint32(0), pre.block.Rejected[0].Ref.TxNum)

	// Snapshot segment: the snapshot TX alone, no rejected.
	require.True(t, snap.hasSnapshot)
	require.Len(t, snap.block.Txs, 1)
	require.Equal(t, committerpb.SnapshotNamespaceID, snap.block.Txs[0].Content.Namespaces[0].NsId)
	require.Empty(t, snap.block.Rejected)

	// Post-snapshot segment: the trailing regular TX plus the duplicate-snapshot reject,
	// which must land here (after the barrier), not on the pre-snapshot segment.
	require.False(t, post.hasSnapshot)
	require.Len(t, post.block.Txs, 1)
	require.Len(t, post.block.Rejected, 1)
	require.Equal(
		t,
		committerpb.Status_REJECTED_DUPLICATE_SNAPSHOT_IN_BLOCK,
		post.block.Rejected[0].Status,
	)
	require.Equal(t, uint32(4), post.block.Rejected[0].Ref.TxNum)
}

// TestSplitSnapshotMappedBlockPositions verifies segment structure for the snapshot at each
// position in the block: first, middle, last, and snapshot-only. It asserts the number of
// segments and which segment carries the snapshot, exercising the leading/trailing empty-segment
// skips in splitSnapshotMappedBlock.
func TestSplitSnapshotMappedBlockPositions(t *testing.T) {
	t.Parallel()

	snapshotTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID}},
			Endorsements: dummyEndorsements(1),
		}
	}
	regularTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:        "ns",
				BlindWrites: []*applicationpb.Write{{Key: []byte("key")}},
			}},
			Endorsements: dummyEndorsements(1),
		}
	}

	tests := []struct {
		name string
		// txs is the ordered list of transaction factories for the block.
		txs              []func() *applicationpb.Tx
		expectedSegments int
		// snapshotSegment is the index of the segment that must carry the snapshot TX.
		snapshotSegment int
	}{
		{
			name:             "snapshot is the only tx",
			txs:              []func() *applicationpb.Tx{snapshotTx},
			expectedSegments: 1,
			snapshotSegment:  0,
		},
		{
			name:             "snapshot is the first tx",
			txs:              []func() *applicationpb.Tx{snapshotTx, regularTx, regularTx},
			expectedSegments: 2,
			snapshotSegment:  0,
		},
		{
			name:             "snapshot is a middle tx",
			txs:              []func() *applicationpb.Tx{regularTx, snapshotTx, regularTx},
			expectedSegments: 3,
			snapshotSegment:  1,
		},
		{
			name:             "snapshot is the last tx",
			txs:              []func() *applicationpb.Tx{regularTx, regularTx, snapshotTx},
			expectedSegments: 2,
			snapshotSegment:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			txb := &workload.TxBuilder{ChannelID: testChannelID}
			loadGenTxs := make([]*servicepb.LoadGenTx, len(tc.txs))
			for i, makeTx := range tc.txs {
				loadGenTxs[i] = txb.MakeTx(makeTx())
			}
			block := workload.MapToOrdererBlock(9, loadGenTxs)

			var txIDToHeight utils.SyncMap[string, servicepb.Height]
			mappedBlock, err := mapBlock(block, &txIDToHeight)
			require.NoError(t, err)
			require.True(t, mappedBlock.hasSnapshot)
			require.Empty(t, mappedBlock.block.Rejected)

			segments := splitSnapshotMappedBlock(mappedBlock)
			require.Len(t, segments, tc.expectedSegments)

			// Exactly one segment carries the snapshot, and it holds only the snapshot TX.
			for i, seg := range segments {
				if i == tc.snapshotSegment {
					require.True(t, seg.hasSnapshot)
					require.Len(t, seg.block.Txs, 1)
					require.Equal(
						t,
						committerpb.SnapshotNamespaceID,
						seg.block.Txs[0].Content.Namespaces[0].NsId,
					)
					continue
				}
				require.False(t, seg.hasSnapshot)
				require.NotEmpty(t, seg.block.Txs)
			}
		})
	}
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
	block, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(t.TempDir(), &testcrypto.ConfigBlock{
		PeerOrganizationCount: 1,
	})
	require.NoError(t, err)
	return block
}

func makeSnapshotTxForTest(t *testing.T, chanID string) *servicepb.LoadGenTx {
	t.Helper()
	txb := workload.TxBuilder{ChannelID: chanID}
	return txb.MakeTx(&applicationpb.Tx{
		Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID, NsVersion: 0}},
		Endorsements: dummyEndorsements(1),
	})
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

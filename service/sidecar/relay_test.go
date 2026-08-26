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
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type relayTestEnv struct {
	relay                      *relay
	coordinator                *mock.Coordinator
	incomingBlockToBeCommitted chan *common.Block
	committedBlock             chan *common.Block
	statusQueue                chan []*committerpb.TxStatus
	// The relay's own stages are connected by these two. The session owns them in production, so
	// the test supplies them here; leaving them nil would silently drop every block.
	mappedBlockQueue chan *blockMappingResult
	statusBatch      chan *servicepb.TxStatusBatch
	metrics          *perfMetrics
	waitingTxsLimit  int
}

const (
	valid     = byte(committerpb.Status_COMMITTED)
	duplicate = byte(committerpb.Status_REJECTED_DUPLICATE_TX_ID)
)

func newRelayTestEnv(t *testing.T) *relayTestEnv {
	t.Helper()
	coord, coordinatorServer := mock.StartMockCoordinatorService(t, test.StartServerParameters{})
	coordinatorEndpoint := coordinatorServer.Configs[0].GRPC.Endpoint

	q := newQueues(10)
	metrics := newPerformanceMetrics(q)
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
		mappedBlockQueue:           make(chan *blockMappingResult, 10),
		statusBatch:                make(chan *servicepb.TxStatusBatch, 10),
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
			mappedBlockQueue:               env.mappedBlockQueue,
			statusBatch:                    env.statusBatch,
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
	requireStatusMetadata(t, committedBlock1, valid)

	committedBlock2 := <-relayEnv.committedBlock
	require.Equal(t, blk2, committedBlock2)
}

// TestRelayUnprocessableConfigBlock verifies that a config block the sidecar cannot process ends
// the relay with a retryable error, so the sidecar restarts its block feed and fetches the block
// again, instead of committing the block without its config TX.
func TestRelayUnprocessableConfigBlock(t *testing.T) {
	t.Parallel()
	relayService := newRelay(time.Second, newPerformanceMetrics(newQueues(10)))
	incomingBlockToBeCommitted := make(chan *common.Block, 1)
	relayService.incomingBlockToBeCommitted = incomingBlockToBeCommitted
	relayService.waitingTxsSlots = utils.NewSlots(100)

	// A config TX with no TX ID in either its nested update or its outer envelope.
	incomingBlockToBeCommitted <- configBlockForTest(configTxForTest(t, configTxParts{}))

	err := relayService.preProcessBlock(t.Context(), make(chan *blockMappingResult, 1))
	require.ErrorContains(t, err, "cannot process the config TX [blk:1,num:0]")
	require.ErrorIs(t, err, retry.ErrBackOff)
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

	t.Log("Block #0 (regular, snapshot, regular): Submit")
	// The snapshot sits in the middle of the block to verify it is always submitted last,
	// regardless of its original position (see submitSnapshotBlock in relay.go).
	regular1 := makeValidTx(t, "ch1")
	regular2 := makeValidTx(t, "ch1")
	snapshot := makeSnapshotTxForTest(t, "ch1")
	blk0 := &common.Block{
		Header: &common.BlockHeader{Number: 0},
		Data: &common.BlockData{Data: [][]byte{
			regular1.SerializedEnvelope,
			snapshot.SerializedEnvelope,
			regular2.SerializedEnvelope,
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
	requireStatusMetadata(t, committedBlock0, valid, valid, valid)

	t.Log("Block #1: Eventually submitted and committed")
	test.EventuallyIntMetric(t, 6, m.transactionsSentTotal, 5*time.Second, 10*time.Millisecond)
	committedBlock1, ok := committed.Read()
	require.True(t, ok)
	require.Equal(t, blk1, committedBlock1)
	requireStatusMetadata(t, committedBlock1, valid, valid, valid)
}

// TestSubmitSnapshotBlockSnapshotOnly verifies that when the block contains only the snapshot
// TX (no regular TXs, no rejects), submitSnapshotBlock queues a single segment carrying the
// snapshot TX alone, and does not queue an empty non-snapshot segment.
func TestSubmitSnapshotBlockSnapshotOnly(t *testing.T) {
	t.Parallel()

	txb := &workload.TxBuilder{ChannelID: testChannelID}
	block := workload.MapToOrdererBlock(9, []*servicepb.LoadGenTx{makeSnapshotLoadGenTxForTest(txb)})

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	require.NoError(t, err)
	require.NotNil(t, mappedBlock.snapshotTx)
	require.Empty(t, mappedBlock.block.Rejected)

	segments := submitSnapshotBlockForTest(t, mappedBlock)
	require.Len(t, segments, 1)
	requireSnapshotOnlySegment(t, segments[0])
}

// TestSubmitSnapshotBlockCarriesAllRejected verifies that all rejected statuses in the block —
// regardless of whether they originally preceded or followed the snapshot — ride the single
// non-snapshot segment, which is queued (and drained) before the snapshot segment. The snapshot
// TX always commits last within its block.
func TestSubmitSnapshotBlockCarriesAllRejected(t *testing.T) {
	t.Parallel()

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
	// stored status and no tx body.
	malformedTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{Endorsements: dummyEndorsements(1)}
	}

	txb := &workload.TxBuilder{ChannelID: testChannelID}
	// Block layout by original TxNum:
	//   0: malformed (rejected; empty namespaces)
	//   1: regular
	//   2: snapshot (accepted; the barrier)
	//   3: regular
	//   4: snapshot (rejected as duplicate)
	block := workload.MapToOrdererBlock(7, []*servicepb.LoadGenTx{
		txb.MakeTx(malformedTx()),
		txb.MakeTx(regularTx()),
		makeSnapshotLoadGenTxForTest(txb),
		txb.MakeTx(regularTx()),
		makeSnapshotLoadGenTxForTest(txb),
	})

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	require.NoError(t, err)
	require.NotNil(t, mappedBlock.snapshotTx)
	require.Len(t, mappedBlock.block.Rejected, 2)

	segments := submitSnapshotBlockForTest(t, mappedBlock)
	// Single non-snapshot segment carrying both regular TXs and both rejects, then the snapshot
	// segment (always last).
	require.Len(t, segments, 2)

	rest, snap := segments[0], segments[1]

	// Non-snapshot segment: both regular TXs plus both rejects (malformed and duplicate-snapshot),
	// regardless of their original position relative to the snapshot.
	require.Len(t, rest.block.Txs, 2)
	require.Len(t, rest.block.Rejected, 2)
	require.Equal(t, committerpb.Status_MALFORMED_EMPTY_NAMESPACES, rest.block.Rejected[0].Status)
	require.Equal(t, uint32(0), rest.block.Rejected[0].Ref.TxNum)
	require.Equal(
		t,
		committerpb.Status_REJECTED_DUPLICATE_SNAPSHOT_IN_BLOCK,
		rest.block.Rejected[1].Status,
	)
	require.Equal(t, uint32(4), rest.block.Rejected[1].Ref.TxNum)

	// Snapshot segment: the snapshot TX alone, no rejected, always last.
	requireSnapshotOnlySegment(t, snap)
	require.Empty(t, snap.block.Rejected)
}

// TestSubmitSnapshotBlockPositions verifies segment structure for the snapshot at each position
// in the block: first, middle, last, and snapshot-only. The snapshot always ends up in the last
// segment (queued, hence committed, last within its block), collapsing to a single
// snapshot-only segment only when the block contains nothing else.
func TestSubmitSnapshotBlockPositions(t *testing.T) {
	t.Parallel()

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
		// snapshotPos is the position of the snapshot TX among the block's TXs.
		snapshotPos      int
		regularCount     int
		expectedSegments int
		expectedRestTxs  int
	}{
		{
			name:             "snapshot is the only tx",
			snapshotPos:      0,
			regularCount:     0,
			expectedSegments: 1,
			expectedRestTxs:  0,
		},
		{
			name:             "snapshot is the first tx",
			snapshotPos:      0,
			regularCount:     2,
			expectedSegments: 2,
			expectedRestTxs:  2,
		},
		{
			name:             "snapshot is a middle tx",
			snapshotPos:      1,
			regularCount:     2,
			expectedSegments: 2,
			expectedRestTxs:  2,
		},
		{
			name:             "snapshot is the last tx",
			snapshotPos:      2,
			regularCount:     2,
			expectedSegments: 2,
			expectedRestTxs:  2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			txb := &workload.TxBuilder{ChannelID: testChannelID}
			loadGenTxs := make([]*servicepb.LoadGenTx, 0, tc.regularCount+1)
			for i := 0; i <= tc.regularCount; i++ {
				if i == tc.snapshotPos {
					loadGenTxs = append(loadGenTxs, makeSnapshotLoadGenTxForTest(txb))
					continue
				}
				loadGenTxs = append(loadGenTxs, txb.MakeTx(regularTx()))
			}
			block := workload.MapToOrdererBlock(9, loadGenTxs)

			var txIDToHeight utils.SyncMap[string, servicepb.Height]
			mappedBlock, err := mapBlock(block, &txIDToHeight)
			require.NoError(t, err)
			require.NotNil(t, mappedBlock.snapshotTx)
			require.Empty(t, mappedBlock.block.Rejected)

			segments := submitSnapshotBlockForTest(t, mappedBlock)
			require.Len(t, segments, tc.expectedSegments)

			// The snapshot always rides the last segment, alone. Any earlier segment is a single
			// non-snapshot segment carrying every other TX in the block, in original order.
			snap := segments[len(segments)-1]
			requireSnapshotOnlySegment(t, snap)

			if tc.expectedRestTxs == 0 {
				return
			}
			rest := segments[0]
			require.Len(t, rest.block.Txs, tc.expectedRestTxs)
		})
	}
}

// makeSnapshotLoadGenTxForTest builds a standalone accepted _snapshot marker TX using txb.
func makeSnapshotLoadGenTxForTest(txb *workload.TxBuilder) *servicepb.LoadGenTx {
	return txb.MakeTx(&applicationpb.Tx{
		Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID}},
		Endorsements: dummyEndorsements(1),
	})
}

// submitSnapshotBlockForTest drives mappedBlock through relay.submitSnapshotBlock with an
// unbounded queue and slots, returning the segments in the order they were queued. It runs
// submitSnapshotBlock concurrently with reading the expected number of segments off the queue,
// releasing each segment's TXs immediately so drain (which blocks until they are "released")
// unblocks in turn.
func submitSnapshotBlockForTest(t *testing.T, mappedBlock *blockMappingResult) []*blockMappingResult {
	t.Helper()

	r := &relay{
		metrics:         newPerformanceMetrics(newQueues(10)),
		waitingTxsSlots: utils.NewSlots(int64(len(mappedBlock.block.Txs)) + 1),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	queueCh := make(chan *blockMappingResult, 2)
	queue := channel.NewWriter(ctx, queueCh)
	reader := channel.NewReader(ctx, queueCh)

	// submitSnapshotBlock queues at most two segments: the non-snapshot segment (skipped when
	// the block has no regular TXs/rejects) and the snapshot segment.
	expectedSegments := 1
	if len(mappedBlock.block.Txs) > 0 || len(mappedBlock.block.Rejected) > 0 {
		expectedSegments = 2
	}

	g, _ := errgroup.WithContext(ctx)
	g.Go(func() error {
		return r.submitSnapshotBlock(ctx, queue, mappedBlock)
	})

	segments := make([]*blockMappingResult, 0, expectedSegments)
	for range expectedSegments {
		segment, ok := reader.Read()
		require.True(t, ok, "timed out waiting for a segment")
		segments = append(segments, segment)
		r.waitingTxsSlots.Release(int64(len(segment.block.Txs)))
	}

	require.NoError(t, g.Wait())
	return segments
}

// requireSnapshotOnlySegment asserts that seg is a snapshot-only segment: it carries exactly the
// accepted snapshot TX, alone, as its single transaction.
func requireSnapshotOnlySegment(t *testing.T, seg *blockMappingResult) {
	t.Helper()
	require.Len(t, seg.block.Txs, 1)
	require.Equal(t, committerpb.SnapshotNamespaceID, seg.block.Txs[0].Content.Namespaces[0].NsId)
}

// requireStatusMetadata asserts that block's status metadata (at statusIdx) holds exactly the
// given per-transaction status bytes, in order.
func requireStatusMetadata(t *testing.T, block *common.Block, expectedStatus ...byte) {
	t.Helper()
	require.NotNil(t, block.Metadata)
	require.Greater(t, len(block.Metadata.Metadata), statusIdx)
	require.Equal(t, expectedStatus, block.Metadata.Metadata[statusIdx])
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

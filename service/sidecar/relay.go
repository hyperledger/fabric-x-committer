/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	relay struct {
		incomingBlockToBeCommitted    <-chan *common.Block
		outgoingCommittedBlock        chan<- *common.Block
		outgoingStatusUpdates         chan<- []*committerpb.TxStatus
		outgoingConfigBlocks          chan<- *common.Block
		outgoingCommittedBlockWithTxs chan<- *committedBlockWithTxs

		// nextBlockNumberToBeCommitted denotes the next block number of to be committed.
		nextBlockNumberToBeCommitted atomic.Uint64

		activeBlocksCount             atomic.Int32
		blkNumToBlkWithStatus         utils.SyncMap[uint64, *blockWithStatus]
		txIDToHeight                  utils.SyncMap[string, servicepb.Height]
		lastCommittedBlockSetInterval time.Duration
		waitingTxsSlots               *utils.Slots
		metrics                       *perfMetrics
		// committedBlockMu protects processCommittedBlocksInOrder from concurrent execution
		// by sendBlocksToCoordinator and processStatusBatch goroutines.
		committedBlockMu sync.Mutex
	}

	relayRunConfig struct {
		coordClient                    servicepb.CoordinatorClient
		nextExpectedBlockByCoordinator uint64
		incomingBlockToBeCommitted     <-chan *common.Block
		outgoingCommittedBlock         chan<- *common.Block
		outgoingStatusUpdates          chan<- []*committerpb.TxStatus
		outgoingConfigBlocks           chan<- *common.Block
		outgoingCommittedBlockWithTxs  chan<- *committedBlockWithTxs
		waitingTxsLimit                int
	}
)

func newRelay(
	lastCommittedBlockSetInterval time.Duration,
	metrics *perfMetrics,
) *relay {
	logger.Info("Initializing new relay")
	return &relay{
		lastCommittedBlockSetInterval: lastCommittedBlockSetInterval,
		metrics:                       metrics,
	}
}

// run starts the relay service. The call to run blocks until an error occurs or the context is canceled.
func (r *relay) run(ctx context.Context, config *relayRunConfig) error { //nolint:contextcheck // false positive
	r.nextBlockNumberToBeCommitted.Store(config.nextExpectedBlockByCoordinator)
	r.incomingBlockToBeCommitted = config.incomingBlockToBeCommitted
	r.outgoingCommittedBlock = config.outgoingCommittedBlock
	r.outgoingStatusUpdates = config.outgoingStatusUpdates
	r.outgoingConfigBlocks = config.outgoingConfigBlocks
	r.outgoingCommittedBlockWithTxs = config.outgoingCommittedBlockWithTxs
	r.blkNumToBlkWithStatus.Clear()
	r.txIDToHeight.Clear()
	r.waitingTxsSlots = utils.NewSlots(int64(config.waitingTxsLimit))

	// Using the errgroup context for the stream ensures that we cancel the stream once one of the tasks fails.
	// And we use the stream's context to ensure that if the stream is closed, we stop all the tasks.
	// Finally, we use `rCtx` to ensure that even if all tasks stops without an error, the stream will be cancelled.
	rCtx, rCancel := context.WithCancel(ctx)
	defer rCancel()
	g, gCtx := errgroup.WithContext(rCtx)
	stream, err := config.coordClient.BlockProcessing(gCtx)
	if err != nil {
		return logAndWrapCoordinatorError(err, "failed to open stream for block processing")
	}
	sCtx := stream.Context()

	logger.Infof("Starting coordinator sender and receiver")

	expectedNextBlockToBeCommitted := r.nextBlockNumberToBeCommitted.Load()

	mappedBlockQueue := make(chan *blockMappingResult, cap(r.incomingBlockToBeCommitted))
	g.Go(func() error {
		return r.preProcessBlock(sCtx, mappedBlockQueue)
	})
	g.Go(func() error {
		return r.sendBlocksToCoordinator(sCtx, mappedBlockQueue, stream)
	})

	statusBatch := make(chan *committerpb.TxStatusBatch, cap(r.outgoingCommittedBlock))
	g.Go(func() error {
		return receiveStatusFromCoordinator(sCtx, stream, statusBatch)
	})
	g.Go(func() error {
		return r.processStatusBatch(sCtx, statusBatch)
	})

	g.Go(func() error {
		return r.setLastCommittedBlockNumber(sCtx, config.coordClient, expectedNextBlockToBeCommitted)
	})

	return utils.ProcessErr(g.Wait(), "stream with the coordinator has ended")
}

func (r *relay) preProcessBlock(
	ctx context.Context,
	mappedBlockQueue chan<- *blockMappingResult,
) error {
	incomingBlockToBeCommitted := channel.NewReader(ctx, r.incomingBlockToBeCommitted)
	queue := channel.NewWriter(ctx, mappedBlockQueue)

	done := context.AfterFunc(ctx, r.waitingTxsSlots.Broadcast)
	defer done()

	for ctx.Err() == nil {
		block, ok := incomingBlockToBeCommitted.Read()
		if !ok {
			break
		}
		// The delivery client guarantees a block with a header and in the correct order.
		logger.Debugf("Block %d arrived in the relay", block.Header.Number)

		start := time.Now()
		mappedBlock, err := mapBlock(block, &r.txIDToHeight)
		if err != nil {
			// This can never occur unless there is a bug in the relay.
			return err
		}
		promutil.Observe(r.metrics.blockMappingInRelaySeconds, time.Since(start))
		if err := r.submitMappedBlock(ctx, queue, block, mappedBlock); err != nil {
			return err
		}
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

func (r *relay) submitMappedBlock(
	ctx context.Context,
	queue channel.Writer[*blockMappingResult],
	block *common.Block,
	mappedBlock *blockMappingResult,
) error {
	if !mappedBlock.hasSnapshot && !mappedBlock.isConfig {
		// Common case: an ordinary user block with no submission barrier.
		r.queueMappedBlock(ctx, queue, mappedBlock)
		return nil
	}

	if mappedBlock.isConfig {
		return r.submitConfigBlock(ctx, queue, block, mappedBlock)
	}
	return r.submitSnapshotBlock(ctx, queue, mappedBlock)
}

// submitConfigBlock submits a config block as a submission barrier: drain all previously
// submitted transactions so the committer processes them before applying the config, forward
// the config block for application, submit it, then drain again so it is processed before any
// later data transaction is submitted.
func (r *relay) submitConfigBlock(
	ctx context.Context,
	queue channel.Writer[*blockMappingResult],
	block *common.Block,
	mappedBlock *blockMappingResult,
) error {
	if err := r.drain(ctx); err != nil {
		return err
	}
	channel.NewWriter(ctx, r.outgoingConfigBlocks).Write(block)
	r.queueMappedBlock(ctx, queue, mappedBlock)
	return r.drain(ctx)
}

// submitSnapshotBlock splits a snapshot block into segments and submits them in order. The
// single snapshot segment is a submission barrier: earlier regular transactions are drained
// before it, and its status is drained after it, before later transactions are submitted.
func (r *relay) submitSnapshotBlock(
	ctx context.Context,
	queue channel.Writer[*blockMappingResult],
	mappedBlock *blockMappingResult,
) error {
	for _, segment := range splitSnapshotMappedBlock(mappedBlock) {
		if segment.hasSnapshot {
			if err := r.drain(ctx); err != nil {
				return err
			}
		}

		r.queueMappedBlock(ctx, queue, segment)

		if segment.hasSnapshot {
			if err := r.drain(ctx); err != nil {
				return err
			}
		}
	}

	return nil
}

// drain blocks until all in-flight transactions have been processed by the committer.
// It returns a wrapped context error if the context is cancelled while waiting.
func (r *relay) drain(ctx context.Context) error {
	r.waitingTxsSlots.WaitTillEmpty(ctx)
	return errors.Wrap(ctx.Err(), "context ended")
}

func (r *relay) queueMappedBlock(
	ctx context.Context,
	queue channel.Writer[*blockMappingResult],
	mappedBlock *blockMappingResult,
) {
	txsCount := len(mappedBlock.block.Txs)
	promutil.AddToCounter(r.metrics.transactionInThroughput, txsCount)
	r.waitingTxsSlots.Acquire(ctx, int64(txsCount))
	promutil.AddToGauge(r.metrics.waitingTransactionsQueueSize, txsCount)
	queue.Write(mappedBlock)
}

func splitSnapshotMappedBlock(mappedBlock *blockMappingResult) []*blockMappingResult {
	if !mappedBlock.hasSnapshot {
		return []*blockMappingResult{mappedBlock}
	}

	// At most three segments: the regular TXs before the snapshot, the snapshot TX alone, and
	// the regular TXs after it. When the snapshot is the first or last TX (and there are no
	// rejected statuses to carry on the empty side), the empty leading/trailing segment is
	// skipped, leaving two segments — or a single snapshot-only segment when the block contains
	// nothing else.
	segments := make([]*blockMappingResult, 0, 3)
	snapshotIndex := mappedBlock.snapshotTxIndex

	// Rejected statuses have no tx body, so they are not part of any Txs slice. Partition them
	// by their original block position relative to the snapshot's original position: rejects
	// before the snapshot ride the pre-snapshot segment, rejects after it (e.g. a duplicate
	// snapshot) ride the post-snapshot segment. This keeps each stored reject's tx_status
	// commit on the correct side of the snapshot barrier, so a post-snapshot reject is not
	// committed into the state before the snapshot clone is taken.
	snapshotTxNum := mappedBlock.block.Txs[snapshotIndex].Ref.TxNum
	var rejectedBefore, rejectedAfter []*committerpb.TxStatus
	for _, rejected := range mappedBlock.block.Rejected {
		if rejected.Ref.TxNum < snapshotTxNum {
			rejectedBefore = append(rejectedBefore, rejected)
		} else {
			rejectedAfter = append(rejectedAfter, rejected)
		}
	}

	// Regular segment before the snapshot. Emit it when there are regular TXs or rejected
	// statuses that precede the snapshot.
	if snapshotIndex > 0 || len(rejectedBefore) > 0 {
		segments = append(segments, mappedBlockSegment(
			mappedBlock,
			mappedBlock.block.Txs[:snapshotIndex],
			rejectedBefore,
			false,
		))
	}

	// Snapshot one-TX segment allows waiting for the snapshot status before later TXs.
	segments = append(segments, mappedBlockSegment(
		mappedBlock,
		mappedBlock.block.Txs[snapshotIndex:snapshotIndex+1],
		nil,
		true,
	))

	// Regular segment after the snapshot, submitted once the snapshot drain completes.
	if snapshotIndex+1 < len(mappedBlock.block.Txs) || len(rejectedAfter) > 0 {
		segments = append(segments, mappedBlockSegment(
			mappedBlock,
			mappedBlock.block.Txs[snapshotIndex+1:],
			rejectedAfter,
			false,
		))
	}

	return segments
}

func mappedBlockSegment(
	mappedBlock *blockMappingResult,
	txs []*servicepb.TxWithRef,
	rejected []*committerpb.TxStatus,
	hasSnapshot bool,
) *blockMappingResult {
	// A segment is never a config block: config blocks return early in submitMappedBlock and
	// are never split, so isConfig is always false here.
	//
	// withStatus and txIDToHeight are shared (not copied) across all segments of the block: they
	// track whole-block state (per-TX statuses, pending count, and the relay-wide txID->height
	// map) keyed by the original block position, so every segment must point at the same
	// instances for status correlation and final block assembly to work. As a result the
	// segment's withStatus.txs may reference more TXs than this segment's CoordinatorBatch
	// carries — that is intentional: the batch is a per-segment slice while withStatus spans the
	// whole block.
	return &blockMappingResult{
		blockNumber: mappedBlock.blockNumber,
		block: &servicepb.CoordinatorBatch{
			Txs:      txs,
			Rejected: rejected,
		},
		withStatus:   mappedBlock.withStatus,
		hasSnapshot:  hasSnapshot,
		txIDToHeight: mappedBlock.txIDToHeight,
	}
}

func (r *relay) sendBlocksToCoordinator(
	ctx context.Context,
	mappedBlockQueue <-chan *blockMappingResult,
	stream servicepb.Coordinator_BlockProcessingClient,
) error {
	queue := channel.NewReader(ctx, mappedBlockQueue)
	outgoingCommittedBlock := channel.NewWriter(ctx, r.outgoingCommittedBlock)
	outgoingCommittedBlockWithTxs := channel.NewWriter(ctx, r.outgoingCommittedBlockWithTxs)

	for {
		mappedBlock, ok := queue.Read()
		if !ok {
			return errors.Wrap(ctx.Err(), "context ended")
		}

		startTime := time.Now()
		// A snapshot block is split into multiple segments that share the same block number and
		// the same whole-block withStatus. Register the block and count it as active only once,
		// on the first segment; later segments observe the existing entry. Note that this shared
		// withStatus tracks all TXs of the original block, so it may reference more txIDs than the
		// current segment's CoordinatorBatch (mappedBlock.block) sends to the coordinator — the
		// remaining txIDs are sent by the other segments of the same block. This is not new to the
		// split: withStatus is always registered here before stream.Send below, so even an
		// unsplit block transiently holds txIDs not yet submitted to the coordinator. An
		// alternative split shape — a single blockMappingResult carrying []CoordinatorBatch plus a
		// snapshotBatchIndex/snapshotTxIndex — would avoid multiple segments but needs extra index
		// bookkeeping, so we keep the simpler per-segment result here.
		if _, alreadyTracked := r.blkNumToBlkWithStatus.LoadOrStore(
			mappedBlock.blockNumber, mappedBlock.withStatus,
		); !alreadyTracked {
			r.activeBlocksCount.Add(1)
		}

		if mappedBlock.withStatus.pendingCount.Load() == 0 {
			r.processCommittedBlocksInOrder(ctx, outgoingCommittedBlock, outgoingCommittedBlockWithTxs)
		}

		if err := stream.Send(mappedBlock.block); err != nil {
			return errors.Wrap(err, "failed to send a block to the coordinator")
		}
		txsCount := len(mappedBlock.block.Txs)
		promutil.AddToCounter(r.metrics.transactionsSentTotal, txsCount)
		logger.Debugf("Sent SC block %d with %d TXs to Coordinator", mappedBlock.blockNumber, txsCount)
		promutil.Observe(r.metrics.mappedBlockProcessingInRelaySeconds, time.Since(startTime))
	}
}

func receiveStatusFromCoordinator(
	ctx context.Context,
	stream servicepb.Coordinator_BlockProcessingClient,
	statusBatch chan<- *committerpb.TxStatusBatch,
) error {
	txsStatus := channel.NewWriter(ctx, statusBatch)
	for {
		response, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "failed to receive statuses from the coordinator")
		}
		logger.Debugf("Received status batch (%d updates) from coordinator", len(response.GetStatus()))

		txsStatus.Write(response)
	}
}

func (r *relay) processStatusBatch(
	ctx context.Context,
	statusBatch <-chan *committerpb.TxStatusBatch,
) error {
	txsStatus := channel.NewReader(ctx, statusBatch)
	outgoingCommittedBlock := channel.NewWriter(ctx, r.outgoingCommittedBlock)
	outgoingCommittedBlockWithTxs := channel.NewWriter(ctx, r.outgoingCommittedBlockWithTxs)
	outgoingStatusUpdates := channel.NewWriter(ctx, r.outgoingStatusUpdates)
	for {
		tStatus, readOK := txsStatus.Read()
		if !readOK {
			return errors.Wrap(ctx.Err(), "context ended")
		}

		txStatusProcessedCount := int64(0)
		startTime := time.Now()
		statusReport := make([]*committerpb.TxStatus, 0, len(tStatus.Status))
		for _, txStatus := range tStatus.Status {
			// We cannot use LoadAndDelete(txID) because it may not match the received statues.
			height, ok := r.txIDToHeight.Load(txStatus.Ref.TxId)
			if !ok || txStatus.Ref.BlockNum != height.BlockNum {
				// - Case 1: Block not found.
				//   Consider a scenario where the connection between the sidecar and the coordinator fails due
				//   to a network issue—not because the coordinator restarts. Assume the relay has already submitted
				//   a block to the coordinator before the connection issue occurs.
				//   When the connection is re-established and execution resumes, we will receive the statuses of
				//   transactions submitted before the connectivity issue. However, the relay will no longer track
				//   these transactions. This is because when the connection fails, the relay returns control to
				//   the sidecar, which then fetches statuses directly using the gRPC API to recover the block store
				//   once the connection is re-established. Consequently, the relay will send transactions to the
				//   coordinator starting from the next block only.
				//   This side effect can be fixed if we couple the signature verifier manager and
				//   validator-committer-manager goroutines in the coordinator with the stream between the sidecar
				//   and the coordinator. Thus, we can create input-output channels within the coordinator at the
				//   stream level to avoid this behavior. However, implementing this solution is significantly
				//   more complex; hence, we have opted for this simpler approach.
				// - Case 2: Block not match.
				//   Assume the same scenario described above. The only difference is that we find the newly
				//   enqueued txID is a duplicate of a previously submitted txID. In such a case, the block
				//   number in the txStatus does not match the block number being tracked by the relay for
				//   the same txID.
				continue
			}

			blkWithStatus, blkOK := r.blkNumToBlkWithStatus.Load(txStatus.Ref.BlockNum)
			if !blkOK {
				// This can never occur unless there is a bug in the relay.
				return errors.Newf("block %d has never been submitted", txStatus.Ref.BlockNum)
			}
			err := blkWithStatus.setFinalStatus(height.TxNum, txStatus.Status)
			if err != nil {
				// This can never occur unless there is a bug in the relay or the coordinator.
				return err
			}
			r.txIDToHeight.Delete(txStatus.Ref.TxId)
			txStatusProcessedCount++

			statusReport = append(statusReport, txStatus)
		}

		if len(statusReport) > 0 {
			outgoingStatusUpdates.Write(statusReport)
		}

		promutil.AddToCounter(r.metrics.transactionOutThroughput, int(txStatusProcessedCount))
		r.waitingTxsSlots.Release(txStatusProcessedCount)
		promutil.AddToGauge(r.metrics.waitingTransactionsQueueSize, -int(txStatusProcessedCount))
		r.processCommittedBlocksInOrder(ctx, outgoingCommittedBlock, outgoingCommittedBlockWithTxs)
		promutil.Observe(r.metrics.transactionStatusesProcessingInRelaySeconds, time.Since(startTime))
	}
}

func (r *relay) processCommittedBlocksInOrder(
	ctx context.Context,
	outgoingCommittedBlock channel.Writer[*common.Block],
	outgoingCommittedBlockWithTxs channel.Writer[*committedBlockWithTxs],
) {
	r.committedBlockMu.Lock()
	defer r.committedBlockMu.Unlock()

	for ctx.Err() == nil {
		nextBlockNumberToBeCommitted := r.nextBlockNumberToBeCommitted.Load()
		blkWithStatus, exists := r.blkNumToBlkWithStatus.Load(nextBlockNumberToBeCommitted)
		if !exists {
			logger.Debugf("Next block [%d] to be committed is not in progress", nextBlockNumberToBeCommitted)
			return
		}
		if blkWithStatus.pendingCount.Load() > 0 {
			return
		}
		logger.Debugf("Next block [%d] has been committed", nextBlockNumberToBeCommitted)

		r.blkNumToBlkWithStatus.Delete(nextBlockNumberToBeCommitted)
		r.nextBlockNumberToBeCommitted.Add(1)
		r.activeBlocksCount.Add(-1)

		statusCount := utils.CountAppearances(blkWithStatus.txStatus)
		for status, count := range statusCount {
			promutil.AddToCounter(r.metrics.transactionsStatusReceivedTotal.WithLabelValues(
				status.String(),
			), count)
		}

		blkWithStatus.setStatusMetadataInBlock()
		outgoingCommittedBlock.Write(blkWithStatus.block)

		// Create committedBlockWithTxs from blockWithStatus for notifier
		outgoingCommittedBlockWithTxs.Write(&committedBlockWithTxs{
			blockNumber: blkWithStatus.blockNumber,
			txs:         blkWithStatus.txs,
			statuses:    blkWithStatus.txStatus,
		})
	}
}

func (r *relay) setLastCommittedBlockNumber(
	ctx context.Context,
	client servicepb.CoordinatorClient,
	expectedNextBlockToBeCommitted uint64,
) error {
	for {
		// NOTE: We are not strictly committing each committed block
		//       number immediately and also not in sequence.
		//       Instead, there is an implicit batching of block number.
		//       Even if the last committed block number
		//       set in the committer is different from the actual last committed
		//       block number, we have adequate recovery mechanism to detect
		//       them and recover correctly after a failure.

		select {
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "context ended")
		case <-time.After(r.lastCommittedBlockSetInterval):
		}

		if r.nextBlockNumberToBeCommitted.Load() == expectedNextBlockToBeCommitted {
			continue
		}

		blkNum := r.nextBlockNumberToBeCommitted.Load() - 1
		logger.Debugf("Setting the last committed block number: %d", blkNum)
		_, err := client.SetLastCommittedBlockNumber(ctx, &servicepb.BlockRef{Number: blkNum})
		if err != nil {
			return logAndWrapCoordinatorError(err,
				fmt.Sprintf("failed to set last committed block number [%d]", blkNum))
		}
		expectedNextBlockToBeCommitted = blkNum + 1
	}
}

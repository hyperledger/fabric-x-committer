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
	"github.com/hyperledger/fabric-x-committer/utils/retry"
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
		// checkpointHoldRetryInterval is how long block intake pauses before the session
		// restarts to re-fetch a held checkpoint block. See processCheckpointFeedback.
		checkpointHoldRetryInterval time.Duration
		// checkpointHeld records that a checkpoint was held, so the gate gauge can be
		// returned to "running" once a later batch arrives with no feedback. It survives the
		// session restarts a hold causes, because the relay outlives them.
		checkpointHeld atomic.Bool
		// checkpointHolds counts consecutive holds so the log can say which attempt this is. A
		// hold has no attempt limit (see processCheckpointFeedback), so the count is the only
		// thing that distinguishes "held once" from "held for an hour".
		checkpointHolds atomic.Uint64
		waitingTxsSlots *utils.Slots
		metrics         *perfMetrics
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
		// mappedBlockQueue and statusBatch connect the relay's own stages. They live for one
		// coordinator session, like incomingBlockToBeCommitted, so the session creates them and
		// reports their sizes.
		mappedBlockQueue chan *blockMappingResult
		statusBatch      chan *servicepb.TxStatusBatch
		waitingTxsLimit  int
	}
)

// checkpointGateState is the value published on the checkpointFeedbackState gauge, so an
// operator can tell a stalled committer's reason from metrics alone. Running is the
// ordinary state and says nothing about checkpoints; the other two are set only by a
// checkpoint verdict.
type checkpointGateState int

const (
	checkpointGateRunning checkpointGateState = 0
	checkpointGateHeld    checkpointGateState = 1
	checkpointGateHalted  checkpointGateState = 2
)

func newRelay(
	lastCommittedBlockSetInterval, checkpointHoldRetryInterval time.Duration,
	metrics *perfMetrics,
) *relay {
	logger.Info("Initializing new relay")
	return &relay{
		lastCommittedBlockSetInterval: lastCommittedBlockSetInterval,
		checkpointHoldRetryInterval:   checkpointHoldRetryInterval,
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
	// The gauge tracks the slots, so it is reset with them. A session that ends with TXs
	// in flight never sees their statuses, so their increments are never subtracted; without
	// this the gauge would drift up by that count on every reconnect and never come back.
	promutil.SetGauge(r.metrics.waitingTransactionsQueueSize, 0)

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

	g.Go(func() error {
		return r.preProcessBlock(sCtx, config.mappedBlockQueue)
	})
	g.Go(func() error {
		return r.sendBlocksToCoordinator(sCtx, config.mappedBlockQueue, stream)
	})

	g.Go(func() error {
		return receiveStatusFromCoordinator(sCtx, stream, config.statusBatch)
	})
	g.Go(func() error {
		return r.processStatusBatch(sCtx, config.statusBatch)
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
			// A config TX that cannot be processed ends the relay, so the sidecar restarts its
			// block feed and fetches the block again (see unprocessableConfigTx). Any other
			// error can never occur unless there is a bug in the relay.
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
	switch {
	case mappedBlock.isConfig:
		return r.submitConfigBlock(ctx, queue, block, mappedBlock)
	case mappedBlock.snapshotTx != nil:
		return r.submitSnapshotBlock(ctx, queue, mappedBlock)
	default: // Common case: an ordinary user block with no submission barrier.
		r.queueMappedBlock(ctx, queue, mappedBlock)
		return nil
	}
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
	if len(mappedBlock.block.Txs) > 0 || len(mappedBlock.block.Rejected) > 0 {
		r.queueMappedBlock(ctx, queue, &blockMappingResult{
			blockNumber: mappedBlock.blockNumber,
			block: &servicepb.CoordinatorBatch{
				Txs:      mappedBlock.block.Txs,
				Rejected: mappedBlock.block.Rejected,
			},
			withStatus: mappedBlock.withStatus,
		})
	}

	if err := r.drain(ctx); err != nil {
		return err
	}
	r.queueMappedBlock(ctx, queue, &blockMappingResult{
		blockNumber: mappedBlock.blockNumber,
		block: &servicepb.CoordinatorBatch{
			Txs: []*servicepb.TxWithRef{mappedBlock.snapshotTx},
		},
		withStatus: mappedBlock.withStatus,
	})
	return r.drain(ctx)
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
		// A snapshot block is split into two segments that share the same block number and
		// the same whole-block withStatus. Register the block and count it as active only once,
		// on the first segment; later segments observe the existing entry. Note that this shared
		// withStatus tracks all TXs of the original block, so it may reference more txIDs than the
		// current segment's CoordinatorBatch (mappedBlock.block) sends to the coordinator — the
		// remaining txIDs are sent by the other segments of the same block. This is not new to the
		// split: withStatus is always registered here before stream.Send below, so even an
		// unsplit block transiently holds txIDs not yet submitted to the coordinator.
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
	statusBatch chan<- *servicepb.TxStatusBatch,
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
	statusBatch <-chan *servicepb.TxStatusBatch,
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

		// A checkpoint that did not verify gates block intake. Handling it before the
		// per-TX statuses is deliberate: the held checkpoint has no status of its own, so
		// there is nothing in the loop below to react to.
		if tStatus.CheckpointFeedback != nil {
			if err := r.processCheckpointFeedback(ctx, tStatus.CheckpointFeedback); err != nil {
				return err
			}
		} else if r.checkpointHeld.CompareAndSwap(true, false) {
			// The pipeline moved past the checkpoint that was held, so the gate is no longer
			// the reason for anything. A halt is never cleared here: it is terminal, and this
			// loop does not run again after it.
			r.setCheckpointGate(checkpointGateRunning)
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

// processCheckpointFeedback gates the pipeline on the committer's checkpoint verdict.
// The caller skips an absent feedback, which is the normal case; an UNSPECIFIED signal is
// treated the same way and leaves the pipeline running.
//
// Both verdicts end the coordinator session by returning an error, which is what stops
// the block pull: the session's retry.Sustain loop (see sendBlocksAndReceiveStatus)
// re-recovers from the coordinator's next expected block, and because the held or halted
// checkpoint never committed, that block is the checkpoint's own. So the block is
// re-delivered and re-mapped from scratch rather than the relay having to unwind the
// per-block bookkeeping (pending count, waiting slots, tracked heights) by hand.
//
// HOLD sleeps first so the retry does not re-read a hash that is still being computed;
// the snapshot hasher publishes it on its own poll interval. The sleep runs in the status
// loop, so it stalls status processing as well as block intake: statuses for blocks that
// already committed are not drained for the length of the pause, and their blocks are not
// written to the block store until the session restarts. That is acceptable because the
// session is about to be torn down anyway and the coordinator re-delivers the statuses on
// reconnect, but it does mean a hold pauses more than intake.
//
// HALT is terminal: it wraps ErrNonRetryable so Sustain stops the sidecar for an operator
// instead of retrying a divergence that cannot resolve itself.
//
// The gate gauge is left at "held" across the session restart, and is cleared by the
// caller only once a batch arrives with no feedback. Resetting it here, before returning,
// would drop it to "running" while the checkpoint is still unresolved, so a scrape between
// restarts would show a healthy committer that is in fact gated.
//
// A hold can repeat indefinitely, and deliberately has no attempt limit: giving up would
// mean either committing an unverified checkpoint or halting on a snapshot hasher that is
// merely slow. Repetition is instead made observable -- checkpointHoldsTotal counts every
// hold of the same checkpoint, so an operator can alert on a checkpoint that is not
// clearing rather than infer it from a gauge that only says "held".
func (r *relay) processCheckpointFeedback(
	ctx context.Context, feedback *servicepb.CheckpointFeedback,
) error {
	if feedback == nil || feedback.Signal == servicepb.CheckpointFeedback_SIGNAL_UNSPECIFIED {
		return nil
	}
	promutil.AddToCounter(r.metrics.checkpointFeedbackTotal.WithLabelValues(feedback.Signal.String()), 1)
	txID := feedback.GetRef().GetTxId()

	if feedback.Signal == servicepb.CheckpointFeedback_HALT {
		r.setCheckpointGate(checkpointGateHalted)
		logger.Errorf("Halting block intake: checkpoint TX [%s] for snapshot block [%d] diverged: %s",
			txID, feedback.SnapshotBlockNumber, feedback.Reason)
		return errors.Wrapf(retry.ErrNonRetryable, "checkpoint TX %s for snapshot block %d diverged: %s",
			txID, feedback.SnapshotBlockNumber, feedback.Reason)
	}

	r.checkpointHeld.Store(true)
	r.setCheckpointGate(checkpointGateHeld)
	holds := r.checkpointHolds.Add(1)
	promutil.AddToCounter(r.metrics.checkpointHoldsTotal, 1)
	logger.Warnf("Pausing block intake for %s (hold %d): checkpoint TX [%s] for snapshot block [%d] "+
		"awaits the local snapshot hash", r.checkpointHoldRetryInterval, holds, txID, feedback.SnapshotBlockNumber)

	start := time.Now()
	select {
	case <-ctx.Done():
		return errors.Wrap(ctx.Err(), "context ended while holding a checkpoint")
	case <-time.After(r.checkpointHoldRetryInterval):
	}
	promutil.AddDurationToCounter(r.metrics.blockPullPausedSecondsTotal, time.Since(start))

	return errors.Wrapf(retry.ErrBackOff,
		"checkpoint TX %s for snapshot block %d is held until the local snapshot hash is computed",
		txID, feedback.SnapshotBlockNumber)
}

// setCheckpointGate publishes the gate state, and resets the hold count when the gate
// returns to running so the next hold's count starts from the checkpoint that caused it.
func (r *relay) setCheckpointGate(state checkpointGateState) {
	if state == checkpointGateRunning {
		r.checkpointHolds.Store(0)
	}
	promutil.SetGauge(r.metrics.checkpointFeedbackState, int(state))
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

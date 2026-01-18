/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
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
		incomingBlockToBeCommitted <-chan *common.Block
		outgoingCommittedBlock     chan<- *common.Block
		outgoingStatusUpdates      chan<- []*committerpb.TxStatus

		// nextBlockNumberToBeReceived denotes the next block number that the sidecar
		// expects to receive from the orderer. This value is initially extracted from the last committed
		// block number in the ledger, then incremented.
		// The sidecar queries the coordinator for this value before starting to pull blocks from the ordering service.
		nextBlockNumberToBeReceived atomic.Uint64
		// nextBlockNumberToBeCommitted denotes the next block number of to be committed.
		nextBlockNumberToBeCommitted atomic.Uint64

		activeBlocksCount             atomic.Int32
		blkNumToBlkWithStatus         utils.SyncMap[uint64, *blockWithStatus]
		txIDToHeight                  utils.SyncMap[string, servicepb.Height]
		lastCommittedBlockSetInterval time.Duration
		waitingTxsSlots               *utils.Slots
		metrics                       *perfMetrics
	}

	relayRunConfig struct {
		coordClient                    servicepb.CoordinatorClient
		nextExpectedBlockByCoordinator uint64
		configUpdater                  func(*common.Block)
		incomingBlockToBeCommitted     <-chan *common.Block
		outgoingCommittedBlock         chan<- *common.Block
		outgoingStatusUpdates          chan<- []*committerpb.TxStatus
		waitingTxsLimit                int
	}
)

const defaultLastCommittedBlockSetInterval = 5 * time.Second

func newRelay(
	lastCommittedBlockSetInterval time.Duration,
	metrics *perfMetrics,
) *relay {
	logger.Info("Initializing new relay")
	if lastCommittedBlockSetInterval == 0 {
		lastCommittedBlockSetInterval = defaultLastCommittedBlockSetInterval
	}
	return &relay{
		lastCommittedBlockSetInterval: lastCommittedBlockSetInterval,
		metrics:                       metrics,
	}
}

// run starts the relay service. The call to run blocks until an error occurs or the context is canceled.
func (r *relay) run(ctx context.Context, config *relayRunConfig) error { //nolint:contextcheck // false positive
	r.nextBlockNumberToBeReceived.Store(config.nextExpectedBlockByCoordinator)
	r.nextBlockNumberToBeCommitted.Store(config.nextExpectedBlockByCoordinator)
	r.incomingBlockToBeCommitted = config.incomingBlockToBeCommitted
	r.outgoingCommittedBlock = config.outgoingCommittedBlock
	r.outgoingStatusUpdates = config.outgoingStatusUpdates
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
		return r.preProcessBlock(sCtx, mappedBlockQueue, config.configUpdater)
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
	configUpdater func(*common.Block),
) error {
	incomingBlockToBeCommitted := channel.NewReader(ctx, r.incomingBlockToBeCommitted)
	queue := channel.NewWriter(ctx, mappedBlockQueue)

	done := context.AfterFunc(ctx, r.waitingTxsSlots.Broadcast)
	defer done()

	for {
		block, ok := incomingBlockToBeCommitted.Read()
		if !ok {
			return errors.Wrap(ctx.Err(), "context ended")
		}
		if block.Header == nil {
			logger.Warn("Received a block without header")
			continue
		}
		logger.Debugf("Block %d arrived in the relay", block.Header.Number)

		blockNum := block.Header.Number
		swapped := r.nextBlockNumberToBeReceived.CompareAndSwap(blockNum, blockNum+1)
		if !swapped {
			errMsg := fmt.Sprintf(
				"sidecar expects block [%d] but received block [%d]",
				r.nextBlockNumberToBeReceived.Load(),
				blockNum,
			)
			logger.Error(errMsg)
			return errors.New(errMsg)
		}

		start := time.Now()
		mappedBlock, err := mapBlock(block, &r.txIDToHeight)
		if err != nil {
			// This can never occur unless there is a bug in the relay.
			return err
		}
		promutil.Observe(r.metrics.blockMappingInRelaySeconds, time.Since(start))
		if mappedBlock.isConfig {
			configUpdater(block)
			// We wait for all previously submitted transactions to be processed by
			// the committer before submitting the config block.
			r.waitingTxsSlots.WaitTillEmpty(ctx)
		}

		txsCount := len(mappedBlock.block.Txs)
		r.waitingTxsSlots.Acquire(ctx, int64(txsCount))
		promutil.AddToGauge(r.metrics.waitingTransactionsQueueSize, txsCount)
		queue.Write(mappedBlock)

		if mappedBlock.isConfig {
			// we wait for the config block to be processed by the committer
			// before submitting any other data transactions.
			r.waitingTxsSlots.WaitTillEmpty(ctx)
		}
	}
}

func (r *relay) sendBlocksToCoordinator(
	ctx context.Context,
	mappedBlockQueue <-chan *blockMappingResult,
	stream servicepb.Coordinator_BlockProcessingClient,
) error {
	queue := channel.NewReader(ctx, mappedBlockQueue)
	outgoingCommittedBlock := channel.NewWriter(ctx, r.outgoingCommittedBlock)

	for {
		mappedBlock, ok := queue.Read()
		if !ok {
			return errors.Wrap(ctx.Err(), "context ended")
		}

		startTime := time.Now()
		r.blkNumToBlkWithStatus.Store(mappedBlock.blockNumber, mappedBlock.withStatus)
		r.activeBlocksCount.Add(1)

		if mappedBlock.withStatus.pendingCount == 0 {
			r.processCommittedBlocksInOrder(ctx, outgoingCommittedBlock)
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

		r.waitingTxsSlots.Release(txStatusProcessedCount)
		promutil.AddToGauge(r.metrics.waitingTransactionsQueueSize, -int(txStatusProcessedCount))
		r.processCommittedBlocksInOrder(ctx, outgoingCommittedBlock)
		promutil.Observe(r.metrics.transactionStatusesProcessingInRelaySeconds, time.Since(startTime))
	}
}

func (r *relay) processCommittedBlocksInOrder(
	ctx context.Context,
	outgoingCommittedBlock channel.Writer[*common.Block],
) {
	for ctx.Err() == nil {
		nextBlockNumberToBeCommitted := r.nextBlockNumberToBeCommitted.Load()
		blkWithStatus, exists := r.blkNumToBlkWithStatus.Load(nextBlockNumberToBeCommitted)
		if !exists {
			logger.Debugf("Next block [%d] to be committed is not in progress", nextBlockNumberToBeCommitted)
			return
		}
		if blkWithStatus.pendingCount > 0 {
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

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"context"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/api/types"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

type (
	// ftDelivery supports fault tolerance delivery.
	ftDelivery struct {
		params           Parameters
		jointOutputBlock chan *deliver.BlockWithSourceID

		// dataStream and headerOnlyStream holds the processing state of each stream respectively.
		// They progress in parallel independently.
		// curDataBlockSourceID is the ID of the orderer currently used as the data block source.
		dataStream           blockVerificationStateMachine
		headerOnlyStream     blockVerificationStateMachine
		curDataBlockSourceID uint32

		// latestConfig holds the latest known config. It is used to have the most
		// up-to-date channel ID, orderer endpoints, and credentials when reconnecting.
		// For verification, each blockVerificationStateMachine holds its own configState.
		latestConfig configState

		// Block withholding detection fields work together to identify when the data stream source
		// may be delaying block delivery:
		//
		// targetNextBlockNum is the block number we expect the data stream to deliver for withholding detection.
		// When the data stream reaches this block, a new target is set based on the header-only stream's progress.
		//
		// targetArrivalTime is the deadline by which targetNextBlockNum should arrive from the data stream.
		// If the block doesn't arrive by this time and nextAllowedSuspicion has passed, a block withholding
		// suspicion is raised and the data block source is switched to a different orderer.
		monitorBlockWithholding bool
		targetNextBlockNum      uint64
		targetArrivalTime       time.Time
	}

	// streamExecutionParams holds all parameters needed to start a single delivery stream worker.
	// These parameters are computed before spawning concurrent goroutines to prevent data races
	// when accessing shared state between startSingleDeliveryStream() and processBlocks().
	streamExecutionParams struct {
		deliver.Parameters
		connMaterial *connection.ClientMaterial
	}
)

// The following errors are used to communicate the stream restart reason and how to proceed.
var (
	errRequireRestart = errors.New("stream restart required")
	errConfigUpdate   = errors.Wrap(errRequireRestart, "config block received")
	errSuspicion      = errors.Wrap(errRequireRestart, "block withholding suspicion")
	errDataBlockError = errors.Wrap(retry.ErrBackOff, "data block error")
)

var logger = flogging.MustGetLogger("deliverorderer")

// ToQueue connects to an orderer delivery server, verifies blocks, and delivers them to a queue (go channel).
// It returns when an error occurs or when the context is done.
// It will attempt to reconnect on errors.
func ToQueue(ctx context.Context, odp Parameters) (*SessionInfo, error) {
	d, err := newFTDelivery(odp)
	if err != nil {
		return nil, err
	}

	err = retry.Sustain(ctx, d.params.Retry, func() error {
		// Initialize stream parameters before spawning goroutines to prevent data races.
		// This ensures that startSingleDeliveryStream() doesn't read shared state (dataStream.nextBlockNum,
		// headerOnlyStream.nextBlockNum, curDataBlockSourceID) while processBlocks() is
		// concurrently modifying it via verificationStepAndUpdateState().
		dataWorker, headerWorkers := d.initStreams()

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return startSingleDeliveryStream(gCtx, dataWorker)
		})
		g.Go(func() error {
			return startHeadersStreams(gCtx, headerWorkers)
		})
		g.Go(func() error {
			return d.processBlocks(gCtx)
		})
		return g.Wait()
	})

	return &SessionInfo{
		LastBlock:                   d.dataStream.lastBlock,
		NextBlockVerificationConfig: d.dataStream.ConfigBlock,
		LatestKnownConfig:           d.latestConfig.ConfigBlock,
	}, err
}

func newFTDelivery(odp Parameters) (*ftDelivery, error) {
	if odp.SuspicionGracePeriodPerBlock == 0 {
		odp.SuspicionGracePeriodPerBlock = DefaultSuspicionGracePeriodPerBlock
	}

	ftLevel, ftErr := ordererdial.GetFaultToleranceLevel(odp.FaultToleranceLevel)
	if ftErr != nil {
		return nil, ftErr
	}

	// We use the maximum between all the provided config blocks.
	// This ensures we won't miss a crucial config-block that updated all the endpoints and/or the credentials.
	state, latestConfig, err := newBlockProcessingState(&SessionInfo{
		LastBlock:                   odp.LastBlock,
		NextBlockVerificationConfig: odp.NextBlockVerificationConfig,
		LatestKnownConfig:           MaxBlock(odp.LatestKnownConfig, odp.NextBlockVerificationConfig),
	})
	if err != nil {
		return nil, err
	}

	jointChannelCapacity := max(cap(odp.OutputBlock), cap(odp.OutputBlockWithSourceID))
	return &ftDelivery{
		params:                  odp,
		jointOutputBlock:        make(chan *deliver.BlockWithSourceID, jointChannelCapacity),
		monitorBlockWithholding: ftLevel == ordererdial.BFT,
		headerOnlyStream:        state,
		dataStream:              state.cloneAsDataBlockStream(),
		latestConfig:            latestConfig,
	}, nil
}

// MaxBlock returns the block with the highest block number between all the blocks in the input.
// Nil blocks are ignored. If all of them are nil, nil is returned.
func MaxBlock(blocks ...*common.Block) *common.Block {
	var ret *common.Block
	for _, b := range blocks {
		if ret == nil || (b != nil && b.Header.Number > ret.Header.Number) {
			ret = b
		}
	}
	return ret
}

func (d *ftDelivery) processBlocks(ctx context.Context) error {
	deliveryBlockOutput := channel.NewWriter(ctx, d.params.OutputBlock)
	deliveryBlockOutputWithSourceID := channel.NewWriter(ctx, d.params.OutputBlockWithSourceID)

	for ctx.Err() == nil {
		deliverBlock, err := d.processNextBlock(ctx)
		if deliverBlock != nil {
			// Write will be no-op if the output buffer is nil.
			deliveryBlockOutput.Write(deliverBlock.Block)
			deliveryBlockOutputWithSourceID.Write(deliverBlock)
		}
		if err != nil {
			return err
		}
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

func (d *ftDelivery) processNextBlock(ctx context.Context) (
	deliverBlock *deliver.BlockWithSourceID, err error,
) {
	blk := d.readNextBlock(ctx)
	if blk == nil {
		// Timeout occurred. This means there are no more blocks in the queue.
		// If we have no running delivery streams, the queue won't
		// be filled, and no progress can be made.
		// However, if we need to restart the streams, we might recover.
		return nil, d.checkBlockWithholding()
	}

	// We pick the processing state according to the block source (data or header only).
	state := d.getProcessingState(blk)

	// Verification step: a block is ignored if it failed verification.
	// Otherwise, we update the internal processing state and config update if necessary.
	verificationErr := state.verificationStepAndUpdateState(blk)
	if verificationErr != nil {
		// The underlying delivery client ensures consecutive blocks.
		// However, the join stream can have blocks out-of-ourder for two reasons.
		//   1. We switched the data stream source, but the queue had some previously unprocessed blocks.
		//      Thus, by the time the first block arrives from the new stream, the actual state might be
		//      ahead of requested next block.
		//   2. We aggregate multiple simultaneous header-only streams into a single channel.
		//      Thus, we get the same block multiple times from different sources.
		// In both cases, we process the first block that arrives in order, and skip the rest.
		// With that, we save processing time of duplicated blocks from different sources.
		// Since getting an unexpected block number from a source other than the last updater is a
		// correct behavior, we don't need to log it.
		if !errors.Is(verificationErr, ErrUnexpectedBlockNumber) || state.updaterSourceID == blk.SourceID {
			logger.Warnf("Block verification failed for source [%d] (data=%v): %v",
				blk.SourceID, state.dataBlockStream, verificationErr)
		}

		// An issue on the main data block stream, requires a restart
		// to ensure progress.
		if state.dataBlockStream {
			return nil, errors.Join(errDataBlockError, verificationErr)
		}
		// However, if the headers-only streams have issues, we don't restart the streams
		// as this can lead to a DOS attack by a malicious party.
		// Additionally, the headers-only stream can receive the same block multiple times,
		// which is a correct behavior.
		return nil, nil
	}

	// Deliver step: if it is a valid data block, deliver it.
	if state.dataBlockStream {
		deliverBlock = blk
	}

	// Next stream action step: if there was a config update or block withholding, restart the streams.
	return deliverBlock, d.checkNextStreamAction(state)
}

func (d *ftDelivery) readNextBlock(ctx context.Context) *deliver.BlockWithSourceID {
	var timeoutChan <-chan time.Time
	if d.monitorBlockWithholding && d.dataStream.nextBlockNum < d.targetNextBlockNum {
		// We wake up to catch the next block deadline.
		// If passed, timeout will be negative and we will return immediately.
		timeoutChan = time.After(time.Until(d.targetArrivalTime))
	}
	select {
	case blk := <-d.jointOutputBlock:
		return blk
	case <-ctx.Done():
		return nil
	case <-timeoutChan:
		return nil
	}
}

func (d *ftDelivery) getProcessingState(block *deliver.BlockWithSourceID) *blockVerificationStateMachine {
	switch block.SourceID {
	case d.curDataBlockSourceID:
		return &d.dataStream
	default:
		return &d.headerOnlyStream
	}
}

func (d *ftDelivery) checkNextStreamAction(state *blockVerificationStateMachine) error {
	if d.latestConfig.configBlockNumber < state.configBlockNumber {
		// If a newer config block appears, it may contain endpoints update.
		// So we restart the streams with the latest config.
		d.latestConfig = state.configState
		return errConfigUpdate
	}

	return d.checkBlockWithholding()
}

func (d *ftDelivery) checkBlockWithholding() error {
	// If the data source is ahead, or we raised suspicion recently, continue.
	if !d.monitorBlockWithholding || d.dataStream.nextBlockNum >= d.headerOnlyStream.nextBlockNum {
		return nil
	}

	// If we already passes the target, set the next target arrival time.
	if d.dataStream.nextBlockNum >= d.targetNextBlockNum {
		d.targetNextBlockNum = d.headerOnlyStream.nextBlockNum
		gap := min(d.targetNextBlockNum-d.dataStream.nextBlockNum, math.MaxInt64)
		//nolint:gosec // Capping the gap at [math.MaxInt64].
		d.targetArrivalTime = time.Now().Add(d.params.SuspicionGracePeriodPerBlock * time.Duration(gap))
		return nil
	}

	// If we haven't reached the target, but we still have time, continue.
	if time.Now().Before(d.targetArrivalTime) {
		return nil
	}

	logger.Warnf("Suspecting block withholding of block [%d] by party ID [%d]. "+
		"Party [%d] already received block [%d]. "+
		"Replacing data block source.",
		d.dataStream.nextBlockNum, d.curDataBlockSourceID, d.headerOnlyStream.updaterSourceID,
		d.headerOnlyStream.nextBlockNum-1)

	// Reset the target block (clears the suspicion).
	d.targetNextBlockNum = 0
	return errSuspicion
}

// initStreams calculates the execution parameters for all delivery stream workers.
// This method must be called before spawning concurrent goroutines to prevent data races.
// It reads the current state (dataStream.nextBlockNum, headerOnlyStream.nextBlockNum) and
// determines which orderer should be used as the data block source.
// Returns the data worker and header-only workers separately.
// CRITICAL: This also updates d.curDataBlockSourceID before goroutines spawn to prevent
// race conditions with getProcessingState() which reads this field.
func (d *ftDelivery) initStreams() (dataWorker streamExecutionParams, headerWorkers []streamExecutionParams) {
	m := ordererdial.NewClientMaterial(d.latestConfig.ConfigBlockMaterial, ordererdial.Parameters{
		API:   types.Deliver,
		TLS:   d.params.TLS,
		Retry: d.params.Retry,
	})

	// Update curDataBlockSourceID here, before spawning goroutines, to prevent race
	// with getProcessingState() which reads this field in the processBlocks() goroutine.
	d.curDataBlockSourceID = d.pickDataBlockStreamID(m)
	logger.Infof("Using party ID [%d] as the data block source", d.curDataBlockSourceID)

	dataSourceParameters := deliver.Parameters{
		ChannelID:               d.latestConfig.ChannelID,
		Signer:                  d.params.Signer,
		TLSCertHash:             d.params.TLSCertHash,
		OutputBlockWithSourceID: d.jointOutputBlock,
		NextBlockNum:            d.dataStream.nextBlockNum,
		HeaderOnly:              false,
		SourceID:                d.curDataBlockSourceID,
	}
	headerSourceParameters := dataSourceParameters
	headerSourceParameters.HeaderOnly = true
	headerSourceParameters.NextBlockNum = d.headerOnlyStream.nextBlockNum

	// If not monitoring block withholding or only one party, create single data worker.
	if !d.monitorBlockWithholding || len(m.PartyIDToMaterial) < 2 {
		return streamExecutionParams{
			Parameters:   dataSourceParameters,
			connMaterial: m.Joint,
		}, nil
	}

	// Create worker params for data stream + all header-only streams.
	dataWorker = streamExecutionParams{
		Parameters:   dataSourceParameters,
		connMaterial: m.PartyIDToMaterial[d.curDataBlockSourceID],
	}
	headerWorkers = make([]streamExecutionParams, 0, len(m.PartyIDToMaterial)-1)
	for id, connMaterial := range m.PartyIDToMaterial {
		if id == d.curDataBlockSourceID {
			continue
		}
		p := headerSourceParameters
		p.SourceID = id
		headerWorkers = append(headerWorkers, streamExecutionParams{
			Parameters:   p,
			connMaterial: connMaterial,
		})
	}
	return dataWorker, headerWorkers
}

// pickDataBlockStreamID returns one of the parties.
// It prefers the party that delivered the latest block.
func (d *ftDelivery) pickDataBlockStreamID(m *ordererdial.ClientMaterial) uint32 {
	if !d.monitorBlockWithholding || len(m.PartyIDToMaterial) < 2 {
		return 0
	}

	parties := slices.Collect(maps.Keys(m.PartyIDToMaterial))

	// We start the delivery with the most advanced source as the data block source.
	// If the headers only stream is ahead, and it uses the latest config block (as expected),
	// we use its latest updater.
	// However, if the last update was the config block, then the IDs might have changed, and we can no longer rely
	// on the last update ID.
	// We also assert that the last updater is indeed one of the parties to protect against possible bugs.
	if d.headerOnlyStream.nextBlockNum > d.dataStream.nextBlockNum &&
		d.latestConfig.configBlockNumber == d.headerOnlyStream.configBlockNumber &&
		d.headerOnlyStream.nextBlockNum-1 > d.headerOnlyStream.configBlockNumber &&
		slices.Contains(parties, d.headerOnlyStream.updaterSourceID) {
		return d.headerOnlyStream.updaterSourceID
	}

	return parties[utils.RandIntN(uint64(len(parties)))]
}

// startHeadersStreams spawns delivery stream workers based on the provided execution parameters.
// To avoid restarting the stream for each header stream failure, we only restart when all of them fail.
// This is a standalone function that doesn't access ftDelivery state, preventing data races.
// All necessary data is passed via the parameters.
func startHeadersStreams(ctx context.Context, headerWorkers []streamExecutionParams) error {
	allErrors := make([]error, len(headerWorkers))
	wg := sync.WaitGroup{}
	for i, worker := range headerWorkers {
		wg.Go(func() {
			allErrors[i] = startSingleDeliveryStream(ctx, worker)
		})
	}
	wg.Wait()
	return errors.Join(allErrors...)
}

// startSingleDeliveryStream starts a single delivery stream worker with the provided parameters.
// This is a standalone function that doesn't access ftDelivery state, preventing data races.
// All necessary data is passed via the params argument.
func startSingleDeliveryStream(ctx context.Context, params streamExecutionParams) error {
	workerType := "data"
	if params.HeaderOnly {
		workerType = "headers"
	}
	workerErr := errors.NewWithDepthf(0, "[%s] delivery worker [ID:%d] ended", workerType, params.SourceID)

	conn, connErr := params.connMaterial.NewLoadBalancedConnection()
	if connErr != nil {
		logger.Errorf("%s with error: %v", workerErr, connErr)
		return errors.Join(workerErr, connErr)
	}
	defer connection.CloseConnectionsLog(conn)

	// Initialize the Deliverer parameter using the established connection.
	params.Deliverer = &ordererDeliverer{client: orderer.NewAtomicBroadcastClient(conn)}
	err := deliver.ToQueue(ctx, params.Parameters)
	if err != nil && ctx.Err() == nil {
		// We only report error if the worker ended unexpectedly.
		logger.Errorf("%s with error: %v", workerErr, err)
	}
	return errors.Join(workerErr, err)
}

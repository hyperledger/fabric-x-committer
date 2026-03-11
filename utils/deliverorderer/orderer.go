/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"maps"
	"math"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/protoutil/identity"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// Parameters defines the configuration for fault-tolerant block delivery from Orderer organizations.
	//
	// Fault Tolerance Level:
	//   - BFT (Byzantine Fault Tolerance): Verifies blocks and monitors for block withholding attacks
	//   - CFT (Crash Fault Tolerance): Verifies blocks only
	//   - NO (No Fault Tolerance): No verification (not for production use)
	//   - Empty: Defaults to BFT (highest fault tolerance)
	//
	// Block Withholding Detection (BFT only):
	// The system uses parallel streams to detect malicious orderers withholding blocks:
	//   1. One stream receives full blocks (data stream)
	//   2. Multiple streams receive block headers only (header-only streams)
	//   3. If a header arrives but the full block doesn't arrive within BlockWithholdingTimeout,
	//      the system suspects the current data stream source and switches to another orderer
	//
	// Detection Parameters:
	//   - BlockWithholdingTimeout: Maximum time to wait for a full block after receiving its header
	//   - MaxBlocksAhead: Maximum number of blocks the header-only stream can be ahead of the data stream
	//     (prevents unbounded memory growth for deadline tracking)
	//   - SuspicionGracePeriod: Cooldown period after raising a suspicion before allowing another
	//   - LivenessCheckInterval: Frequency of checking that all delivery streams are alive
	//
	// Output Channels:
	//   - OutputBlock: Receives verified data blocks (without source information)
	//   - OutputBlockWithSourceID: Receives verified data blocks with source orderer ID
	//   - At least one output channel must be provided
	//
	// Session holds updetable session information for future runs.
	Parameters struct {
		FaultToleranceLevel     string
		TLS                     connection.TLSMaterials
		Retry                   *connection.RetryProfile
		Signer                  identity.SignerSerializer
		BlockWithholdingTimeout time.Duration
		SuspicionGracePeriod    time.Duration
		LivenessCheckInterval   time.Duration
		MaxBlocksAhead          uint64

		OutputBlock             chan<- *common.Block
		OutputBlockWithSourceID chan<- *deliver.BlockWithSourceID

		Session *SessionInfo
	}

	// The SessionInfo struct maintains state about processed blocks to support recovery and resumption:
	//   - LastBlock: The most recently processed block (do NOT pass the genesis block here to allow the delivery to
	//	   deliver it for processing)
	//   - NextBlockVerificationConfig: Config block used to verify the next incoming block
	//   - LastestKnownConfig: The newest known config block (can be ahead of NextBlockVerificationConfig)
	//
	// State Relationships:
	//   - If LastestKnownConfig is not provided, NextBlockVerificationConfig is used as the latest config
	//   - If NextBlockVerificationConfig is not provided, policy verification starts only when a config
	//     block is delivered (LastestKnownConfig is NOT used for verification as it may not be relevant)
	//   - LastestKnownConfig helps recover when delivery missed a config block that updated all endpoints
	//   - These fields are updated at the end of the delivery process for future runs
	SessionInfo struct {
		NextBlockVerificationConfig *common.Block
		LastestKnownConfig          *common.Block
		LastBlock                   *common.Block
	}

	// ftDelivery supports fault tolerance delivery.
	// It uses a single go-routine, thus, no locks are required.
	ftDelivery struct {
		params                  *Parameters
		monitorBlockWithholding bool
		jointOutputBlock        chan *deliver.BlockWithSourceID
		tlsCertHash             []byte

		// dataStream and headerOnlyStream holds the processing state of each stream respectively.
		// They progress in parallel independently.
		dataStream       blockVerificationStateMachine
		headerOnlyStream blockVerificationStateMachine

		// latestConfig holds the latest known config. It is used to have the most
		// up-to-date channel ID, orderer endpoints, and credentials when reconnecting.
		// For verification, each blockVerificationStateMachine holds its own configState.
		latestConfig configState

		// dataBlockDeadlineBuffer is a circular buffer indexed by (blockNum % MaxBlocksAhead).
		// It stores deadlines for blocks that have been received as headers but not yet as data.
		// The buffer size is bounded by MaxBlocksAhead to prevent unbounded memory growth.
		dataBlockDeadlineBuffer []time.Time

		curDataBlockSourceID uint32
		wg                   sync.WaitGroup
		cancelWorkers        context.CancelFunc
		activeWorkers        atomic.Int64
		restartBackoff       *backoff.ExponentialBackOff
		lastAction           streamAction
		nextAllowedSuspicion time.Time
	}

	// streamAction defines the action to take on the delivery streams after processing each block.
	streamAction uint
)

// Default parameters for orderer delivery. These are used when the parameters are not set by the user.
const (
	DefaultBlockWithholdingTimeout = time.Second
	DefaultLivenessCheckInterval   = time.Second
	DefaultSuspicionGracePeriod    = time.Second
	DefaultMaxBlocksAhead          = uint64(1_000)
)

const (
	streamActionNone streamAction = iota
	streamActionSuspicion
	streamActionDataBlockError
	streamActionConfigUpdate
	streamActionStalemate
)

var logger = flogging.MustGetLogger("deliverorderer")

// ToQueue connects to an orderer delivery server, verifies blocks, and delivers them to a queue (go channel).
// It returns when an error occurs or when the context is done.
// It will attempt to reconnect on errors.
func ToQueue(ctx context.Context, odp Parameters) error {
	d, err := newFTDelivery(&odp)
	if err != nil {
		return err
	}
	defer d.finalize()

	cCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	err = d.run(cCtx)

	// Update the session parameters for future runs.
	odp.Session.LastBlock = d.dataStream.prevBlock
	odp.Session.NextBlockVerificationConfig = d.dataStream.ConfigBlock
	odp.Session.LastestKnownConfig = d.latestConfig.ConfigBlock
	return err
}

func newFTDelivery(odp *Parameters) (*ftDelivery, error) {
	if odp.LivenessCheckInterval == 0 {
		odp.LivenessCheckInterval = DefaultLivenessCheckInterval
	}

	ftLevel, ftErr := ordererconn.GetFaultToleranceLevel(odp.FaultToleranceLevel)
	if ftErr != nil {
		return nil, ftErr
	}
	tlsCertHash := sha256.Sum256(odp.TLS.Cert)

	d := &ftDelivery{
		params:                  odp,
		tlsCertHash:             tlsCertHash[:],
		jointOutputBlock:        make(chan *deliver.BlockWithSourceID, cap(odp.OutputBlock)),
		monitorBlockWithholding: ftLevel == ordererconn.BFT,
		restartBackoff:          odp.Retry.NewBackoff(),
	}
	err := d.initializeStreamStates()
	if err != nil {
		return nil, err
	}
	if d.monitorBlockWithholding {
		if odp.BlockWithholdingTimeout == 0 {
			odp.BlockWithholdingTimeout = DefaultBlockWithholdingTimeout
		}
		if odp.SuspicionGracePeriod == 0 {
			odp.SuspicionGracePeriod = DefaultSuspicionGracePeriod
		}
		if odp.MaxBlocksAhead == 0 {
			odp.MaxBlocksAhead = DefaultMaxBlocksAhead
		}
		d.dataBlockDeadlineBuffer = make([]time.Time, odp.MaxBlocksAhead)
		d.nextAllowedSuspicion = time.Now().Add(odp.SuspicionGracePeriod)
	}
	return d, nil
}

func (d *ftDelivery) initializeStreamStates() error {
	s := newBlockProcessingState(false)

	session := d.params.Session

	// We initialize the processing state from the last block and start the following blocks processing from it.
	// We use a headers-only stream to allow providing data-less block as the previous block.
	// We process the last block before applying the config block to avoid verifying the last block.
	// This is because the last block might be signed by previous configuration.
	if session.LastBlock != nil && session.LastBlock.Header != nil {
		s.nextBlockNum = session.LastBlock.Header.Number
		err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{
			Block: session.LastBlock,
			// We use a large ID to ensure we do not confuse it with a real source.
			SourceID: math.MaxUint32,
		})
		if err != nil {
			return errors.WithMessage(err, "error loading last block")
		}
	}

	if session.NextBlockVerificationConfig != nil {
		err := s.updateIfConfigBlock(session.NextBlockVerificationConfig)
		if err != nil {
			return errors.WithMessage(err, "error loading next block verification config")
		}
	}
	if session.LastestKnownConfig != nil {
		err := d.latestConfig.updateIfConfigBlock(session.LastestKnownConfig)
		if err != nil {
			return errors.WithMessage(err, "error loading last known config")
		}
	}

	// We assert that the latest config is indeed the latest.
	// This is useful in cases that the latest config block was given by the user,
	// but the system have a newer config block in the ledger.
	if d.latestConfig.ConfigBlockMaterial == nil || s.configBlockNumber > d.latestConfig.configBlockNumber {
		d.latestConfig = s.configState
	}

	// We validate that the config block is ahead the next expected block to fail fast.
	if s.nextBlockNum < s.configBlockNumber {
		return errors.Newf("config block number [%d] is ahead of the next expected block [%d]",
			s.configBlockNumber, s.nextBlockNum)
	}

	// We start the data stream and the headers-only streams from the same block.
	d.headerOnlyStream = s
	d.dataStream = s
	d.dataStream.dataBlockStream = true
	return nil
}

func (d *ftDelivery) finalize() {
	if d.cancelWorkers != nil {
		// Cancel workers if any.
		d.cancelWorkers()
	}
	d.wg.Wait()
}

func (d *ftDelivery) run(ctx context.Context) error {
	err := d.updateDeliveryStreams(ctx, streamActionConfigUpdate)
	if err != nil {
		return err
	}
	deliveryBlockOutput := channel.NewWriter(ctx, d.params.OutputBlock)
	deliveryBlockOutputWithSourceID := channel.NewWriter(ctx, d.params.OutputBlockWithSourceID)

	for ctx.Err() == nil {
		deliverBlock, action := d.processNextBlock(ctx)
		if deliverBlock != nil {
			// Write will be no-op if the output buffer is nil.
			deliveryBlockOutput.Write(deliverBlock.Block)
			deliveryBlockOutputWithSourceID.Write(deliverBlock)
		}
		if action == streamActionStalemate {
			return errors.New("delivery sources are not available")
		}
		err = d.updateDeliveryStreams(ctx, action)
		if err != nil {
			return err
		}
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

func (d *ftDelivery) processNextBlock(ctx context.Context) (
	deliverBlock *deliver.BlockWithSourceID, action streamAction,
) {
	blk := d.readNextBlock(ctx)
	if blk == nil { // Timeout occurred.
		action = d.checkBlockWithholding()
		// Timeout means there are no more blocks in the queue.
		// If we have no running delivery streams, the queue won't
		// be filled, and no progress can be made.
		// However, if we need to restart the streams, we might recover.
		if action == streamActionNone && d.activeWorkers.Load() == 0 {
			action = streamActionStalemate
		}
		return nil, action
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
			return nil, streamActionDataBlockError
		}
		// However, if the headers-only streams have issues, we don't restart the streams
		// as this can lead to a DOS attack by a malicious party.
		// Additionally, the headers-only stream can receive the same block multiple times,
		// which is a correct behavior.
		return nil, streamActionNone
	}

	// Deliver step: if it is a valid data block, deliver it.
	if state.dataBlockStream {
		deliverBlock = blk
	}

	// If a new header was received, we add the deadline for the block.
	if d.monitorBlockWithholding && !state.dataBlockStream {
		d.addDataBlockDeadline(blk.Block.Header.Number)
	}

	// Next stream action step: if there was a config update or block withholding, restart the streams.
	return deliverBlock, d.checkNextStreamAction(state)
}

func (d *ftDelivery) readNextBlock(ctx context.Context) *deliver.BlockWithSourceID {
	timeout := d.params.LivenessCheckInterval
	if d.monitorBlockWithholding {
		// We wake up to catch the next block deadline when the next suspicion event is permitted.
		// If it is already permitted, then we wake up on the next deadline.
		// If we already passed the next deadline, we return nil to indicate timeout.
		suspicionTimeout := time.Until(d.nextAllowedSuspicion)
		blockTimeout := d.getNextDataBlockTimeoutFromNow()
		switch {
		case suspicionTimeout > 0:
			timeout = min(timeout, suspicionTimeout)
		case blockTimeout > 0:
			timeout = min(timeout, blockTimeout)
		default:
			return nil
		}
	}
	select {
	case blk := <-d.jointOutputBlock:
		return blk
	case <-ctx.Done():
		return nil
	case <-time.After(timeout):
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

func (d *ftDelivery) checkNextStreamAction(state *blockVerificationStateMachine) streamAction {
	if d.latestConfig.configBlockNumber < state.configBlockNumber {
		// If a newer config block appears, it may contain endpoints update.
		// So we restart the streams with the latest config.
		d.latestConfig = state.configState
		return streamActionConfigUpdate
	}

	return d.checkBlockWithholding()
}

func (d *ftDelivery) checkBlockWithholding() streamAction {
	// If we raised suspicion recently, we have to wait.
	if !d.monitorBlockWithholding || time.Now().Before(d.nextAllowedSuspicion) {
		return streamActionNone
	}

	// Check if we have a deadline for the next data block.
	if d.getNextDataBlockTimeoutFromNow() > 0 {
		// We still have time.
		return streamActionNone
	}

	logger.Warnf("Suspecting block withholding of block [%d] by party ID [%d]. "+
		"Party [%d] already received block [%d]. "+
		"Replacing data block source.",
		d.dataStream.nextBlockNum, d.curDataBlockSourceID, d.headerOnlyStream.updaterSourceID,
		d.headerOnlyStream.nextBlockNum-1)

	// Set the next allowed suspicion.
	d.nextAllowedSuspicion = time.Now().Add(d.params.SuspicionGracePeriod)
	return streamActionSuspicion
}

func (d *ftDelivery) getNextDataBlockTimeoutFromNow() time.Duration {
	if d.dataBlockDeadlineBuffer == nil {
		return math.MaxInt64
	}
	if d.dataStream.nextBlockNum >= d.headerOnlyStream.nextBlockNum {
		return math.MaxInt64
	}
	gap := d.headerOnlyStream.nextBlockNum - d.dataStream.nextBlockNum
	if gap < d.params.MaxBlocksAhead {
		return time.Until(d.dataBlockDeadlineBuffer[d.dataStream.nextBlockNum%d.params.MaxBlocksAhead])
	}
	// If the headers-only stream is too far ahead of the data stream, the deadline is now.
	return 0
}

func (d *ftDelivery) addDataBlockDeadline(blockNum uint64) {
	if d.dataBlockDeadlineBuffer == nil {
		return
	}
	deadline := time.Now().Add(d.params.BlockWithholdingTimeout)
	// If the headers stream is too much ahead, the following operation might overwrite
	// deadlines that are still relevant to the data stream.
	// This is OK since for data blocks that are too far behind, we don't care about the deadline,
	// and we raise suspicion automatically.
	d.dataBlockDeadlineBuffer[blockNum%d.params.MaxBlocksAhead] = deadline
}

func (d *ftDelivery) updateDeliveryStreams(ctx context.Context, action streamAction) error {
	if action == streamActionNone && d.cancelWorkers != nil {
		d.lastAction = action
		return nil
	}

	// In case of consecutive data block errors, we back off before retrying.
	if action == streamActionDataBlockError && d.lastAction == streamActionDataBlockError {
		err := connection.WaitForNextBackOffDuration(ctx, d.restartBackoff)
		if err != nil {
			return err
		}
	} else {
		d.restartBackoff.Reset()
	}
	d.lastAction = action

	logger.Infof("Staring delivery streams...")

	// Cancel workers if any. We don't wait for them to finish.
	// It may cause duplicated blocks in the stream.
	// But it is OK since, we only verify the first received instance of each block.
	if d.cancelWorkers != nil {
		d.cancelWorkers()
	}

	m := d.latestConfig.OrdererConnectionMaterial(ordererconn.MaterialParameters{
		API:   types.Deliver,
		TLS:   d.params.TLS,
		Retry: d.params.Retry,
	})

	dCtx, cancel := context.WithCancel(ctx)
	d.cancelWorkers = cancel
	if !d.monitorBlockWithholding || len(m.PerID) < 2 {
		d.curDataBlockSourceID = 0
		d.startSingleDeliveryStream(dCtx, m.Joint, d.curDataBlockSourceID)
		return nil
	}

	d.curDataBlockSourceID = d.pickDataBlockStreamID(m)
	logger.Infof("Using party ID [%d] as the data block source", d.curDataBlockSourceID)
	for id, connMaterial := range m.PerID {
		d.startSingleDeliveryStream(dCtx, connMaterial, id)
	}
	return nil
}

func (d *ftDelivery) startSingleDeliveryStream(
	ctx context.Context, connMaterial *connection.ClientMaterial, sourceID uint32,
) {
	d.activeWorkers.Add(1)
	d.wg.Go(func() {
		defer d.activeWorkers.Add(-1)
		conn, connErr := connMaterial.NewLoadBalancedConnection()
		if connErr != nil {
			logger.Errorf("Delivery worker connection failed with error: %v", connErr)
			return
		}
		defer connection.CloseConnectionsLog(conn)

		headerOnly := sourceID != d.curDataBlockSourceID
		nextBlockNum := d.dataStream.nextBlockNum
		if headerOnly {
			nextBlockNum = d.headerOnlyStream.nextBlockNum
		}
		err := deliver.ToQueue(ctx, deliver.Parameters{
			StreamCreator:           ordererStreamCreator(conn),
			ChannelID:               d.latestConfig.ChannelID,
			Signer:                  d.params.Signer,
			TLSCertHash:             d.tlsCertHash,
			OutputBlockWithSourceID: d.jointOutputBlock,
			NextBlockNum:            nextBlockNum,
			HeaderOnly:              headerOnly,
			SourceID:                sourceID,
		})
		if err != nil && ctx.Err() == nil {
			// We only report error if the worker ended unexpectedly.
			logger.Errorf("Delivery worker ended with error: %v", err)
		}
	})
}

// pickDataBlockStreamID returns one of the parties.
// It prefers the party that delivered the latest block.
// Note: It must only be called with more than one party.
func (d *ftDelivery) pickDataBlockStreamID(m *ordererconn.ConnectionMaterial) uint32 {
	parties := slices.Collect(maps.Keys(m.PerID))

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

	// crypto/rand works with big.Int.
	// It returns a value in the range [0, n).
	idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(parties))))
	if err != nil {
		return parties[0]
	}
	return parties[idx.Int64()]
}

func ordererStreamCreator(conn *grpc.ClientConn) func(ctx context.Context) (deliver.Streamer, error) {
	client := orderer.NewAtomicBroadcastClient(conn)
	return func(ctx context.Context) (deliver.Streamer, error) {
		deliverStream, deliverErr := client.Deliver(ctx)
		if deliverErr != nil {
			return nil, deliverErr
		}
		return &ordererDeliverStream{AtomicBroadcast_DeliverClient: deliverStream}, nil
	}
}

// ordererDeliverStream implements deliver.Streamer.
type ordererDeliverStream struct {
	orderer.AtomicBroadcast_DeliverClient
}

// RecvBlockOrStatus receives the created block from the ordering service. The first
// block number to be received is dependent on the seek position
// sent in DELIVER_SEEK_INFO message.
func (c *ordererDeliverStream) RecvBlockOrStatus() (*common.Block, *common.Status, error) {
	msg, err := c.Recv()
	if err != nil {
		return nil, nil, err
	}
	switch t := msg.Type.(type) {
	case *orderer.DeliverResponse_Status:
		return nil, &t.Status, nil
	case *orderer.DeliverResponse_Block:
		return t.Block, nil, nil
	default:
		return nil, nil, errors.New("unexpected message")
	}
}

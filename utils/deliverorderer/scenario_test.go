/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// scenarioTestEnv drives the block routing/verification/suspicion logic of ftDelivery
// deterministically, without any gRPC streams or real timers. Blocks are pushed into the
// joint output queue in a controlled order and processed one at a time via processNextBlock.
type scenarioTestEnv struct {
	d *ftDelivery
	// p builds the next correctly-signed, hash-chained block. p.PrevBlock starts at the genesis.
	p testcrypto.BlockPrepareParameters
}

// newScenarioTestEnv builds an ftDelivery in a steady state at nextBlockNum=1 on both streams,
// with the genesis config loaded. It reuses prepare() (verify_test.go) for the genesis + consenter
// signing identities, then constructs the delivery via the real newFTDelivery constructor.
func newScenarioTestEnv(t *testing.T) *scenarioTestEnv {
	t.Helper()
	_, p := prepare(t)
	genesis := p.PrevBlock

	d, err := newFTDelivery(Parameters{
		FaultToleranceLevel: ordererdial.BFT,
		// A long grace period ensures suspicions are raised but never auto-confirmed by elapsed
		// time. Tests that need confirmation set targetArrivalTime in the past explicitly.
		SuspicionGracePeriodPerBlock: time.Hour,
		LastBlock:                    genesis,
		NextBlockVerificationConfig:  genesis,
		LatestKnownConfig:            genesis,
		OutputBlock:                  make(chan *common.Block, 100),
		Metrics:                      NewMetrics(monitoring.NewProvider(), monitoring.MetricsParameters{}),
	})
	require.NoError(t, err)
	// Sanity: both streams start just past the genesis block.
	require.EqualValues(t, 1, d.dataStream.nextBlockNum)
	require.EqualValues(t, 1, d.headerOnlyStream.nextBlockNum)
	require.True(t, d.monitorBlockWithholding)
	return &scenarioTestEnv{d: d, p: p}
}

// nextDataBlock builds the next full data block (advancing the chain) and returns it.
func (e *scenarioTestEnv) nextDataBlock(t *testing.T) *common.Block {
	t.Helper()
	txs := workload.GenerateTransactions(t, nil, 10)
	blk := testcrypto.PrepareBlockHeaderAndMetadata(workload.MapToOrdererBlock(0, txs), e.p)
	e.p.PrevBlock = blk
	return blk
}

// nextHeaderOnlyBlock builds the next block and returns a header-only copy (Data == nil).
// The chain still advances using the full header, mirroring how a real header-only stream
// delivers the same header without the payload.
func (e *scenarioTestEnv) nextHeaderOnlyBlock(t *testing.T) *common.Block {
	t.Helper()
	full := e.nextDataBlock(t)
	headerOnly := proto.CloneOf(full)
	headerOnly.Data = nil
	return headerOnly
}

// pushAndProcess enqueues a block with the given source ID and processes exactly one block.
func (e *scenarioTestEnv) pushAndProcess(
	t *testing.T, block *common.Block, sourceID uint32,
) (*deliver.BlockWithSourceID, error) {
	t.Helper()
	e.d.jointOutputBlock <- &deliver.BlockWithSourceID{Block: block, SourceID: sourceID}
	return e.d.processNextBlock(t.Context())
}

func (e *scenarioTestEnv) streamErrorCount(t *testing.T, streamType string, sourceID uint32, errLabel string) int {
	t.Helper()
	id := SourceLabel(sourceID)
	return test.GetIntMetricValue(t, e.d.metrics.StreamErrorsTotal.WithLabelValues(streamType, id, errLabel))
}

// --- Scenario A: getProcessingState routing matrix (the core of the #632 fix) ---

func TestGetProcessingStateRouting(t *testing.T) {
	t.Parallel()
	e := newScenarioTestEnv(t)
	const cur = 7
	e.d.curDataBlockSourceID = cur

	withData := &common.Block{Header: &common.BlockHeader{}, Data: &common.BlockData{}}
	noData := &common.Block{Header: &common.BlockHeader{}} // Data == nil

	for _, tc := range []struct {
		name     string
		block    *common.Block
		sourceID uint32
		wantData bool // true => routed to data stream
	}{
		{name: "header-only from current data source", block: noData, sourceID: cur, wantData: false},
		{name: "header-only from other source", block: noData, sourceID: cur + 1, wantData: false},
		{name: "full block from current data source", block: withData, sourceID: cur, wantData: true},
		{name: "full block from other source", block: withData, sourceID: cur + 1, wantData: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := e.d.getProcessingState(&deliver.BlockWithSourceID{Block: tc.block, SourceID: tc.sourceID})
			if tc.wantData {
				require.Same(t, &e.d.dataStream, actual, "expected routing to the data stream")
			} else {
				require.Same(t, &e.d.headerOnlyStream, actual, "expected routing to the header-only stream")
			}
		})
	}
}

// --- Scenario B: the exact bug — a stale header-only block from the promoted data source ---

// A header-only block (Data == nil) delivered by the source that is now the data source must be
// processed on the header-only stream, NOT mis-classified as a malformed data block. Before the
// fix this returned ErrMalformedBlock -> errDataBlockError, forcing an immediate restart.
func TestStaleHeaderOnlyBlockFromPromotedSourceDoesNotRestartDataStream(t *testing.T) {
	t.Parallel()
	e := newScenarioTestEnv(t)
	const promoted = 4
	e.d.curDataBlockSourceID = promoted

	headerOnly := e.nextHeaderOnlyBlock(t) // block 1, no payload

	deliverBlock, err := e.pushAndProcess(t, headerOnly, promoted)

	require.NoError(t, err, "stale header-only block must not error the data stream")
	require.Nil(t, deliverBlock, "header-only block must not be delivered as a data block")
	require.EqualValues(t, 2, e.d.headerOnlyStream.nextBlockNum, "header stream should advance")
	require.EqualValues(t, 1, e.d.dataStream.nextBlockNum, "data stream must not advance")
	// It must not be recorded as a malformed data-stream block (the pre-fix symptom).
	require.Zero(t, e.streamErrorCount(t, "data", promoted, ErrMalformedBlock.label))
}

// --- Scenario C: cascade across multiple promoted sources cannot form ---

// Repeatedly promoting a new source that has a stale header-only block queued must never cascade
// into data-stream restarts. Each promoted source delivers a header-only block; the header stream
// keeps progressing and no errDataBlockError/errSuspicion is ever returned.
func TestCascadingPromotionDoesNotHaltDelivery(t *testing.T) {
	t.Parallel()
	e := newScenarioTestEnv(t)

	// Three distinct sources are promoted in turn, each delivering a header-only block.
	for i, source := range []uint32{0, 1, 2, 0, 1} {
		e.d.curDataBlockSourceID = source
		headerOnly := e.nextHeaderOnlyBlock(t)
		deliverBlock, err := e.pushAndProcess(t, headerOnly, source)

		require.NoErrorf(t, err, "iteration %d (source %d) must not error", i, source)
		require.NotErrorIs(t, err, errDataBlockError)
		require.NotErrorIs(t, err, errSuspicion)
		require.Nil(t, deliverBlock)
	}
	// Header stream advanced through every block; data stream never moved or restarted.
	require.EqualValues(t, 6, e.d.headerOnlyStream.nextBlockNum)
	require.EqualValues(t, 1, e.d.dataStream.nextBlockNum)
	require.Zero(t, test.GetIntMetricValue(t, e.d.metrics.StreamRestartsTotal.WithLabelValues(errDataBlockError.label)))
}

// --- Scenario D: a stale full-data block from a previous data source goes to the header stream ---

// After a source switch, the queue may still hold a full block from the previous data source.
// Such a block (Data != nil, SourceID != current) must be routed to the header-only stream and
// must NOT be delivered as a data block.
func TestStaleFullBlockFromPreviousSourceRoutedToHeaderStream(t *testing.T) {
	t.Parallel()
	e := newScenarioTestEnv(t)
	const previous, current = 0, 1
	e.d.curDataBlockSourceID = current

	fullBlock := e.nextDataBlock(t) // block 1, full payload, delivered by the previous source
	deliverBlock, err := e.pushAndProcess(t, fullBlock, previous)

	require.NoError(t, err)
	require.Nil(t, deliverBlock, "a block from a non-current source must not be delivered as data")
	require.EqualValues(t, 2, e.d.headerOnlyStream.nextBlockNum, "header stream should advance")
	require.EqualValues(t, 1, e.d.dataStream.nextBlockNum, "data stream must not advance")
}

// --- Scenario E: duplicate / out-of-order blocks after a switch are handled correctly ---

func TestDuplicateAndOutOfOrderHandling(t *testing.T) {
	t.Parallel()

	t.Run("duplicate from non-updater source is silently skipped", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		e.d.curDataBlockSourceID = 5 // ensure the header path is exercised

		block1 := e.nextDataBlock(t)
		// First arrival on the header stream from source 1 sets updaterSourceID = 1.
		_, err := e.pushAndProcess(t, block1, 1)
		require.NoError(t, err)
		require.EqualValues(t, 1, e.d.headerOnlyStream.updaterSourceID)

		// The same block re-delivered by a different source (2) is a correct, expected duplicate.
		deliverBlock, err := e.pushAndProcess(t, block1, 2)
		require.NoError(t, err)
		require.Nil(t, deliverBlock)
		// Not counted as an error, since it did not come from the last updater.
		require.Zero(t, e.streamErrorCount(t, "header", 2, ErrDuplicateBlock.label))
	})

	t.Run("duplicate from the updater source is counted", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		e.d.curDataBlockSourceID = 5

		block1 := e.nextDataBlock(t)
		_, err := e.pushAndProcess(t, block1, 1)
		require.NoError(t, err)

		// The same source re-delivering the same block is unexpected and is recorded.
		deliverBlock, err := e.pushAndProcess(t, block1, 1)
		require.NoError(t, err)
		require.Nil(t, deliverBlock)
		require.GreaterOrEqual(t, e.streamErrorCount(t, "header", 1, ErrDuplicateBlock.label), 1)
	})

	t.Run("out-of-order on header stream is skipped without restart", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		e.d.curDataBlockSourceID = 5

		_ = e.nextDataBlock(t)            // skip block 1
		futureBlock := e.nextDataBlock(t) // block 2, arrives before block 1
		deliverBlock, err := e.pushAndProcess(t, futureBlock, 1)
		require.NoError(t, err, "header-stream errors must not trigger a restart")
		require.Nil(t, deliverBlock)
		require.EqualValues(t, 1, e.d.headerOnlyStream.nextBlockNum, "header stream must not advance")
		require.GreaterOrEqual(t, e.streamErrorCount(t, "header", 1, ErrUnexpectedBlockNumber.label), 1)
	})

	t.Run("out-of-order on data stream triggers restart", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		const cur = 0
		e.d.curDataBlockSourceID = cur

		_ = e.nextDataBlock(t)            // skip block 1
		futureBlock := e.nextDataBlock(t) // block 2, arrives before block 1, from the data source
		deliverBlock, err := e.pushAndProcess(t, futureBlock, cur)
		require.ErrorIs(t, err, errDataBlockError, "data-stream errors must trigger a restart")
		require.Nil(t, deliverBlock)
		require.EqualValues(t, 1, e.d.dataStream.nextBlockNum)
	})
}

// --- Scenario F: block-withholding suspicion lifecycle (checkBlockWithholding) ---

func TestSuspicionLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("raise when header stream gets ahead", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		const cur = 1
		e.d.curDataBlockSourceID = cur
		e.d.dataStream.nextBlockNum = 1
		e.d.headerOnlyStream.nextBlockNum = 3

		err := e.d.checkBlockWithholding()
		require.NoError(t, err)
		require.EqualValues(t, 3, e.d.targetNextBlockNum, "target set to the header stream height")
		require.GreaterOrEqual(t,
			test.GetIntMetricValue(t, e.d.metrics.SuspicionRaisedTotal.WithLabelValues(SourceLabel(cur))), 1)
		require.Positive(t,
			test.GetMetricValue(t, e.d.metrics.TargetArrivalDeadline.WithLabelValues(SourceLabel(cur))))
	})

	t.Run("clear when data stream catches up", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		const cur = 1
		e.d.curDataBlockSourceID = cur
		// A suspicion is already active.
		e.d.targetNextBlockNum = 3
		e.d.dataStream.nextBlockNum = 3
		e.d.headerOnlyStream.nextBlockNum = 3

		err := e.d.checkBlockWithholding()
		require.NoError(t, err)
		require.Zero(t, e.d.targetNextBlockNum, "suspicion target cleared")
		require.GreaterOrEqual(t,
			test.GetIntMetricValue(t, e.d.metrics.SuspicionClearedTotal.WithLabelValues(SourceLabel(cur))), 1)
	})

	t.Run("confirm when the deadline passes", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		const cur = 1
		e.d.curDataBlockSourceID = cur
		e.d.targetNextBlockNum = 3
		e.d.dataStream.nextBlockNum = 1
		e.d.headerOnlyStream.nextBlockNum = 3
		e.d.targetArrivalTime = time.Now().Add(-time.Hour) // deadline already elapsed

		err := e.d.checkBlockWithholding()
		require.ErrorIs(t, err, errSuspicion)
		require.Zero(t, e.d.targetNextBlockNum, "target reset so the next source starts fresh")
		require.GreaterOrEqual(t,
			test.GetIntMetricValue(t, e.d.metrics.SuspicionConfirmedTotal.WithLabelValues(SourceLabel(cur))), 1)
	})

	t.Run("stay within the grace period", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		const cur = 1
		e.d.curDataBlockSourceID = cur
		e.d.targetNextBlockNum = 3
		e.d.dataStream.nextBlockNum = 1
		e.d.headerOnlyStream.nextBlockNum = 3
		e.d.targetArrivalTime = time.Now().Add(time.Hour) // deadline still in the future

		err := e.d.checkBlockWithholding()
		require.NoError(t, err, "must not confirm before the deadline")
		require.EqualValues(t, 3, e.d.targetNextBlockNum)
	})

	t.Run("empty queue past the deadline confirms via readNextBlock timeout", func(t *testing.T) {
		t.Parallel()
		e := newScenarioTestEnv(t)
		const cur = 1
		e.d.curDataBlockSourceID = cur
		e.d.targetNextBlockNum = 3
		e.d.dataStream.nextBlockNum = 1
		e.d.headerOnlyStream.nextBlockNum = 3
		e.d.targetArrivalTime = time.Now().Add(-time.Hour)

		// No block is enqueued: readNextBlock times out immediately and checkBlockWithholding runs.
		deliverBlock, err := e.d.processNextBlock(t.Context())
		require.ErrorIs(t, err, errSuspicion)
		require.Nil(t, deliverBlock)
	})
}

// --- Scenario G: a config block arriving on the header-only stream still triggers a config update ---

// A config block (Data != nil) delivered by a non-data source is routed to the header-only stream.
// It must still be detected as a config update so endpoint/credential changes are never missed
// just because the block arrived off the data source.
func TestConfigBlockOnHeaderStreamTriggersConfigUpdate(t *testing.T) {
	t.Parallel()
	e := newScenarioTestEnv(t)
	const cur = 1
	e.d.curDataBlockSourceID = cur

	// Build a fresh config block (genesis of a second crypto set) chained after the real genesis,
	// signed by the current consenters — mirrors verify_test.go's "New Config block" case.
	_, p2 := prepare(t)
	configBlock := testcrypto.PrepareBlockHeaderAndMetadata(p2.PrevBlock, e.p)
	require.EqualValues(t, 1, configBlock.Header.Number)

	configUpdatesBefore := test.GetIntMetricValue(t, e.d.metrics.ConfigUpdatesTotal)

	// Deliver it from a source other than the current data source -> header-only stream.
	deliverBlock, err := e.pushAndProcess(t, configBlock, cur+1)

	require.ErrorIs(t, err, errConfigUpdate, "config update must be detected on the header stream")
	require.Nil(t, deliverBlock, "a header-stream block is not delivered as a data block")
	require.Greater(t, test.GetIntMetricValue(t, e.d.metrics.ConfigUpdatesTotal), configUpdatesBefore)
	require.EqualValues(t, 1, e.d.latestConfig.configBlockNumber, "latest config advanced to the new block")
}

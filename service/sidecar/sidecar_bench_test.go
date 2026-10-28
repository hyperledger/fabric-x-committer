/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// The knobs below are held fixed so a run only varies the block size. They mirror a deployment
// that is tuned for throughput rather than for a small memory footprint.
const (
	// benchE2EWaitingTxsLimit is the sidecar's in-flight TX limit. It only has to be large enough
	// that the relay never blocks on it, since the coordinator stub returns statuses immediately;
	// with a real coordinator it is a latency knob rather than a throughput one.
	benchE2EWaitingTxsLimit = 500_000
	// benchE2EChannelBufferSize sizes the sidecar's internal block channels, counted in blocks.
	benchE2EChannelBufferSize = 20
)

// BenchmarkSidecarEndToEnd measures the sidecar as a whole service, at the block sizes a
// deployment realistically chooses between. Every stage the sidecar owns is in the measurement: a
// block is pulled over the orderer's Deliver stream and its consenter signatures verified, mapped
// into a coordinator batch (envelope parsing, form validation, TX ID dedup), submitted to the
// coordinator over a real gRPC stream, its statuses collected as they come back out of order, and
// the block appended to a real on-disk ledger with its status metadata attached.
//
// Only the two peers are stubbed, because both live on other machines in a deployment and would
// otherwise be measured alongside the sidecar on this one:
//   - The orderer serves blocks that were built and signed before the timer started, so delivery
//     costs a cache read and a gRPC write. See mock.Orderer.SubmitPreparedBlock for why block
//     preparation cannot stay on that path at these rates.
//   - The coordinator is benchCoordinator, which echoes COMMITTED without validating anything.
//
// b.N counts transactions, so ns/op and tx/s stay per transaction however the block size changes.
// The benchmark holds every generated transaction in memory before it starts — roughly 700 bytes
// each — so drive it with an explicit count (`-benchtime 2000000x`) rather than a duration, which
// would let the transaction pool grow until the machine swaps.
func BenchmarkSidecarEndToEnd(b *testing.B) {
	for _, tc := range []struct {
		name      string
		blockSize int
	}{
		{name: "blockSize=100", blockSize: 100},
		{name: "blockSize=500", blockSize: 500},
		{name: "blockSize=1000", blockSize: 1_000},
		{name: "blockSize=5000", blockSize: 5_000},
		{name: "blockSize=10000", blockSize: 10_000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			runSidecarE2E(b, tc.blockSize)
		})
	}
}

// runSidecarE2E feeds b.N transactions through the sidecar in blocks of blockSize and returns once
// the last one is in the ledger.
func runSidecarE2E(b *testing.B, blockSize int) {
	b.Helper()
	flogging.ActivateSpec("fatal")

	blockCount := (b.N + blockSize - 1) / blockSize
	env := newSidecarBenchEnv(b, blockCount)
	blocks := env.prepareBlocks(b, b.N, blockSize)
	env.start(b)

	// The orderer serves the genesis config block as block 0. Waiting for it to reach the ledger
	// keeps the sidecar's start-up, and the submission barrier every config block imposes, out of
	// the measurement, so the timer covers steady-state block processing alone.
	env.awaitLedgerHeight(b, 1)

	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()

	// The profile of this benchmark is dominated by GC, so the allocation counters are as much
	// the result as tx/s is: a change that does not reduce B/op is unlikely to raise throughput.
	b.ReportAllocs()
	b.ResetTimer()
	go func() {
		for _, blk := range blocks {
			if !env.orderer.Orderer.SubmitPreparedBlock(ctx, blk) {
				return
			}
		}
	}()
	// The genesis block sits below the prepared ones, so the final height is one past the last.
	env.awaitLedgerHeight(b, uint64(len(blocks))+1)
	b.StopTimer()

	test.ReportTxPerSecond(b)
}

// sidecarBenchEnv is the sidecar under test together with the two stubbed peers around it.
type sidecarBenchEnv struct {
	orderer      *mock.OrdererTestEnv
	sidecar      *Service
	serverConfig *serve.Config
	// blockParams carries the chain state prepareBlocks needs: the previous block's hash and
	// number, and the consenter identities whose signatures the sidecar's delivery client checks.
	blockParams testcrypto.BlockPrepareParameters
}

// newSidecarBenchEnv builds the sidecar and its two stubbed peers. blockCount is the number of
// blocks the run will submit, which fixes the depth of the orderer's outgoing block ring.
func newSidecarBenchEnv(b *testing.B, blockCount int) *sidecarBenchEnv {
	b.Helper()
	_, coordinatorServer := startBenchCoordinator(b)
	ordererEnv := mock.NewOrdererTestEnv(b, &mock.OrdererTestParameters{
		ChanID: "bench",
		NumIDs: 1,
		OrdererConfig: &mock.OrdererConfig{
			// The orderer cuts no blocks of its own here — every block is submitted prepared — so
			// BlockSize only sizes its internal channels, and a large value would allocate a
			// needlessly large one. The timeout keeps its ticker off the measurement.
			BlockSize:    1,
			BlockTimeout: time.Hour,
			// The ring holds the genesis block and every block the run submits, so no block is
			// ever overwritten. It has to: the ring tracks one delivered-block position for all
			// streams, while the sidecar reads it with two — one for data blocks and one for
			// headers — so a ring that wraps lets the faster stream free a slot the other has
			// not read, and the orderer then serves an empty block in place of the lost one.
			// Sizing it to the whole run also removes the producer's backpressure, leaving the
			// sidecar's own channels as the only thing that paces delivery. The blocks are held
			// in memory for the run either way, so this costs one pointer each.
			OutBlockCapacity: blockCount + 1,
			SendGenesisBlock: true,
		},
	})

	sidecarConf := &Config{
		Committer: test.NewTLSClientConfig(
			connection.TLSConfig{}, &coordinatorServer.Configs[0].GRPC.Endpoint,
		),
		Ledger: LedgerConfig{
			Path: b.TempDir(),
			// The TX ID index costs an index entry per transaction rather than per block, and its
			// compaction is the largest consumer of sidecar CPU under load. A deployment tuned for
			// throughput turns it off, so the benchmark measures that shape by default.
			DisableTxIDIndex: true,
		},
		Notification: NotificationServiceConfig{
			MaxTimeout:         time.Minute,
			MaxActiveTxIDs:     100_000,
			MaxTxIDsPerRequest: 1_000,
			StreamWriteTimeout: 30 * time.Second,
		},
		LastCommittedBlockSetInterval: time.Second,
		WaitingTxsLimit:               benchE2EWaitingTxsLimit,
		ChannelBufferSize:             benchE2EChannelBufferSize,
		Orderer:                       ordererEnv.OrdererConnConfig,
	}
	sidecar, err := New(sidecarConf)
	require.NoError(b, err)

	consenters, err := testcrypto.GetSigningIdentities(
		testcrypto.GetConsenterMspDirs(ordererEnv.ArtifactsPath)...,
	)
	require.NoError(b, err)

	return &sidecarBenchEnv{
		orderer:      ordererEnv,
		sidecar:      sidecar,
		serverConfig: test.NewLocalHostServiceConfig(connection.TLSConfig{}),
		blockParams:  testcrypto.BlockPrepareParameters{ConsenterSigners: consenters},
	}
}

// prepareBlocks builds txCount transactions, groups them into blocks of at most blockSize, and
// signs each block as the orderer would. This is the whole cost the benchmark moves off the
// delivery path, so none of it may happen while the timer runs.
//
// The blocks chain from the genesis block the orderer serves as block 0, which is read back from
// the orderer here because it is what fixes the first prepared block's number and previous hash.
func (env *sidecarBenchEnv) prepareBlocks(b *testing.B, txCount, blockSize int) []*common.Block {
	b.Helper()
	genesis, err := env.orderer.Orderer.GetBlock(b.Context(), 0)
	require.NoError(b, err)
	env.blockParams.PrevBlock = genesis
	env.blockParams.LastConfigBlockIndex = genesis.Header.Number

	// Generating and signing the transactions is by far the largest cost in this function, so it
	// runs on every core: left on one it would dominate both the benchmark's wall time and any CPU
	// profile taken over the run, hiding the sidecar it is meant to expose.
	txs := workload.GenerateTransactionsConcurrently(b, benchTxProfile(), txCount, runtime.NumCPU())
	blocks := make([]*common.Block, 0, (txCount+blockSize-1)/blockSize)
	for offset := 0; offset < txCount; offset += blockSize {
		// MapToOrdererBlock's block number is overwritten by PrepareBlockHeaderAndMetadata, which
		// numbers each block from the previous one, so it does not matter here.
		raw := workload.MapToOrdererBlock(0, txs[offset:min(offset+blockSize, txCount)])
		prepared := testcrypto.PrepareBlockHeaderAndMetadata(raw, env.blockParams)
		env.blockParams.PrevBlock = prepared
		blocks = append(blocks, prepared)
	}
	return blocks
}

func (env *sidecarBenchEnv) start(b *testing.B) {
	b.Helper()
	test.RunServiceAndServeForTest(b.Context(), b, env.sidecar, env.serverConfig)
}

// awaitLedgerHeight blocks until the sidecar's ledger has committed every block. The ledger is the
// end of the sidecar's pipeline, so its height is the completion signal that adds no cost of its
// own — unlike a delivery client, which would pay to unmarshal every block again on this machine.
func (env *sidecarBenchEnv) awaitLedgerHeight(b *testing.B, height uint64) {
	b.Helper()
	for env.sidecar.blockStore.GetBlockHeight() < height {
		if b.Context().Err() != nil {
			b.Fatalf("the sidecar stopped at ledger height %d of %d",
				env.sidecar.blockStore.GetBlockHeight(), height)
		}
		time.Sleep(time.Millisecond)
	}
}

// benchCoordinator is a coordinator that commits everything, doing the least work a coordinator
// can while still holding up its end of the block-processing stream: it echoes a COMMITTED status
// for every TX of every batch it receives, in one status batch per batch received.
//
// mock.Coordinator is unsuitable here even though it commits everything too. It shuffles each
// block's statuses, splits them into randomly-sized chunks, and records every TX ID in a cache
// behind a single mutex, which together cost more per transaction than the sidecar does — the
// benchmark would measure the stub.
type benchCoordinator struct {
	servicepb.CoordinatorServer
	healthcheck *health.Server
}

// RegisterService registers the stub's gRPC services.
func (c *benchCoordinator) RegisterService(s serve.Servers) {
	servicepb.RegisterCoordinatorServer(s.GRPC, c)
	healthgrpc.RegisterHealthServer(s.GRPC, c.healthcheck)
}

func startBenchCoordinator(b *testing.B) (*benchCoordinator, *test.Servers) {
	b.Helper()
	coordinator := &benchCoordinator{healthcheck: serve.DefaultHealthCheckService()}
	servers := test.ServeManyForTest(b.Context(), b, test.StartServerParameters{NumService: 1}, coordinator)
	return coordinator, servers
}

// BlockProcessing echoes a COMMITTED status for every TX it receives. Recv and Send run on the
// same goroutine: the sidecar reads statuses on a goroutine of its own and buffers them, so a
// status batch never has to wait for the sidecar to finish with the previous one.
func (*benchCoordinator) BlockProcessing(
	stream grpc.BidiStreamingServer[servicepb.CoordinatorBatch, servicepb.TxStatusBatch],
) error {
	for stream.Context().Err() == nil {
		batch, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "failed to receive a batch")
		}
		status := make([]*committerpb.TxStatus, 0, len(batch.Txs)+len(batch.Rejected))
		status = append(status, batch.Rejected...)
		for _, tx := range batch.Txs {
			status = append(status, &committerpb.TxStatus{Ref: tx.Ref, Status: committerpb.Status_COMMITTED})
		}
		if err := stream.Send(&servicepb.TxStatusBatch{Status: status}); err != nil {
			return errors.Wrap(err, "failed to send statuses")
		}
	}
	return errors.Wrap(stream.Context().Err(), "context ended")
}

// GetNextBlockNumberToCommit reports that nothing has been committed: the benchmark always starts
// from an empty ledger, so the sidecar has no recovery to do.
func (*benchCoordinator) GetNextBlockNumberToCommit(
	context.Context, *emptypb.Empty,
) (*servicepb.BlockRef, error) {
	return &servicepb.BlockRef{Number: 0}, nil
}

// NoPendingTransactionProcessing reports an idle coordinator, which the sidecar waits for before
// it recovers.
func (*benchCoordinator) NoPendingTransactionProcessing(
	context.Context, *emptypb.Empty,
) (*wrapperspb.BoolValue, error) {
	return wrapperspb.Bool(true), nil
}

// SetLastCommittedBlockNumber is called periodically by the sidecar and discarded here.
func (*benchCoordinator) SetLastCommittedBlockNumber(
	context.Context, *servicepb.BlockRef,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

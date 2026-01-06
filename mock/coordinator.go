/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

// Coordinator is a mock implementation of servicepb.CoordinatorServer.
type Coordinator struct {
	servicepb.CoordinatorServer
	lastCommittedBlock atomic.Pointer[servicepb.BlockRef]
	nextBlock          atomic.Uint64
	streamActive       atomic.Bool
	numWaitingTxs      atomic.Int32
	txsStatus          *fifoCache[*committerpb.TxStatus]
	txsStatusMu        sync.Mutex
	configTransaction  atomic.Pointer[applicationpb.ConfigTransaction]
	latency            atomic.Pointer[time.Duration]
	healthcheck        *health.Server
}

// We don't want to utilize unlimited memory for storing the transactions status.
// A value of 100,000 TXs is adequate for most of the unit-test.
var defaultTxStatusStorageSize = 100_000

// NewMockCoordinator creates a new mock coordinator.
func NewMockCoordinator() *Coordinator {
	return &Coordinator{
		txsStatus:   newFifoCache[*committerpb.TxStatus](defaultTxStatusStorageSize),
		healthcheck: connection.DefaultHealthCheckService(),
	}
}

// RegisterService registers for the coordinator's GRPC services.
func (c *Coordinator) RegisterService(server *grpc.Server) {
	servicepb.RegisterCoordinatorServer(server, c)
	healthgrpc.RegisterHealthServer(server, c.healthcheck)
}

// GetConfigTransaction return the latest configuration transaction.
func (c *Coordinator) GetConfigTransaction(context.Context, *emptypb.Empty) (*applicationpb.ConfigTransaction, error) {
	return c.configTransaction.Load(), nil
}

// SetConfigTransaction stores the given envelope data as the current config transaction.
func (c *Coordinator) SetConfigTransaction(data []byte) {
	c.configTransaction.Store(&applicationpb.ConfigTransaction{Envelope: data})
}

// SetLastCommittedBlockNumber sets the last committed block number.
func (c *Coordinator) SetLastCommittedBlockNumber(
	_ context.Context, lastBlock *servicepb.BlockRef,
) (*emptypb.Empty, error) {
	c.lastCommittedBlock.Store(lastBlock)
	return nil, nil
}

// GetNextBlockNumberToCommit returns the next expected block number to be received by the coordinator.
func (c *Coordinator) GetNextBlockNumberToCommit(
	context.Context,
	*emptypb.Empty,
) (*servicepb.BlockRef, error) {
	return &servicepb.BlockRef{Number: c.nextBlock.Load()}, nil
}

// GetTransactionsStatus returns the status of given set of transaction identifiers.
func (c *Coordinator) GetTransactionsStatus(
	_ context.Context,
	q *committerpb.TxIDsBatch,
) (*committerpb.TxStatusBatch, error) {
	status := make([]*committerpb.TxStatus, len(q.TxIds))
	c.txsStatusMu.Lock()
	defer c.txsStatusMu.Unlock()
	for i, txID := range q.TxIds {
		v, _ := c.txsStatus.get(txID)
		status[i] = v
	}
	return &committerpb.TxStatusBatch{Status: status}, nil
}

// NumberOfWaitingTransactionsForStatus returns the number of transactions waiting to get the final status.
func (c *Coordinator) NumberOfWaitingTransactionsForStatus(
	context.Context,
	*emptypb.Empty,
) (*servicepb.WaitingTransactions, error) {
	return &servicepb.WaitingTransactions{Count: c.numWaitingTxs.Load()}, nil
}

// IsStreamActive returns true if the stream from the sidecar is active.
func (c *Coordinator) IsStreamActive() bool {
	return c.streamActive.Load()
}

// BlockProcessing processes a block.
func (c *Coordinator) BlockProcessing(stream servicepb.Coordinator_BlockProcessingServer) error {
	if !c.streamActive.CompareAndSwap(false, true) {
		return errors.New("stream is already active. Only one stream is allowed")
	}
	defer c.streamActive.CompareAndSwap(true, false)
	logger.Info("Starting block processing stream")
	defer logger.Info("Closed block processing stream")

	g, gCtx := errgroup.WithContext(stream.Context())
	blockQueue := channel.Make[*servicepb.CoordinatorBatch](gCtx, 1000)
	g.Go(func() error {
		return c.receiveBlocks(gCtx, stream, blockQueue)
	})
	g.Go(func() error {
		return c.sendTxsValidationStatus(gCtx, stream, blockQueue)
	})
	return grpcerror.WrapCancelled(g.Wait())
}

func (c *Coordinator) receiveBlocks(
	ctx context.Context,
	stream servicepb.Coordinator_BlockProcessingServer,
	blockQueue channel.Writer[*servicepb.CoordinatorBatch],
) error {
	for ctx.Err() == nil {
		block, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "receive block failed")
		}

		maxBlock := uint64(0)
		if len(block.Txs) > 0 {
			maxBlock = max(maxBlock, block.Txs[len(block.Txs)-1].Ref.BlockNum)
		}
		if len(block.Rejected) > 0 {
			maxBlock = max(maxBlock, block.Rejected[len(block.Rejected)-1].Ref.BlockNum)
		}
		c.nextBlock.Store(maxBlock + 1)

		logger.Debugf("Received batch with %d transactions", len(block.Txs))
		c.numWaitingTxs.Add(int32(len(block.Txs))) //nolint:gosec

		// send to the validation
		blockQueue.Write(block)
	}
	return errors.Wrap(ctx.Err(), "context cancelled")
}

func (c *Coordinator) sendTxsValidationStatus(
	ctx context.Context,
	stream servicepb.Coordinator_BlockProcessingServer,
	blockQueue channel.Reader[*servicepb.CoordinatorBatch],
) error {
	for ctx.Err() == nil {
		scBlock, ok := blockQueue.Read()
		if !ok {
			break
		}

		latency := c.latency.Load()
		if latency != nil {
			tc := time.NewTicker(*latency)
			select {
			case <-ctx.Done():
				return errors.Wrap(ctx.Err(), "context cancelled")
			case <-tc.C:
			}
		}

		info := scBlock.Rejected
		for _, tx := range scBlock.Txs {
			info = append(info, &committerpb.TxStatus{
				Ref:    tx.Ref,
				Status: committerpb.Status_COMMITTED,
			})
		}
		rand.Shuffle(len(info), func(i, j int) { info[i], info[j] = info[j], info[i] })

		for len(info) > 0 {
			chunkSize := rand.Intn(len(info)) + 1
			if err := c.sendTxsStatusChunk(stream, info[:chunkSize]); err != nil {
				return err
			}
			info = info[chunkSize:]
		}
	}
	return errors.Wrap(ctx.Err(), "context cancelled")
}

func (c *Coordinator) sendTxsStatusChunk(
	stream servicepb.Coordinator_BlockProcessingServer,
	txs []*committerpb.TxStatus,
) error {
	b := &committerpb.TxStatusBatch{
		Status: make([]*committerpb.TxStatus, len(txs)),
	}
	c.txsStatusMu.Lock()
	defer c.txsStatusMu.Unlock()
	for i, info := range txs {
		s := committerpb.NewTxStatusFromRef(info.Ref, info.Status)
		b.Status[i] = s
		c.txsStatus.addIfNotExist(info.Ref.TxId, s)
	}
	if err := stream.Send(b); err != nil {
		return errors.Wrap(err, "failed to send status")
	}
	logger.Debugf("Sent back batch with %d TXs", len(b.Status))
	c.numWaitingTxs.Add(-int32(len(b.Status))) //nolint:gosec
	return nil
}

// SetWaitingTxsCount sets the waiting transactions count. The purpose
// of this method is to set the count manually for testing purpose.
func (c *Coordinator) SetWaitingTxsCount(count int32) {
	c.numWaitingTxs.Store(count)
}

// SetDelay sets the duration to wait before sending statuses.
func (c *Coordinator) SetDelay(d time.Duration) {
	c.latency.Store(&d)
}

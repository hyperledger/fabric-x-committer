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

type (
	// VcService is a mock implementation of servicepb.ValidationAndCommitServiceServer.
	// It is used for testing the client which is the coordinator service.
	VcService struct {
		servicepb.ValidationAndCommitServiceServer
		streamStateManager[VCStreamState]
		nextBlock   atomic.Pointer[servicepb.BlockRef]
		txsStatus   *fifoCache[*committerpb.TxStatus]
		txsStatusMu sync.Mutex
		healthcheck *health.Server
		// NumBatchesReceived is the number of batches received by VcService.
		NumBatchesReceived atomic.Uint32
		// MockFaultyNodeDropSize allows mocking a faulty node by dropping some TXs.
		MockFaultyNodeDropSize int
	}

	// VCStreamState holds the stream's batch queue.
	VCStreamState struct {
		StreamInfo
		q chan *servicepb.VcBatch
	}
)

// NewMockVcService returns a new VcService.
func NewMockVcService() *VcService {
	return &VcService{
		txsStatus:   newFifoCache[*committerpb.TxStatus](defaultTxStatusStorageSize),
		healthcheck: connection.DefaultHealthCheckService(),
	}
}

// RegisterService registers for the validator-committer's GRPC services.
func (v *VcService) RegisterService(server *grpc.Server) {
	servicepb.RegisterValidationAndCommitServiceServer(server, v)
	healthgrpc.RegisterHealthServer(server, v.healthcheck)
}

// SetLastCommittedBlockNumber set the last committed block number in the database/ledger.
func (v *VcService) SetLastCommittedBlockNumber(
	_ context.Context,
	lastBlock *servicepb.BlockRef,
) (*emptypb.Empty, error) {
	lastBlock.Number++
	v.nextBlock.Store(lastBlock)
	return nil, nil
}

// GetNextBlockNumberToCommit get the next expected block number in the database/ledger.
func (v *VcService) GetNextBlockNumberToCommit(
	context.Context,
	*emptypb.Empty,
) (*servicepb.BlockRef, error) {
	return v.nextBlock.Load(), nil
}

// GetNamespacePolicies is a mock implementation of the protovcservice.GetNamespacePolicies.
func (*VcService) GetNamespacePolicies(
	context.Context,
	*emptypb.Empty,
) (*applicationpb.NamespacePolicies, error) {
	return &applicationpb.NamespacePolicies{}, nil
}

// GetConfigTransaction is a mock implementation of the protovcservice.GetConfigTransaction.
func (*VcService) GetConfigTransaction(
	context.Context,
	*emptypb.Empty,
) (*applicationpb.ConfigTransaction, error) {
	return &applicationpb.ConfigTransaction{}, nil
}

// GetTransactionsStatus get the status for a given set of transactions IDs.
func (v *VcService) GetTransactionsStatus(
	_ context.Context,
	query *committerpb.TxIDsBatch,
) (*committerpb.TxStatusBatch, error) {
	s := &committerpb.TxStatusBatch{Status: make([]*committerpb.TxStatus, len(query.TxIds))}
	v.txsStatusMu.Lock()
	defer v.txsStatusMu.Unlock()
	for i, id := range query.TxIds {
		if status, ok := v.txsStatus.get(id); ok {
			s.Status[i] = status
		}
	}
	return s, nil
}

// SetupSystemTablesAndNamespaces creates the required system tables and namespaces.
func (*VcService) SetupSystemTablesAndNamespaces(
	context.Context,
	*emptypb.Empty,
) (*emptypb.Empty, error) {
	return nil, nil
}

// StartValidateAndCommitStream is the mock implementation of the
// [protovcservice.ValidationAndCommitServiceServer] interface.
func (v *VcService) StartValidateAndCommitStream(
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamServer,
) error {
	g, gCtx := errgroup.WithContext(stream.Context())
	state := v.registerStream(gCtx, func(info StreamInfo) *VCStreamState {
		return &VCStreamState{
			StreamInfo: info,
			q:          make(chan *servicepb.VcBatch),
		}
	})

	g.Go(func() error {
		return v.receiveAndProcessTransactions(gCtx, stream, state.q)
	})
	g.Go(func() error {
		return v.sendTransactionStatus(gCtx, stream, state.q)
	})
	return grpcerror.WrapCancelled(g.Wait())
}

func (v *VcService) receiveAndProcessTransactions(
	ctx context.Context,
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamServer,
	txBatchChan chan *servicepb.VcBatch,
) error {
	txBatchChanWriter := channel.NewWriter(ctx, txBatchChan)
	for ctx.Err() == nil {
		txBatch, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "error receiving transactions")
		}

		preTxNum := txBatch.Transactions[0].Ref.TxNum
		for _, tx := range txBatch.Transactions[1:] {
			if preTxNum == tx.Ref.TxNum {
				return errors.New("duplication tx num detected")
			}
		}

		v.NumBatchesReceived.Add(1)
		txBatchChanWriter.Write(txBatch)
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

func (v *VcService) sendTransactionStatus(
	ctx context.Context,
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamServer,
	txBatchChan chan *servicepb.VcBatch,
) error {
	txBatchChanReader := channel.NewReader(ctx, txBatchChan)
	for ctx.Err() == nil {
		txBatch, ok := txBatchChanReader.Read()
		if !ok {
			break
		}
		txsStatus := &committerpb.TxStatusBatch{
			Status: make([]*committerpb.TxStatus, 0, len(txBatch.Transactions)),
		}
		v.txsStatusMu.Lock()
		for i, tx := range txBatch.Transactions {
			if i < v.MockFaultyNodeDropSize {
				// We simulate a faulty node by not responding to the first X TXs.
				continue
			}
			code := committerpb.Status_COMMITTED
			if tx.PrelimInvalidTxStatus != nil {
				code = *tx.PrelimInvalidTxStatus
			}
			s := committerpb.NewTxStatusFromRef(tx.Ref, code)
			txsStatus.Status = append(txsStatus.Status, s)
			v.txsStatus.addIfNotExist(tx.Ref.TxId, s)
		}
		v.txsStatusMu.Unlock()

		if err := stream.Send(txsStatus); err != nil {
			return errors.Wrap(err, "error sending transaction status")
		}
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

// SubmitTransactions enqueues the given transactions to a queue read by status sending goroutine.
// This method helps the test code to bypass the stream to submit transactions to the mock vcservice.
func (v *VcService) SubmitTransactions(ctx context.Context, txsBatch *servicepb.VcBatch) error {
	states := v.StreamsStates()

	if len(states) == 0 {
		return errors.New("Trying to send transactions before channel created (no channels in map)")
	}

	s := states[rand.Intn(len(states))]
	channel.NewWriter(ctx, s.q).Write(txsBatch)
	return nil
}

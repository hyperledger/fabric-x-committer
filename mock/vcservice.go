/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protovcservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

// VcService implements the [protovcservice.ValidationAndCommitServiceServer] interface.
// It is used for testing the client which is the coordinator service.
type VcService struct {
	protovcservice.ValidationAndCommitServiceServer
	txBatchChan        chan *protovcservice.TransactionBatch
	numBatchesReceived atomic.Uint32
	lastCommittedBlock atomic.Pointer[protoblocktx.BlockInfo]
	txsStatus          *fifoCache[*protoblocktx.StatusWithHeight]
	txsStatusMu        sync.Mutex
	// MockFaultyNodeDropSize allows mocking a faulty node by dropping some TXs.
	MockFaultyNodeDropSize int
}

// NewMockVcService returns a new VcService.
func NewMockVcService() *VcService {
	return &VcService{
		txBatchChan: make(chan *protovcservice.TransactionBatch),
		txsStatus:   newFifoCache[*protoblocktx.StatusWithHeight](defaultTxStatusStorageSize),
	}
}

// SetLastCommittedBlockNumber set the last committed block number in the database/ledger.
func (v *VcService) SetLastCommittedBlockNumber(
	_ context.Context,
	lastBlock *protoblocktx.BlockInfo,
) (*protovcservice.Empty, error) {
	v.lastCommittedBlock.Store(lastBlock)
	return nil, nil
}

// GetLastCommittedBlockNumber get the last committed block number in the database/ledger.
func (v *VcService) GetLastCommittedBlockNumber(
	_ context.Context,
	_ *protovcservice.Empty,
) (*protoblocktx.LastCommittedBlock, error) {
	return &protoblocktx.LastCommittedBlock{Block: v.lastCommittedBlock.Load()}, nil
}

// GetNamespacePolicies is a mock implementation of the protovcservice.GetNamespacePolicies.
func (*VcService) GetNamespacePolicies(
	context.Context,
	*protovcservice.Empty,
) (*protoblocktx.NamespacePolicies, error) {
	return &protoblocktx.NamespacePolicies{}, nil
}

// GetConfigTransaction is a mock implementation of the protovcservice.GetConfigTransaction.
func (*VcService) GetConfigTransaction(
	context.Context,
	*protovcservice.Empty,
) (*protoblocktx.ConfigTransaction, error) {
	return &protoblocktx.ConfigTransaction{}, nil
}

// GetTransactionsStatus get the status for a given set of transactions IDs.
func (v *VcService) GetTransactionsStatus(
	_ context.Context,
	query *protoblocktx.QueryStatus,
) (*protoblocktx.TransactionsStatus, error) {
	s := &protoblocktx.TransactionsStatus{Status: make(map[string]*protoblocktx.StatusWithHeight)}
	v.txsStatusMu.Lock()
	defer v.txsStatusMu.Unlock()
	for _, id := range query.TxIDs {
		if status, ok := v.txsStatus.get(id); ok {
			s.Status[id] = status
		}
	}
	return s, nil
}

// SetupSystemTablesAndNamespaces creates the required system tables and namespaces.
func (*VcService) SetupSystemTablesAndNamespaces(
	context.Context,
	*protovcservice.Empty,
) (*protovcservice.Empty, error) {
	return nil, nil
}

// StartValidateAndCommitStream is the mock implementation of the
// [protovcservice.ValidationAndCommitServiceServer] interface.
func (v *VcService) StartValidateAndCommitStream(
	stream protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamServer,
) error {
	logger.Info("Starting validate and commit stream")
	defer logger.Info("Closed validate and commit stream")
	g, gCtx := errgroup.WithContext(stream.Context())
	g.Go(func() error {
		return v.receiveAndProcessTransactions(gCtx, stream)
	})
	g.Go(func() error {
		return v.sendTransactionStatus(gCtx, stream)
	})
	return grpcerror.WrapCancelled(g.Wait())
}

func (v *VcService) receiveAndProcessTransactions(
	ctx context.Context, stream protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamServer,
) error {
	txBatchChan := channel.NewWriter(ctx, v.txBatchChan)
	for ctx.Err() == nil {
		txBatch, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "error receiving transactions")
		}

		preTxNum := txBatch.Transactions[0].TxNum
		for _, tx := range txBatch.Transactions[1:] {
			if preTxNum == tx.TxNum {
				return errors.New("duplication tx num detected")
			}
		}

		v.numBatchesReceived.Add(1)
		txBatchChan.Write(txBatch)
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

func (v *VcService) sendTransactionStatus(
	ctx context.Context, stream protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamServer,
) error {
	txBatchChan := channel.NewReader(ctx, v.txBatchChan)
	for ctx.Err() == nil {
		txBatch, ok := txBatchChan.Read()
		if !ok {
			break
		}
		txsStatus := &protoblocktx.TransactionsStatus{
			Status: make(map[string]*protoblocktx.StatusWithHeight, len(txBatch.Transactions)-v.MockFaultyNodeDropSize),
		}
		v.txsStatusMu.Lock()
		for i, tx := range txBatch.Transactions {
			if i < v.MockFaultyNodeDropSize {
				// We simulate a faulty node by not responding to the first X TXs.
				continue
			}
			code := protoblocktx.Status_COMMITTED
			if tx.PrelimInvalidTxStatus != nil {
				code = tx.PrelimInvalidTxStatus.Code
			}
			s := types.CreateStatusWithHeight(code, tx.BlockNumber, int(tx.TxNum))
			txsStatus.Status[tx.ID] = s
			v.txsStatus.addIfNotExist(tx.ID, s)
		}
		v.txsStatusMu.Unlock()

		if err := stream.Send(txsStatus); err != nil {
			return errors.Wrap(err, "error sending transaction status")
		}
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

// GetNumBatchesReceived returns the number of batches received by VcService.
func (v *VcService) GetNumBatchesReceived() uint32 {
	return v.numBatchesReceived.Load()
}

// SubmitTransactions enqueues the given transactions to a queue read by status sending goroutine.
// This methods helps the test code to bypass the stream to submit transactions to the mock
// vcservice.
func (v *VcService) SubmitTransactions(ctx context.Context, txsBatch *protovcservice.TransactionBatch) {
	channel.NewWriter(ctx, v.txBatchChan).Write(txsBatch)
}

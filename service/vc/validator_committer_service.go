/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
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
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

var logger = logging.New("validator and committer service")

// ValidatorCommitterService is the service that receives transactions from the client, prepares them,
// validates them and commits them to the database.
// It is composed of a preparer, a validator and a committer.
// The preparer receives transactions from the client and prepares them for validation.
// The validator receives prepared transactions from the preparer and validates them.
// The committer receives validated transactions from the validator and commits them to the database.
// The service also sends the status of the transactions to the client.
// ValidatorCommitterService is a gRPC service that implements the ValidationAndCommitService interface.
type ValidatorCommitterService struct {
	servicepb.UnimplementedValidationAndCommitServiceServer
	preparer                 *transactionPreparer
	validator                *transactionValidator
	committer                *transactionCommitter
	receivedTxBatch          chan *servicepb.VcBatch
	toPrepareTxs             chan *servicepb.VcBatch
	preparedTxs              chan *preparedTransactions
	validatedTxs             chan *validatedTransactions
	txsStatus                chan *committerpb.TxStatusBatch
	db                       *database
	metrics                  *perfMetrics
	minTxBatchSize           int
	timeoutForMinTxBatchSize time.Duration
	config                   *Config
	healthcheck              *health.Server

	// isStreamActive indicates whether a stream from the client (i.e., coordinator) to the vcservice
	// is currently active. We permit a maximum of one active stream at a time. If multiple
	// streams were allowed concurrently, transaction status might not reach the client
	// reliably, as we are not currently associating requests and their corresponding responses
	// (i.e., status updates) with specific streams.
	// Further, when isStreamActive is active, NumberOfWaitingTransactionsForStatus would return an
	// error as this gRPC api can be called only when the stream is inactive.
	isStreamActive atomic.Bool
}

// Limits is the struct that contains the limits of the service.
type Limits struct {
	MaxWorkersForPreparer  int
	MaxWorkersForValidator int
	MaxWorkersForCommitter int
}

// NewValidatorCommitterService creates a new ValidatorCommitterService.
// It creates the preparer, the validator and the committer.
// It also creates the channels that are used to communicate between the preparer, the validator and the committer.
// It also creates the database connection.
func NewValidatorCommitterService(
	ctx context.Context,
	config *Config,
) (*ValidatorCommitterService, error) {
	logger.Info("Initializing new validator committer service.")
	l := config.ResourceLimits

	// TODO: make queueMultiplier configurable
	queueMultiplier := 1
	receivedTxBatch := make(chan *servicepb.VcBatch, l.MaxWorkersForPreparer*queueMultiplier)
	toPrepareTxs := make(chan *servicepb.VcBatch, l.MaxWorkersForPreparer*queueMultiplier)
	preparedTxs := make(chan *preparedTransactions, l.MaxWorkersForValidator*queueMultiplier)
	validatedTxs := make(chan *validatedTransactions, queueMultiplier)
	txsStatus := make(chan *committerpb.TxStatusBatch, l.MaxWorkersForCommitter*queueMultiplier)

	metrics := newVCServiceMetrics()
	db, err := newDatabase(ctx, config.Database, metrics)
	if err != nil {
		logger.ErrorStackTrace(err)
		return nil, err
	}

	vc := &ValidatorCommitterService{
		preparer:                 newPreparer(toPrepareTxs, preparedTxs, metrics),
		validator:                newValidator(db, preparedTxs, validatedTxs, metrics),
		committer:                newCommitter(db, validatedTxs, txsStatus, metrics),
		receivedTxBatch:          receivedTxBatch,
		toPrepareTxs:             toPrepareTxs,
		preparedTxs:              preparedTxs,
		validatedTxs:             validatedTxs,
		txsStatus:                txsStatus,
		db:                       db,
		metrics:                  metrics,
		minTxBatchSize:           config.ResourceLimits.MinTransactionBatchSize,
		timeoutForMinTxBatchSize: config.ResourceLimits.TimeoutForMinTransactionBatchSize,
		config:                   config,
		healthcheck:              connection.DefaultHealthCheckService(),
	}

	return vc, nil
}

// Run starts the validator and committer service.
func (vc *ValidatorCommitterService) Run(ctx context.Context) error {
	logger.Info("Starting ValidatorCommitterService")
	defer vc.Close()
	g, eCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("Starting Prometheus monitoring server")
		_ = vc.metrics.StartPrometheusServer(
			eCtx, vc.config.Monitoring.Server, vc.monitorQueues,
		)
		// We don't return error here to avoid stopping the service due to monitoring error.
		// But we use the errgroup to ensure the method returns only when the server exits.
		return nil
	})

	g.Go(func() error {
		logger.Info("Starting transaction batching and forwarding process")
		vc.batchReceivedTransactionsAndForwardForProcessing(eCtx)
		return nil
	})

	l := vc.config.ResourceLimits
	logger.Infof("Starting %d workers for the transaction preparer", l.MaxWorkersForPreparer)
	g.Go(func() error {
		return vc.preparer.run(eCtx, l.MaxWorkersForPreparer)
	})

	logger.Infof("Starting %d workers for the transaction validator", l.MaxWorkersForValidator)
	g.Go(func() error {
		return vc.validator.run(eCtx, l.MaxWorkersForValidator)
	})

	logger.Infof("Starting %d workers for the transaction committer", l.MaxWorkersForCommitter)
	g.Go(func() error {
		return vc.committer.run(eCtx, l.MaxWorkersForCommitter)
	})

	if err := g.Wait(); err != nil {
		logger.Errorf("vcservice processing has been stopped due to err [%+v]", err)
		return err
	}
	logger.Info("ValidatorCommitterService stopped gracefully")
	return nil
}

// WaitForReady wait for the service to be ready to be exposed as gRPC service.
// If the context ended before the service is ready, returns false.
func (*ValidatorCommitterService) WaitForReady(context.Context) bool {
	return true
}

// RegisterService registers for the validator-committer's GRPC services.
func (vc *ValidatorCommitterService) RegisterService(server *grpc.Server) {
	servicepb.RegisterValidationAndCommitServiceServer(server, vc)
	healthgrpc.RegisterHealthServer(server, vc.healthcheck)
}

func (vc *ValidatorCommitterService) monitorQueues(ctx context.Context) {
	// TODO: make sampling time configurable
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		promutil.SetGauge(vc.metrics.preparerInputQueueSize, len(vc.toPrepareTxs))
		promutil.SetGauge(vc.metrics.validatorInputQueueSize, len(vc.preparedTxs))
		promutil.SetGauge(vc.metrics.committerInputQueueSize, len(vc.validatedTxs))
		promutil.SetGauge(vc.metrics.txStatusOutputQueueSize, len(vc.txsStatus))
	}
}

// SetLastCommittedBlockNumber set the last committed block number in the database/ledger.
func (vc *ValidatorCommitterService) SetLastCommittedBlockNumber(
	ctx context.Context,
	lastCommittedBlock *servicepb.BlockRef,
) (*emptypb.Empty, error) {
	err := vc.db.setLastCommittedBlockNumber(ctx, lastCommittedBlock)
	logger.ErrorStackTrace(err)
	return nil, grpcerror.WrapInternalError(err)
}

// GetNextBlockNumberToCommit get the last committed block number in the database/ledger.
func (vc *ValidatorCommitterService) GetNextBlockNumberToCommit(
	ctx context.Context,
	_ *emptypb.Empty,
) (*servicepb.BlockRef, error) {
	blkInfo, err := vc.db.getNextBlockNumberToCommit(ctx)
	logger.ErrorStackTrace(err)
	return blkInfo, grpcerror.WrapInternalError(err)
}

// GetTransactionsStatus gets the status of a given set of transaction IDs.
func (vc *ValidatorCommitterService) GetTransactionsStatus(
	ctx context.Context,
	query *committerpb.TxIDsBatch,
) (*committerpb.TxStatusBatch, error) {
	if len(query.TxIds) == 0 {
		return nil, grpcerror.WrapInvalidArgument(errors.New("query is empty"))
	}
	txIDs := make([][]byte, len(query.TxIds))
	for i, txID := range query.TxIds {
		txIDs[i] = []byte(txID)
	}

	txIDsStatus, err := vc.db.readStatusWithHeight(ctx, txIDs)
	if err != nil {
		logger.ErrorStackTrace(err)
		return nil, grpcerror.WrapInternalError(err)
	}

	return &committerpb.TxStatusBatch{Status: txIDsStatus}, nil
}

// GetNamespacePolicies retrieves the policy data from the database.
func (vc *ValidatorCommitterService) GetNamespacePolicies(
	ctx context.Context,
	_ *emptypb.Empty,
) (*applicationpb.NamespacePolicies, error) {
	policies, err := vc.db.readNamespacePolicies(ctx)
	logger.ErrorStackTrace(err)
	return policies, grpcerror.WrapInternalError(err)
}

// GetConfigTransaction retrieves the config block from the database.
func (vc *ValidatorCommitterService) GetConfigTransaction(
	ctx context.Context,
	_ *emptypb.Empty,
) (*applicationpb.ConfigTransaction, error) {
	policies, err := vc.db.readConfigTX(ctx)
	logger.ErrorStackTrace(err)
	return policies, grpcerror.WrapInternalError(err)
}

// SetupSystemTablesAndNamespaces creates the required system tables and namespaces.
func (vc *ValidatorCommitterService) SetupSystemTablesAndNamespaces(
	ctx context.Context,
	_ *emptypb.Empty,
) (*emptypb.Empty, error) {
	return nil, grpcerror.WrapInternalError(vc.db.setupSystemTablesAndNamespaces(ctx))
}

// StartValidateAndCommitStream is the function that starts the stream between the client and the service.
// It receives transactions from the client, prepares them, validates them and commits them to the database.
// It also sends the status of the transactions to the client.
func (vc *ValidatorCommitterService) StartValidateAndCommitStream(
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamServer,
) error {
	if !vc.isStreamActive.CompareAndSwap(false, true) {
		return grpcerror.WrapFailedPrecondition(utils.ErrActiveStream)
	}
	defer vc.isStreamActive.Store(false)

	g, ctx := errgroup.WithContext(stream.Context())

	g.Go(func() error {
		logger.Info("Started a goroutine to receive and process transactions")
		return vc.receiveTransactions(ctx, stream)
	})

	g.Go(func() error {
		logger.Info("Started a goroutine to send transaction status to the submitter")
		return vc.sendTransactionStatus(ctx, stream)
	})

	err := g.Wait()
	logger.ErrorStackTrace(err)
	return grpcerror.WrapInternalError(err)
}

func (vc *ValidatorCommitterService) receiveTransactions(
	ctx context.Context,
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamServer,
) error {
	for ctx.Err() == nil {
		b, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "failed to receive transactions from the coordinator")
		}
		txCount := len(b.Transactions)
		logger.Debugf("Received batch of %d transactions", txCount)
		promutil.AddToCounter(vc.metrics.transactionReceivedTotal, txCount)
		vc.receivedTxBatch <- b
	}

	return nil
}

func (vc *ValidatorCommitterService) batchReceivedTransactionsAndForwardForProcessing(ctx context.Context) {
	largerBatch := &servicepb.VcBatch{}
	timer := time.NewTimer(vc.timeoutForMinTxBatchSize)
	defer timer.Stop()
	toPrepareTxs := channel.NewWriter(ctx, vc.toPrepareTxs)

	sendLargeBatch := func() {
		defer timer.Reset(vc.timeoutForMinTxBatchSize)
		if len(largerBatch.Transactions) == 0 {
			return
		}
		if ok := toPrepareTxs.Write(largerBatch); !ok {
			return
		}
		largerBatch = &servicepb.VcBatch{}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			sendLargeBatch()
		case txBatch, ok := <-vc.receivedTxBatch:
			if !ok {
				return
			}
			largerBatch.Transactions = append(largerBatch.Transactions, txBatch.Transactions...)
			logger.Debugf("New batch with %d TXs received in vc."+
				" Large batch contains %d TXs and the minimum batch size is %d",
				len(txBatch.Transactions), len(txBatch.Transactions)+len(largerBatch.Transactions),
				vc.minTxBatchSize)
			if len(largerBatch.Transactions) >= vc.minTxBatchSize {
				sendLargeBatch()
			}
		}
	}
}

// sendTransactionStatus sends the status of the transactions to the client.
func (vc *ValidatorCommitterService) sendTransactionStatus(
	ctx context.Context,
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamServer,
) error {
	logger.Info("Send transaction status")

	txsStatus := channel.NewReader(ctx, vc.txsStatus)
	for {
		txStatus, ok := txsStatus.Read()
		if !ok {
			return nil
		}

		if err := stream.Send(txStatus); err != nil {
			return errors.Wrap(err, "failed to send transactions status to the coordinator")
		}
		committed := 0
		mvcc := 0
		dup := 0
		for _, s := range txStatus.Status {
			switch s.Status { //nolint:revive // default case is not needed.
			case committerpb.Status_COMMITTED:
				committed++
			case committerpb.Status_ABORTED_MVCC_CONFLICT:
				mvcc++
			case committerpb.Status_REJECTED_DUPLICATE_TX_ID:
				dup++
			}
		}

		logger.Debugf("Sent transaction status update: Committed: %d, MVCC Conflicts: %d, Duplicates: %d, Total: %d",
			committed, mvcc, dup, len(txStatus.Status))
		promutil.AddToCounter(vc.metrics.transactionCommittedTotal, committed)
		promutil.AddToCounter(vc.metrics.transactionMVCCConflictTotal, mvcc)
		promutil.AddToCounter(vc.metrics.transactionDuplicateTxTotal, dup)
		promutil.AddToCounter(vc.metrics.transactionProcessedTotal, len(txStatus.Status))
	}
}

// Close is closing the db connection.
func (vc *ValidatorCommitterService) Close() {
	vc.db.close()
}

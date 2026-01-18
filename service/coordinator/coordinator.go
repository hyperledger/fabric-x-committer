/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
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
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

var logger = logging.New("coordinator service")

type (
	// Service is responsible for coordinating signature verification, dependency tracking, and
	// validation and commit of each transaction.
	Service struct {
		servicepb.UnimplementedCoordinatorServer
		dependencyMgr         *dependencygraph.Manager
		signatureVerifierMgr  *signatureVerifierManager
		validatorCommitterMgr *validatorCommitterManager
		policyMgr             *policyManager
		queues                *channels
		config                *Config
		metrics               *perfMetrics

		// initializationDone is used to find out whether the coordinator service has
		// been initialized or not.
		initializationDone *channel.Ready

		// streamActive guards against concurrent streams from the client (sidecar)
		// to the coordinator and prevents conflicting operations while a stream is
		// active. Only one active stream is allowed at a time to ensure reliable
		// delivery of transaction status updates. Currently, requests and their
		// corresponding responses are not associated with specific streams, so
		// multiple concurrent streams could lead to unreliable status delivery.
		streamActive sync.RWMutex

		numWaitingTxsForStatus *atomic.Int32

		txBatchIDToDepGraph uint64

		healthcheck *health.Server
	}

	channels struct {
		// sender: coordinator sends received transactions to this channel.
		// receiver: dependency graph manager receives transactions batch from this channel and construct dependency
		//           graph.
		coordinatorToDepGraphTxs chan *dependencygraph.TransactionBatch

		// sender: dependency graph manager sends dependency free transactions nodes to this channel.
		// receiver: signature verifier manager receives dependency free transactions nodes from this channel.
		depGraphToSigVerifierFreeTxs chan dependencygraph.TxNodeBatch

		// sender: signature verifier manager sends valid transactions to this channel.
		// receiver: validator-committer manager receives valid transactions from this channel.
		sigVerifierToVCServiceValidatedTxs chan dependencygraph.TxNodeBatch

		// sender: validator committer manager sends validated transactions nodes to this channel. For each validator
		// 	       committer server, there is a goroutine that sends validated transactions nodes to this channel.
		// receiver: dependency graph manager receives validated transactions nodes from this channel and update
		//           the dependency graph.
		// TODO: As signValidatedTxsNode and vcServiceToDepGraphValidatedTxs can cause confusion, it would be better to
		//       rename vcServiceToDepGraphValidatedTxs to committedOrAbortedTxsNode.
		vcServiceToDepGraphValidatedTxs chan dependencygraph.TxNodeBatch

		// sender: validator committer manager sends transaction status to this channel. For each validator committer
		// 	       server, there is a goroutine that sends transaction status to this channel.
		// receiver: coordinator receives transaction status from this channel and forwards them to the sidecar.
		vcServiceToCoordinatorTxStatus chan *committerpb.TxStatusBatch
	}
)

var (
	// ErrActiveStreamWaitingTransactions is returned when NumberOfWaitingTransactionsForStatus is called
	// while a stream is active. This value cannot be reliably determined in this state.
	ErrActiveStreamWaitingTransactions = errors.New("cannot determine number of waiting transactions for " +
		"status while stream is active")

	// ErrExistingStreamOrConflictingOp indicates that a stream cannot be created because a stream already exists
	// or a conflicting gRPC API call is being made concurrently.
	ErrExistingStreamOrConflictingOp = errors.New("stream already exists or conflicting operation in progress")
)

// NewCoordinatorService creates a new coordinator service.
func NewCoordinatorService(c *Config) *Service {
	// We need to calculate the buffer size for each channel based on the number of goroutines accessing the channel
	// in each manager. For sign verifier manager and validator committer manager, we have a goroutine per server to
	// read from and write to the channel. Hence, we define a buffer size for each manager by multiplying the number
	// of servers with the buffer size per goroutine. For dependency graph manager, we have a goroutine for each
	// local dependency constructors. Hence, we define a buffer size for dependency graph manager by multiplying the
	// number of local dependency constructors with the buffer size per goroutine. We follow this approach to avoid
	// giving too many configurations to the user as it would add complexity to the user experience.
	bufSzPerChanForSignVerifierMgr := c.ChannelBufferSizePerGoroutine * len(c.Verifier.Endpoints)
	bufSzPerChanForValCommitMgr := c.ChannelBufferSizePerGoroutine * len(c.ValidatorCommitter.Endpoints)
	bufSzPerChanForLocalDepMgr := c.ChannelBufferSizePerGoroutine * c.DependencyGraph.NumOfLocalDepConstructors

	queues := &channels{
		coordinatorToDepGraphTxs:           make(chan *dependencygraph.TransactionBatch, bufSzPerChanForLocalDepMgr),
		depGraphToSigVerifierFreeTxs:       make(chan dependencygraph.TxNodeBatch, bufSzPerChanForValCommitMgr),
		sigVerifierToVCServiceValidatedTxs: make(chan dependencygraph.TxNodeBatch, bufSzPerChanForSignVerifierMgr),
		vcServiceToDepGraphValidatedTxs:    make(chan dependencygraph.TxNodeBatch, bufSzPerChanForValCommitMgr),
		vcServiceToCoordinatorTxStatus:     make(chan *committerpb.TxStatusBatch, bufSzPerChanForValCommitMgr),
	}

	metrics := newPerformanceMetrics()

	depMgr := dependencygraph.NewManager(
		&dependencygraph.Parameters{
			IncomingTxs:               queues.coordinatorToDepGraphTxs,
			OutgoingDepFreeTxsNode:    queues.depGraphToSigVerifierFreeTxs,
			IncomingValidatedTxsNode:  queues.vcServiceToDepGraphValidatedTxs,
			NumOfLocalDepConstructors: c.DependencyGraph.NumOfLocalDepConstructors,
			WaitingTxsLimit:           c.DependencyGraph.WaitingTxsLimit,
			PrometheusMetricsProvider: metrics.Provider,
		},
	)

	policyMgr := newPolicyManager()

	svMgr := newSignatureVerifierManager(
		&signVerifierManagerConfig{
			clientConfig:             &c.Verifier,
			incomingTxsForValidation: queues.depGraphToSigVerifierFreeTxs,
			outgoingValidatedTxs:     queues.sigVerifierToVCServiceValidatedTxs,
			metrics:                  metrics,
			policyManager:            policyMgr,
		},
	)

	vcMgr := newValidatorCommitterManager(
		&validatorCommitterManagerConfig{
			clientConfig:                   &c.ValidatorCommitter,
			incomingTxsForValidationCommit: queues.sigVerifierToVCServiceValidatedTxs,
			outgoingValidatedTxsNode:       queues.vcServiceToDepGraphValidatedTxs,
			outgoingTxsStatus:              queues.vcServiceToCoordinatorTxStatus,
			metrics:                        metrics,
			policyMgr:                      policyMgr,
		},
	)

	return &Service{
		dependencyMgr:          depMgr,
		signatureVerifierMgr:   svMgr,
		validatorCommitterMgr:  vcMgr,
		policyMgr:              policyMgr,
		queues:                 queues,
		config:                 c,
		metrics:                metrics,
		initializationDone:     channel.NewReady(),
		numWaitingTxsForStatus: &atomic.Int32{},
		txBatchIDToDepGraph:    1,
		healthcheck:            connection.DefaultHealthCheckService(),
	}
}

// Run starts each manager in the coordinator service.
func (c *Service) Run(ctx context.Context) error {
	canCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, eCtx := errgroup.WithContext(canCtx)

	g.Go(func() error {
		_ = c.metrics.StartPrometheusServer(eCtx, c.config.Monitoring.Server, c.monitorQueues)
		// We don't return error here to avoid stopping the service due to monitoring error.
		// But we use the errgroup to ensure the method returns only when the server exits.
		return nil
	})

	g.Go(func() error {
		logger.Info("Starting dependency graph manager")
		c.dependencyMgr.Run(eCtx)
		return nil
	})

	g.Go(func() error {
		logger.Info("Starting signature verifier manager")
		if err := c.signatureVerifierMgr.run(eCtx); err != nil {
			logger.Errorf("coordinator service stops due to an error returned by signature verifier manager: %v", err)
			return err
		}
		return nil
	})

	g.Go(func() error {
		logger.Info("Starting validator committer manager")
		if err := c.validatorCommitterMgr.run(eCtx); err != nil {
			logger.Errorf("coordinator service stopds due to an error returned by validator committer manager: %v", err)
			return err
		}
		return nil
	})

	if !c.validatorCommitterMgr.ready.WaitForReady(eCtx) {
		return g.Wait()
	}

	// We attempt to recover the policy manager and the last committed block number from the state DB.
	if err := c.validatorCommitterMgr.recoverPolicyManagerFromStateDB(ctx); err != nil {
		return err
	}

	c.initializationDone.SignalReady()

	return utils.ProcessErr(g.Wait(), "coordinator processing has been stopped due to err")
}

// WaitForReady wait for coordinator to be ready to be exposed as gRPC service.
// If the context ended before the service is ready, returns false.
func (c *Service) WaitForReady(ctx context.Context) bool {
	// NOTE: We set `initializationDone` only if signature verifier manager
	//       and vcservice manager are ready. Hence, we can just wait for
	//       the coordinator initialization.
	return c.initializationDone.WaitForReady(ctx)
}

// RegisterService registers for the coordinator's GRPC services.
func (c *Service) RegisterService(server *grpc.Server) {
	servicepb.RegisterCoordinatorServer(server, c)
	healthgrpc.RegisterHealthServer(server, c.healthcheck)
}

// GetConfigTransaction get the config transaction from the state DB.
func (c *Service) GetConfigTransaction(
	ctx context.Context, _ *emptypb.Empty,
) (*applicationpb.ConfigTransaction, error) {
	// Error is already wrapped with proper gRPC status code by validatorCommitterMgr.
	return c.validatorCommitterMgr.getConfigTransaction(ctx)
}

// SetLastCommittedBlockNumber set the last committed block number in the database/ledger through a vcservice.
func (c *Service) SetLastCommittedBlockNumber(
	ctx context.Context,
	lastBlock *servicepb.BlockRef,
) (*emptypb.Empty, error) {
	// Error is already wrapped with proper gRPC status code by validatorCommitterMgr.
	return &emptypb.Empty{}, c.validatorCommitterMgr.setLastCommittedBlockNumber(ctx, lastBlock)
}

// GetNextBlockNumberToCommit returns the next expected block number to be received by the coordinator.
func (c *Service) GetNextBlockNumberToCommit(
	ctx context.Context,
	_ *emptypb.Empty,
) (*servicepb.BlockRef, error) {
	// Error is already wrapped with proper gRPC status code by validatorCommitterMgr.
	return c.validatorCommitterMgr.getNextBlockNumberToCommit(ctx)
}

// GetTransactionsStatus returns the status of given transactions identifiers.
func (c *Service) GetTransactionsStatus(
	ctx context.Context,
	q *committerpb.TxIDsBatch,
) (*committerpb.TxStatusBatch, error) {
	// Error is already wrapped with proper gRPC status code by validatorCommitterMgr.
	return c.validatorCommitterMgr.getTransactionsStatus(ctx, q)
}

// NumberOfWaitingTransactionsForStatus returns the number of transactions waiting to get the final status.
func (c *Service) NumberOfWaitingTransactionsForStatus(
	context.Context,
	*emptypb.Empty,
) (*servicepb.WaitingTransactions, error) {
	if !c.streamActive.TryLock() {
		return nil, grpcerror.WrapFailedPrecondition(ErrActiveStreamWaitingTransactions)
	}
	defer c.streamActive.Unlock()

	return &servicepb.WaitingTransactions{
		Count: c.numWaitingTxsForStatus.Load() - int32(len(c.queues.vcServiceToCoordinatorTxStatus)), //nolint:gosec
	}, nil
}

// BlockProcessing receives a stream of blocks from the client and processes them.
func (c *Service) BlockProcessing(
	stream servicepb.Coordinator_BlockProcessingServer,
) error {
	if !c.streamActive.TryLock() {
		return grpcerror.WrapFailedPrecondition(ErrExistingStreamOrConflictingOp)
	}
	defer c.streamActive.Unlock()

	logger.Info("Start validate and commit stream")

	g, eCtx := errgroup.WithContext(stream.Context())

	// NOTE: The below two goroutines are tightly coupled to the lifecycle of the stream.
	//       They terminate when the stream ends. Other goroutines within the coordinator
	//       service can safely continue operating to process existing blocks and enqueue
	//       transaction statuses, even after the stream concludes.

	g.Go(func() error {
		logger.Info("Started a goroutine to receive block and forward it to the dependency graph manager")
		if err := c.receiveAndProcessBlock(eCtx, stream); err != nil {
			logger.Warnf("stream to the coordinator is ending with an error: %v", err)
			return err
		}
		return nil
	})

	g.Go(func() error {
		logger.Info("Started a goroutine to receive transaction status from validator committer manager and" +
			" forward the status to client")
		if err := c.sendTxStatus(eCtx, stream); err != nil {
			logger.Warnf("stream to the coordinator is ending with an error: %v", err)
		}
		return nil
	})

	return utils.ProcessErr(g.Wait(), "stream with the sidecar has ended")
}

func (c *Service) receiveAndProcessBlock(
	ctx context.Context,
	stream servicepb.Coordinator_BlockProcessingServer,
) error {
	txsBatchForDependencyGraph := channel.NewWriter(ctx, c.queues.coordinatorToDepGraphTxs)
	txBatchForVcService := channel.NewWriter(ctx, c.queues.sigVerifierToVCServiceValidatedTxs)

	for ctx.Err() == nil {
		blk, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "failed to receive blocks from the sidecar")
		}
		logger.Debugf("Coordinator received a batch with %d TXs", len(blk.Txs))

		// NOTE: Block processing is decoupled from the stream's lifecycle.
		// Even if the stream terminates unexpectedly, any received blocks will
		// still be forwarded to downstream components for complete processing.

		promutil.AddToCounter(c.metrics.transactionReceivedTotal, len(blk.Txs)+len(blk.Rejected))
		c.numWaitingTxsForStatus.Add(int32(len(blk.Txs))) //nolint:gosec

		if len(blk.Txs) > 0 {
			// TODO: make it configurable.
			chunkSizeForDepGraph := min(c.config.DependencyGraph.WaitingTxsLimit, 500)
			for i := 0; i < len(blk.Txs); i += chunkSizeForDepGraph {
				end := min(i+chunkSizeForDepGraph, len(blk.Txs))
				txsBatchForDependencyGraph.Write(&dependencygraph.TransactionBatch{
					ID:  c.txBatchIDToDepGraph,
					Txs: blk.Txs[i:end],
				})
				c.txBatchIDToDepGraph++
			}
		}

		if len(blk.Rejected) > 0 {
			rejected := make(dependencygraph.TxNodeBatch, len(blk.Rejected))
			for i, tx := range blk.Rejected {
				rejected[i] = dependencygraph.NewRejectedTransactionNode(tx)
			}
			txBatchForVcService.Write(rejected)
		}
	}

	return nil
}

func (c *Service) sendTxStatus(
	ctx context.Context,
	stream servicepb.Coordinator_BlockProcessingServer,
) error {
	txsStatus := channel.NewReader(ctx, c.queues.vcServiceToCoordinatorTxStatus)
	for {
		txStatus, ctxAlive := txsStatus.Read()
		if !ctxAlive {
			return nil
		}
		c.numWaitingTxsForStatus.Add(-int32(len(txStatus.Status))) //nolint:gosec

		if err := stream.Send(txStatus); err != nil {
			return errors.Wrap(err, "failed to send transaction status batch to the sidecar")
		}

		logger.Debugf("Batch with %d TX statuses forwarded to output stream.", len(txStatus.Status))

		statusCount := make(map[committerpb.Status]int)
		for _, s := range txStatus.Status {
			statusCount[s.Status]++
		}
		m := c.metrics
		for status, count := range statusCount {
			promutil.AddToCounter(m.transactionCommittedTotal.WithLabelValues(status.String()), count)
		}
	}
}

func (c *Service) monitorQueues(ctx context.Context) {
	// TODO: make sampling time configurable
	ticker := time.NewTicker(100 * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		m := c.metrics
		q := c.queues
		promutil.SetGauge(m.sigverifierInputTxBatchQueueSize, len(q.depGraphToSigVerifierFreeTxs))
		promutil.SetGauge(m.sigverifierOutputValidatedTxBatchQueueSize, len(q.sigVerifierToVCServiceValidatedTxs))
		promutil.SetGauge(m.vcserviceOutputValidatedTxBatchQueueSize, len(q.vcServiceToDepGraphValidatedTxs))
		promutil.SetGauge(m.vcserviceOutputTxStatusBatchQueueSize, len(q.vcServiceToCoordinatorTxStatus))
	}
}

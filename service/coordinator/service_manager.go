/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

type (
	// serviceManager manages the communication with a pool of service instances that
	// process transaction batches: it sends batches to them, applies the returned
	// statuses to the transaction nodes, and forwards the results onward.
	//
	// Both the signature verifier manager and the validator-committer manager are
	// instances of it; the per-service behavior lives in their serviceAdaptor.
	serviceManager struct {
		params serviceManagerParams
		// workers holds all the active workers. It is not read by the manager itself,
		// only by tests.
		workers atomic.Pointer[[]*serviceWorker]
	}

	// serviceManagerParams collects the wiring of a single service manager.
	serviceManagerParams struct {
		clientConfig *connection.MultiClientConfig
		adaptor      serviceAdaptor
		incomingTxs  <-chan dependencygraph.TxNodeBatch
		outgoingTxs  chan<- dependencygraph.TxNodeBatch
		// outgoingResults receives the status batches produced by the workers. It is nil for
		// managers that do not forward statuses, such as the verifier manager.
		outgoingResults *txStatusQueue
		metrics         *managerMetrics
	}

	// serviceAdaptor adapts the shared manager to a specific service.
	serviceAdaptor interface {
		// NewStream creates a new independent stream for a worker.
		NewStream(ctx context.Context, conn *grpc.ClientConn) (serviceStream, error)
		// ApplyResult applies a returned status to its transaction node.
		ApplyResult(txNode *dependencygraph.TransactionNode, status *committerpb.TxStatus)
	}

	// serviceStream adapts a specific service's bidirectional stream to the shared manager.
	serviceStream interface {
		// Send submits a transaction batch on the stream.
		Send(txsNode dependencygraph.TxNodeBatch) error
		// Recv receives a status batch from the stream.
		Recv() ([]*committerpb.TxStatus, error)
	}

	// serviceWorker handles the communication with a single service instance.
	serviceWorker struct {
		conn   *grpc.ClientConn
		params *serviceManagerParams

		// txBeingProcessed stores the transactions currently being processed by this service
		// instance, so a returned status can be matched back to its transaction node.
		txBeingProcessed utils.SyncMap[servicepb.Height, *dependencygraph.TransactionNode]
	}
)

// run starts the service manager and all its workers.
func (m *serviceManager) run(ctx context.Context) error {
	p := m.params

	dCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, eCtx := errgroup.WithContext(dCtx)

	// Create internal transaction queue.
	txQueue := channel.NewReaderWriter(eCtx, make(chan dependencygraph.TxNodeBatch, cap(p.outgoingTxs)))
	g.Go(func() error {
		ingestIncomingTxsToInternalQueue(channel.NewReader(eCtx, p.incomingTxs), txQueue)
		return nil
	})

	// Create connections to all service endpoints.
	connections, connErr := connection.NewConnectionPerEndpoint(p.clientConfig)
	if connErr != nil {
		return fmt.Errorf("failed to create connections: %w", connErr)
	}
	defer connection.CloseConnectionsLog(connections...)
	logger.Infof("Connections to %d services will be opened", len(connections))

	// Start a worker for each connection.
	workers := make([]*serviceWorker, len(connections))
	for i, conn := range connections {
		label := conn.CanonicalTarget()
		p.metrics.connection.Disconnected(label)

		w := &serviceWorker{
			conn:   conn,
			params: &m.params,
		}
		// We store the workers for tests.
		workers[i] = w

		g.Go(func() error {
			return retry.Sustain(eCtx, p.clientConfig.Retry, func() error {
				defer w.recoverPendingTransactions(txQueue)
				return w.processTxsAndForwardResults(eCtx, txQueue)
			})
		})
	}

	// We assign the array only after it is filled.
	m.workers.Store(&workers)

	return utils.ProcessErr(g.Wait(), "service manager failed")
}

func ingestIncomingTxsToInternalQueue(
	incomingTxs channel.Reader[dependencygraph.TxNodeBatch],
	txQueue channel.Writer[dependencygraph.TxNodeBatch],
) {
	for {
		txsNode, ctxAlive := incomingTxs.Read()
		if !ctxAlive {
			return
		}
		txQueue.Write(txsNode)
	}
}

// processTxsAndForwardResults manages a single bidirectional stream with the service.
func (w *serviceWorker) processTxsAndForwardResults(
	ctx context.Context,
	inputTxs channel.ReaderWriter[dependencygraph.TxNodeBatch],
) error {
	label := w.conn.CanonicalTarget()
	defer w.params.metrics.connection.Disconnected(label)

	g, gCtx := errgroup.WithContext(ctx)

	// We create a new stream for each attempt to reset the adaptor's stream state (if any).
	stream, err := w.params.adaptor.NewStream(gCtx, w.conn)
	if err != nil {
		return errors.Join(retry.ErrBackOff, err)
	}

	// If the stream is started, the connection has been established.
	w.params.metrics.connection.Connected(label)

	// NOTE: sendTxsToService and receiveResultsAndForward must always return an error on exit.
	g.Go(func() error {
		return w.sendTxsToService(stream, inputTxs.WithContext(gCtx))
	})

	g.Go(func() error {
		// NOTE: The output writes must not depend on the stream context, so `ctx` is the
		//       manager's context rather than gCtx. receiveResultsAndForward removes a
		//       transaction from txBeingProcessed before its results are queued. Were the
		//       writes bound to the stream, every stream failure would abort them mid-batch,
		//       and a batch whose statuses were already queued would be re-queued and
		//       processed again. The service is idempotent, so it would re-emit those
		//       statuses, and Service.numTxsInProgress would be decremented twice for one
		//       transaction, breaking the `numTxsInProgress >= readyCount >= 0` invariant
		//       that NoPendingTransactionProcessing relies on to report idle.
		return w.receiveResultsAndForward(ctx, stream, channel.NewWriter(ctx, w.params.outgoingTxs))
	})

	return utils.ProcessErr(g.Wait(), "worker stream processing failed")
}

func (w *serviceWorker) sendTxsToService(
	stream serviceStream,
	inputTxs channel.Reader[dependencygraph.TxNodeBatch],
) error {
	firstBatch := true
	for {
		txsNode, ctxAlive := inputTxs.Read()
		if !ctxAlive {
			return errors.Wrap(inputTxs.Context().Err(), "context ended")
		}
		if len(txsNode) == 0 {
			continue
		}

		w.addTxsBeingProcessed(txsNode)

		txBatches := []dependencygraph.TxNodeBatch{txsNode}
		if firstBatch {
			txBatches = splitBatchByBlock(txsNode)
			firstBatch = false
		}

		for _, tb := range txBatches {
			if err := stream.Send(tb); err != nil {
				return errors.Wrap(err, "sending to stream failed")
			}
		}
	}
}

// splitBatchByBlock splits a batch by block number to avoid gRPC message size limits.
// We group transactions by block to ensure our batch sizes do not exceed the gRPC message limit.
// This strategy prevents RESOURCE_EXHAUSTED errors because the orderer's maximum block size
// will be configured to be safely smaller than the gRPC send/receive limit.
func splitBatchByBlock(txsNode dependencygraph.TxNodeBatch) []dependencygraph.TxNodeBatch {
	blkToBatch := make(map[uint64]dependencygraph.TxNodeBatch)
	for _, txNode := range txsNode {
		blkNum := txNode.VCTx.Ref.BlockNum
		txBatch, ok := blkToBatch[blkNum]
		if !ok {
			txBatch = make(dependencygraph.TxNodeBatch, 0, len(txsNode))
		}
		blkToBatch[blkNum] = append(txBatch, txNode)
	}

	if len(blkToBatch) < 2 {
		return []dependencygraph.TxNodeBatch{txsNode}
	}

	return slices.Collect(maps.Values(blkToBatch))
}

func (w *serviceWorker) receiveResultsAndForward(
	ctx context.Context,
	stream serviceStream,
	outputTxsNode channel.Writer[dependencygraph.TxNodeBatch],
) error {
	for {
		statusBatch, err := stream.Recv()
		if err != nil {
			return classifyStreamRecvError(err)
		}

		txsNode, untrackedIdx := w.getTxsAndApplyResults(statusBatch)
		statusBatch = dropUntrackedStatuses(statusBatch, untrackedIdx)
		if len(statusBatch) == 0 {
			continue
		}

		// NOTE: getTxsAndApplyResults removes the transactions from txBeingProcessed before their
		//       results are queued, so a failed write must return them to the map. Otherwise
		//       recoverPendingTransactions has nothing to re-queue and the transactions are lost:
		//       their status never reaches the sidecar, and their nodes never free their dependents
		//       in the dependency graph.
		// NOTE: The statuses are written first, and the nodes are forwarded only if that
		//       succeeded (`||` short-circuits), so the dependency graph never frees the
		//       dependents of a batch whose statuses were dropped. The reverse order of failure —
		//       statuses delivered, nodes dropped, batch re-queued — would deliver those statuses
		//       twice; what rules it out is that both writes are bound to the manager's context,
		//       see processTxsAndForwardResults.
		if !w.writeResults(ctx, statusBatch) || !outputTxsNode.Write(txsNode) {
			w.addTxsBeingProcessed(txsNode)
			return errors.Wrap(ctx.Err(), "context ended")
		}

		promutil.AddToCounter(w.params.metrics.processedTotal, len(statusBatch))
	}
}

// classifyStreamRecvError maps a receive error from a service stream to a retry decision. An
// InvalidArgument is not retryable: it means the request we sent can never be accepted, which
// points at corrupted or altered state, or at a bug in the committer. Every other error ends
// the stream and is retried under the sustain policy.
func classifyStreamRecvError(err error) error {
	if grpcerror.HasCode(err, codes.InvalidArgument) {
		return errors.Join(retry.ErrNonRetryable, err)
	}
	// The stream ended or the manager was closed.
	return errors.Wrap(err, "receive from stream ended with error")
}

// dropUntrackedStatuses removes the statuses at the given indices, which have no transaction node
// to be matched with. They can occur only when the service restarts, as it might receive the same
// transaction twice and report its status twice, while the txBeingProcessed lookup succeeds only
// once. Negligible performance impact is fine as this is a rare case.
func dropUntrackedStatuses(statusBatch []*committerpb.TxStatus, untrackedIdx []int) []*committerpb.TxStatus {
	for _, i := range slices.Backward(untrackedIdx) {
		statusBatch = append(statusBatch[:i], statusBatch[i+1:]...)
	}
	return statusBatch
}

// writeResults forwards the status batch to the manager's result queue.
// It is a no-op for managers that do not forward statuses, such as the verifier manager.
//
// NOTE: The statuses the validator-committer manager queues here are read by the coordinator and
// sent back to the sidecar, which is also where the transactions came from. Although there is a
// cycle in the producer-consumer flow (sidecar -> coordinator -> sidecar), this is not an issue.
// If the sidecar becomes bottlenecked and cannot receive the statuses quickly, the gRPC flow
// control will activate and slow down the whole system, allowing the sidecar to catch up.
func (w *serviceWorker) writeResults(ctx context.Context, statusBatch []*committerpb.TxStatus) bool {
	if w.params.outgoingResults == nil {
		return true
	}
	return w.params.outgoingResults.write(ctx, &committerpb.TxStatusBatch{Status: statusBatch})
}

func (w *serviceWorker) getTxsAndApplyResults(statusBatch []*committerpb.TxStatus) (
	txsNode dependencygraph.TxNodeBatch, untrackedIdx []int,
) {
	txsNode = make(dependencygraph.TxNodeBatch, 0, len(statusBatch))
	for i, txStatus := range statusBatch {
		txNode, ok := w.txBeingProcessed.LoadAndDelete(*servicepb.NewHeightFromTxRef(txStatus.Ref))
		if !ok {
			untrackedIdx = append(untrackedIdx, i)
			continue
		}
		w.params.adaptor.ApplyResult(txNode, txStatus)
		txsNode = append(txsNode, txNode)
	}
	return txsNode, untrackedIdx
}

func (w *serviceWorker) addTxsBeingProcessed(txsNode dependencygraph.TxNodeBatch) {
	for _, txNode := range txsNode {
		// VCTx.Ref is always populated for both regular and rejected nodes, whereas
		// VerifierTx may be nil. Both refer to the same TxRef, so it is the safe key.
		w.txBeingProcessed.Store(*servicepb.NewHeightFromTxRef(txNode.VCTx.Ref), txNode)
	}
}

func (w *serviceWorker) recoverPendingTransactions(txQueue channel.Writer[dependencygraph.TxNodeBatch]) {
	pendingTxs := slices.Collect(w.txBeingProcessed.IterValues())
	w.txBeingProcessed.Clear()

	if len(pendingTxs) == 0 {
		return
	}

	promutil.AddToCounter(w.params.metrics.retriedTotal, len(pendingTxs))
	txQueue.Write(pendingTxs)
}

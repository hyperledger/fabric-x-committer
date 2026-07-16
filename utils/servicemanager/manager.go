/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package servicemanager contains a generic service task manager implementation.
// The current implementation is specific for the coordinator types, but it can be modified
// to support any types if required in the future.
package servicemanager

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"time"

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
	// Manager manages multiple service workers that process tasks.
	Manager struct {
		Params Parameters
		// workers holds all the active workers. It is not used internally, but can be used for testing.
		workers atomic.Pointer[[]*worker]
	}

	// Parameters for the generic service manager.
	Parameters struct {
		ClientConfig    *connection.MultiClientConfig
		IncomingTasks   <-chan []*dependencygraph.TransactionNode
		OutgoingTasks   chan<- []*dependencygraph.TransactionNode
		OutgoingResults ResultWriter
		Adaptor         Adaptor
		Metrics         *Metrics
	}

	// ResultWriter receives the result batches produced by the workers.
	// It is an interface (rather than a plain channel) so callers can attach
	// their own accounting around the enqueue, e.g., the coordinator counts
	// statuses before enqueueing to track the number of in-flight transactions.
	// Implementations must bind their own destination; the manager only passes
	// the worker context so the write can be canceled when the worker stops.
	// A nil ResultWriter is treated as a no-op (used by managers that do not
	// forward results, such as the verifier manager).
	ResultWriter interface {
		// Write enqueues a result batch. It returns false if the context is
		// done before the batch could be enqueued.
		Write(ctx context.Context, results []*committerpb.TxStatus) bool
	}

	// Adaptor is used to adapt the general implementation to the specific use case.
	Adaptor interface {
		// NewStream creates a new independent stream for a worker.
		NewStream(ctx context.Context, conn *grpc.ClientConn) (Stream, error)
		// ApplyResult applies a result to the task.
		ApplyResult(task *dependencygraph.TransactionNode, result *committerpb.TxStatus)
	}

	// Stream is used to adopt the general stream to the specific use case.
	Stream interface {
		// Send submit a task batch on the stream.
		Send(tasks []*dependencygraph.TransactionNode) error
		// Recv receives a result batch from the stream.
		Recv() ([]*committerpb.TxStatus, error)
	}

	// worker handles communication with a single service instance.
	// tasksBeingProcessed stores tasks currently being processed by this worker.
	worker struct {
		conn                *grpc.ClientConn
		params              *Parameters
		tasksBeingProcessed utils.SyncMap[servicepb.Height, *dependencygraph.TransactionNode]
	}
)

// Run starts the service manager and all workers.
func (m *Manager) Run(ctx context.Context) error {
	p := m.Params

	dCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, eCtx := errgroup.WithContext(dCtx)
	g.Go(func() error {
		m.monitorQueues(eCtx)
		return nil
	})

	// Create internal task queue.
	taskQueue := channel.NewReaderWriter(eCtx, make(chan []*dependencygraph.TransactionNode, cap(p.OutgoingTasks)))
	g.Go(func() error {
		ingestTasksToInternalQueue(
			channel.NewReader(eCtx, p.IncomingTasks),
			taskQueue,
		)
		return nil
	})

	// Create connections to all service endpoints.
	connections, connErr := connection.NewConnectionPerEndpoint(p.ClientConfig)
	if connErr != nil {
		return fmt.Errorf("failed to create connections: %w", connErr)
	}
	defer connection.CloseConnectionsLog(connections...)

	// Start a worker for each connection
	workers := make([]*worker, len(connections))
	for i, conn := range connections {
		label := conn.CanonicalTarget()
		p.Metrics.Connection.Disconnected(label)

		w := &worker{
			conn:   conn,
			params: &m.Params,
		}
		// We store the workers for tests.
		workers[i] = w

		g.Go(func() error {
			return retry.Sustain(eCtx, p.ClientConfig.Retry, func() error {
				defer w.recoverPendingTasks(taskQueue)
				return w.processTasksAndForwardResults(eCtx, taskQueue)
			})
		})
	}

	// We assign the array only after it is filled.
	m.workers.Store(&workers)

	return utils.ProcessErr(g.Wait(), "service manager failed")
}

func ingestTasksToInternalQueue(
	incomingTasks channel.Reader[[]*dependencygraph.TransactionNode],
	taskQueue channel.Writer[[]*dependencygraph.TransactionNode],
) {
	for {
		tasks, ctxAlive := incomingTasks.Read()
		if !ctxAlive {
			return
		}
		taskQueue.Write(tasks)
	}
}

// processTasksAndForwardResults manages the bidirectional stream with the service.
func (w *worker) processTasksAndForwardResults(
	ctx context.Context,
	inputJobs channel.ReaderWriter[[]*dependencygraph.TransactionNode],
) error {
	label := w.conn.CanonicalTarget()
	defer w.params.Metrics.Connection.Disconnected(label)

	g, gCtx := errgroup.WithContext(ctx)
	p := w.params

	// We create a new adaptor for each new stream to reset the adaptor state (if exist).
	stream, err := w.params.Adaptor.NewStream(gCtx, w.conn)
	if err != nil {
		return errors.Join(retry.ErrBackOff, err)
	}

	w.params.Metrics.Connection.Connected(label)

	g.Go(func() error {
		return w.sendTasksToService(stream, inputJobs.WithContext(gCtx))
	})

	g.Go(func() error {
		outTasks := channel.NewWriter(gCtx, p.OutgoingTasks)
		return w.receiveResultsAndForward(gCtx, stream, outTasks)
	})

	return utils.ProcessErr(g.Wait(), "worker stream processing failed")
}

func (w *worker) sendTasksToService(
	stream Stream,
	inputTasks channel.Reader[[]*dependencygraph.TransactionNode],
) error {
	firstBatch := true
	for {
		tasks, ctxAlive := inputTasks.Read()
		if !ctxAlive {
			return errors.Wrap(inputTasks.Context().Err(), "context ended")
		}
		if len(tasks) == 0 {
			continue
		}

		w.addTasksBeingProcessed(tasks)

		taskBatches := [][]*dependencygraph.TransactionNode{tasks}
		if firstBatch {
			taskBatches = splitBatchByBlock(tasks)
			firstBatch = false
		}

		for _, tb := range taskBatches {
			if err := stream.Send(tb); err != nil {
				return errors.Wrap(err, "sending to stream failed")
			}
		}
	}
}

func (w *worker) receiveResultsAndForward(
	ctx context.Context,
	stream Stream,
	outputTasks channel.Writer[[]*dependencygraph.TransactionNode],
) error {
	for {
		resultBatch, err := stream.Recv()
		if err != nil {
			if grpcerror.HasCode(err, codes.InvalidArgument) {
				// While it is unlikely that the service would send an invalid argument, it could happen
				// due to corrupted or maliciously altered state, or if there is a bug in the committer.
				return errors.Join(retry.ErrNonRetryable, err)
			}
			return errors.Wrap(err, "receive from stream failed")
		}

		tasks, untrackedResIdx := w.getTasksAndApplyResults(resultBatch)
		if len(untrackedResIdx) > 0 {
			// untrackedResIdx can be non-empty only when the service restarts.
			// Negligible performance impact is fine as this is a rare case.
			for _, i := range slices.Backward(untrackedResIdx) {
				resultBatch = append(resultBatch[:i], resultBatch[i+1:]...)
			}
		}

		successWriteResult := w.writeResults(ctx, resultBatch)
		successWriteTasks := outputTasks.Write(tasks)
		if !successWriteResult || !successWriteTasks {
			// Re-add tasks if we couldn't write them.
			w.addTasksBeingProcessed(tasks)
			return errors.Wrap(ctx.Err(), "context ended")
		}

		promutil.AddToCounter(w.params.Metrics.ProcessedTotal, len(resultBatch))
	}
}

// writeResults forwards the result batch through the configured ResultWriter.
// A nil ResultWriter (e.g., for managers that do not forward results) is a no-op.
func (w *worker) writeResults(ctx context.Context, resultBatch []*committerpb.TxStatus) bool {
	if w.params.OutgoingResults == nil {
		return true
	}
	return w.params.OutgoingResults.Write(ctx, resultBatch)
}

func (w *worker) getTasksAndApplyResults(resultBatch []*committerpb.TxStatus) (
	tasks []*dependencygraph.TransactionNode, untrackedResIdx []int,
) {
	tasks = make([]*dependencygraph.TransactionNode, 0, len(resultBatch))
	for i, resultItem := range resultBatch {
		if task, ok := w.tasksBeingProcessed.LoadAndDelete(*servicepb.NewHeightFromTxRef(resultItem.Ref)); ok {
			w.params.Adaptor.ApplyResult(task, resultItem)
			tasks = append(tasks, task)
		} else {
			untrackedResIdx = append(untrackedResIdx, i)
		}
	}
	return tasks, untrackedResIdx
}

func (w *worker) addTasksBeingProcessed(tasks []*dependencygraph.TransactionNode) {
	for _, task := range tasks {
		// VCTx.Ref is always populated for both regular and rejected nodes, whereas
		// VerifierTx may be nil. Both refer to the same TxRef, so it is the safe key.
		w.tasksBeingProcessed.Store(*servicepb.NewHeightFromTxRef(task.VCTx.Ref), task)
	}
}

func (w *worker) recoverPendingTasks(
	taskQueue channel.Writer[[]*dependencygraph.TransactionNode],
) {
	pendingTasks := slices.Collect(w.tasksBeingProcessed.IterValues())
	w.tasksBeingProcessed.Clear()

	if len(pendingTasks) == 0 {
		return
	}

	promutil.AddToCounter(w.params.Metrics.RetriedTotal, len(pendingTasks))
	taskQueue.Write(pendingTasks)
}

func (m *Manager) monitorQueues(ctx context.Context) {
	// TODO: make sampling time configurable
	ticker := time.NewTicker(100 * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		p := &m.Params
		promutil.SetGauge(p.Metrics.InputQueueSize, len(p.IncomingTasks))
		promutil.SetGauge(p.Metrics.OutputQueueSize, len(p.OutgoingTasks))
	}
}

// splitBatchByBlock splits a batch by block number to avoid gRPC message size limits.
// We group tasks by block to ensure our batch sizes do not exceed the gRPC message limit.
// This strategy prevents RESOURCE_EXHAUSTED errors because the orderer's maximum block size
// will be configured to be safely smaller than the gRPC send/receive limit.
func splitBatchByBlock(tasks []*dependencygraph.TransactionNode) [][]*dependencygraph.TransactionNode {
	// Group transactions by block number
	blkToBatch := make(map[uint64][]*dependencygraph.TransactionNode)
	for _, task := range tasks {
		blkNum := task.VCTx.Ref.BlockNum
		taskBatch, ok := blkToBatch[blkNum]
		if !ok {
			taskBatch = make([]*dependencygraph.TransactionNode, 0, len(tasks))
			blkToBatch[blkNum] = taskBatch
		}
		blkToBatch[blkNum] = append(taskBatch, task)
	}

	if len(blkToBatch) < 2 {
		return [][]*dependencygraph.TransactionNode{tasks}
	}

	return slices.Collect(maps.Values(blkToBatch))
}

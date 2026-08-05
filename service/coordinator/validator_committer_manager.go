/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"fmt"
	"slices"

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

const streamEndErrWrap = "sending to stream ended with an error"

type (
	// validatorCommitterManager is responsible for managing all communication with
	// all vcservices. It is responsible for:
	// 1. Sending transactions to be validated and committed to the vcservices.
	// 2. Receiving the status of the transactions from the vcservices.
	// 3. Forwarding the validated transactions node to the dependency graph manager.
	// 4. Forwarding the status of the transactions to the coordinator.
	//
	// The request/response API that any vcservice can serve is not part of this manager; see
	// validatorCommitterAPI.
	validatorCommitterManager struct {
		config             *validatorCommitterManagerConfig
		validatorCommitter []*validatorCommitter

		// validatorCommitterReady signals once run has finished allocating
		// validatorCommitter and populating every slot (each slot's *grpc.ClientConn
		// exists, though its underlying connection may still be dialing/reconnecting
		// -- see anyVCOwnsSnapshotHashJob's gRPC-unavailable handling for that
		// separate, always-open case). Any caller that reads validatorCommitter
		// before that point -- e.g. the coordinator's startup snapshot sweep, which
		// runs concurrently with run's own goroutine, or a per-VC-disconnect sweep
		// spawned by run itself while its connection-setup loop is still running --
		// must wait on this first, or it can observe a nil/partially-populated
		// slice and wrongly conclude no VC could possibly own the job.
		validatorCommitterReady *channel.Ready
	}

	// validatorCommitter is responsible for managing the communication with a single
	// vcserver.
	validatorCommitter struct {
		conn      *grpc.ClientConn
		client    servicepb.ValidationAndCommitServiceClient
		metrics   *perfMetrics
		policyMgr *policyManager

		// txBeingValidated stores the transactions currently being validated by this vcservice, so the
		// status returned by the vcservice can be matched back to its transaction node.
		txBeingValidated utils.SyncMap[servicepb.Height, *dependencygraph.TransactionNode]
	}

	validatorCommitterManagerConfig struct {
		clientConfig                   *connection.MultiClientConfig
		incomingTxsForValidationCommit <-chan dependencygraph.TxNodeBatch
		outgoingValidatedTxsNode       chan<- dependencygraph.TxNodeBatch
		outgoingTxsStatus              *txStatusQueue
		metrics                        *perfMetrics
		policyMgr                      *policyManager
	}
)

func newValidatorCommitterManager(c *validatorCommitterManagerConfig) *validatorCommitterManager {
	logger.Info("Initializing new ValidatorCommitterManager")
	return &validatorCommitterManager{
		config:                  c,
		validatorCommitterReady: channel.NewReady(),
	}
}

// sweepSnapshotRecovery derives, from live state only, whether a snapshot hash
// job needs (re)starting right now — no coordinator-side memory of which VC
// previously owned it, no persisted tracking. Safe to call repeatedly and
// concurrently; every branch is a no-op if there is nothing to do. Called on
// every VC disconnect and once at coordinator startup; both call sites are
// identical because rejectSnapshotIfPriorNotCheckpointed (VC-side) guarantees
// at most one non-terminal snapshot exists system-wide, so there is nothing
// case-specific to distinguish between "disconnect" and "restart" here: in
// both cases we simply need to know, right now, whether the one active
// snapshot's hash job is currently owned by a live VC, and (re)start it on
// some VC if not.
//
// This also correctly recovers a coordinator+all-VC simultaneous crash, even
// though the sidecar never resubmits the snapshot tx in that scenario: the
// sidecar drains before/after a snapshot TX and submits it exactly once, so
// once it has been acknowledged as committed the sidecar has no reason to
// ever resend it, regardless of what happens to the coordinator or VCs
// afterward. Recovery here does not depend on that resubmission at all --
// api.getLatestSnapshotState reads the _snapshot row directly from VC-side
// durable storage (survives both processes crashing), and that alone is
// enough for this sweep to rediscover the outstanding job and restart it on
// whichever VC (re)connects first.
func (vcm *validatorCommitterManager) sweepSnapshotRecovery(ctx context.Context, api *validatorCommitterAPI) error {
	state, err := api.getLatestSnapshotState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get latest snapshot state: %w", err)
	}
	if state.TxRef == nil {
		return nil // no snapshot has ever been accepted.
	}
	switch state.Status {
	case committerpb.SnapshotState_COMPLETED, committerpb.SnapshotState_CHECKPOINTED:
		return nil // nothing to restart.
	}

	owned, err := vcm.anyVCOwnsSnapshotHashJob(ctx, state.TxRef.TxId)
	if err != nil {
		return fmt.Errorf("failed to broadcast snapshot hash ownership query: %w", err)
	}
	if owned {
		return nil // a live VC is already running this job.
	}

	if err := api.restartSnapshotHash(ctx, state.TxRef.TxId); err != nil {
		return fmt.Errorf("failed to restart snapshot hash for tx %s: %w", state.TxRef.TxId, err)
	}
	return nil
}

// anyVCOwnsSnapshotHashJob broadcasts OwnsSnapshotHashJob to every currently
// connected validatorCommitter. It first waits for validatorCommitterReady, so
// vcm.validatorCommitter is guaranteed allocated and fully populated (every
// slot's *grpc.ClientConn constructed, though a given connection may still be
// dialing/reconnecting) before this reads it -- without that wait, a caller
// racing run's own connection-setup loop (either the coordinator's startup
// sweep, which runs concurrently with run in its own goroutine, or a
// per-VC-disconnect sweep, which run spawns via g.Go from *inside* that same
// loop and so can itself fire before the loop has populated every later slot)
// could observe a nil or partially-empty slice and wrongly conclude no VC
// could possibly own the job, even though every VC is actually up and one of
// them owns it right now. If ctx ends first, WaitForReady returns false and
// this returns that context error instead of silently proceeding on a
// possibly-empty slice.
//
// Once ready, a nil entry means that endpoint's connection attempt itself
// failed at dial time (see run); it is treated as not claiming, consistent
// with "unclaimed" being the safe default. Returns true as soon as any VC
// claims ownership.
func (vcm *validatorCommitterManager) anyVCOwnsSnapshotHashJob(ctx context.Context, txID string) (bool, error) {
	if !vcm.validatorCommitterReady.WaitForReady(ctx) {
		return false, errors.Wrap(ctx.Err(), "context ended before validator-committer connections were ready")
	}

	g, gCtx := errgroup.WithContext(ctx)
	owned := make([]bool, len(vcm.validatorCommitter))
	for i, vc := range vcm.validatorCommitter {
		if vc == nil {
			continue
		}
		g.Go(func() error {
			resp, err := vc.client.OwnsSnapshotHashJob(gCtx, &servicepb.SnapshotTxIDRequest{TxId: txID})
			if err != nil {
				// Only a transport-level failure (the VC is genuinely unreachable
				// right now, which is exactly the disconnect this sweep is
				// reacting to) is treated as "does not claim" rather than a real
				// error — checked via the gRPC status code, not by assuming every
				// error means unreachable. Any other code (e.g. InvalidArgument,
				// Internal) is a genuine bug/misbehavior and must propagate so the
				// sweep does not silently proceed as if no VC claimed the job.
				switch grpcerror.GetCode(err) {
				case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
					return nil // VC unreachable, so it does not claim ownership.
				default:
					return fmt.Errorf("OwnsSnapshotHashJob failed on %s: %w", vc.conn.CanonicalTarget(), err)
				}
			}
			owned[i] = resp.GetValue()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return false, err
	}
	return slices.Contains(owned, true), nil
}

func (vcm *validatorCommitterManager) run(ctx context.Context, api *validatorCommitterAPI) error {
	c := vcm.config
	logger.Infof("Connections to %d vc's will be opened from vc manager", len(c.clientConfig.Endpoints))
	vcm.validatorCommitter = make([]*validatorCommitter, len(c.clientConfig.Endpoints))

	g, eCtx := errgroup.WithContext(ctx)

	txBatchQueue := channel.NewReaderWriter(eCtx,
		make(chan dependencygraph.TxNodeBatch, cap(c.incomingTxsForValidationCommit)))
	g.Go(func() error {
		ingestIncomingTxsToInternalQueue(
			channel.NewReader(eCtx, c.incomingTxsForValidationCommit),
			txBatchQueue,
		)
		return nil
	})

	connections, connErr := connection.NewConnectionPerEndpoint(c.clientConfig)
	if connErr != nil {
		// The slice stays all-nil, which is its correct final state: zero
		// connections were established, so there is trivially no VC that could own
		// anything. Signal readiness anyway so a startup sweep waiting on
		// validatorCommitterReady is not left blocked forever by this early return.
		vcm.validatorCommitterReady.SignalReady()
		return fmt.Errorf("failed to create connection to validator persister: %w", connErr)
	}
	defer connection.CloseConnectionsLog(connections...)
	for i, conn := range connections {
		label := conn.CanonicalTarget()
		c.metrics.vcs.connection.Disconnected(label)

		vc := newValidatorCommitter(conn, c.metrics, c.policyMgr)
		vcm.validatorCommitter[i] = vc
		logger.Infof("Client [%d] successfully created and connected to vc at %s", i, label)

		g.Go(func() error {
			return retry.Sustain(eCtx, vcm.config.clientConfig.Retry, func() (err error) {
				defer vc.recoverPendingTransactions(txBatchQueue)
				// sendTransactionsAndForwardStatus below returns whenever this VC's
				// stream ends, i.e. on every reconnect attempt retry.Sustain drives
				// (not gated behind gRPC's own transparent retry budget). If this VC
				// was the one running the active snapshot's hash job, that job dies
				// with the connection and nothing else will notice; sweeping here,
				// right after every disconnect, is what re-discovers that and moves
				// the job to another live VC (or restarts it once this VC reconnects).
				defer func() {
					if sweepErr := vcm.sweepSnapshotRecovery(eCtx, api); sweepErr != nil {
						logger.Errorf("snapshot recovery sweep on VC disconnect failed: %+v", sweepErr)
					}
				}()
				return vc.sendTransactionsAndForwardStatus(
					eCtx,
					txBatchQueue,
					channel.NewWriter(eCtx, c.outgoingValidatedTxsNode),
					c.outgoingTxsStatus,
				)
			})
		})
	}

	// Every slot's *grpc.ClientConn now exists (a slot may still be dialing/
	// reconnecting underneath -- that is a separate, always-open case handled by
	// anyVCOwnsSnapshotHashJob's gRPC-unavailable branch, not this signal). Any
	// caller of anyVCOwnsSnapshotHashJob that first waits on
	// validatorCommitterReady is guaranteed validatorCommitter is not nil/empty
	// by the time it reads it.
	vcm.validatorCommitterReady.SignalReady()

	return utils.ProcessErr(g.Wait(), "validator-committer manager failed")
}

func newValidatorCommitter(conn *grpc.ClientConn, metrics *perfMetrics, policyMgr *policyManager) *validatorCommitter {
	return &validatorCommitter{
		conn:      conn,
		client:    servicepb.NewValidationAndCommitServiceClient(conn),
		metrics:   metrics,
		policyMgr: policyMgr,
	}
}

func (vc *validatorCommitter) sendTransactionsAndForwardStatus(
	ctx context.Context,
	inputTxBatch channel.ReaderWriter[dependencygraph.TxNodeBatch],
	outputValidatedTxsNode channel.Writer[dependencygraph.TxNodeBatch],
	outputTxsStatus *txStatusQueue,
) error {
	defer vc.metrics.vcs.connection.Disconnected(vc.conn.CanonicalTarget())

	g, gCtx := errgroup.WithContext(ctx)

	stream, err := vc.client.StartValidateAndCommitStream(gCtx)
	if err != nil {
		return errors.Join(retry.ErrBackOff, err)
	}

	// if the stream is started, the connection has been established.
	vc.metrics.vcs.connection.Connected(vc.conn.CanonicalTarget())

	// NOTE: sendTransactionsToVCService and receiveStatusAndForwardToOutput must
	//       always return an error on exist.
	g.Go(func() error { //nolint:contextcheck
		return vc.sendTransactionsToVCService(stream, inputTxBatch.WithContext(stream.Context()))
	})

	g.Go(func() error {
		// NOTE: The channels outputValidatedTxsNode and outputTxsStatus should not depend on the stream context.
		//       Doing so can result in permanently lost validation results. Specifically, after reading a
		//       transaction from the stream and removing it from txBeingValidated, if the stream context is
		//       canceled before we can write to these two channels, the validation results are lost forever.
		//       Similarly, the first argument, i.e., context should not be stream context.
		//       Binding them to the stream would also make the re-queue in
		//       receiveStatusAndForwardToOutput reachable on every stream failure: a batch whose
		//       statuses were already queued would be re-sent, the vcservice would re-emit those
		//       statuses, and Service.numTxsInProgress would be decremented twice for one
		//       transaction. That breaks the numTxsInProgress >= readyCount >= 0 invariant that
		//       NoPendingTransactionProcessing relies on to report idle, so the sidecar could
		//       never re-establish its stream.
		return vc.receiveStatusAndForwardToOutput(ctx, stream, outputValidatedTxsNode, outputTxsStatus)
	})

	return utils.ProcessErr(g.Wait(), "sendTransactionsAndForwardStatus run failed")
}

func (vc *validatorCommitter) sendTransactionsToVCService(
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient,
	inputTxsNode channel.Reader[dependencygraph.TxNodeBatch],
) error {
	firstBatch := true
	for {
		txsNode, ok := inputTxsNode.Read()
		if !ok {
			return errors.Wrap(inputTxsNode.Context().Err(), "context ended")
		}

		logger.Debugf("New TX node came from dependency graph manager to vc manager")
		if len(txsNode) == 0 {
			continue
		}

		vc.addTxsBeingValidated(txsNode)
		txBatch := make([]*servicepb.VcTx, len(txsNode))
		for i, txNode := range txsNode {
			txBatch[i] = txNode.VCTx
		}

		if firstBatch {
			if err := splitAndSendToVC(stream, txBatch); err != nil {
				return err
			}
			firstBatch = false
			continue
		}

		if err := stream.Send(&servicepb.VcBatch{
			Transactions: txBatch,
		}); err != nil {
			return errors.Wrap(err, streamEndErrWrap)
		}
		logger.Debugf("TX node contains %d TXs, and was sent to a vcservice", len(txBatch))
	}
}

func splitAndSendToVC(
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient,
	txBatch []*servicepb.VcTx,
) error {
	blkToBatch := make(map[uint64]*servicepb.VcBatch)
	for _, tx := range txBatch {
		rBatch, ok := blkToBatch[tx.Ref.BlockNum]
		if !ok {
			rBatch = &servicepb.VcBatch{
				Transactions: make([]*servicepb.VcTx, 0, len(txBatch)),
			}
			blkToBatch[tx.Ref.BlockNum] = rBatch
		}

		rBatch.Transactions = append(rBatch.Transactions, tx)
	}

	for _, rBatch := range blkToBatch {
		if err := stream.Send(rBatch); err != nil {
			return errors.Wrap(err, streamEndErrWrap)
		}
	}

	return nil
}

func (vc *validatorCommitter) receiveStatusAndForwardToOutput(
	ctx context.Context,
	stream servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient,
	outputTxsNode channel.Writer[dependencygraph.TxNodeBatch],
	outputTxsStatus *txStatusQueue,
) error {
	for {
		txsStatus, err := stream.Recv()
		if err != nil {
			return classifyStreamRecvError(err)
		}

		logger.Debugf("Batch contains %d TX statuses", len(txsStatus.Status))

		txsNode, untrackedTxIdx := vc.getTxsAndUpdatePolicies(txsStatus)
		if len(untrackedTxIdx) > 0 {
			// untrackedTxIdx can be non-empty only when the coordinator restarts.
			// Negligible performance impact is fine as this is a rare case.
			for _, i := range slices.Backward(untrackedTxIdx) {
				txsStatus.Status = append(txsStatus.Status[:i], txsStatus.Status[i+1:]...)
			}
		}

		if len(txsStatus.Status) == 0 {
			continue
		}

		// NOTE: The sidecar reads transactions from the ordering service stream and sends
		//       them to the coordinator. The coordinator then forwards the transactions to the
		//       dependency graph manager. The dependency graph manager forwards the transactions
		//       to the validator committer manager. The validator committer manager sends the
		//       transactions to the VC services. The VC services validate and commit the
		//       transactions, sending the status back to the validator committer manager.
		//       The validator committer manager then sends the status to the coordinator.
		//       The coordinator sends the status back to the sidecar. The sidecar accumulates
		//       the transaction statuses at the block level and sends them to all connected clients.
		//       Although there is a cycle in the producer-consumer flow (sidecar -> coordinator -> sidecar),
		//       this is not an issue. If the sidecar becomes bottlenecked and cannot receive
		//       the statuses quickly, the gRPC flow control will activate and slow down the
		//       whole system, allowing the sidecar to catch up.
		// NOTE: getTxsAndUpdatePolicies removes the transactions from txBeingValidated before their
		//       results are queued, so a failed write must return them to the map. Otherwise
		//       recoverPendingTransactions has nothing to re-queue and the transactions are lost:
		//       their status never reaches the sidecar, and their nodes never free their dependents
		//       in the dependency graph. The signature verifier manager guards the same invariant.
		if ok := outputTxsStatus.write(ctx, txsStatus); !ok {
			vc.addTxsBeingValidated(txsNode)
			return errors.Wrap(ctx.Err(), "context ended")
		}
		logger.Debugf("Forwarded batch with %d TX statuses back to coordinator", len(txsStatus.Status))

		promutil.AddToCounter(vc.metrics.vcs.processedTotal, len(txsStatus.Status))

		if len(txsNode) > 0 && !outputTxsNode.Write(txsNode) {
			vc.addTxsBeingValidated(txsNode)
			return errors.Wrap(outputTxsNode.Context().Err(), "context ended")
		}
		logger.Debugf("Forwarded batch with %d TX statuses back to dep graph", len(txsStatus.Status))
	}
}

func (vc *validatorCommitter) recoverPendingTransactions(inputTxsNode channel.Writer[dependencygraph.TxNodeBatch],
) {
	pendingTxs := slices.Collect(vc.txBeingValidated.IterValues())
	vc.txBeingValidated.Clear()

	if len(pendingTxs) == 0 {
		return
	}

	promutil.AddToCounter(vc.metrics.vcs.retriedTotal, len(pendingTxs))
	inputTxsNode.Write(pendingTxs)
}

func (vc *validatorCommitter) getTxsAndUpdatePolicies(txsStatus *committerpb.TxStatusBatch) (
	txsNode []*dependencygraph.TransactionNode, untrackedTxIdx []int,
) {
	txsNode = make([]*dependencygraph.TransactionNode, 0, len(txsStatus.Status))
	for i, txStatus := range txsStatus.Status {
		txNode, ok := vc.txBeingValidated.LoadAndDelete(*servicepb.NewHeightFromTxRef(txStatus.Ref))
		if !ok {
			// Because the VC manager might submit the same transaction multiple times (for example,
			// if a VC service fails or the coordinator reconnects to a failed VC service), it could
			// receive duplicate responses.  However, the txBeingValidated lookup will succeed only once.
			// Therefore, if the transaction is not found in txBeingValidated, we must proceed to
			// the next status.
			untrackedTxIdx = append(untrackedTxIdx, i)
			continue
		}
		txsNode = append(txsNode, txNode)

		if txStatus.Status != committerpb.Status_COMMITTED {
			continue
		}

		// Updating policy before sending transaction nodes to the dependency
		// graph manager to free dependent transactions. Otherwise, dependent transactions
		// might be validated against a stale policy.
		vc.policyMgr.updateFromTx(txNode.VCTx.Namespaces)
	}

	return txsNode, untrackedTxIdx
}

func (vc *validatorCommitter) addTxsBeingValidated(txsNode dependencygraph.TxNodeBatch) {
	for _, txNode := range txsNode {
		vc.txBeingValidated.Store(*servicepb.NewHeightFromTxRef(txNode.VCTx.Ref), txNode)
	}
}

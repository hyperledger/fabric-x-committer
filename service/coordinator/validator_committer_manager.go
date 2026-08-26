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

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
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
		config: c,
	}
}

func (vcm *validatorCommitterManager) run(ctx context.Context) error {
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
				return vc.sendTransactionsAndForwardStatus(
					eCtx,
					txBatchQueue,
					channel.NewWriter(eCtx, c.outgoingValidatedTxsNode),
					c.outgoingTxsStatus,
				)
			})
		})
	}

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

func (vc *validatorCommitter) getTxsAndUpdatePolicies(txsStatus *servicepb.TxStatusBatch) (
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

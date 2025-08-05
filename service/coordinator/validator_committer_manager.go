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
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/protovcservice"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

const streamEndErrWrap = "sending to stream ended with an error"

type (
	// validatorCommitterManager is responsible for managing all communication with
	// all vcservices. It is responsible for:
	// 1. Sending transactions to be validated and committed to the vcservices.
	// 2. Receiving the status of the transactions from the vcservices.
	// 3. Forwarding the validated transactions node to the dependency graph manager.
	// 4. Forwarding the status of the transactions to the coordinator.
	validatorCommitterManager struct {
		config              *validatorCommitterManagerConfig
		commonClient        protovcservice.ValidationAndCommitServiceClient
		validatorCommitter  []*validatorCommitter
		txsStatusBufferSize int
		// ready indicates that the validatorCommitter array is initialized.
		ready *channel.Ready
	}

	// validatorCommitter is responsible for managing the communication with a single
	// vcserver.
	validatorCommitter struct {
		conn      *grpc.ClientConn
		client    protovcservice.ValidationAndCommitServiceClient
		metrics   *perfMetrics
		policyMgr *policyManager

		// vc service returns only the txID and the status of the transaction. To find the
		// transaction node associated with the txID, we use txBeingValidated map.
		txBeingValidated utils.SyncMap[string, *dependencygraph.TransactionNode]
	}

	validatorCommitterManagerConfig struct {
		clientConfig                   *connection.ClientConfig
		incomingTxsForValidationCommit <-chan dependencygraph.TxNodeBatch
		outgoingValidatedTxsNode       chan<- dependencygraph.TxNodeBatch
		outgoingTxsStatus              chan<- *protoblocktx.TransactionsStatus
		metrics                        *perfMetrics
		policyMgr                      *policyManager
	}
)

func newValidatorCommitterManager(c *validatorCommitterManagerConfig) *validatorCommitterManager {
	logger.Info("Initializing new ValidatorCommitterManager")
	return &validatorCommitterManager{
		config:              c,
		txsStatusBufferSize: cap(c.outgoingTxsStatus),
		ready:               channel.NewReady(),
	}
}

func (vcm *validatorCommitterManager) run(ctx context.Context) error {
	defer vcm.ready.Reset()
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

	commonDial, dialErr := connection.NewLoadBalancedDialConfig(c.clientConfig)
	if dialErr != nil {
		return fmt.Errorf("failed to create connection to validator persisters: %w", dialErr)
	}
	commonConn, err := connection.Connect(commonDial)
	if err != nil {
		return fmt.Errorf("failed to create connection to validator persisters: %w", err)
	}
	vcm.commonClient = protovcservice.NewValidationAndCommitServiceClient(commonConn)
	_, setupErr := vcm.commonClient.SetupSystemTablesAndNamespaces(ctx, nil)
	if setupErr != nil {
		return errors.Wrap(setupErr, "failed to setup system tables and namespaces")
	}

	dialConfigs, dialErr := connection.NewDialConfigPerEndpoint(c.clientConfig)
	if dialErr != nil {
		return dialErr
	}
	for i, d := range dialConfigs {
		logger.Debugf("vc manager creates client to vc [%d] listening on %s", i, d.Address)
		conn, connErr := connection.Connect(d)
		if connErr != nil {
			return fmt.Errorf("failed to create connection to validator persister running at %s", d.Address)
		}
		logger.Infof("validator persister manager connected to validator persister at %s", d.Address)
		vc := newValidatorCommitter(conn, c.metrics, c.policyMgr)

		logger.Debugf("Client [%d] successfully created and connected to vc", i)
		vcm.validatorCommitter[i] = vc

		g.Go(func() error {
			defer connection.CloseConnectionsLog(vc.conn)
			return connection.Sustain(eCtx, func() (err error) {
				defer vc.recoverPendingTransactions(txBatchQueue)
				return vc.sendTransactionsAndForwardStatus(
					eCtx,
					txBatchQueue,
					channel.NewWriter(eCtx, c.outgoingValidatedTxsNode),
					channel.NewWriter(eCtx, c.outgoingTxsStatus),
				)
			})
		})
	}

	vcm.ready.SignalReady()
	return utils.ProcessErr(g.Wait(), "validator-committer manager failed")
}

func (vcm *validatorCommitterManager) setLastCommittedBlockNumber(
	ctx context.Context,
	lastBlock *protoblocktx.BlockInfo,
) error {
	_, err := vcm.commonClient.SetLastCommittedBlockNumber(ctx, lastBlock)
	return errors.Wrap(err, "failed setting the last committed block number")
}

func (vcm *validatorCommitterManager) getLastCommittedBlockNumber(
	ctx context.Context,
) (*protoblocktx.LastCommittedBlock, error) {
	ret, err := vcm.commonClient.GetLastCommittedBlockNumber(ctx, nil)
	return ret, errors.Wrap(err, "failed getting the last committed block number")
}

func (vcm *validatorCommitterManager) getTransactionsStatus(
	ctx context.Context,
	query *protoblocktx.QueryStatus,
) (*protoblocktx.TransactionsStatus, error) {
	ret, err := vcm.commonClient.GetTransactionsStatus(ctx, query)
	return ret, errors.Wrap(err, "failed getting transactions status")
}

func (vcm *validatorCommitterManager) getNamespacePolicies(
	ctx context.Context,
) (*protoblocktx.NamespacePolicies, error) {
	ret, err := vcm.commonClient.GetNamespacePolicies(ctx, nil)
	return ret, errors.Wrap(err, "failed loading policies")
}

func (vcm *validatorCommitterManager) getConfigTransaction(
	ctx context.Context,
) (*protoblocktx.ConfigTransaction, error) {
	ret, err := vcm.commonClient.GetConfigTransaction(ctx, nil)
	return ret, errors.Wrap(err, "failed loading config transaction")
}

func (vcm *validatorCommitterManager) recoverPolicyManagerFromStateDB(ctx context.Context) error {
	policyMsg, err := vcm.getNamespacePolicies(ctx)
	if err != nil {
		return err
	}
	configMsg, err := vcm.getConfigTransaction(ctx)
	if err != nil {
		return err
	}
	if len(policyMsg.Policies) == 0 && configMsg.Envelope == nil {
		return nil
	}
	vcm.config.policyMgr.update(&protosigverifierservice.Update{
		NamespacePolicies: policyMsg,
		Config:            configMsg,
	})
	return nil
}

func newValidatorCommitter(conn *grpc.ClientConn, metrics *perfMetrics, policyMgr *policyManager) *validatorCommitter {
	label := conn.CanonicalTarget()
	metrics.vcservicesConnection.Disconnected(label)
	client := protovcservice.NewValidationAndCommitServiceClient(conn)
	return &validatorCommitter{
		conn:      conn,
		client:    client,
		metrics:   metrics,
		policyMgr: policyMgr,
	}
}

func (vc *validatorCommitter) sendTransactionsAndForwardStatus(
	ctx context.Context,
	inputTxBatch channel.ReaderWriter[dependencygraph.TxNodeBatch],
	outputValidatedTxsNode channel.Writer[dependencygraph.TxNodeBatch],
	outputTxsStatus channel.Writer[*protoblocktx.TransactionsStatus],
) error {
	defer vc.metrics.vcservicesConnection.Disconnected(vc.conn.CanonicalTarget())

	g, gCtx := errgroup.WithContext(ctx)

	stream, err := vc.client.StartValidateAndCommitStream(gCtx)
	if err != nil {
		return errors.Join(connection.ErrBackOff, err)
	}

	// if the stream is started, the connection has been established.
	vc.metrics.vcservicesConnection.Connected(vc.conn.CanonicalTarget())

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
		return vc.receiveStatusAndForwardToOutput(stream, outputValidatedTxsNode, outputTxsStatus)
	})

	return utils.ProcessErr(g.Wait(), "sendTransactionsAndForwardStatus run failed")
}

func (vc *validatorCommitter) sendTransactionsToVCService(
	stream protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamClient,
	inputTxsNode channel.Reader[dependencygraph.TxNodeBatch],
) error {
	firstBatch := true
	for {
		txsNode, ok := inputTxsNode.Read()
		if !ok {
			return errors.Wrap(inputTxsNode.Context().Err(), "context ended")
		}

		logger.Debugf("New TX node came from dependency graph manager to vc manager")
		txBatch := make([]*protovcservice.Transaction, len(txsNode))
		for i, txNode := range txsNode {
			vc.txBeingValidated.Store(txNode.Tx.ID, txNode)
			txBatch[i] = txNode.Tx
		}

		if firstBatch {
			if err := splitAndSendToVC(stream, txBatch); err != nil {
				return err
			}
			firstBatch = false
			continue
		}

		if err := stream.Send(&protovcservice.TransactionBatch{
			Transactions: txBatch,
		}); err != nil {
			return errors.Wrap(err, streamEndErrWrap)
		}
		logger.Debugf("TX node contains %d TXs, and was sent to a vcservice", len(txBatch))
	}
}

func splitAndSendToVC(
	stream protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamClient,
	txBatch []*protovcservice.Transaction,
) error {
	blkToBatch := make(map[uint64]*protovcservice.TransactionBatch)
	for _, tx := range txBatch {
		rBatch, ok := blkToBatch[tx.BlockNumber]
		if !ok {
			rBatch = &protovcservice.TransactionBatch{
				Transactions: make([]*protovcservice.Transaction, 0, len(txBatch)),
			}
			blkToBatch[tx.BlockNumber] = rBatch
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
	stream protovcservice.ValidationAndCommitService_StartValidateAndCommitStreamClient,
	outputTxsNode channel.Writer[dependencygraph.TxNodeBatch],
	outputTxsStatus channel.Writer[*protoblocktx.TransactionsStatus],
) error {
	for {
		txsStatus, err := stream.Recv()
		if err != nil {
			// The stream ended or the SVM was closed.
			return errors.Wrap(err, "receive from stream ended with error")
		}

		logger.Debugf("Batch contains %d TX statuses", len(txsStatus.Status))

		txsNode, untrackedTxIDs := vc.getTxsAndUpdatePolicies(txsStatus)
		for _, txID := range untrackedTxIDs {
			// untrackedTxIDs can be non-empty only when the coordinator restarts.
			delete(txsStatus.Status, txID)
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
		if ok := outputTxsStatus.Write(txsStatus); !ok {
			return errors.Wrap(outputTxsStatus.Context().Err(), "context ended")
		}
		logger.Debugf("Forwarded batch with %d TX statuses back to coordinator", len(txsStatus.Status))

		promutil.AddToCounter(vc.metrics.vcserviceTransactionProcessedTotal, len(txsStatus.Status))

		if len(txsNode) > 0 && !outputTxsNode.Write(txsNode) {
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

	promutil.AddToCounter(vc.metrics.vcservicesRetriedTransactionTotal, len(pendingTxs))
	inputTxsNode.Write(pendingTxs)
}

func (vc *validatorCommitter) getTxsAndUpdatePolicies(txsStatus *protoblocktx.TransactionsStatus) (
	[]*dependencygraph.TransactionNode, []string,
) {
	txsNode := make([]*dependencygraph.TransactionNode, 0, len(txsStatus.Status))
	var untrackedTxIDs []string
	for txID, txStatus := range txsStatus.Status {
		txNode, ok := vc.txBeingValidated.LoadAndDelete(txID)
		if !ok {
			// Because the VC manager might submit the same transaction multiple times (for example,
			// if a VC service fails or the coordinator reconnects to a failed VC service), it could
			// receive duplicate responses.  However, the txBeingValidated lookup will succeed only once.
			// Therefore, if the transaction ID is not found in txBeingValidated, we must proceed to
			// the next status.
			untrackedTxIDs = append(untrackedTxIDs, txID)
			continue
		}
		txsNode = append(txsNode, txNode)

		if txStatus.Code != protoblocktx.Status_COMMITTED {
			continue
		}

		// Updating policy before sending transaction nodes to the dependency
		// graph manager to free dependent transactions. Otherwise, dependent transactions
		// might be validated against a stale policy.
		vc.policyMgr.updateFromTx(txNode.Tx.Namespaces)
	}

	return txsNode, untrackedTxIDs
}

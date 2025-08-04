/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"fmt"
	"sync"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/protovcservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	// signatureVerifierManager is responsible for managing all communication with
	// all signature verifier servers. It is responsible for:
	// 1. Sending transactions to be verified to the signature verifier servers.
	// 2. Receiving the status of the transactions from the signature verifier servers.
	// 3. Forwarding the status of the transactions to the coordinator.
	signatureVerifierManager struct {
		config       *signVerifierManagerConfig
		signVerifier []*signatureVerifier
		metrics      *perfMetrics
	}

	// signatureVerifier is responsible for managing the communication with a single
	// signature verifier server.
	signatureVerifier struct {
		conn    *grpc.ClientConn
		client  protosigverifierservice.VerifierClient
		metrics *perfMetrics

		// txBeingValidated stores transactions currently being validated by the signature verifier.
		// The key is the Height (block number, transaction index), and the value is the
		// dependencygraph.TransactionNode. If signature verifier service fails, these transactions are
		// requeued to the input queue for processing by other signature verifiers.
		txBeingValidated map[types.Height]*dependencygraph.TransactionNode
		txMu             *sync.Mutex

		policyManager *policyManager
	}

	signVerifierManagerConfig struct {
		clientConfig             *connection.ClientConfig
		incomingTxsForValidation <-chan dependencygraph.TxNodeBatch
		outgoingValidatedTxs     chan<- dependencygraph.TxNodeBatch
		metrics                  *perfMetrics
		policyManager            *policyManager
	}
)

var sigInvalidTxStatus = &protovcservice.InvalidTxStatus{
	Code: protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
}

func newSignatureVerifierManager(config *signVerifierManagerConfig) *signatureVerifierManager {
	logger.Info("Initializing newSignatureVerifierManager")
	return &signatureVerifierManager{
		config:  config,
		metrics: config.metrics,
	}
}

func (svm *signatureVerifierManager) run(ctx context.Context) error {
	c := svm.config
	logger.Infof("Connections to %d sv's will be opened from sv manager", len(c.clientConfig.Endpoints))
	svm.signVerifier = make([]*signatureVerifier, len(c.clientConfig.Endpoints))

	derivedCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, eCtx := errgroup.WithContext(derivedCtx)

	txBatchQueue := channel.NewReaderWriter(eCtx, make(chan dependencygraph.TxNodeBatch, cap(c.outgoingValidatedTxs)))
	g.Go(func() error {
		ingestIncomingTxsToInternalQueue(
			channel.NewReader(eCtx, c.incomingTxsForValidation),
			txBatchQueue,
		)
		return nil
	})

	dialConfigs, dialErr := connection.NewDialConfigPerEndpoint(c.clientConfig)
	if dialErr != nil {
		return dialErr
	}
	for i, d := range dialConfigs {
		conn, err := connection.Connect(d)
		if err != nil {
			return fmt.Errorf("failed to create connection to signature verifier [%d] at %s: %w",
				i, d.Address, err)
		}
		logger.Infof("connected to signature verifier [%d] at %s", i, d.Address)
		label := conn.CanonicalTarget()
		c.metrics.verifiersConnection.Disconnected(label)

		sv := newSignatureVerifier(c, conn)
		svm.signVerifier[i] = sv
		logger.Debugf("Client [%d] successfully created and connected to sv", i)

		g.Go(func() error {
			defer connection.CloseConnectionsLog(conn)
			// error should never occur unless there is a bug or malicious activity. Hence, it is fine to crash for now.
			return connection.Sustain(eCtx, func() error {
				defer sv.recoverPendingTransactions(txBatchQueue)
				return sv.sendTransactionsAndForwardStatus(
					eCtx,
					txBatchQueue,
					channel.NewWriter(eCtx, c.outgoingValidatedTxs),
				)
			})
		})
	}
	return utils.ProcessErr(g.Wait(), "signature verifier manager run failed")
}

func ingestIncomingTxsToInternalQueue(
	incomingTxBatch channel.Reader[dependencygraph.TxNodeBatch],
	txsQueue channel.Writer[dependencygraph.TxNodeBatch],
) {
	for {
		txs, ctxAlive := incomingTxBatch.Read()
		if !ctxAlive {
			return
		}

		batchSize := len(txs)
		logger.Debugf("New transaction batch (size: %d) received", batchSize)

		txsQueue.Write(txs)
	}
}

func newSignatureVerifier(
	config *signVerifierManagerConfig,
	conn *grpc.ClientConn,
) *signatureVerifier {
	logger.Info("Initializing new SignatureVerifier")
	return &signatureVerifier{
		conn:             conn,
		client:           protosigverifierservice.NewVerifierClient(conn),
		metrics:          config.metrics,
		txBeingValidated: make(map[types.Height]*dependencygraph.TransactionNode),
		txMu:             &sync.Mutex{},
		policyManager:    config.policyManager,
	}
}

// sendTransactionsAndForwardStatus initiates a stream to a verifier
// and use it to send transactions, and receive the results.
// It reconnects the stream in case of failure.
// It stops when the context was cancelled, i.e., the SVM have closed, or according to the retry policy.
func (sv *signatureVerifier) sendTransactionsAndForwardStatus(
	ctx context.Context,
	inputTxBatch channel.ReaderWriter[dependencygraph.TxNodeBatch],
	outputValidatedTxs channel.Writer[dependencygraph.TxNodeBatch],
) error {
	defer sv.metrics.verifiersConnection.Disconnected(sv.conn.CanonicalTarget())
	g, gCtx := errgroup.WithContext(ctx)

	stream, err := sv.client.StartStream(gCtx)
	if err != nil {
		return errors.Join(connection.ErrBackOff, err)
	}

	// if the stream is started, the connection is established.
	sv.metrics.verifiersConnection.Connected(sv.conn.CanonicalTarget())

	// NOTE: sendTransactionsToSVService and receiveStatusAndForwardToOutput must
	//       always return an error on exist.
	g.Go(func() error {
		return sv.sendTransactionsToSVService(stream, inputTxBatch.WithContext(gCtx))
	})

	g.Go(func() error {
		return sv.receiveStatusAndForwardToOutput(stream, outputValidatedTxs.WithContext(gCtx))
	})

	return utils.ProcessErr(g.Wait(), "sendTransactionsAndForwardStatus run failed")
}

// NOTE: sendTransactionsToSVService filters all transient connection related errors.
func (sv *signatureVerifier) sendTransactionsToSVService(
	stream protosigverifierservice.Verifier_StartStreamClient,
	inputTxBatch channel.Reader[dependencygraph.TxNodeBatch],
) error {
	var policyVersion uint64
	firstBatch := true
	for {
		txBatch, ctxAlive := inputTxBatch.Read()
		if !ctxAlive {
			return errors.Wrap(inputTxBatch.Context().Err(), "context ended")
		}

		sv.addTxsBeingValidated(txBatch)

		batchSize := len(txBatch)
		logger.Debugf("Batch containing %d TXs was stored in the being validated list", batchSize)

		request := &protosigverifierservice.Batch{
			Requests: make([]*protosigverifierservice.Tx, batchSize),
		}

		request.Update, policyVersion = sv.policyManager.getUpdates(policyVersion)

		for idx, txNode := range txBatch {
			request.Requests[idx] = &protosigverifierservice.Tx{
				Ref: txNode.Tx.Ref,
				Tx: &protoblocktx.Tx{
					Namespaces: txNode.Tx.Namespaces,
					Signatures: txNode.Signatures,
				},
			}
		}

		if firstBatch {
			if err := splitAndSendToVerifier(stream, request); err != nil {
				return err
			}
			firstBatch = false
			continue
		}

		if err := stream.Send(request); err != nil {
			return errors.Wrap(err, streamEndErrWrap)
		}
		logger.Debugf("Batch contains %d TXs, and was stored in the accumulator and sent to a sv", batchSize)
	}
}

func splitAndSendToVerifier(
	stream protosigverifierservice.Verifier_StartStreamClient,
	r *protosigverifierservice.Batch,
) error {
	// We group transactions by block to ensure our batch sizes do not exceed the gRPC message limit.
	// This strategy prevents RESOURCE_EXHAUSTED errors because the orderer's maximum block size
	// will be configured to be safely smaller than the gRPC send/receive limit.
	// For added safety, we can split each block's transactions into more batches, but this is deferred for now
	// until the orderer implements all sanity checks on the configuration provided in the config block.
	// For example, if the orderer can enforce that the maximum block size should be at most half of the
	// maximum message size in gRPC, one batch would be adequate.
	blkToBatch := make(map[uint64]*protosigverifierservice.Batch)
	for _, req := range r.Requests {
		rBatch, ok := blkToBatch[req.Ref.BlockNum]
		if !ok {
			rBatch = &protosigverifierservice.Batch{
				Requests: make([]*protosigverifierservice.Tx, 0, len(r.Requests)),
			}
			blkToBatch[req.Ref.BlockNum] = rBatch
		}

		rBatch.Requests = append(rBatch.Requests, req)
	}

	updateSent := false
	for _, rBatch := range blkToBatch {
		if !updateSent {
			rBatch.Update = r.Update
			updateSent = true
		}

		if err := stream.Send(rBatch); err != nil {
			// ResourceExhausted should not occur here, as we have split a block's transactions
			// into two batches, assuming the block size is less than the maximum gRPC message size.
			return errors.Wrap(err, streamEndErrWrap)
		}
	}

	return nil
}

func (sv *signatureVerifier) receiveStatusAndForwardToOutput(
	stream protosigverifierservice.Verifier_StartStreamClient,
	outputValidatedTxs channel.Writer[dependencygraph.TxNodeBatch],
) error {
	for {
		response, err := stream.Recv()
		if err != nil {
			if grpcerror.HasCode(err, codes.InvalidArgument) {
				// While it is unlikely that svm would send an invalid policy, it could happen
				// if the stored policy in the database is corrupted or maliciously altered, or
				// if there is a bug in the committer that modifies the policy bytes.
				return errors.Join(connection.ErrNonRetryable, err)
			}
			// The stream ended or the SVM was closed.
			return errors.Wrap(err, "receive from stream ended with error")
		}

		logger.Debugf("New batch came from sv to sv manager, contains %d items", len(response.Responses))

		validatedTxs := sv.fetchAndDeleteTxBeingValidated(response)
		if !outputValidatedTxs.Write(validatedTxs) {
			// Since transactions are loaded and deleted from txBeingValidated before their
			// validation results are queued, we must re-queue the transaction to txBeingValidated
			// if its result cannot be added to the outputValidatedTxs queue.
			sv.addTxsBeingValidated(validatedTxs)
			return errors.Wrap(outputValidatedTxs.Context().Err(), "context ended")
		}

		promutil.AddToCounter(sv.metrics.sigverifierTransactionProcessedTotal, len(response.Responses))
	}
}

func (sv *signatureVerifier) fetchAndDeleteTxBeingValidated(
	response *protosigverifierservice.ResponseBatch,
) dependencygraph.TxNodeBatch {
	validatedTxs := dependencygraph.TxNodeBatch(make([]*dependencygraph.TransactionNode, 0, len(response.Responses)))
	// TODO: introduce metrics to measure the lock wait/holding duration.
	sv.txMu.Lock()
	defer sv.txMu.Unlock()
	for _, resp := range response.Responses {
		k := *types.NewHeightFromTxRef(resp.Ref)
		txNode, ok := sv.txBeingValidated[k]
		if !ok {
			continue
		}
		delete(sv.txBeingValidated, k)
		if resp.Status != protoblocktx.Status_COMMITTED {
			txNode.Tx.PrelimInvalidTxStatus = &protovcservice.InvalidTxStatus{Code: resp.Status}
		}
		validatedTxs = append(validatedTxs, txNode)
	}
	return validatedTxs
}

func (sv *signatureVerifier) recoverPendingTransactions(inputTxBatch channel.Writer[dependencygraph.TxNodeBatch]) {
	sv.txMu.Lock()
	defer sv.txMu.Unlock()

	if len(sv.txBeingValidated) == 0 {
		return
	}

	pendingTxs := dependencygraph.TxNodeBatch{}
	for txHeight, txNode := range sv.txBeingValidated {
		logger.Debugf("Recovering tx: %v", txHeight)
		pendingTxs = append(pendingTxs, txNode)
	}
	sv.txBeingValidated = make(map[types.Height]*dependencygraph.TransactionNode)

	inputTxBatch.Write(pendingTxs)
	promutil.AddToCounter(sv.metrics.verifiersRetriedTransactionTotal, len(pendingTxs))
}

func (sv *signatureVerifier) addTxsBeingValidated(txBatch dependencygraph.TxNodeBatch) {
	sv.txMu.Lock()
	defer sv.txMu.Unlock()
	for _, txNode := range txBatch {
		sv.txBeingValidated[*types.NewHeightFromTxRef(txNode.Tx.Ref)] = txNode
	}
}

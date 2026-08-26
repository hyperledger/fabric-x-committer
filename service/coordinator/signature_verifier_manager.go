/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	verifierManagerParams struct {
		clientConfig             *connection.MultiClientConfig
		incomingTxsForValidation <-chan dependencygraph.TxNodeBatch
		outgoingValidatedTxs     chan<- dependencygraph.TxNodeBatch
		metrics                  *managerMetrics
		policyManager            *policyManager
	}

	// verifierAdaptor implements the serviceAdaptor interface for the verifier service.
	verifierAdaptor struct {
		policyManager *policyManager
	}

	// verifierStream implements the serviceStream interface for signature verification.
	verifierStream struct {
		stream        grpc.BidiStreamingClient[servicepb.VerifierBatch, committerpb.TxStatusBatch]
		policyManager *policyManager
		policyVersion uint64
	}
)

var sigInvalidTxStatus = committerpb.Status_ABORTED_SIGNATURE_INVALID

// newVerifierManager instantiate a manager for the verifier services.
// It is responsible for managing all communication with
// all verifier servers. It is responsible for:
// 1. Sending transactions to be verified to the verifier servers.
// 2. Receiving the status of the transactions from the verifier servers.
// 3. Forwarding the verified transactions node to the validator-committer manager.
func newVerifierManager(config *verifierManagerParams) *serviceManager {
	logger.Info("Initializing new VerifierManager")
	return &serviceManager{
		params: serviceManagerParams{
			adaptor:      &verifierAdaptor{policyManager: config.policyManager},
			clientConfig: config.clientConfig,
			incomingTxs:  config.incomingTxsForValidation,
			outgoingTxs:  config.outgoingValidatedTxs,
			metrics:      config.metrics,
		},
	}
}

// NewStream creates a new verifierStream and starts a new stream with the signature verifier server.
//
//nolint:ireturn // returns stream interface by design.
func (va *verifierAdaptor) NewStream(ctx context.Context, conn *grpc.ClientConn) (serviceStream, error) {
	s, err := servicepb.NewVerifierClient(conn).StartStream(ctx)
	if err != nil {
		return nil, err
	}
	return &verifierStream{stream: s, policyManager: va.policyManager}, nil
}

// ApplyResult applies a transaction status to the node.
func (*verifierAdaptor) ApplyResult(txNode *dependencygraph.TransactionNode, status *committerpb.TxStatus) {
	if status.Status != committerpb.Status_COMMITTED {
		txNode.VCTx.PrelimInvalidTxStatus = &status.Status
	}
}

// Send converts a batch of transaction nodes to a verifier batch request.
//
// NOTE: We forward the full VerifierTx (servicepb.TxWithRef) as received from the coordinator,
// so the verifier receives the complete transaction content, including the metadata field.
// Reconstructing the content here would risk dropping fields (see bugfix #629).
func (vs *verifierStream) Send(txsNode dependencygraph.TxNodeBatch) error {
	request := &servicepb.VerifierBatch{
		Requests: make([]*servicepb.TxWithRef, len(txsNode)),
	}

	request.Update, vs.policyVersion = vs.policyManager.getUpdates(vs.policyVersion)

	for i, txNode := range txsNode {
		request.Requests[i] = txNode.VerifierTx
	}

	return vs.stream.Send(request)
}

// Recv extracts the transaction statuses from a result batch.
func (vs *verifierStream) Recv() ([]*committerpb.TxStatus, error) {
	batch, err := vs.stream.Recv()
	if err != nil {
		return nil, err
	}
	return batch.Status, nil
}

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
	"github.com/hyperledger/fabric-x-committer/utils/servicemanager"
)

type (
	verifierManagerParams struct {
		clientConfig             *connection.MultiClientConfig
		incomingTxsForValidation <-chan dependencygraph.TxNodeBatch
		outgoingValidatedTxs     chan<- dependencygraph.TxNodeBatch
		metrics                  *perfMetrics
		policyManager            *policyManager
	}

	// verifierAdaptor implements the servicemanager.Adaptor interface for verifier service.
	verifierAdaptor struct {
		policyManager *policyManager
	}

	// verifierStream implements the servicemanager.Stream interface for signature verification.
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
// 3. Forwarding the status of the transactions to the coordinator.
func newVerifierManager(config *verifierManagerParams) *servicemanager.Manager {
	return &servicemanager.Manager{
		Params: servicemanager.Parameters{
			Adaptor:       &verifierAdaptor{policyManager: config.policyManager},
			ClientConfig:  config.clientConfig,
			IncomingTasks: config.incomingTxsForValidation,
			OutgoingTasks: config.outgoingValidatedTxs,
			Metrics:       config.metrics.verifiers,
		},
	}
}

// NewStream creates a new verifierAdapter and starts a new stream with the signature verifier server.
//
//nolint:ireturn // returns stream interface by design.
func (vsc *verifierAdaptor) NewStream(ctx context.Context, conn *grpc.ClientConn) (servicemanager.Stream, error) {
	s, err := servicepb.NewVerifierClient(conn).StartStream(ctx)
	if err != nil {
		return nil, err
	}
	return &verifierStream{stream: s, policyManager: vsc.policyManager}, nil
}

// ApplyResult applies a transaction status result to the node.
func (*verifierAdaptor) ApplyResult(job *dependencygraph.TransactionNode, result *committerpb.TxStatus) {
	if result.Status != committerpb.Status_COMMITTED {
		job.VCTx.PrelimInvalidTxStatus = &result.Status
	}
}

// Send converts a slice of transaction nodes to a verifier batch request.
//
// NOTE: We forward the full VerifierTx (servicepb.TxWithRef) as received from the coordinator,
// so the verifier receives the complete transaction content, including the metadata field.
// Reconstructing the content here would risk dropping fields (see bugfix #629).
func (a *verifierStream) Send(jobs []*dependencygraph.TransactionNode) error {
	request := &servicepb.VerifierBatch{
		Requests: make([]*servicepb.TxWithRef, len(jobs)),
	}

	request.Update, a.policyVersion = a.policyManager.getUpdates(a.policyVersion)

	for i, txNode := range jobs {
		request.Requests[i] = txNode.VerifierTx
	}

	return a.stream.Send(request)
}

// Recv extracts transaction status items from a result batch.
func (a *verifierStream) Recv() ([]*committerpb.TxStatus, error) {
	batch, err := a.stream.Recv()
	if err != nil {
		return nil, err
	}
	return batch.Status, nil
}

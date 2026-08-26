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
	validatorCommitterManagerConfig struct {
		clientConfig                   *connection.MultiClientConfig
		incomingTxsForValidationCommit <-chan dependencygraph.TxNodeBatch
		outgoingValidatedTxsNode       chan<- dependencygraph.TxNodeBatch
		outgoingTxsStatus              *txStatusQueue
		metrics                        *managerMetrics
		policyMgr                      *policyManager
	}

	// vcAdaptor implements the serviceAdaptor interface for the validator-committer service.
	vcAdaptor struct {
		policyMgr *policyManager
	}

	// vcStream implements the serviceStream interface for the validator-committer service.
	vcStream struct {
		stream grpc.BidiStreamingClient[servicepb.VcBatch, committerpb.TxStatusBatch]
	}
)

// newValidatorCommitterManager instantiate a manager for the VC services.
// It is responsible for managing all communication with
// all VC services. It is responsible for:
// 1. Sending transactions to be validated and committed to the VC services.
// 2. Receiving the status of the transactions from the VC services.
// 3. Forwarding the validated transactions node to the dependency graph manager.
// 4. Forwarding the status of the transactions to the coordinator.
//
// The request/response API that any vcservice can serve is not part of this manager; see
// validatorCommitterAPI.
func newValidatorCommitterManager(c *validatorCommitterManagerConfig) *serviceManager {
	logger.Info("Initializing new ValidatorCommitterManager")
	return &serviceManager{
		params: serviceManagerParams{
			adaptor:         &vcAdaptor{policyMgr: c.policyMgr},
			clientConfig:    c.clientConfig,
			incomingTxs:     c.incomingTxsForValidationCommit,
			outgoingTxs:     c.outgoingValidatedTxsNode,
			outgoingResults: c.outgoingTxsStatus,
			metrics:         c.metrics,
		},
	}
}

// NewStream creates a new vcStream and starts a new stream with the validator-committer service.
//
//nolint:ireturn // returns stream interface by design.
func (*vcAdaptor) NewStream(ctx context.Context, conn *grpc.ClientConn) (serviceStream, error) {
	s, err := servicepb.NewValidationAndCommitServiceClient(conn).StartValidateAndCommitStream(ctx)
	if err != nil {
		return nil, err
	}
	return &vcStream{stream: s}, nil
}

// ApplyResult applies a transaction status to the node and updates the policies if needed.
func (vca *vcAdaptor) ApplyResult(txNode *dependencygraph.TransactionNode, status *committerpb.TxStatus) {
	if status.Status == committerpb.Status_COMMITTED {
		// Updating policy before sending transaction nodes to the dependency
		// graph manager to free dependent transactions. Otherwise, dependent transactions
		// might be validated against a stale policy.
		vca.policyMgr.updateFromTx(txNode.VCTx.Namespaces)
	}
}

// Send converts a batch of transaction nodes to a VcBatch and sends it to the VC service.
func (vs *vcStream) Send(txsNode dependencygraph.TxNodeBatch) error {
	vcBatch := &servicepb.VcBatch{
		Transactions: make([]*servicepb.VcTx, len(txsNode)),
	}
	for i, txNode := range txsNode {
		vcBatch.Transactions[i] = txNode.VCTx
	}

	logger.Debugf("Sending batch with %d transactions to VC service", len(txsNode))
	return vs.stream.Send(vcBatch)
}

// Recv extracts the transaction statuses from a result batch.
func (vs *vcStream) Recv() ([]*committerpb.TxStatus, error) {
	batch, err := vs.stream.Recv()
	if err != nil {
		return nil, err
	}

	logger.Debugf("Received batch with %d transaction statuses from VC service", len(batch.Status))
	return batch.Status, nil
}

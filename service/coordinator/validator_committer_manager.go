/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/servicemanager"
)

type (
	// validatorCommitterAPI expose API to interact with any VC.
	validatorCommitterAPI struct {
		conn      *grpc.ClientConn
		client    servicepb.ValidationAndCommitServiceClient
		policyMgr *policyManager
	}

	validatorCommitterManagerConfig struct {
		clientConfig                   *connection.MultiClientConfig
		incomingTxsForValidationCommit <-chan dependencygraph.TxNodeBatch
		outgoingValidatedTxsNode       chan<- dependencygraph.TxNodeBatch
		outgoingTxsStatus              *txStatusQueue
		metrics                        *perfMetrics
		policyMgr                      *policyManager
	}

	// vcAdaptor implements the servicemanager.Adaptor interface for validator-committer service.
	vcAdaptor struct {
		policyMgr *policyManager
	}

	// vcStream implements the servicemanager.Stream interface for validator-committer service.
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
func newValidatorCommitterManager(c *validatorCommitterManagerConfig) *servicemanager.Manager {
	logger.Info("Initializing new ValidatorCommitterManager")
	return &servicemanager.Manager{
		Params: servicemanager.Parameters{
			Adaptor:         &vcAdaptor{policyMgr: c.policyMgr},
			ClientConfig:    c.clientConfig,
			IncomingTasks:   c.incomingTxsForValidationCommit,
			OutgoingTasks:   c.outgoingValidatedTxsNode,
			OutgoingResults: c.outgoingTxsStatus,
			Metrics:         c.metrics.vcs,
		},
	}
}

// NewStream creates a new vcStream and starts a new stream with the validator-committer service.
//
//nolint:ireturn // returns stream interface by design.
func (*vcAdaptor) NewStream(ctx context.Context, conn *grpc.ClientConn) (servicemanager.Stream, error) {
	s, err := servicepb.NewValidationAndCommitServiceClient(conn).StartValidateAndCommitStream(ctx)
	if err != nil {
		return nil, err
	}
	return &vcStream{stream: s}, nil
}

// ApplyResult applies a transaction status result to the node and updates policies if needed.
func (vca *vcAdaptor) ApplyResult(job *dependencygraph.TransactionNode, result *committerpb.TxStatus) {
	if result.Status == committerpb.Status_COMMITTED {
		// Updating policy before sending transaction nodes to the dependency
		// graph manager to free dependent transactions. Otherwise, dependent transactions
		// might be validated against a stale policy.
		vca.policyMgr.updateFromTx(job.VCTx.Namespaces)
	}
}

// Send converts a slice of transaction nodes to a VcBatch and sends it to the VC service.
func (vs *vcStream) Send(jobs []*dependencygraph.TransactionNode) error {
	batchSize := len(jobs)

	vcBatch := &servicepb.VcBatch{
		Transactions: make([]*servicepb.VcTx, batchSize),
	}

	for i, txNode := range jobs {
		vcBatch.Transactions[i] = txNode.VCTx
	}

	logger.Debugf("Sending batch with %d transactions to VC service", batchSize)
	return vs.stream.Send(vcBatch)
}

// Recv extracts transaction status items from a result batch.
func (vs *vcStream) Recv() ([]*committerpb.TxStatus, error) {
	batch, err := vs.stream.Recv()
	if err != nil {
		return nil, err
	}

	logger.Debugf("Received batch with %d transaction statuses from VC service", len(batch.Status))
	return batch.Status, nil
}

func newValidatorCommitterAPI(ctx context.Context, conf *connection.MultiClientConfig, policyMgr *policyManager) (
	*validatorCommitterAPI, error,
) {
	logger.Infof("Connections to %d vc's will be opened from vc manager", len(conf.Endpoints))

	// Create common connection for setup and query operations
	commonConn, err := connection.NewLoadBalancedConnection(conf)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection to validator persisters: %w", err)
	}
	commonClient := servicepb.NewValidationAndCommitServiceClient(commonConn)

	_, setupErr := commonClient.SetupSystemTablesAndNamespaces(ctx, nil)
	if setupErr != nil {
		connection.CloseConnectionsLog(commonConn)
		return nil, errors.Wrap(setupErr, "failed to setup system tables and namespaces")
	}
	return &validatorCommitterAPI{
		conn:      commonConn,
		client:    commonClient,
		policyMgr: policyMgr,
	}, nil
}

func (vca *validatorCommitterAPI) close() {
	connection.CloseConnectionsLog(vca.conn)
}

func (vca *validatorCommitterAPI) setLastCommittedBlockNumber(
	ctx context.Context,
	lastBlock *servicepb.BlockRef,
) error {
	_, err := vca.client.SetLastCommittedBlockNumber(ctx, lastBlock)
	return grpcerror.WrapWithContext(err, "failed setting the last committed block number")
}

func (vca *validatorCommitterAPI) getNextBlockNumberToCommit(
	ctx context.Context,
) (*servicepb.BlockRef, error) {
	ret, err := vca.client.GetNextBlockNumberToCommit(ctx, nil)
	return ret, grpcerror.WrapWithContext(err, "failed getting the next expected block number")
}

func (vca *validatorCommitterAPI) getTransactionsStatus(
	ctx context.Context,
	query *committerpb.TxIDsBatch,
) (*committerpb.TxStatusBatch, error) {
	ret, err := vca.client.GetTransactionsStatus(ctx, query)
	return ret, grpcerror.WrapWithContext(err, "failed getting transactions status")
}

func (vca *validatorCommitterAPI) getNamespacePolicies(
	ctx context.Context,
) (*applicationpb.NamespacePolicies, error) {
	ret, err := vca.client.GetNamespacePolicies(ctx, nil)
	return ret, grpcerror.WrapWithContext(err, "failed loading policies")
}

func (vca *validatorCommitterAPI) getConfigTransaction(
	ctx context.Context,
) (*applicationpb.ConfigTransaction, error) {
	ret, err := vca.client.GetConfigTransaction(ctx, nil)
	return ret, grpcerror.WrapWithContext(err, "failed loading config transaction")
}

func (vca *validatorCommitterAPI) recoverPolicyManagerFromStateDB(ctx context.Context) error {
	policyMsg, err := vca.getNamespacePolicies(ctx)
	if err != nil {
		return err
	}
	configMsg, err := vca.getConfigTransaction(ctx)
	if err != nil {
		return err
	}
	if len(policyMsg.Policies) == 0 && configMsg.Envelope == nil {
		return nil
	}
	vca.policyMgr.update(&servicepb.VerifierUpdates{
		NamespacePolicies: policyMsg,
		Config:            configMsg,
	})
	return nil
}

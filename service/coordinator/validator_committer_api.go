/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"fmt"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

// validatorCommitterAPI exposes the request/response API that any vcservice can serve.
//
// These calls are unrelated to the transaction pipeline that the validator-committer manager
// drives: they are served by whichever vcservice a load-balanced connection happens to pick, and
// the coordinator issues them outside the pipeline -- once during recovery, and then on demand from
// its own gRPC handlers. Holding them here leaves the manager responsible only for streaming
// transactions to the vcservices.
type validatorCommitterAPI struct {
	conn      *grpc.ClientConn
	client    servicepb.ValidationAndCommitServiceClient
	policyMgr *policyManager
}

func newValidatorCommitterAPI(
	conf *connection.MultiClientConfig,
	policyMgr *policyManager,
) (*validatorCommitterAPI, error) {
	conn, err := connection.NewLoadBalancedConnection(conf)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection to validator persisters: %w", err)
	}
	return &validatorCommitterAPI{
		conn:      conn,
		client:    servicepb.NewValidationAndCommitServiceClient(conn),
		policyMgr: policyMgr,
	}, nil
}

func (vca *validatorCommitterAPI) close() {
	connection.CloseConnectionsLog(vca.conn)
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
) (*servicepb.TxStatusBatch, error) {
	ret, err := vca.client.GetTransactionsStatus(ctx, query)
	return ret, grpcerror.WrapWithContext(err, "failed getting transactions status")
}

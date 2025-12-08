/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

// SigVerifier is a mock implementation of the protosignverifierservice.VerifierServer.
// SigVerifier marks valid and invalid flag as follows:
// - when the tx has empty signature, it is invalid.
// - when the tx has non-empty signature, it is valid.
type SigVerifier struct {
	protosigverifierservice.UnimplementedVerifierServer
	updates                    []*protosigverifierservice.VerifierUpdate
	requestBatch               chan *protosigverifierservice.VerifierBatch
	numBlocksReceived          atomic.Uint32
	returnErrForUpdatePolicies atomic.Bool
	policyUpdateCounter        atomic.Uint64
	healthcheck                *health.Server
	// MockFaultyNodeDropSize allows mocking a faulty node by dropping some TXs.
	MockFaultyNodeDropSize int
}

// NewMockSigVerifier returns a new mock verifier.
func NewMockSigVerifier() *SigVerifier {
	return &SigVerifier{
		requestBatch: make(chan *protosigverifierservice.VerifierBatch, 10),
		healthcheck:  connection.DefaultHealthCheckService(),
	}
}

// RegisterService registers for the verifier's GRPC services.
func (m *SigVerifier) RegisterService(server *grpc.Server) {
	protosigverifierservice.RegisterVerifierServer(server, m)
	healthgrpc.RegisterHealthServer(server, m.healthcheck)
}

func (m *SigVerifier) updatePolicies(update *protosigverifierservice.VerifierUpdate) error {
	if update == nil {
		return nil
	}
	m.policyUpdateCounter.Add(1)
	if m.returnErrForUpdatePolicies.CompareAndSwap(true, false) {
		return errors.Wrap(verifier.ErrUpdatePolicies, "failed to update the policies")
	}
	m.updates = append(m.updates, update)
	logger.Info("policies has been updated")
	return nil
}

// StartStream is a mock implementation of the [protosignverifierservice.VerifierServer].
func (m *SigVerifier) StartStream(stream protosigverifierservice.Verifier_StartStreamServer) error {
	logger.Info("Starting verifier stream")
	defer logger.Info("Closed verifier stream")
	g, eCtx := errgroup.WithContext(stream.Context())
	g.Go(func() error {
		return m.receiveRequestBatch(eCtx, stream)
	})
	g.Go(func() error {
		return m.sendResponseBatch(eCtx, stream)
	})

	err := g.Wait()
	if errors.Is(err, verifier.ErrUpdatePolicies) {
		return grpcerror.WrapInvalidArgument(err)
	}
	return grpcerror.WrapCancelled(err)
}

func (m *SigVerifier) receiveRequestBatch(
	ctx context.Context,
	stream protosigverifierservice.Verifier_StartStreamServer,
) error {
	requestBatch := channel.NewWriter(ctx, m.requestBatch)
	for ctx.Err() == nil {
		reqBatch, err := stream.Recv()
		if err != nil {
			return errors.Wrap(err, "error receiving request batch")
		}

		err = m.updatePolicies(reqBatch.Update)
		if err != nil {
			return err
		}

		logger.Debugf("new batch received at the mock sig verifier with %d requests.", len(reqBatch.Requests))
		requestBatch.Write(reqBatch)
		m.numBlocksReceived.Add(1)
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

func (m *SigVerifier) sendResponseBatch(
	ctx context.Context,
	stream protosigverifierservice.Verifier_StartStreamServer,
) error {
	requestBatch := channel.NewReader(ctx, m.requestBatch)
	for ctx.Err() == nil {
		reqBatch, ok := requestBatch.Read()
		if !ok {
			break
		}
		respBatch := &protosigverifierservice.VerifierResponseBatch{
			Responses: make([]*protosigverifierservice.VerifierResponse, 0, len(reqBatch.Requests)),
		}

		for i, req := range reqBatch.Requests {
			if i < m.MockFaultyNodeDropSize {
				// We simulate a faulty node by not responding to the first X TXs.
				continue
			}
			status := applicationpb.Status_COMMITTED
			isConfig := len(req.Tx.Namespaces) == 1 && req.Tx.Namespaces[0].NsId == committerpb.ConfigNamespaceID
			if len(req.Tx.Endorsements) == 0 && !isConfig {
				status = applicationpb.Status_ABORTED_SIGNATURE_INVALID
			}
			respBatch.Responses = append(respBatch.Responses, &protosigverifierservice.VerifierResponse{
				Ref:    req.Ref,
				Status: status,
			})
		}

		if err := stream.Send(respBatch); err != nil {
			return errors.Wrap(err, "error sending response batch")
		}
	}
	return errors.Wrap(ctx.Err(), "context ended")
}

// GetNumBlocksReceived returns the number of blocks received by the mock verifier.
func (m *SigVerifier) GetNumBlocksReceived() uint32 {
	return m.numBlocksReceived.Load()
}

// GetUpdates returns the updates received.
func (m *SigVerifier) GetUpdates() []*protosigverifierservice.VerifierUpdate {
	return m.updates
}

// SetReturnErrorForUpdatePolicies configures the SigVerifier to return an error during policy updates.
// When setError is true, the verifier will signal an error during the update policies process.
// It is a one time event, after which the flag will return to false.
func (m *SigVerifier) SetReturnErrorForUpdatePolicies(setError bool) {
	m.returnErrForUpdatePolicies.Store(setError)
}

// GetPolicyUpdateCounter returns the number of policy updates to check progress.
func (m *SigVerifier) GetPolicyUpdateCounter() uint64 {
	return m.policyUpdateCounter.Load()
}

// ClearPolicies allows resetting the known policies to mimic a new instance.
func (m *SigVerifier) ClearPolicies() {
	m.updates = nil
}

// SendRequestBatchWithoutStream allows the caller to bypass the stream to send
// a request batch.
// Returns true if successful.
func (m *SigVerifier) SendRequestBatchWithoutStream(
	ctx context.Context,
	requestBatch *protosigverifierservice.VerifierBatch,
) bool {
	return channel.NewWriter(ctx, m.requestBatch).Write(requestBatch)
}

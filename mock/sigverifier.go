/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

type (
	// Verifier is a mock implementation of servicepb.VerifierServer.
	// Verifier marks valid and invalid flag as follows:
	// - when the tx has empty signature, it is invalid.
	// - when the tx has non-empty signature, it is valid.
	Verifier struct {
		servicepb.UnimplementedVerifierServer
		streamStateManager[VerifierStreamState]
		healthcheck *health.Server
	}

	// VerifierStreamState holds the state of a verifier stream.
	VerifierStreamState struct {
		requestBatch               chan *servicepb.VerifierBatch
		updates                    []*servicepb.VerifierUpdates
		numBlocksReceived          atomic.Uint32
		returnErrForUpdatePolicies atomic.Bool
		policyUpdateCounter        atomic.Uint64
		// MockFaultyNodeDropSize allows mocking a faulty node by dropping some TXs.
		MockFaultyNodeDropSize int
	}
)

// NewMockSigVerifier returns a new mock verifier.
func NewMockSigVerifier() *Verifier {
	return &Verifier{
		healthcheck: connection.DefaultHealthCheckService(),
	}
}

// RegisterService registers for the verifier's GRPC services.
func (m *Verifier) RegisterService(server *grpc.Server) {
	servicepb.RegisterVerifierServer(server, m)
	healthgrpc.RegisterHealthServer(server, m.healthcheck)
}

// StartStream is a mock implementation of the [protosignverifierservice.VerifierServer].
func (m *Verifier) StartStream(stream servicepb.Verifier_StartStreamServer) error {
	s := &VerifierStreamState{
		requestBatch: make(chan *servicepb.VerifierBatch, 10),
	}
	state := m.allocateStream(stream.Context(), s)
	defer m.releaseStream(state)

	g, eCtx := errgroup.WithContext(stream.Context())
	g.Go(func() error {
		return s.receiveRequestBatch(eCtx, stream)
	})
	g.Go(func() error {
		return s.sendResponseBatch(eCtx, stream)
	})

	err := g.Wait()
	if errors.Is(err, verifier.ErrUpdatePolicies) {
		return grpcerror.WrapInvalidArgument(err)
	}
	return grpcerror.WrapCancelled(err)
}

func (m *VerifierStreamState) updatePolicies(update *servicepb.VerifierUpdates) error {
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

func (m *VerifierStreamState) receiveRequestBatch(
	ctx context.Context,
	stream servicepb.Verifier_StartStreamServer,
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

func (m *VerifierStreamState) sendResponseBatch(
	ctx context.Context,
	stream servicepb.Verifier_StartStreamServer,
) error {
	requestBatch := channel.NewReader(ctx, m.requestBatch)
	for ctx.Err() == nil {
		reqBatch, ok := requestBatch.Read()
		if !ok {
			break
		}
		respBatch := &committerpb.TxStatusBatch{
			Status: make([]*committerpb.TxStatus, 0, len(reqBatch.Requests)),
		}

		for i, req := range reqBatch.Requests {
			if i < m.MockFaultyNodeDropSize {
				// We simulate a faulty node by not responding to the first X TXs.
				continue
			}
			status := committerpb.Status_COMMITTED
			txNs := req.Content.Namespaces
			isConfig := len(txNs) == 1 && txNs[0].NsId == committerpb.ConfigNamespaceID
			if len(req.Content.Endorsements) == 0 && !isConfig {
				status = committerpb.Status_ABORTED_SIGNATURE_INVALID
			}
			respBatch.Status = append(respBatch.Status, &committerpb.TxStatus{
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
func (m *VerifierStreamState) GetNumBlocksReceived() uint32 {
	return m.numBlocksReceived.Load()
}

// GetUpdates returns the updates received.
func (m *VerifierStreamState) GetUpdates() []*servicepb.VerifierUpdates {
	return m.updates
}

// SetReturnErrorForUpdatePolicies configures the Verifier to return an error during policy updates.
// When setError is true, the verifier will signal an error during the update policies process.
// It is a one time event, after which the flag will return to false.
func (m *VerifierStreamState) SetReturnErrorForUpdatePolicies(setError bool) {
	m.returnErrForUpdatePolicies.Store(setError)
}

// GetPolicyUpdateCounter returns the number of policy updates to check progress.
func (m *VerifierStreamState) GetPolicyUpdateCounter() uint64 {
	return m.policyUpdateCounter.Load()
}

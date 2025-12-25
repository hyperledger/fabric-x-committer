/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

// Server implements verifier.Server.
type Server struct {
	servicepb.UnimplementedVerifierServer
	config      *Config
	metrics     *metrics
	healthcheck *health.Server
}

var (
	logger = logging.New("verifier")

	// ErrUpdatePolicies is returned when UpdatePolicies fails to parse a given policy.
	ErrUpdatePolicies = errors.New("failed to update policies")
)

// New instantiate a new VerifierServer.
func New(config *Config) *Server {
	logger.Info("Initializing new verifier server")
	return &Server{
		config:      config,
		metrics:     newMonitoring(),
		healthcheck: connection.DefaultHealthCheckService(),
	}
}

// Run the verifier background service.
func (s *Server) Run(ctx context.Context) error {
	_ = s.metrics.Provider.StartPrometheusServer(ctx, s.config.Monitoring.Server)
	// We don't return error here to avoid stopping the service due to monitoring error.
	// But we use the errgroup to ensure the method returns only when the server exits.
	return nil
}

// WaitForReady wait for service to be ready to be exposed as gRPC service.
// If the context ended before the service is ready, returns false.
func (*Server) WaitForReady(context.Context) bool {
	return true
}

// RegisterService registers for the verifier's GRPC services.
func (s *Server) RegisterService(server *grpc.Server) {
	servicepb.RegisterVerifierServer(server, s)
	healthgrpc.RegisterHealthServer(server, s.healthcheck)
}

// StartStream starts a verification stream.
func (s *Server) StartStream(stream servicepb.Verifier_StartStreamServer) error {
	defer logger.Debug("Interrupted stream.")
	s.metrics.ActiveStreams.Inc()
	defer s.metrics.ActiveStreams.Dec()

	// We create a new executor for each stream to avoid answering to the wrong stream.
	executor := newParallelExecutor(&s.config.ParallelExecutor)
	g, gCtx := errgroup.WithContext(stream.Context())
	g.Go(func() error {
		return s.handleInputs(gCtx, stream, executor)
	})
	g.Go(func() error {
		return s.handleOutputs(gCtx, stream, executor)
	})
	g.Go(func() error {
		executor.handleCutoff(gCtx)
		return gCtx.Err()
	})
	for range executor.config.Parallelism {
		g.Go(func() error {
			executor.handleChannelInput(gCtx)
			return gCtx.Err()
		})
	}

	err := g.Wait()
	if errors.Is(err, ErrUpdatePolicies) {
		return grpcerror.WrapInvalidArgument(err)
	}
	return grpcerror.WrapCancelled(g.Wait())
}

func (s *Server) handleInputs(
	ctx context.Context,
	stream servicepb.Verifier_StartStreamServer,
	executor *parallelExecutor,
) error {
	// ctx should be a child of stream.Context() so it will end with it.
	input := channel.NewWriter(ctx, executor.inputCh)
	for {
		batch, rpcErr := stream.Recv()
		if rpcErr != nil {
			return errors.Wrap(rpcErr, "stream ended")
		}
		logger.Debugf("Received input from client with %v requests", len(batch.Requests))

		// NOTE: When the sidecar receives a config transaction, it waits for all preceding
		// transactions to complete before submitting it to the coordinator. The actual
		// configuration update on the verifier is deferred. It is applied just-in-time,
		// immediately before the next data transaction is processed, ensuring that
		// transaction and all subsequent ones operate under the new configuration.

		// Update policies if included in the batch.
		err := executor.verifier.updatePolicies(batch.Update)
		if err != nil {
			return errors.Join(ErrUpdatePolicies, err)
		}
		promutil.AddToCounter(s.metrics.VerifierServerInTxs, len(batch.Requests))
		promutil.AddToGauge(s.metrics.ActiveRequests, len(batch.Requests))

		// Pass verification requests for processing.
		for _, r := range batch.Requests {
			if ok := input.Write(r); !ok {
				return errors.Wrap(stream.Context().Err(), "context ended")
			}
		}
	}
}

func (s *Server) handleOutputs(
	ctx context.Context,
	stream servicepb.Verifier_StartStreamServer,
	executor *parallelExecutor,
) error {
	// ctx should be a child of stream.Context() so it will end with it.
	output := channel.NewReader(ctx, executor.outputCh)
	for {
		outputs, ok := output.Read()
		if !ok {
			return errors.Wrap(stream.Context().Err(), "context ended")
		}
		promutil.AddToCounter(s.metrics.VerifierServerOutTxs, len(outputs))
		promutil.AddToGauge(s.metrics.ActiveRequests, -len(outputs))
		logger.Debugf("Received output: %v", output)
		rpcErr := stream.Send(&committerpb.TxStatusBatch{Status: outputs})
		if rpcErr != nil {
			return errors.Wrap(rpcErr, "stream ended")
		}
		logger.Debugf("Forwarded output to client.")
	}
}

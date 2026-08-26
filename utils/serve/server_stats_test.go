/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve_test

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	healthCheckMethod = healthgrpc.Health_Check_FullMethodName
	healthWatchMethod = healthgrpc.Health_Watch_FullMethodName

	// statusOK is the gRPC status of a successful unary RPC; statusCanceled is the status the
	// server records for a stream the client tears down by cancelling its context.
	statusOK       = "OK"
	statusCanceled = "Canceled"
)

type (
	// statsRegisterer wires the stats-handler metrics and a health service onto a test server.
	// The health service is either the real one or a stubHealthServer, so a test can choose whether
	// its Check/Watch RPCs succeed or fail with a chosen gRPC status.
	statsRegisterer struct {
		serverMetrics *serve.ServerMetrics
		healthServer  healthgrpc.HealthServer
	}

	// serverStatsTestEnv bundles a running server wired with the gRPC stats handler and a health
	// client, so a test can drive real RPCs against it and read back the recorded metrics.
	serverStatsTestEnv struct {
		metrics      *serve.ServerMetrics
		health       healthgrpc.HealthClient
		grpcEndpoint connection.Endpoint
	}

	// stubHealthServer is a health service whose Check (unary) and Watch (streaming) RPCs return
	// returnedErr, so a test can drive a real RPC to a chosen gRPC status and observe how the stats
	// handler recorded it. A nil returnedErr makes both RPCs succeed.
	stubHealthServer struct {
		healthgrpc.UnimplementedHealthServer
		returnedErr error
	}
)

func (r *statsRegisterer) RegisterService(srv serve.Servers) {
	serve.RegisterServerMetrics(srv.StatsHandler, r.serverMetrics)
	healthgrpc.RegisterHealthServer(srv.GRPC, r.healthServer)
}

func (s stubHealthServer) Check(
	context.Context, *healthgrpc.HealthCheckRequest,
) (*healthgrpc.HealthCheckResponse, error) {
	if s.returnedErr != nil {
		return nil, s.returnedErr
	}
	return &healthgrpc.HealthCheckResponse{Status: healthgrpc.HealthCheckResponse_SERVING}, nil
}

func (s stubHealthServer) Watch(*healthgrpc.HealthCheckRequest, healthgrpc.Health_WatchServer) error {
	return s.returnedErr
}

// TestServerConnStatsHandler verifies the active-connections gauge end to end:
// wired through the normal RegisterService path, it rises as real clients connect
// and returns to zero as they disconnect.
func TestServerConnStatsHandler(t *testing.T) {
	t.Parallel()

	t.Log("Starting service")

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env := newServerStatsTestEnv(ctx, t, serve.DefaultHealthCheckService())

	t.Log("Creating clients")
	conn := test.NewInsecureConnection(t, &env.grpcEndpoint)
	conn2 := test.NewInsecureConnection(t, &env.grpcEndpoint)

	test.RequireIntMetricValue(t, 0, env.metrics.ActiveConnections)

	t.Log("Connecting clients")
	conn.Connect()
	test.EventuallyIntMetric(t, 1, env.metrics.ActiveConnections, 30*time.Second, 100*time.Millisecond)
	conn2.Connect()
	test.EventuallyIntMetric(t, 2, env.metrics.ActiveConnections, 30*time.Second, 100*time.Millisecond)

	t.Log("Disconnecting clients")
	require.NoError(t, conn.Close())
	require.NoError(t, conn2.Close())
	test.EventuallyIntMetric(t, 0, env.metrics.ActiveConnections, 30*time.Second, 100*time.Millisecond)
}

// TestServerStatsHandlerUnaryRPC verifies the handler's unary workflow: a completed unary RPC
// increments requestsTotal and records its latency, and is never counted as an active stream.
func TestServerStatsHandlerUnaryRPC(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env := newServerStatsTestEnv(ctx, t, serve.DefaultHealthCheckService())

	_, err := env.health.Check(ctx, &healthgrpc.HealthCheckRequest{})
	require.NoError(t, err)

	test.EventuallyIntMetric(
		t, 1,
		env.metrics.RequestsTotal.WithLabelValues(healthCheckMethod),
		30*time.Second, 100*time.Millisecond,
	)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Positive(ct, metricVecValue(ct,
			env.metrics.LatencySeconds.MetricVec, healthCheckMethod, statusOK))
	}, 30*time.Second, 100*time.Millisecond)

	// A unary RPC must never be treated as a stream.
	test.EventuallyIntMetric(
		t, 0,
		env.metrics.ActiveStreams.WithLabelValues(healthCheckMethod),
		30*time.Second, 100*time.Millisecond,
	)
}

// TestServerStatsHandlerStreamingRPC verifies the handler's streaming workflow: an open stream is
// counted in activeStreams, and tearing it down decrements the gauge, increments requestsTotal,
// and records the stream's duration.
func TestServerStatsHandlerStreamingRPC(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env := newServerStatsTestEnv(ctx, t, serve.DefaultHealthCheckService())

	streamCtx, cancelStream := context.WithCancel(ctx)
	t.Cleanup(cancelStream)

	stream, err := env.health.Watch(streamCtx, &healthgrpc.HealthCheckRequest{})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.NoError(t, err)

	test.EventuallyIntMetric(
		t, 1,
		env.metrics.ActiveStreams.WithLabelValues(healthWatchMethod),
		30*time.Second, 100*time.Millisecond,
	)

	// The RPC started, so requestsTotal should be 1.
	test.EventuallyIntMetric(
		t, 1,
		env.metrics.RequestsTotal.WithLabelValues(healthWatchMethod),
		30*time.Second, 100*time.Millisecond,
	)

	// Tearing the stream down completes the RPC: the gauge returns to zero and the stream duration is recorded.
	cancelStream()

	test.EventuallyIntMetric(
		t, 0,
		env.metrics.ActiveStreams.WithLabelValues(healthWatchMethod),
		30*time.Second, 100*time.Millisecond,
	)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Positive(ct, metricVecValue(ct,
			env.metrics.StreamDurationSeconds.MetricVec, healthWatchMethod, statusCanceled))
	}, 30*time.Second, 100*time.Millisecond)
}

// TestServerStatsHandlerRecordsRPCStatus drives real RPCs whose handler returns a chosen error (or
// nil for success), and asserts the stats handler records each under the gRPC status the server
// produced.
func TestServerStatsHandlerRecordsRPCStatus(t *testing.T) {
	t.Parallel()

	// Each gRPC status error is recorded under its own code.
	for _, code := range []codes.Code{
		codes.Canceled,
		codes.Unknown,
		codes.InvalidArgument,
		codes.NotFound,
		codes.AlreadyExists,
		codes.PermissionDenied,
		codes.FailedPrecondition,
		codes.Aborted,
		codes.OutOfRange,
		codes.Unimplemented,
		codes.Internal,
		codes.DataLoss,
		codes.Unauthenticated,
	} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			requireRPCStatusRecorded(t, status.Error(code, ""), code.String())
		})
	}

	// A non-gRPC error is recorded as Unknown.
	t.Run("non-gRPC error", func(t *testing.T) {
		t.Parallel()
		requireRPCStatusRecorded(t, errors.New("not a gRPC error"), codes.Unknown.String())
	})

	// A nil error is recorded as OK.
	t.Run("nil error", func(t *testing.T) {
		t.Parallel()
		requireRPCStatusRecorded(t, nil, codes.OK.String())
	})
}

// requireRPCStatusRecorded drives a real unary (Check) and streaming (Watch) RPC whose handler
// returns rpcErr, and asserts each is recorded under wantStatus. Check under LatencySeconds,
// Watch under StreamDurationSeconds.
func requireRPCStatusRecorded(t *testing.T, rpcErr error, wantStatus string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env := newServerStatsTestEnv(ctx, t, stubHealthServer{returnedErr: rpcErr})

	// Both RPCs return rpcErr; gRPC turns it into a status and reports it to the stats handler.
	_, _ = env.health.Check(ctx, &healthgrpc.HealthCheckRequest{})
	stream, err := env.health.Watch(ctx, &healthgrpc.HealthCheckRequest{})
	require.NoError(t, err)
	_, _ = stream.Recv() // drive the stream to completion so its End callback fires

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, 1, testutil.CollectAndCount(env.metrics.LatencySeconds))
		require.Positive(
			ct,
			metricVecValue(ct, env.metrics.LatencySeconds.MetricVec, healthCheckMethod, wantStatus),
		)
		require.Equal(ct, 1, testutil.CollectAndCount(env.metrics.StreamDurationSeconds))
		require.Positive(
			ct,
			metricVecValue(ct, env.metrics.StreamDurationSeconds.MetricVec, healthWatchMethod, wantStatus),
		)
	}, 30*time.Second, 100*time.Millisecond)
}

// newServerStatsTestEnv starts a server wired with the stats handler and the given health service,
// and returns the recorded metrics together with a connected health client.
func newServerStatsTestEnv(
	ctx context.Context, t *testing.T, healthServer healthgrpc.HealthServer,
) *serverStatsTestEnv {
	t.Helper()
	m := serve.NewServerMetrics(monitoring.NewProvider(), monitoring.MetricsParameters{
		Namespace: "test",
		Subsystem: "server_stats",
	})
	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	test.ServeForTest(ctx, t, serverConfig, &statsRegisterer{serverMetrics: m, healthServer: healthServer})
	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	return &serverStatsTestEnv{
		metrics:      m,
		health:       healthgrpc.NewHealthClient(conn),
		grpcEndpoint: serverConfig.GRPC.Endpoint,
	}
}

func metricVecValue(t test.TestingT, mv *prometheus.MetricVec, lvs ...string) float64 {
	t.Helper()
	m, err := mv.GetMetricWithLabelValues(lvs...)
	require.NoError(t, err)
	return test.GetMetricValue(t, m)
}

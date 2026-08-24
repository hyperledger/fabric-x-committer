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
	"google.golang.org/grpc/stats"
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

	unaryMethod  = "/test.Service/Unary"
	streamMethod = "/test.Service/Stream"
)

type (
	statsRegisterer struct {
		serverMetrics *serve.ServerMetrics
	}

	// serverStatsTestEnv bundles a running server wired with the gRPC stats handler and a health
	// client, so each workflow test can drive real RPCs against it.
	serverStatsTestEnv struct {
		metrics      *serve.ServerMetrics
		health       healthgrpc.HealthClient
		grpcEndpoint connection.Endpoint
	}

	recordRPCParams struct {
		handler        *serve.ServerStatsHandler
		methodName     string
		isServerStream bool
		isClientStream bool
		recordedError  error
	}
)

func (r *statsRegisterer) RegisterService(srv serve.Servers) {
	serve.RegisterServerMetrics(srv.StatsHandler, r.serverMetrics)
	healthgrpc.RegisterHealthServer(srv.GRPC, serve.DefaultHealthCheckService())
}

// newServerStatsTestEnv starts a server wired with the stats handler and a health service, and
// returns the recorded metrics together with a connected health client.
func newServerStatsTestEnv(ctx context.Context, t *testing.T) *serverStatsTestEnv {
	t.Helper()
	m := serve.NewServerMetrics(monitoring.NewProvider(), monitoring.MetricsParameters{
		Namespace: "test",
		Subsystem: "server_stats",
	})
	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	test.ServeForTest(ctx, t, serverConfig, &statsRegisterer{serverMetrics: m})
	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	return &serverStatsTestEnv{
		metrics:      m,
		health:       healthgrpc.NewHealthClient(conn),
		grpcEndpoint: serverConfig.GRPC.Endpoint,
	}
}

// TestServerConnStatsHandler verifies the active-connections gauge end to end:
// wired through the normal RegisterService path, it rises as real clients connect
// and returns to zero as they disconnect.
func TestServerConnStatsHandler(t *testing.T) {
	t.Parallel()

	t.Log("Starting service")

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	env := newServerStatsTestEnv(ctx, t)

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
	env := newServerStatsTestEnv(ctx, t)

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
	env := newServerStatsTestEnv(ctx, t)

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

// TestServerStatsHandlerGRPCStatusCodes verifies that a completed RPC is recorded under the label of
// its gRPC status code, for every code the server may return.
func TestServerStatsHandlerGRPCStatusCodes(t *testing.T) {
	t.Parallel()

	for _, code := range []codes.Code{
		codes.OK,
		codes.Canceled,
		codes.Unknown,
		codes.InvalidArgument,
		codes.DeadlineExceeded,
		codes.NotFound,
		codes.AlreadyExists,
		codes.PermissionDenied,
		codes.ResourceExhausted,
		codes.FailedPrecondition,
		codes.Aborted,
		codes.OutOfRange,
		codes.Unimplemented,
		codes.Internal,
		codes.Unavailable,
		codes.DataLoss,
		codes.Unauthenticated,
	} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			requireRPCStatusRecorded(t, status.Error(code, ""), code.String())
		})
	}
}

// TestServerStatsHandlerRecordsUnknownStatusForNonGRPCError verifies that
// non-gRPC errors are recorded with the Unknown gRPC status code.
func TestServerStatsHandlerRecordsUnknownStatusForNonGRPCError(t *testing.T) {
	t.Parallel()
	requireRPCStatusRecorded(t, errors.New("not a gRPC error"), "Unknown")
}

// requireRPCStatusRecorded drives one unary and one streaming RPC that end with rpcErr, and
// asserts each is recorded under wantStatus in its histogram (unary latency, stream duration).
func requireRPCStatusRecorded(t *testing.T, rpcErr error, wantStatus string) {
	t.Helper()
	serverMetrics := serve.NewServerMetrics(monitoring.NewProvider(), monitoring.MetricsParameters{
		Namespace: "test",
		Subsystem: "server_stats",
	})
	statHandler := &serve.ServerStatsHandler{}
	serve.RegisterServerMetrics(statHandler, serverMetrics)

	// A completed unary RPC records its latency under the status label.
	recordRPC(t, recordRPCParams{
		handler:       statHandler,
		methodName:    unaryMethod,
		recordedError: rpcErr,
	})
	require.Equal(t, 1, testutil.CollectAndCount(serverMetrics.LatencySeconds))
	require.Positive(t, metricVecValue(t, serverMetrics.LatencySeconds.MetricVec, unaryMethod, wantStatus))

	// A completed streaming RPC records its duration under the status label.
	recordRPC(t, recordRPCParams{
		handler:        statHandler,
		methodName:     streamMethod,
		isServerStream: true,
		recordedError:  rpcErr,
	})
	require.Equal(t, 1, testutil.CollectAndCount(serverMetrics.StreamDurationSeconds))
	require.Positive(t, metricVecValue(t, serverMetrics.StreamDurationSeconds.MetricVec, streamMethod, wantStatus))
}

// recordRPC drives the stats handler through one RPC's lifecycle: TagRPC to resolve the method,
// then a Begin/End pair carrying a fixed positive duration, so the recorded histogram value is
// positive.
func recordRPC(t *testing.T, params recordRPCParams) {
	t.Helper()
	h := params.handler
	ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: params.methodName})
	begin := time.Unix(0, 0)
	h.HandleRPC(ctx,
		&stats.Begin{
			BeginTime:      begin,
			IsServerStream: params.isServerStream,
			IsClientStream: params.isClientStream,
		})
	h.HandleRPC(ctx,
		&stats.End{
			BeginTime: begin,
			EndTime:   begin.Add(time.Second),
			Error:     params.recordedError,
		})
}

func metricVecValue(t test.TestingT, mv *prometheus.MetricVec, lvs ...string) float64 {
	t.Helper()
	m, err := mv.GetMetricWithLabelValues(lvs...)
	require.NoError(t, err)
	return test.GetMetricValue(t, m)
}

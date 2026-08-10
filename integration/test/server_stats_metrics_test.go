/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
)

const (
	queryRequestsTotalMetric = "queryservice_grpc_requests_total"
	queryLatencyMetric       = "queryservice_grpc_requests_latency_seconds_count"
	queryActiveConnsMetric   = "queryservice_grpc_active_connections"

	sidecarActiveStreamsMetric  = "sidecar_grpc_active_streams"
	sidecarStreamDurationMetric = "sidecar_grpc_stream_duration_seconds_count"

	getTransactionStatusMethod   = "/committerpb.QueryService/GetTransactionStatus"
	openNotificationStreamMethod = "/committerpb.Notifier/OpenNotificationStream"

	method = "method"
)

// TestServerStatsMetricsFullSystem verifies that the gRPC stats handler records RPC-level metrics
// on the full system through actual client calls, validating the whole mechanism - server wiring,
// method labeling, and metric recording. The subtests run in parallel against one shared runtime:
// they are independent because the unary and connection cases observe query-service metrics while
// the streaming case observes sidecar metrics, and the unary case reuses the runtime's connection
// rather than opening one, so only the connection case moves the query active-connections gauge.
func TestServerStatsMetricsFullSystem(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{BlockTimeout: 2 * time.Second})
	c.Start(t, runner.FullTxPathWithQuery)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	queryMetrics := runner.NewMetricsScraper(t, c, c.SystemConfig.Services.Query.HTTPEndpoint)
	sidecarMetrics := runner.NewMetricsScraper(t, c, c.SystemConfig.Services.Sidecar.HTTPEndpoint)

	t.Run("Unary RPC Value And Latency", func(t *testing.T) {
		t.Parallel()
		unaryLabels := map[string]string{method: getTransactionStatusMethod}
		preRequests := queryMetrics.ValueWithLabels(t, queryRequestsTotalMetric, unaryLabels)

		_, err := c.QueryServiceClient.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
			TxIds: []string{"non-existent-tx"},
		})
		require.NoError(t, err)

		requireEventuallyWithTerm(t, func(ct *assert.CollectT) {
			require.Equal(ct, preRequests+1, queryMetrics.ValueWithLabels(t, queryRequestsTotalMetric, unaryLabels))
			require.Positive(ct, queryMetrics.ValueWithLabels(t, queryLatencyMetric, map[string]string{
				method:   getTransactionStatusMethod,
				"status": "OK",
			}))
		})
	})

	t.Run("Streaming RPC Duration And Active Stream Count", func(t *testing.T) {
		t.Parallel()
		streamLabels := map[string]string{method: openNotificationStreamMethod}
		preActiveStreams := sidecarMetrics.ValueWithLabels(t, sidecarActiveStreamsMetric, streamLabels)
		preStreamDuration := sidecarMetrics.ValueWithLabels(t, sidecarStreamDurationMetric, streamLabels)

		streamCtx, cancelStream := context.WithCancel(ctx)
		t.Cleanup(cancelStream)
		stream, err := c.NotifyClient.OpenNotificationStream(streamCtx)
		require.NoError(t, err)

		require.NoError(t, stream.Send(&committerpb.NotificationRequest{
			TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{"non-existent-tx"}},
		}))

		requireEventuallyWithTerm(t, func(ct *assert.CollectT) {
			activeStreams := sidecarMetrics.ValueWithLabels(t, sidecarActiveStreamsMetric, streamLabels)
			require.Equal(ct, preActiveStreams+1, activeStreams)
		})

		cancelStream()

		requireEventuallyWithTerm(t, func(ct *assert.CollectT) {
			activeStreams := sidecarMetrics.ValueWithLabels(t, sidecarActiveStreamsMetric, streamLabels)
			streamDuration := sidecarMetrics.ValueWithLabels(t, sidecarStreamDurationMetric, streamLabels)
			require.Equal(ct, preActiveStreams, activeStreams)
			require.Positive(ct, streamDuration-preStreamDuration)
		})
	})

	//nolint:paralleltest // this test examine the active-connections gauge, while other tests,
	// activating lazy connections which could lead to flaky results if run in parallel.
	t.Run("Client Connection Lifecycle Count", func(t *testing.T) {
		preActiveConns := queryMetrics.Value(t, queryActiveConnsMetric)

		conn, err := grpc.NewClient(
			c.SystemConfig.Services.Query.GrpcEndpoint.Address(),
			grpc.WithTransportCredentials(clientCredentials(t, c)),
		)
		require.NoError(t, err)

		conn.Connect()
		requireEventuallyWithTerm(t, func(ct *assert.CollectT) {
			require.Equal(ct, connectivity.Ready, conn.GetState())
		})

		requireEventuallyWithTerm(t, func(ct *assert.CollectT) {
			require.Equal(ct, preActiveConns+1, queryMetrics.Value(t, queryActiveConnsMetric))
		})

		require.NoError(t, conn.Close())
		requireEventuallyWithTerm(t, func(ct *assert.CollectT) {
			require.Equal(ct, preActiveConns, queryMetrics.Value(t, queryActiveConnsMetric))
		})
	})
}

func requireEventuallyWithTerm(t *testing.T, term func(ct *assert.CollectT)) {
	t.Helper()
	require.EventuallyWithT(t, term, 30*time.Second, 200*time.Millisecond)
}

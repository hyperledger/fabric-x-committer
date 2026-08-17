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
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	queryRequestsTotalMetric = "queryservice_grpc_requests_total"
	queryLatencyCountMetric  = "queryservice_grpc_requests_latency_seconds_count"
	queryActiveConnsMetric   = "queryservice_grpc_active_connections"

	sidecarActiveStreamsMetric       = "sidecar_grpc_active_streams"
	sidecarStreamDurationCountMetric = "sidecar_grpc_stream_duration_seconds_count"
	sidecarStreamDurationSumMetric   = "sidecar_grpc_stream_duration_seconds_sum"

	getTransactionStatusMethod   = "/committerpb.QueryService/GetTransactionStatus"
	openNotificationStreamMethod = "/committerpb.Notifier/OpenNotificationStream"

	method = "method"
)

// TestServerStatsMetricsFullSystem verifies that the gRPC stats handler records RPC-level metrics
// on the full system through actual client calls, validating the whole mechanism - server wiring,
// method labeling, and metric recording.
func TestServerStatsMetricsFullSystem(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{BlockTimeout: 2 * time.Second})
	c.Start(t, runner.FullTxPathWithQuery)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	queryMetrics := test.NewMetricsScraper(t, c.SystemConfig.ClientTLS, c.SystemConfig.Services.Query.HTTPEndpoint)
	sidecarMetrics := test.NewMetricsScraper(t, c.SystemConfig.ClientTLS, c.SystemConfig.Services.Sidecar.HTTPEndpoint)

	t.Run("Unary RPC Value And Latency", func(t *testing.T) {
		t.Parallel()
		unaryLabels := map[string]string{method: getTransactionStatusMethod}
		preRequests := queryMetrics.ValueWithLabels(t, queryRequestsTotalMetric, unaryLabels)

		_, err := c.QueryServiceClient.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
			TxIds: []string{"non-existent-tx"},
		})
		require.NoError(t, err)

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			require.Equal(ct, preRequests+1, queryMetrics.ValueWithLabels(ct, queryRequestsTotalMetric, unaryLabels))
			require.Positive(ct, queryMetrics.ValueWithLabels(ct, queryLatencyCountMetric, map[string]string{
				method:   getTransactionStatusMethod,
				"status": "OK",
			}))
		}, 30*time.Second, 200*time.Millisecond)
	})

	t.Run("Streaming RPC Duration And Active Stream Count", func(t *testing.T) {
		t.Parallel()
		streamLabels := map[string]string{method: openNotificationStreamMethod}
		preActiveStreams := sidecarMetrics.ValueWithLabels(t, sidecarActiveStreamsMetric, streamLabels)
		preStreamDurationCount := sidecarMetrics.ValueWithLabels(t, sidecarStreamDurationCountMetric, streamLabels)
		preStreamDurationSum := sidecarMetrics.FloatValueWithLabels(t, sidecarStreamDurationSumMetric, streamLabels)

		streamCtx, cancelStream := context.WithCancel(ctx)
		t.Cleanup(cancelStream)
		stream, err := c.NotifyClient.OpenNotificationStream(streamCtx)
		require.NoError(t, err)

		require.NoError(t, stream.Send(&committerpb.NotificationRequest{
			TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{"non-existent-tx"}},
		}))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			activeStreams := sidecarMetrics.ValueWithLabels(ct, sidecarActiveStreamsMetric, streamLabels)
			require.Equal(ct, preActiveStreams+1, activeStreams)
		}, 30*time.Second, 200*time.Millisecond)

		cancelStream()

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			activeStreams := sidecarMetrics.ValueWithLabels(ct, sidecarActiveStreamsMetric, streamLabels)
			streamDurationCount := sidecarMetrics.ValueWithLabels(ct, sidecarStreamDurationCountMetric, streamLabels)
			streamDurationSum := sidecarMetrics.FloatValueWithLabels(ct, sidecarStreamDurationSumMetric, streamLabels)
			require.Equal(ct, preActiveStreams, activeStreams)
			require.Equal(ct, preStreamDurationCount+1, streamDurationCount)
			require.Positive(ct, streamDurationSum-preStreamDurationSum)
		}, 30*time.Second, 200*time.Millisecond)
	})
}

// TestActiveConnectionCountFullSystem verifies
// that the gRPC stats handler correctly tracks the number of active connections
// on the full system through actual client calls, validating the whole mechanism - server wiring,
// connection tracking, and metric recording.
func TestActiveConnectionCountFullSystem(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{BlockTimeout: 2 * time.Second})
	c.Start(t, runner.FullTxPathWithQuery)

	queryMetrics := test.NewMetricsScraper(t, c.SystemConfig.ClientTLS, c.SystemConfig.Services.Query.HTTPEndpoint)

	preActiveConns := queryMetrics.Value(t, queryActiveConnsMetric)

	conn, err := grpc.NewClient(
		c.SystemConfig.Services.Query.GrpcEndpoint.Address(),
		grpc.WithTransportCredentials(clientCredentials(t, c)),
	)
	require.NoError(t, err)

	conn.Connect()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, connectivity.Ready, conn.GetState())
	}, 30*time.Second, 200*time.Millisecond)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, preActiveConns+1, queryMetrics.Value(ct, queryActiveConnsMetric))
	}, 30*time.Second, 200*time.Millisecond)

	require.NoError(t, conn.Close())
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, preActiveConns, queryMetrics.Value(ct, queryActiveConnsMetric))
	}, 30*time.Second, 200*time.Millisecond)
}

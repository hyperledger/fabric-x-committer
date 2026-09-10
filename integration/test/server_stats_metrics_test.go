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
	queryRequestsTotalMetric   = "queryservice_grpc_requests_total"
	queryRequestsLatencyMetric = "queryservice_grpc_requests_latency_seconds"
	queryActiveConnsMetric     = "queryservice_grpc_active_connections"
	queryMessageRecvSizeMetric = "queryservice_grpc_message_received_size_bytes"
	queryMessageSentSizeMetric = "queryservice_grpc_message_sent_size_bytes"

	sidecarActiveStreamsMetric  = "sidecar_grpc_active_streams"
	sidecarStreamDurationMetric = "sidecar_grpc_stream_duration_seconds"

	getTransactionStatusMethod   = committerpb.QueryService_GetTransactionStatus_FullMethodName
	openNotificationStreamMethod = committerpb.Notifier_OpenNotificationStream_FullMethodName

	method = "method"
)

// TestServerStatsMetricsFullSystem verifies that the gRPC stats handler records RPC-level metrics
// across the full system through actual client calls, validating the whole mechanism: server
// wiring, method labeling, and metric recording.
func TestServerStatsMetricsFullSystem(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{BlockTimeout: 2 * time.Second})
	c.Start(t, runner.FullTxPathWithQuery)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	queryMetrics := test.NewMetricsConnectionParameters(
		t, c.SystemConfig.ClientTLS, c.SystemConfig.Services.Query.HTTPEndpoint,
	)
	sidecarMetrics := test.NewMetricsConnectionParameters(
		t, c.SystemConfig.ClientTLS, c.SystemConfig.Services.Sidecar.HTTPEndpoint,
	)

	t.Run("Unary RPC Value Latency And Messages", func(t *testing.T) {
		t.Parallel()
		methodLabel := map[string]string{method: getTransactionStatusMethod}
		requestsTotal := test.GetMetricValueParameters{
			MetricsConnectionParameters: queryMetrics,
			MetricName:                  queryRequestsTotalMetric,
			Labels:                      methodLabel,
		}
		requestsLatency := test.GetMetricValueParameters{
			MetricsConnectionParameters: queryMetrics,
			MetricName:                  queryRequestsLatencyMetric,
			Labels:                      map[string]string{method: getTransactionStatusMethod, "status": "OK"},
		}
		messageRecvSize := test.GetMetricValueParameters{
			MetricsConnectionParameters: queryMetrics,
			MetricName:                  queryMessageRecvSizeMetric,
			Labels:                      methodLabel,
		}
		messageSentSize := test.GetMetricValueParameters{
			MetricsConnectionParameters: queryMetrics,
			MetricName:                  queryMessageSentSizeMetric,
			Labels:                      methodLabel,
		}
		preRequests := test.GetCounterOrGaugeValueFromURL(t, requestsTotal)
		preLatencyCount, _ := test.GetHistogramCountAndSumValueFromURL(t, requestsLatency)
		preRecvSizeCount, preRecvSizeSum := test.GetHistogramCountAndSumValueFromURL(t, messageRecvSize)
		preSentSizeCount, preSentSizeSum := test.GetHistogramCountAndSumValueFromURL(t, messageSentSize)

		_, err := c.QueryServiceClient.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
			TxIds: []string{"non-existent-tx"},
		})
		require.NoError(t, err)

		// A unary RPC is exactly one message in and one message out, each of a non-zero wire size:
		// the size histograms' observation counts are the per-direction message counts.
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			latencyCount, _ := test.GetHistogramCountAndSumValueFromURL(ct, requestsLatency)
			recvSizeCount, recvSizeSum := test.GetHistogramCountAndSumValueFromURL(ct, messageRecvSize)
			sentSizeCount, sentSizeSum := test.GetHistogramCountAndSumValueFromURL(ct, messageSentSize)

			require.Equal(ct, preRequests+1, test.GetCounterOrGaugeValueFromURL(ct, requestsTotal))
			require.Equal(ct, preLatencyCount+1, latencyCount)
			require.Equal(ct, preRecvSizeCount+1, recvSizeCount)
			require.Equal(ct, preSentSizeCount+1, sentSizeCount)
			require.Positive(ct, recvSizeSum-preRecvSizeSum)
			require.Positive(ct, sentSizeSum-preSentSizeSum)
		}, 30*time.Second, 200*time.Millisecond)
	})

	t.Run("Streaming RPC Duration And Active Stream Count", func(t *testing.T) {
		t.Parallel()
		streamLabels := map[string]string{method: openNotificationStreamMethod}
		activeStreamsMetric := test.GetMetricValueParameters{
			MetricsConnectionParameters: sidecarMetrics,
			MetricName:                  sidecarActiveStreamsMetric,
			Labels:                      streamLabels,
		}
		streamDurationMetric := test.GetMetricValueParameters{
			MetricsConnectionParameters: sidecarMetrics,
			MetricName:                  sidecarStreamDurationMetric,
			Labels:                      streamLabels,
		}
		preActiveStreams := test.GetCounterOrGaugeValueFromURL(t, activeStreamsMetric)
		preStreamDurationCount, preStreamDurationSum := test.GetHistogramCountAndSumValueFromURL(
			t, streamDurationMetric,
		)

		streamCtx, cancelStream := context.WithCancel(ctx)
		t.Cleanup(cancelStream)
		stream, err := c.NotifyClient.OpenNotificationStream(streamCtx)
		require.NoError(t, err)

		require.NoError(t, stream.Send(&committerpb.NotificationRequest{
			TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{"non-existent-tx"}},
		}))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			require.Equal(ct, preActiveStreams+1, test.GetCounterOrGaugeValueFromURL(ct, activeStreamsMetric))
		}, 30*time.Second, 200*time.Millisecond)

		cancelStream()

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			streamDurationCount, streamDurationSum := test.GetHistogramCountAndSumValueFromURL(ct, streamDurationMetric)

			require.Equal(ct, preActiveStreams, test.GetCounterOrGaugeValueFromURL(ct, activeStreamsMetric))
			require.Equal(ct, preStreamDurationCount+1, streamDurationCount)
			require.Positive(ct, streamDurationSum-preStreamDurationSum)
		}, 30*time.Second, 200*time.Millisecond)
	})
}

// TestActiveConnectionCountFullSystem verifies that the gRPC stats handler tracks the number of
// active connections on the full system through actual client calls, validating the whole
// mechanism: server wiring, connection tracking, and metric recording.
func TestActiveConnectionCountFullSystem(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{BlockTimeout: 2 * time.Second})
	c.Start(t, runner.FullTxPathWithQuery)

	queryMetrics := test.NewMetricsConnectionParameters(
		t, c.SystemConfig.ClientTLS, c.SystemConfig.Services.Query.HTTPEndpoint,
	)

	activeConnsMetric := test.GetMetricValueParameters{
		MetricsConnectionParameters: queryMetrics,
		MetricName:                  queryActiveConnsMetric,
	}
	preActiveConns := test.GetCounterOrGaugeValueFromURL(t, activeConnsMetric)

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
		require.Equal(ct, preActiveConns+1, test.GetCounterOrGaugeValueFromURL(ct, activeConnsMetric))
	}, 30*time.Second, 200*time.Millisecond)

	require.NoError(t, conn.Close())
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, preActiveConns, test.GetCounterOrGaugeValueFromURL(ct, activeConnsMetric))
	}, 30*time.Second, 200*time.Millisecond)
}

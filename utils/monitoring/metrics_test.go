/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestConnectionMetrics(t *testing.T) {
	t.Parallel()

	target := "localhost:7051"

	t.Run("Connected", func(t *testing.T) {
		t.Parallel()
		m := NewConnectionMetrics(NewProvider(), MetricsParameters{})

		m.Connected(target)
		requireStatus(t, m, target, int(connection.Connected))
	})

	t.Run("Disconnected_AfterConnected", func(t *testing.T) {
		t.Parallel()
		m := NewConnectionMetrics(NewProvider(), MetricsParameters{})
		m.Connected(target)
		m.Disconnected(target)

		requireStatus(t, m, target, int(connection.Disconnected))
		requireFailureTotal(t, m, target, 1)
	})

	t.Run("Disconnected_WithoutPriorConnect", func(t *testing.T) {
		t.Parallel()
		m := NewConnectionMetrics(NewProvider(), MetricsParameters{})

		m.Disconnected(target)

		requireStatus(t, m, target, int(connection.Disconnected))
		requireFailureTotal(t, m, target, 0)
	})

	t.Run("Disconnected_Twice", func(t *testing.T) {
		t.Parallel()
		m := NewConnectionMetrics(NewProvider(), MetricsParameters{})

		m.Connected(target)
		m.Disconnected(target)
		m.Disconnected(target)

		requireFailureTotal(t, m, target, 1)
	})

	t.Run("MultipleTargets", func(t *testing.T) {
		t.Parallel()
		m := NewConnectionMetrics(NewProvider(), MetricsParameters{})
		target2 := "localhost:7052"

		m.Connected(target)
		m.Connected(target2)
		// Disconnect only target; target2 failure count must stay at 0.
		m.Disconnected(target)

		requireFailureTotal(t, m, target, 1)
		requireStatus(t, m, target2, int(connection.Connected))
		requireFailureTotal(t, m, target2, 0)
	})

	t.Run("Reconnect", func(t *testing.T) {
		t.Parallel()
		m := NewConnectionMetrics(NewProvider(), MetricsParameters{})

		m.Connected(target)
		m.Disconnected(target)
		m.Connected(target)
		m.Disconnected(target)

		requireFailureTotal(t, m, target, 2)
	})
}

// TestNewServerMetrics verifies both families are created and record values. The RPC metrics
// are labelled by the full gRPC method, so the constructor takes no method list.
func TestNewServerMetrics(t *testing.T) {
	t.Parallel()
	const method = "/committerpb.QueryService/GetRows"
	m := NewServerMetrics(NewProvider(), MetricsParameters{
		Namespace: "test",
		Subsystem: "grpc",
	})

	require.NotNil(t, m.RequestsTotal)
	require.NotNil(t, m.LatencySeconds)
	require.NotNil(t, m.StreamDurationSeconds)
	require.NotNil(t, m.ActiveStreams)
	require.NotNil(t, m.ActiveConnections)

	m.RequestsTotal.WithLabelValues(method).Inc()
	m.ActiveStreams.WithLabelValues(method).Inc()
	m.ActiveStreams.WithLabelValues(method).Inc()

	promutil.Observe(m.LatencySeconds.WithLabelValues(method, "OK"), time.Second)
	promutil.Observe(m.LatencySeconds.WithLabelValues(method, "Internal"), time.Second)
	promutil.Observe(m.StreamDurationSeconds.WithLabelValues(method, "OK"), time.Second)

	m.ActiveConnections.Inc()
	m.ActiveConnections.Inc()
	m.ActiveConnections.Dec()

	test.RequireIntMetricValue(t, 1, m.RequestsTotal.WithLabelValues(method))
	test.RequireIntMetricValue(t, 2, m.ActiveStreams.WithLabelValues(method))
	test.RequireIntMetricValue(t, 1, m.ActiveConnections)

	// A single observation of 1 makes the histogram's mean (sum/count) exactly 1, per status.
	requireHistogramMean(t, 1, m.LatencySeconds, method, "OK")
	requireHistogramMean(t, 1, m.LatencySeconds, method, "Internal")
	requireHistogramMean(t, 1, m.StreamDurationSeconds, method, "OK")
}

// requireHistogramMean asserts the histogram vec's mean (sum/count) for the given labels.
func requireHistogramMean(t *testing.T, expected int, hv *prometheus.HistogramVec, labels ...string) {
	t.Helper()
	metric, err := hv.MetricVec.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	t.Logf("metric: %v", metric)
	test.RequireIntMetricValue(t, expected, metric)
}

func requireStatus(t *testing.T, m *ConnectionMetrics, target string, expected int) {
	t.Helper()
	metric, err := m.Status.GetMetricWithLabelValues(target)
	require.NoError(t, err)
	test.RequireIntMetricValue(t, expected, metric)
}

func requireFailureTotal(t *testing.T, m *ConnectionMetrics, target string, expected int) {
	t.Helper()
	metric, err := m.FailureTotal.GetMetricWithLabelValues(target)
	require.NoError(t, err)
	test.RequireIntMetricValue(t, expected, metric)
}

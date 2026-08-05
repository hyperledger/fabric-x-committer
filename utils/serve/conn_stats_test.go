/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/stats"

	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type connStatsRegisterer struct {
	activeConnections prometheus.Gauge
}

func (r *connStatsRegisterer) RegisterService(srv serve.Servers) {
	serve.RegisterServerMetrics(srv.StatsHandler, &serve.ServerMetrics{
		ActiveConnections: r.activeConnections,
	})
}

// TestServerConnStatsHandler verifies the active-connections gauge end to end:
// wired through the normal RegisterService path, it rises as real clients connect
// and returns to zero as they disconnect.
func TestServerConnStatsHandler(t *testing.T) {
	t.Parallel()

	activeConnGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_active_connections",
		Help: "Test gauge for the connection stats handler.",
	})

	t.Log("Starting service")
	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	test.ServeForTest(ctx, t, serverConfig, &connStatsRegisterer{activeConnections: activeConnGauge})

	t.Log("Creating clients")
	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	conn2 := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)

	test.RequireIntMetricValue(t, 0, activeConnGauge)

	t.Log("Connecting clients")
	conn.Connect()
	test.EventuallyIntMetric(t, 1, activeConnGauge, 30*time.Second, 100*time.Millisecond)
	conn2.Connect()
	test.EventuallyIntMetric(t, 2, activeConnGauge, 30*time.Second, 100*time.Millisecond)

	t.Log("Disconnecting clients")
	require.NoError(t, conn.Close())
	require.NoError(t, conn2.Close())
	test.EventuallyIntMetric(t, 0, activeConnGauge, 30*time.Second, 100*time.Millisecond)
}

// TestServerStatsHandlerUnaryRPC verifies a unary RPC records count and latency labeled by
// its full method, and does not touch the active-streams gauge.
func TestServerStatsHandlerUnaryRPC(t *testing.T) {
	t.Parallel()
	const method = "/committerpb.QueryService/GetRows"
	m := newTestServerMetrics()

	h := &serve.ServerStatsHandler{}
	serve.RegisterServerMetrics(h, m)

	ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: method})
	h.HandleRPC(ctx, &stats.Begin{}) // unary: IsServerStream/IsClientStream are false.
	h.HandleRPC(ctx, &stats.End{
		BeginTime: time.Unix(0, 0),
		EndTime:   time.Unix(0, int64(time.Second)), // 1s
	})

	test.RequireIntMetricValue(t, 1, m.RequestsTotal.WithLabelValues(method))
	// A nil End.Error records the "OK" status; the histogram count is the per-outcome count.
	require.Equal(t, 1, readHistogramCount(t, m.LatencySeconds.WithLabelValues(method, "OK")))
	require.InDelta(t, 1, readHistogramSum(t, m.LatencySeconds.WithLabelValues(method, "OK")), 0.1)
	test.RequireIntMetricValue(t, 0, m.ActiveStreams.WithLabelValues(method))
}

// TestServerStatsHandlerStreamRPC verifies a streaming RPC drives the active-streams gauge up
// on Begin and back down on End, counts the request, but is not observed for latency.
func TestServerStatsHandlerStreamRPC(t *testing.T) {
	t.Parallel()
	const method = "/servicepb.Verifier/StartStream"
	m := newTestServerMetrics()

	h := &serve.ServerStatsHandler{}
	serve.RegisterServerMetrics(h, m)

	ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: method})
	h.HandleRPC(ctx, &stats.Begin{IsServerStream: true})
	test.RequireIntMetricValue(t, 1, m.ActiveStreams.WithLabelValues(method))

	h.HandleRPC(ctx, &stats.End{BeginTime: time.Unix(0, 0), EndTime: time.Unix(0, int64(time.Second))})
	test.RequireIntMetricValue(t, 0, m.ActiveStreams.WithLabelValues(method))
	test.RequireIntMetricValue(t, 1, m.RequestsTotal.WithLabelValues(method))
	// A stream's duration is recorded as a stream duration, not observed as request latency.
	require.Zero(t, readHistogramCount(t, m.LatencySeconds.WithLabelValues(method, "OK")))
	require.Equal(t, 1, readHistogramCount(t, m.StreamDurationSeconds.WithLabelValues(method, "OK")))
	require.InDelta(t, 1, readHistogramSum(t, m.StreamDurationSeconds.WithLabelValues(method, "OK")), 0.1)
}

// TestServerStatsHandlerNoRegistration verifies the handler is a safe no-op when nothing is
// registered and when nil families are registered, so callbacks never panic on nil metrics.
func TestServerStatsHandlerNoRegistration(t *testing.T) {
	t.Parallel()
	const method = "/committerpb.QueryService/GetRows"

	activator := func(h *serve.ServerStatsHandler) {
		ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: method})
		h.HandleRPC(ctx, &stats.Begin{IsServerStream: true})
		h.HandleRPC(ctx, &stats.End{
			BeginTime: time.Unix(0, 0),
			EndTime:   time.Unix(0, int64(time.Second)),
		})
		h.HandleConn(ctx, &stats.ConnBegin{})
		h.HandleConn(ctx, &stats.ConnEnd{})
	}

	require.NotPanics(t, func() { activator(&serve.ServerStatsHandler{}) }, "nothing registered")

	h := &serve.ServerStatsHandler{}
	serve.RegisterServerMetrics(h, &serve.ServerMetrics{})
	require.NotPanics(t, func() { activator(h) }, "empty families registered")
}

func readHistogramCount(t *testing.T, o prometheus.Observer) int {
	t.Helper()
	return int(readHistogram(t, o).GetSampleCount()) //nolint:gosec // small test counts.
}

func readHistogramSum(t *testing.T, o prometheus.Observer) float64 {
	t.Helper()
	return readHistogram(t, o).GetSampleSum()
}

// readHistogram writes the observer's current state, failing the test if it is not a histogram.
func readHistogram(t *testing.T, o prometheus.Observer) *dto.Histogram {
	t.Helper()
	h, ok := o.(prometheus.Histogram)
	require.True(t, ok)
	var m dto.Metric
	require.NoError(t, h.Write(&m))
	return m.GetHistogram()
}

func newTestServerMetrics() *serve.ServerMetrics {
	const methodLabel = "method"
	return &serve.ServerMetrics{
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "t_requests_total",
				Help: "test requests",
			}, []string{methodLabel},
		),
		LatencySeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "t_latency_seconds",
				Help: "test latency",
			}, []string{methodLabel, "status"},
		),
		StreamDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "t_stream_duration_seconds",
				Help: "test stream duration",
			}, []string{methodLabel, "status"},
		),
		ActiveStreams: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "t_active_streams",
				Help: "test streams",
			}, []string{methodLabel},
		),
	}
}

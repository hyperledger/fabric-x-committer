/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

const method = "method"

// ServerMetrics holds the server-side connection and RPC metrics a service records. The RPC
// metrics are labeled by the full gRPC method ("/pkg.Service/Method"). Every field is
// A nil field is simply not recorded.
type ServerMetrics struct {
	// requestsTotal counts completed RPCs (unary and streaming).
	requestsTotal *prometheus.CounterVec
	// latencySeconds observes RPC duration, labeled by method and by the RPC's
	// gRPC status code. Streaming RPCs are not observed, as their duration is the lifetime
	// of the stream rather than request latency; streamDurationSeconds records those instead.
	latencySeconds *prometheus.HistogramVec
	// streamDurationSeconds observes how long a streaming RPC was active from start to end, labeled
	// by method and by the stream's gRPC status code.
	streamDurationSeconds *prometheus.HistogramVec
	// activeStreams reflects the number of streaming RPCs currently in progress.
	activeStreams *prometheus.GaugeVec
	// activeConnections is incremented when the server accepts a connection and decremented
	// when it is torn down, so it reflects the number of connections currently open.
	activeConnections prometheus.Gauge
}

// NewServerMetrics creates the server-side metrics recorded by the gRPC stats handler.
func NewServerMetrics(p *monitoring.Provider, params monitoring.MetricsParameters) *ServerMetrics {
	latencyBuckets := []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1, 2, 3, 4, 5, 10}
	return &ServerMetrics{
		requestsTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "requests_total",
			Help:      "Number of requests by the service",
		}, []string{method}),
		latencySeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "requests_latency_seconds",
			Help:      "The latency (seconds) of requests by the service, by method and gRPC status code",
			Buckets:   latencyBuckets,
		}, []string{method, "status"}),
		streamDurationSeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "stream_duration_seconds",
			Help:      "The duration (seconds) a stream was active from start to end, by method and gRPC status code",
			Buckets:   latencyBuckets,
		}, []string{method, "status"}),
		activeStreams: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "active_streams",
			Help:      "Number of gRPC streams currently open on the server",
		}, []string{method}),
		activeConnections: p.NewGauge(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "active_connections",
			Help:      "Number of client connections currently open on the server",
		}),
	}
}

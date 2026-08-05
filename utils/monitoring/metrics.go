/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const methodLabel = "method"

type (
	// MetricsParameters describes metrics namespace and subsystem.
	MetricsParameters struct {
		Namespace string
		Subsystem string
	}

	// ConnectionMetrics supports common connection metrics.
	ConnectionMetrics struct {
		Status       *prometheus.GaugeVec
		FailureTotal *prometheus.CounterVec
		connected    sync.Map // tracks connected grpc targets using map[string]any
	}

	// ThroughputMetrics supports common throughput metrics.
	ThroughputMetrics struct {
		Input  prometheus.Counter
		Output prometheus.Counter
	}
)

// NewThroughputMetrics creates a new prometheus throughput counter.
func NewThroughputMetrics(p *Provider, params MetricsParameters) *ThroughputMetrics {
	return &ThroughputMetrics{
		Input: p.NewCounter(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "input_throughput",
			Help:      "Incoming requests for a component",
		}),
		Output: p.NewCounter(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "output_throughput",
			Help:      "Outgoing responses for a component",
		}),
	}
}

// NewConnectionMetrics supports common connection metrics.
func NewConnectionMetrics(p *Provider, params MetricsParameters) *ConnectionMetrics {
	return &ConnectionMetrics{
		Status: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "connection_status",
			Help: fmt.Sprintf(
				"Connection status to %s service by grpc target (1 = connected, 0 = disconnected).",
				params.Subsystem,
			),
		}, []string{"grpc_target"}),
		FailureTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "connection_failure_total",
			Help: fmt.Sprintf(
				"Total number of connection failures to %s service. Short-lived failures may not always be captured.",
				params.Subsystem,
			),
		}, []string{"grpc_target"}),
	}
}

// NewServerMetrics creates the server-side metrics recorded by the gRPC stats handler.
func NewServerMetrics(p *Provider, params MetricsParameters) *serve.ServerMetrics {
	latencyBuckets := []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1, 2, 3, 4, 5, 10}
	return &serve.ServerMetrics{
		RequestsTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "requests_total",
			Help:      "Number of requests by the service",
		}, []string{methodLabel}),
		LatencySeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "requests_latency_seconds",
			Help:      "The latency (seconds) of requests by the service, by method and gRPC status code",
			Buckets:   latencyBuckets,
		}, []string{methodLabel, "status"}),
		StreamDurationSeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "stream_duration_seconds",
			Help:      "The duration (seconds) a stream ran from start to end, by method and gRPC status code",
			Buckets:   latencyBuckets,
		}, []string{methodLabel, "status"}),
		ActiveStreams: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "active_streams",
			Help:      "Number of gRPC streams currently open on the server",
		}, []string{"method"}),
		ActiveConnections: p.NewGauge(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "active_connections",
			Help:      "Number of client connections currently open on the server",
		}),
	}
}

// Connected observed connected.
func (m *ConnectionMetrics) Connected(grpcTarget string) {
	promutil.SetGaugeVec(m.Status, []string{grpcTarget}, connection.Connected)
	m.connected.Store(grpcTarget, nil)
}

// Disconnected observe disconnected. The failure count is increased only if the status was connected.
func (m *ConnectionMetrics) Disconnected(grpcTarget string) {
	promutil.SetGaugeVec(m.Status, []string{grpcTarget}, connection.Disconnected)
	if _, loaded := m.connected.LoadAndDelete(grpcTarget); loaded {
		promutil.AddToCounterVec(m.FailureTotal, []string{grpcTarget}, 1)
	}
}

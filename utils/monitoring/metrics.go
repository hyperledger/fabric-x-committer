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
)

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

// LatencyBuckets are the shared histogram bucket boundaries (seconds) for the gRPC request-latency
// metric, so those histograms are comparable across services. It reaches 10s to cover slow RPCs;
// components timing shorter internal work (e.g. VC and dependency-graph batches) keep their own
// narrower buckets.
var LatencyBuckets = []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1, 2, 3, 4, 5, 10}

// StreamDurationBuckets are the histogram bucket boundaries (seconds) for the gRPC stream-duration
// metric. Streams (notification subscriptions, verification streams, health watches) stay open from
// well under a second to hours, so the buckets span that range rather than reusing LatencyBuckets,
// whose 10s ceiling would collapse every long-lived stream into the +Inf bucket. The range is 0.1s
// to 6h.
var StreamDurationBuckets = []float64{.1, .5, 1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 10800, 21600}

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

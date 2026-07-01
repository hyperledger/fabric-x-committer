/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicemanager

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

// Metrics defines the metrics used by the generic service manager.
type Metrics struct {
	// Connection tracks connection state to service endpoints
	Connection *monitoring.ConnectionMetrics
	// ProcessedTotal counts successfully processed tasks
	ProcessedTotal prometheus.Counter
	// RetriedTotal counts tasks resubmitted after failure
	RetriedTotal prometheus.Counter
	// InputQueueSize tracks the size of the input batch queue of the manager.
	InputQueueSize prometheus.Gauge
	// OutputQueueSize tracks the size of the output batch queue of the manager.
	OutputQueueSize prometheus.Gauge
}

// NewMetrics creates a new Metrics instance with the specified provider and namespace.
func NewMetrics(p *monitoring.Provider, params monitoring.MetricsParameters) *Metrics {
	return &Metrics{
		Connection: monitoring.NewConnectionMetrics(p, params),
		ProcessedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "transaction_processed_total",
			Help:      "Total number of transactions processed by the manager.",
		}),
		RetriedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "transaction_retired_total",
			Help:      "Total number of transactions retried by the manager.",
		}),
		InputQueueSize: p.NewGauge(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "input_batch_queue_size",
			Help:      "Size of the input batch queue of the manager.",
		}),
		OutputQueueSize: p.NewGauge(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "output_batch_queue_size",
			Help:      "Size of the output batch queue of the manager.",
		}),
	}
}

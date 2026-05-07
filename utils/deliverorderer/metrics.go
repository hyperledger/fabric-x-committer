/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

// Metrics holds all Prometheus metrics for the deliverorderer package.
// It tracks stream progress, block verification, timeouts, source suspicion,
// and error handling across data and header-only streams.
type Metrics struct {
	// Stream Progress Metrics.
	CurrentDataSourceID  prometheus.Gauge
	StreamBlockNumber    *prometheus.GaugeVec   // labels: stream_type
	BlocksDeliveredTotal *prometheus.CounterVec // labels: stream_type, source_id

	// Block Verification Metrics.
	BlockVerificationSeconds *prometheus.HistogramVec // labels: stream_type, source_id, result

	// Stream Lifecycle Metrics
	StreamRestartsTotal *prometheus.CounterVec // labels: reason
	StreamStartsTotal   *prometheus.CounterVec // labels: stream_type, source_id
	ActiveStreams       *prometheus.GaugeVec   // labels: stream_type, source_id
	StreamErrorsTotal   *prometheus.CounterVec // labels: stream_type, source_id, error_type

	// Block Withholding Detection (BFT mode).
	BlockGapGauge           *prometheus.GaugeVec   // labels: source_id
	SuspicionRaisedTotal    *prometheus.CounterVec // labels: source_id
	SuspicionClearedTotal   *prometheus.CounterVec // labels: source_id
	SuspicionConfirmedTotal *prometheus.CounterVec // labels: source_id
	TargetArrivalDeadline   *prometheus.GaugeVec   // labels: source_id

	// Config Updates.
	ConfigUpdatesTotal prometheus.Counter
	ConfigBlockNumber  prometheus.Gauge
}

// NewMetrics creates a new Metrics instance with the specified provider and namespace.
func NewMetrics(p *monitoring.Provider, params monitoring.MetricsParameters) *Metrics {
	latencyBuckets := []float64{1e-6, 1e-5, 1e-4, 1e-3, 1e-2, 1e-1, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 1.5, 2}
	return &Metrics{
		// Stream and Source Progress Metrics.
		CurrentDataSourceID: p.NewGauge(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "data_source_id",
			Help:      "Current orderer party ID used as the data block source.",
		}),
		StreamBlockNumber: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "block_number",
			Help:      "Current block number by stream type (data or headers).",
		}, []string{"stream_type"}),
		BlocksDeliveredTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "blocks_delivered_total",
			Help:      "Total number of blocks delivered by stream type, and source ID.",
		}, []string{"stream_type", "source_id"}),

		// Block Verification Metrics.
		BlockVerificationSeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "block_verification_seconds",
			Help:      "Time spent verifying blocks by stream type, source ID, and result.",
			Buckets:   latencyBuckets,
		}, []string{"stream_type", "source_id", "result"}),

		// Stream Lifecycle Metrics.
		StreamRestartsTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "stream_restarts_total",
			Help:      "Total number of stream restarts by reason.",
		}, []string{"reason"}),
		StreamStartsTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "stream_starts_total",
			Help:      "Total number of stream starts by stream type and source ID.",
		}, []string{"stream_type", "source_id"}),
		ActiveStreams: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "stream_active",
			Help:      "Number of currently active streams by stream type, and source ID.",
		}, []string{"stream_type", "source_id"}),
		StreamErrorsTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "stream_errors_total",
			Help:      "Total number of stream errors by stream type, source ID, and error type.",
		}, []string{"stream_type", "source_id", "error_type"}),

		// Block Withholding Detection.
		SuspicionRaisedTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "withholding_suspicion_raised_total",
			Help:      "Total number of block withholding suspicions raised by data source ID.",
		}, []string{"source_id"}),
		SuspicionClearedTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "withholding_suspicion_cleared_total",
			Help:      "Total number of block withholding suspicions cleared by data source ID.",
		}, []string{"source_id"}),
		SuspicionConfirmedTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "withholding_suspicion_confirmed_total",
			Help:      "Total number of block withholding suspicions confirmed by data source ID.",
		}, []string{"source_id"}),
		BlockGapGauge: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "stream_block_gap",
			Help: "Current block gap between header and data streams (data - header) by data source ID." +
				"Positive value indicates that data stream is ahead of header stream.",
		}, []string{"source_id"}),
		TargetArrivalDeadline: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "target_arrival_deadline_unix_milliseconds",
			Help: "Unix timestamp (milliseconds) of the target block arrival deadline for withholding " +
				"detection by source ID.",
		}, []string{"source_id"}),

		// Config Updates
		ConfigUpdatesTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "config_updates_total",
			Help:      "Total number of config block updates detected.",
		}),
		ConfigBlockNumber: p.NewGauge(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "config_block_number",
			Help:      "Current config block number.",
		}),
	}
}

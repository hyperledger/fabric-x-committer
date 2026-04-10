/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

type perfMetrics struct {
	*monitoring.MetricsProvider

	// received and processed transactions
	transactionReceivedTotal  prometheus.Counter
	transactionCommittedTotal *prometheus.CounterVec

	// queue sizes
	sigverifierInputTxBatchQueueSize           prometheus.Gauge
	sigverifierOutputValidatedTxBatchQueueSize prometheus.Gauge
	vcserviceOutputTxStatusBatchQueueSize      prometheus.Gauge
	vcserviceOutputValidatedTxBatchQueueSize   prometheus.Gauge

	// processed transactions by each manager
	sigverifierTransactionProcessedTotal prometheus.Counter
	vcserviceTransactionProcessedTotal   prometheus.Counter

	// connection failure
	verifiersConnection               *monitoring.ConnectionMetrics
	verifiersRetriedTransactionTotal  prometheus.Counter
	vcservicesConnection              *monitoring.ConnectionMetrics
	vcservicesRetriedTransactionTotal prometheus.Counter
}

func newPerformanceMetrics(mp *monitoring.MetricsProvider) *perfMetrics {
	return &perfMetrics{
		MetricsProvider: mp,
		transactionReceivedTotal: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "grpc",
			Name:      "received_transaction_total",
			Help:      "Total number of transactions received by the coordinator service from the client.",
		}),
		transactionCommittedTotal: mp.NewCounterVec(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "grpc",
			Name:      "committed_transaction_total",
			Help:      "Total number of transactions committed status sent by the coordinator service to the client.",
		}, []string{"status"}),
		sigverifierInputTxBatchQueueSize: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "coordinator",
			Subsystem: "sigverifier",
			Name:      "input_tx_batch_queue_size",
			Help:      "Size of the input transaction batch queue of the signature verifier manager.",
		}),
		sigverifierOutputValidatedTxBatchQueueSize: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "coordinator",
			Subsystem: "sigverifier",
			Name:      "output_validated_tx_batch_queue_size",
			Help:      "Size of the output validated transaction batch queue of the signature verifier manager.",
		}),
		vcserviceOutputTxStatusBatchQueueSize: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "coordinator",
			Subsystem: "vcservice",
			Name:      "output_tx_status_batch_queue_size",
			Help: "Size of the output transaction status batch queue of " +
				"the validation and committer service manager.",
		}),
		vcserviceOutputValidatedTxBatchQueueSize: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "coordinator",
			Subsystem: "vcservice",
			Name:      "output_validated_tx_batch_queue_size",
			Help: "Size of the output validated transaction batch queue " +
				"of the validation and committer service manager.",
		}),
		sigverifierTransactionProcessedTotal: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "sigverifier",
			Name:      "transaction_processed_total",
			Help:      "Total number of transactions processed by the signature verifier manager.",
		}),
		vcserviceTransactionProcessedTotal: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "vcservice",
			Name:      "transaction_processed_total",
			Help:      "Total number of transactions processed by the validation and committer service manager.",
		}),
		verifiersConnection: mp.NewConnectionMetrics(monitoring.ConnectionMetricsOpts{
			Namespace:       "coordinator",
			RemoteNamespace: "verifier",
		}),
		verifiersRetriedTransactionTotal: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "vcservice",
			Name:      "retired_transaction_total",
			Help:      "Total number of transactions retried by the validation and committer service manager.",
		}),
		vcservicesConnection: mp.NewConnectionMetrics(monitoring.ConnectionMetricsOpts{
			Namespace:       "coordinator",
			RemoteNamespace: "vcservice",
		}),
		vcservicesRetriedTransactionTotal: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "sigverifier",
			Name:      "retired_transaction_total",
			Help:      "Total number of transactions retried by the signature verifier manager.",
		}),
	}
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

type perfMetrics struct {
	*monitoring.MetricsProvider

	// received and processed transactions
	transactionsSentTotal           prometheus.Counter
	transactionsStatusReceivedTotal *prometheus.CounterVec

	// processing duration in relay service
	// block and transaction status batch can be of different sizes but the processing time is still useful.
	blockMappingInRelaySeconds                  prometheus.Histogram
	mappedBlockProcessingInRelaySeconds         prometheus.Histogram
	transactionStatusesProcessingInRelaySeconds prometheus.Histogram

	waitingTransactionsQueueSize prometheus.Gauge

	// queue sizes
	yetToBeCommittedBlocksQueueSize prometheus.Gauge
	committedBlocksQueueSize        prometheus.Gauge

	coordConnection *monitoring.ConnectionMetrics

	appendBlockToLedgerSeconds prometheus.Histogram
	blockHeight                prometheus.Gauge

	// throughput metrics
	transactionInThroughput  prometheus.Counter
	transactionOutThroughput prometheus.Counter

	// notifier metrics
	notifierActiveStreams          prometheus.Gauge
	notifierPendingTxIDs           prometheus.Gauge
	notifierUniquePendingTxIDs     prometheus.Gauge
	notifierTxIDsStatusDeliveries  prometheus.Counter
	notifierTxIDsTimeoutDeliveries prometheus.Counter
}

func newPerformanceMetrics(mp *monitoring.MetricsProvider) *perfMetrics {
	histoBuckets := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.2, 0.3, 0.4, 0.5, 0.75, 1}
	return &perfMetrics{
		MetricsProvider: mp,
		transactionsSentTotal: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "sidecar",
			Subsystem: "grpc_coordinator",
			Name:      "sent_transaction_total",
			Help:      "Total number of transactions sent to the coordinator service.",
		}),
		transactionsStatusReceivedTotal: mp.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sidecar",
			Subsystem: "grpc_coordinator",
			Name:      "received_transaction_status_total",
			Help:      "Total number of transactions statuses received from the coordinator service.",
		}, []string{"status"}),
		blockMappingInRelaySeconds: mp.NewHistogram(prometheus.HistogramOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "block_mapping_seconds",
			Help:      "Time spent mapping a received block to an internal block.",
			Buckets:   histoBuckets,
		}),
		mappedBlockProcessingInRelaySeconds: mp.NewHistogram(prometheus.HistogramOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "mapped_block_processing_seconds",
			Help:      "Time spent processing an internal block and sending it to the coordinator.",
			Buckets:   histoBuckets,
		}),
		transactionStatusesProcessingInRelaySeconds: mp.NewHistogram(prometheus.HistogramOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "transaction_status_batch_processing_seconds",
			Help:      "Time spent processing a received status batch from the coordinator.",
			Buckets:   histoBuckets,
		}),
		waitingTransactionsQueueSize: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "waiting_transactions_queue_size",
			Help:      "Total number of transactions waiting at the relay for statuses.",
		}),
		yetToBeCommittedBlocksQueueSize: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "input_block_queue_size",
			Help:      "Size of the input block queue of the relay service.",
		}),
		committedBlocksQueueSize: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "output_committed_block_queue_size",
			Help:      "Size of the output committed block queue of the relay service.",
		}),
		coordConnection: mp.NewConnectionMetrics(monitoring.ConnectionMetricsOpts{
			Namespace:       "sidecar",
			RemoteNamespace: "coordinator",
		}),
		appendBlockToLedgerSeconds: mp.NewHistogram(prometheus.HistogramOpts{
			Namespace: "sidecar",
			Subsystem: "ledger",
			Name:      "append_block_seconds",
			Help:      "Time spent appending a block to the ledger.",
			Buckets:   histoBuckets,
		}),
		blockHeight: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "sidecar",
			Subsystem: "ledger",
			Name:      "block_height",
			Help:      "The current block height of the ledger.",
		}),
		transactionInThroughput: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "transaction_in_total",
			Help:      "Total number of transactions received from the orderer.",
		}),
		transactionOutThroughput: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "sidecar",
			Subsystem: "relay",
			Name:      "transaction_out_total",
			Help:      "Total number of transaction statuses processed from the coordinator.",
		}),
		notifierActiveStreams: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "sidecar",
			Subsystem: "notifier",
			Name:      "active_streams",
			Help:      "Number of active notification streams.",
		}),
		notifierPendingTxIDs: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "sidecar",
			Subsystem: "notifier",
			Name:      "pending_tx_ids",
			Help:      "Number of pending (txID, request) subscriptions waiting for status notification.",
		}),
		notifierUniquePendingTxIDs: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "sidecar",
			Subsystem: "notifier",
			Name:      "unique_pending_tx_ids",
			Help:      "Number of unique transaction IDs pending across all requests.",
		}),
		notifierTxIDsStatusDeliveries: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "sidecar",
			Subsystem: "notifier",
			Name:      "tx_ids_status_deliveries_total",
			Help:      "Total number of transaction IDs' status deliveries to clients.",
		}),
		notifierTxIDsTimeoutDeliveries: mp.NewCounter(prometheus.CounterOpts{
			Namespace: "sidecar",
			Subsystem: "notifier",
			Name:      "tx_ids_timeout_deliveries_total",
			Help:      "Total number of transaction IDs' timeout deliveries to clients.",
		}),
	}
}

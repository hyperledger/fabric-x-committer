/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/deliverorderer"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const (
	namespace = "sidecar"

	subsystemRelay           = "relay"
	subsystemNotifier        = "notifier"
	subsystemGRPCCoordinator = "grpc_coordinator"
	subsystemLedger          = "ledger"
)

type perfMetrics struct {
	*monitoring.Provider

	// received and processed transactions
	transactionsSentTotal           prometheus.Counter
	transactionsStatusReceivedTotal *prometheus.CounterVec

	// processing duration in relay service
	// block and transaction status batch can be of different sizes but the processing time is still useful.
	blockMappingInRelaySeconds                  prometheus.Histogram
	mappedBlockProcessingInRelaySeconds         prometheus.Histogram
	transactionStatusesProcessingInRelaySeconds prometheus.Histogram

	waitingTransactionsQueueSize prometheus.Gauge
	serverMetrics                *serve.ServerMetrics

	// queue sizes
	yetToBeCommittedBlocksQueueSize prometheus.GaugeFunc
	mappedBlocksQueueSize           prometheus.GaugeFunc
	statusBatchQueueSize            prometheus.GaugeFunc
	committedBlocksQueueSize        prometheus.GaugeFunc

	coordConnection *monitoring.ConnectionMetrics

	appendBlockToLedgerSeconds prometheus.Histogram
	blockHeight                prometheus.Gauge

	// throughput metrics
	transactionInThroughput  prometheus.Counter
	transactionOutThroughput prometheus.Counter

	// notifier metrics
	notifierPendingTxIDs           prometheus.Gauge
	notifierUniquePendingTxIDs     prometheus.Gauge
	notifierTxIDsStatusDeliveries  prometheus.Counter
	notifierTxIDsTimeoutDeliveries prometheus.Counter
	notifierInputBlockQueueSize    prometheus.GaugeFunc
	notifierInputStatusQueueSize   prometheus.GaugeFunc
	notifierRequestQueueSize       prometheus.GaugeFunc
	notifierTimeoutQueueSize       prometheus.GaugeFunc

	// A StreamAllTransactions subscription has its own block queue, so the queue is reported per
	// stream and summed by the operator, e.g. sum(sidecar_notifier_stream_block_queue_size).
	allTxStreamBlockQueueSize *monitoring.ChannelLenGaugeVec[*committedBlockWithTxs]

	// delivery metrics
	delivery *deliverorderer.Metrics
}

func newPerformanceMetrics(q *queues) *perfMetrics {
	p := monitoring.NewProvider()

	histoBuckets := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.2, 0.3, 0.4, 0.5, 0.75, 1}
	return &perfMetrics{
		Provider: p,
		transactionsSentTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPCCoordinator,
			Name:      "sent_transaction_total",
			Help:      "Total number of transactions sent to the coordinator service.",
		}),
		transactionsStatusReceivedTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPCCoordinator,
			Name:      "received_transaction_status_total",
			Help:      "Total number of transactions statuses received from the coordinator service.",
		}, []string{"status"}),
		blockMappingInRelaySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "block_mapping_seconds",
			Help:      "Time spent mapping a received block to an internal block.",
			Buckets:   histoBuckets,
		}),
		mappedBlockProcessingInRelaySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "mapped_block_processing_seconds",
			Help:      "Time spent processing an internal block and sending it to the coordinator.",
			Buckets:   histoBuckets,
		}),
		transactionStatusesProcessingInRelaySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "transaction_status_batch_processing_seconds",
			Help:      "Time spent processing a received status batch from the coordinator.",
			Buckets:   histoBuckets,
		}),
		waitingTransactionsQueueSize: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "waiting_transactions_queue_size",
			Help:      "Total number of transactions waiting at the relay for statuses.",
		}),
		committedBlocksQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "output_committed_block_queue_size",
			Help:      "Size of the output committed block queue of the relay service.",
		}, q.committedBlock),
		notifierInputBlockQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "input_block_queue_size",
			Help:      "Size of the committed block queue delivered from the relay to the notifier.",
		}, q.committedBlockWithTxs),
		notifierInputStatusQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "input_status_queue_size",
			Help:      "Size of the transaction status queue delivered from the relay to the notifier.",
		}, q.statusQueue),
		notifierRequestQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "request_queue_size",
			Help:      "Size of the queue of notification requests received from clients.",
		}, q.notifierRequests),
		notifierTimeoutQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "timeout_queue_size",
			Help:      "Size of the queue of notification requests that have timed out.",
		}, q.notifierTimeouts),
		allTxStreamBlockQueueSize: monitoring.NewChannelLenGaugeVec[*committedBlockWithTxs](
			p, prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystemNotifier,
				Name:      "stream_block_queue_size",
				Help:      "Size of one all-transactions stream's queue of blocks waiting to be sent.",
			}, []string{"stream"},
		),
		coordConnection: monitoring.NewConnectionMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: "coordinator",
		}),
		serverMetrics: serve.NewServerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: "grpc",
		}),
		appendBlockToLedgerSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemLedger,
			Name:      "append_block_seconds",
			Help:      "Time spent appending a block to the ledger.",
			Buckets:   histoBuckets,
		}),
		blockHeight: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemLedger,
			Name:      "block_height",
			Help:      "The current block height of the ledger.",
		}),
		transactionInThroughput: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "transaction_in_total",
			Help:      "Total number of transactions received from the orderer.",
		}),
		transactionOutThroughput: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "transaction_out_total",
			Help:      "Total number of transaction statuses processed from the coordinator.",
		}),
		notifierPendingTxIDs: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "pending_tx_ids",
			Help:      "Number of pending (txID, request) subscriptions waiting for status notification.",
		}),
		notifierUniquePendingTxIDs: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "unique_pending_tx_ids",
			Help:      "Number of unique transaction IDs pending across all requests.",
		}),
		notifierTxIDsStatusDeliveries: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "tx_ids_status_deliveries_total",
			Help:      "Total number of transaction IDs' status deliveries to clients.",
		}),
		notifierTxIDsTimeoutDeliveries: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemNotifier,
			Name:      "tx_ids_timeout_deliveries_total",
			Help:      "Total number of transaction IDs' timeout deliveries to clients.",
		}),
		delivery: deliverorderer.NewMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: "delivery",
		}),
		yetToBeCommittedBlocksQueueSize: p.NewAtomicChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "input_block_queue_size",
			Help:      "Size of the input block queue of the relay service.",
		}, &q.relayInputBlock),
		mappedBlocksQueueSize: p.NewAtomicChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "mapped_block_queue_size",
			Help:      "Size of the relay's queue of mapped blocks waiting to be sent to the coordinator.",
		}, &q.relayMappedBlock),
		statusBatchQueueSize: p.NewAtomicChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemRelay,
			Name:      "input_status_batch_queue_size",
			Help:      "Size of the relay's queue of status batches received from the coordinator.",
		}, &q.relayStatusBatch),
	}
}

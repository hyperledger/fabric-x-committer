/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const (
	namespace = "vcservice"

	subsystemGRPC      = "grpc"
	subsystemPreparer  = "preparer"
	subsystemValidator = "validator"
	subsystemCommitter = "committer"
	subsystemDatabase  = "database"

	nameInputQueueSize        = "input_queue_size"
	nameTxBatchLatencySeconds = "tx_batch_latency_seconds"
)

var buckets = []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1}

type perfMetrics struct {
	*monitoring.Provider

	serverMetrics *serve.ServerMetrics

	// transaction received and processed counters
	transactionReceivedTotal     prometheus.Counter
	transactionProcessedTotal    prometheus.Counter
	transactionCommittedTotal    prometheus.Counter
	transactionMVCCConflictTotal prometheus.Counter
	transactionDuplicateTxTotal  prometheus.Counter

	// queue sizes for each sub-component
	batcherInputQueueSize   prometheus.GaugeFunc
	preparerInputQueueSize  prometheus.GaugeFunc
	validatorInputQueueSize prometheus.GaugeFunc
	committerInputQueueSize prometheus.GaugeFunc
	txStatusOutputQueueSize prometheus.GaugeFunc

	// time taken by each sub-component
	preparerTxBatchLatencySeconds  prometheus.Histogram
	validatorTxBatchLatencySeconds prometheus.Histogram
	committerTxBatchLatencySeconds prometheus.Histogram

	databaseTxBatchValidationLatencySeconds                  prometheus.Histogram
	databaseTxBatchQueryVersionLatencySeconds                prometheus.Histogram
	databaseTxBatchCommitLatencySeconds                      prometheus.Histogram
	databaseTxBatchCommitTxsStatusLatencySeconds             prometheus.Histogram
	databaseTxBatchCommitUpdateLatencySeconds                prometheus.Histogram
	databaseTxBatchCommitInsertNewKeyWithValueLatencySeconds prometheus.Histogram
}

func newVCServiceMetrics(q *queues) *perfMetrics {
	p := monitoring.NewProvider()

	return &perfMetrics{
		Provider: p,
		transactionReceivedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
			Name:      "received_transaction_total",
			Help:      "Number of transactions received by the service",
		}),
		transactionProcessedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
			Name:      "processed_transaction_total",
			Help:      "Number of transactions processed by the service",
		}),
		serverMetrics: serve.NewServerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
		}),
		transactionCommittedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "committed_transaction_total",
			Help:      "The total number of transactions committed",
		}),
		transactionMVCCConflictTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "mvcc_conflict_total",
			Help:      "The total number of transactions that failed due to MVCC conflict",
		}),
		transactionDuplicateTxTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "duplicate_transaction_total",
			Help:      "The total number of duplicate transactions",
		}),
		batcherInputQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "batcher",
			Name:      nameInputQueueSize,
			Help:      "The batcher input queue size, holding the batches received from the client",
		}, q.receivedTxBatch),
		preparerInputQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemPreparer,
			Name:      nameInputQueueSize,
			Help:      "The preparer input queue size",
		}, q.toPrepareTxs),
		validatorInputQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemValidator,
			Name:      nameInputQueueSize,
			Help:      "The validator input queue size",
		}, q.preparedTxs),
		committerInputQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemCommitter,
			Name:      nameInputQueueSize,
			Help:      "The committer input queue size",
		}, q.validatedTxs),
		txStatusOutputQueueSize: p.NewChannelLenGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "txstatus",
			Name:      "output_queue_size",
			Help:      "The txstatus output queue size",
		}, q.txsStatus),
		preparerTxBatchLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemPreparer,
			Name:      nameTxBatchLatencySeconds,
			Help:      "The latency of the preparer processing a batch of transactions",
			Buckets:   buckets,
		}),
		validatorTxBatchLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemValidator,
			Name:      nameTxBatchLatencySeconds,
			Help:      "The latency of the validator processing a batch of transactions",
			Buckets:   buckets,
		}),
		committerTxBatchLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemCommitter,
			Name:      nameTxBatchLatencySeconds,
			Help:      "The latency of the committer processing a batch of transactions",
			Buckets:   buckets,
		}),
		databaseTxBatchValidationLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_validation_latency_seconds",
			Help:      "The latency of the database validating a batch of transactions",
			Buckets:   buckets,
		}),
		databaseTxBatchQueryVersionLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_query_version_latency_seconds",
			Help:      "The latency of the database querying version for keys in a batch of transactions",
			Buckets:   buckets,
		}),
		databaseTxBatchCommitLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_latency_seconds",
			Help:      "The latency of the database committing a batch of transactions",
			Buckets:   buckets,
		}),
		databaseTxBatchCommitTxsStatusLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_txs_status_latency_seconds",
			Help:      "The latency of the database committing a batch of transactions and updating their status",
			Buckets:   buckets,
		}),
		databaseTxBatchCommitUpdateLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_update_latency_seconds",
			Help: "The latency of the database committing a batch of transactions which involes " +
				"updating existing keys",
			Buckets: buckets,
		}),
		databaseTxBatchCommitInsertNewKeyWithValueLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_insert_new_key_with_value_latency_seconds",
			Help: "The latency of the database committing a batch of transactions which involes " +
				"inserting new keys with values",
			Buckets: buckets,
		}),
	}
}

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

	// How a commit attempt ended. A rejected attempt is rolled back and retried by the caller, so it
	// is a different operation from a clean commit and must not be averaged with it. Duplicates and
	// conflicts are kept apart because they are rejected by different tables.
	commitSuccess   = "success"
	commitDuplicate = "duplicate"
	commitConflict  = "conflict"
	commitError     = "error"
)

// Shared so the label name is written once. The doc generator inlines a slice declared this way,
// which it cannot do for a bare constant inside the literal.
var commitStatusLabels = []string{"status"}

// The 2 s and 5 s buckets keep a rejected commit attempt, which can exceed 1 s, out of +Inf.
var buckets = []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1, 2, 5}

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
	databaseTxBatchCommitLatencySeconds                      *prometheus.HistogramVec
	databaseTxBatchCommitTxsStatusLatencySeconds             *prometheus.HistogramVec
	databaseTxBatchCommitUpdateLatencySeconds                *prometheus.HistogramVec
	databaseTxBatchCommitInsertNewKeyWithValueLatencySeconds *prometheus.HistogramVec
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
		databaseTxBatchCommitLatencySeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_latency_seconds",
			Help: "The latency of the database committing a batch of transactions, by how the attempt " +
				"ended: success, duplicate (the batch carried already-committed transaction IDs), " +
				"conflict (it carried already-existing keys) or error. A rejected attempt is rolled " +
				"back and retried",
			Buckets: buckets,
		}, commitStatusLabels),
		databaseTxBatchCommitTxsStatusLatencySeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_txs_status_latency_seconds",
			Help: "The latency of the database committing a batch of transactions and updating their " +
				"status, by outcome: success, duplicate (some transaction IDs were already committed, " +
				"so their IDs are read back) or error",
			Buckets: buckets,
		}, commitStatusLabels),
		databaseTxBatchCommitUpdateLatencySeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_update_latency_seconds",
			Help: "The latency of the database committing a batch of transactions which involves " +
				"updating existing keys, by outcome: success or error",
			Buckets: buckets,
		}, commitStatusLabels),
		databaseTxBatchCommitInsertNewKeyWithValueLatencySeconds: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "tx_batch_commit_insert_new_key_with_value_latency_seconds",
			Help: "The latency of the database committing a batch of transactions which involves " +
				"inserting new keys with values, by outcome: success, conflict (a key already " +
				"existed, so the insert raised unique_violation and the existing keys were read " +
				"back) or error",
			Buckets: buckets,
		}, commitStatusLabels),
	}
}

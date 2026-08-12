/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

const (
	namespace = "coordinator"

	subsystemGlobalDependencyGraph = "global_dependency_graph"
	subsystemLocalDependencyGraph  = "local_dependency_graph"

	nameInputTxBatchQueueSize = "input_tx_batch_queue_size"
	nameTxProcessedTotal      = "tx_processed_total"
)

var bucket = []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1}

type perfMetrics struct {
	provider *monitoring.Provider

	// queue sizes
	ldgInputTxBatchQueueSize prometheus.Gauge
	gdgInputTxBatchQueueSize prometheus.Gauge
	gdgWaitingTxQueueSize    prometheus.Gauge

	// processed transactions by each manager
	ldgTxProcessedTotal          prometheus.Counter
	gdgTxProcessedTotal          prometheus.Counter
	gdgValidatedTxProcessedTotal prometheus.Counter

	// performance of constructDependencyGraph()
	gdgConstructionSeconds             prometheus.Histogram
	gdgConstructorWaitForLockSeconds   prometheus.Histogram
	gdgAddTxToGraphSeconds             prometheus.Histogram
	gdgUpdateDependencyDetectorSeconds prometheus.Histogram

	// performance of processValidatedTransactions()
	gdgValidatedTxProcessingSeconds           prometheus.Histogram
	gdgValidatedTxProcessorWaitForLockSeconds prometheus.Histogram
	gdgRemoveDependentsOfValidatedTxSeconds   prometheus.Histogram
	gdgAddFreedTxSeconds                      prometheus.Histogram

	// performance of outputFreedExistingTransactions()
	gdgOutputFreedTxSeconds prometheus.Histogram

	dependentTransactionsQueueSize prometheus.Gauge
}

func newPerformanceMetrics(p *monitoring.Provider) *perfMetrics {
	return &perfMetrics{
		provider: p,
		ldgInputTxBatchQueueSize: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemLocalDependencyGraph,
			Name:      nameInputTxBatchQueueSize,
			Help:      "Size of the input transaction batch queue of the local dependency graph manager",
		}),
		gdgInputTxBatchQueueSize: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      nameInputTxBatchQueueSize,
			Help:      "Size of the input transaction batch queue of the global dependency graph manager",
		}),
		gdgWaitingTxQueueSize: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "size",
			Help: "Size of the global dependency graph manager in terms " +
				"of the number of transactions waiting to be processed",
		}),
		ldgTxProcessedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemLocalDependencyGraph,
			Name:      nameTxProcessedTotal,
			Help:      "Total number of new transactions processed by the local dependency graph manager",
		}),
		gdgTxProcessedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      nameTxProcessedTotal,
			Help:      "Total number of new transactions processed by the global dependency graph manager",
		}),
		gdgValidatedTxProcessedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "validated_tx_processed_total",
			Help:      "Total number of validated transactions processed by the global dependency graph manager",
		}),
		gdgConstructionSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "construction_seconds",
			Help: "Time spent adding a transaction batch to the global dependency graph " +
				"in the global dependency graph manager",
			Buckets: bucket,
		}),
		gdgConstructorWaitForLockSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace:   namespace,
			Subsystem:   subsystemGlobalDependencyGraph,
			Name:        "constructor_wait_for_lock_seconds",
			Help:        "Time spent waiting for the lock in the constructor of the global dependency graph manager",
			ConstLabels: map[string]string{},
			Buckets:     bucket,
		}),
		gdgAddTxToGraphSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "add_tx_batch_to_graph_seconds",
			Help:      "Time spent adding a transaction batch to the graph in the global dependency graph manager",
			Buckets:   bucket,
		}),
		gdgUpdateDependencyDetectorSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "update_dependency_detector_seconds",
			Help: "Time spent updating the dependency detector with a transaction batch " +
				"in the global dependency graph manager",
			Buckets: bucket,
		}),
		gdgValidatedTxProcessingSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "validated_tx_batch_processing_seconds",
			Help: "Time spent processing a validated transaction batch in the global " +
				"dependency graph manager",
			Buckets: bucket,
		}),
		gdgValidatedTxProcessorWaitForLockSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "validated_tx_batch_processor_wait_for_lock_seconds",
			Help: "Time spent waiting for the lock in the validated transaction " +
				"processor of the global dependency graph manager",
			Buckets: bucket,
		}),
		gdgRemoveDependentsOfValidatedTxSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "remove_dependents_of_validated_tx_batch_seconds",
			Help: "Time spent removing the dependents of a validated transaction batch " +
				"in the global dependency graph manager",
			Buckets: bucket,
		}),
		gdgAddFreedTxSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "add_freed_tx_batch_seconds",
			Help:      "Time spent adding a freed transaction batch to a queue in the global dependency graph manager",
			Buckets:   bucket,
		}),
		gdgOutputFreedTxSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemGlobalDependencyGraph,
			Name:      "output_freed_tx_batch_seconds",
			Help:      "Time spent outputting a freed transaction batch in the global dependency graph manager",
			Buckets:   bucket,
		}),
		dependentTransactionsQueueSize: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "dependency_graph",
			Name:      "dependent_transactions_queue_size",
			Help:      "The number of transactions currently waiting on dependencies.",
		}),
	}
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

const (
	namespace = "coordinator"

	subsystemGRPC      = "grpc"
	subsystemVCService = "vcservice"
	subsystemVerifier  = "verifier"
)

type (
	perfMetrics struct {
		*monitoring.Provider

		// received and processed transactions
		transactionReceivedTotal  prometheus.Counter
		transactionCommittedTotal *prometheus.CounterVec

		// per-service-manager metrics
		verifiers *managerMetrics
		vcs       *managerMetrics

		// Verifier-Only: These histograms measure mutex usage stats
		// from the status-receive path (fetchAndDeleteTxBeingValidated).
		verifierFetchValidatedTxsLockWaitSeconds prometheus.Histogram
		verifierFetchValidatedTxsLockHoldSeconds prometheus.Histogram

		// The status queue is read by the coordinator itself, so it has no manager to report it.
		// Its saturation is what stalls the idle handshake, see NoPendingTransactionProcessing.
		vcserviceOutputTxStatusBatchQueueSize prometheus.GaugeFunc
	}

	// managerMetrics holds the metrics that every service manager reports. Defining them
	// once and instantiating them per manager keeps the two managers from drifting apart.
	managerMetrics struct {
		// connection tracks the connection state to the service endpoints.
		connection *monitoring.ConnectionMetrics
		// processedTotal counts the transactions whose status was received and forwarded.
		processedTotal prometheus.Counter
		// retriedTotal counts the transactions resubmitted after a failure.
		retriedTotal prometheus.Counter
		// inputQueueSize reports the size of the manager's input batch queue.
		inputQueueSize prometheus.GaugeFunc
		// outputQueueSize reports the size of the manager's output batch queue.
		outputQueueSize prometheus.GaugeFunc
	}
)

func newPerformanceMetrics(q *channels) *perfMetrics {
	var lockLatencyBuckets = []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1}
	p := monitoring.NewProvider()

	return &perfMetrics{
		Provider: p,
		transactionReceivedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
			Name:      "received_transaction_total",
			Help:      "Total number of transactions received by the coordinator service from the client.",
		}),
		transactionCommittedTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
			Name:      "committed_transaction_total",
			Help:      "Total number of transactions committed status sent by the coordinator service to the client.",
		}, []string{"status"}),
		verifiers: newManagerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: "verifier",
		}, q.depGraphToSigVerifierFreeTxs, q.sigVerifierToVCServiceValidatedTxs),
		vcs: newManagerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: subsystemVCService,
		}, q.sigVerifierToVCServiceValidatedTxs, q.vcServiceToDepGraphValidatedTxs),
		verifierFetchValidatedTxsLockWaitSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemVerifier,
			Name:      "fetch_validated_txs_lock_wait_seconds",
			Help: "Time spent waiting to acquire the txBeingValidated lock while fetching validated " +
				"transactions from a signature verifier.",
			Buckets: lockLatencyBuckets,
		}),
		verifierFetchValidatedTxsLockHoldSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemVerifier,
			Name:      "fetch_validated_txs_lock_hold_seconds",
			Help: "Time spent holding the txBeingValidated lock while fetching validated " +
				"transactions from a signature verifier.",
			Buckets: lockLatencyBuckets,
		}),
		vcserviceOutputTxStatusBatchQueueSize: p.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemVCService,
			Name:      "output_tx_status_batch_queue_size",
			Help: "Size of the output transaction status batch queue of " +
				"the validation and committer service manager.",
		}, func() float64 {
			return float64(q.vcServiceToCoordinatorTxStatus.len())
		}),
	}
}

// newManagerMetrics creates the metric set of a single service manager. The namespace and
// subsystem identify which manager reports them, and the two queues are reported on scrape.
func newManagerMetrics(
	p *monitoring.Provider,
	params monitoring.MetricsParameters,
	inputQueue <-chan dependencygraph.TxNodeBatch,
	outputQueue chan<- dependencygraph.TxNodeBatch,
) *managerMetrics {
	return &managerMetrics{
		connection: monitoring.NewConnectionMetrics(p, params),
		processedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "transaction_processed_total",
			Help:      "Total number of transactions processed by the manager.",
		}),
		retriedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "transaction_retried_total",
			Help:      "Total number of transactions retried by the manager.",
		}),
		inputQueueSize: p.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "input_batch_queue_size",
			Help:      "Size of the input batch queue of the manager.",
		}, func() float64 {
			return float64(len(inputQueue))
		}),
		outputQueueSize: p.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: params.Namespace,
			Subsystem: params.Subsystem,
			Name:      "output_batch_queue_size",
			Help:      "Size of the output batch queue of the manager.",
		}, func() float64 {
			return float64(len(outputQueue))
		}),
	}
}

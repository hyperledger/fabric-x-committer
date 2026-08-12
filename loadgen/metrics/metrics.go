/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/prometheus/client_golang/prometheus"
	promgo "github.com/prometheus/client_model/go"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

const namespace = "loadgen"

type (
	// PerfMetrics is a struct that contains the metrics for the block generator.
	PerfMetrics struct {
		*monitoring.Provider

		blockSentTotal            prometheus.Counter
		blockReceivedTotal        prometheus.Counter
		transactionSentTotal      prometheus.Counter
		transactionReceivedTotal  prometheus.Counter
		transactionCommittedTotal prometheus.Counter
		transactionAbortedTotal   prometheus.Counter
		validLatency              prometheus.Histogram
		invalidLatency            prometheus.Histogram

		// Scrape-time counters read from the shared tx-index counter; no caller updates them.
		createdKeysTotal         prometheus.CounterFunc
		referencedReadKeysTotal  prometheus.CounterFunc
		referencedWriteKeysTotal prometheus.CounterFunc

		latencyTracker *latencyReceiverSender
	}

	// MetricState is a collection of the current values of the metrics.
	MetricState struct {
		BlocksSent            uint64
		BlocksReceived        uint64
		TransactionsSent      uint64
		TransactionsReceived  uint64
		TransactionsCommitted uint64
		TransactionsAborted   uint64
		CreatedKeys           uint64
		ReferencedReadKeys    uint64
		ReferencedWriteKeys   uint64
	}

	// TxStatus is used to report a batch item.
	TxStatus struct {
		TxID   string
		Status committerpb.Status
	}
)

// NewLoadgenServiceMetrics creates a new PerfMetrics instance. The key-generation counters read from the
// shared counter at scrape time via KeyStats.
func NewLoadgenServiceMetrics(c *Config, counter *workload.TxCounter) *PerfMetrics {
	p := monitoring.NewProvider()
	latencyTracker := newLatencyReceiverSender(&c.Latency)
	return &PerfMetrics{
		Provider:       p,
		latencyTracker: latencyTracker,
		blockSentTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "block_sent_total",
			Help:      "Total number of blocks sent by the block generator",
		}),
		blockReceivedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "block_received_total",
			Help:      "Total number of blocks received by the block generator",
		}),
		transactionSentTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "transaction_sent_total",
			Help:      "Total number of transactions sent by the block generator",
		}),
		transactionReceivedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "transaction_received_total",
			Help:      "Total number of transactions received by the block generator",
		}),
		transactionCommittedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "transaction_committed_total",
			Help:      "Total number of transaction commit statuses received by the block generator",
		}),
		transactionAbortedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "transaction_aborted_total",
			Help:      "Total number of transaction abort statuses received by the block generator",
		}),
		validLatency: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "valid_transaction_latency_seconds",
			Help:      "Latency of valid transactions in seconds",
			Buckets:   latencyTracker.buckets,
		}),
		invalidLatency: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "invalid_transaction_latency_seconds",
			Help:      "Latency of invalid transactions in seconds",
			Buckets:   latencyTracker.buckets,
		}),
		createdKeysTotal: p.NewCounterFunc(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "created_keys_total",
			Help: "Total number of new keys the workload has introduced, each counted once when it is " +
				"first created; some may never be committed (for example, when the transaction that " +
				"creates the key aborts)",
		}, func() float64 { return float64(counter.KeyStats().KeyFrontier) }),
		referencedReadKeysTotal: p.NewCounterFunc(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "referenced_read_keys_total",
			Help: "Total number of read-only accesses that reused an already-created key instead of " +
				"introducing a new one",
		}, func() float64 { return float64(counter.KeyStats().ReferencedReadKeys) }),
		referencedWriteKeysTotal: p.NewCounterFunc(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "referenced_write_keys_total",
			Help: "Total number of write accesses (read-write and blind-write) that reused an " +
				"already-created key instead of introducing a new one",
		}, func() float64 { return float64(counter.KeyStats().ReferencedWriteKeys) }),
	}
}

// GetState returns the number of committed transactions.
func (c *PerfMetrics) GetState() MetricState {
	return MetricState{
		BlocksSent:            getCounterValue(c.blockSentTotal),
		BlocksReceived:        getCounterValue(c.blockReceivedTotal),
		TransactionsSent:      getCounterValue(c.transactionSentTotal),
		TransactionsReceived:  getCounterValue(c.transactionReceivedTotal),
		TransactionsCommitted: getCounterValue(c.transactionCommittedTotal),
		TransactionsAborted:   getCounterValue(c.transactionAbortedTotal),
		CreatedKeys:           getCounterValue(c.createdKeysTotal),
		ReferencedReadKeys:    getCounterValue(c.referencedReadKeysTotal),
		ReferencedWriteKeys:   getCounterValue(c.referencedWriteKeysTotal),
	}
}

// getCounterValue reads a counter-typed metric's value. The parameter is prometheus.Metric so it accepts
// both plain counters and CounterFuncs.
func getCounterValue(c prometheus.Metric) uint64 {
	gm := promgo.Metric{}
	if err := c.Write(&gm); err != nil {
		logger.Warnf("Failed reading counter value: %v", err)
		return 0
	}
	return uint64(gm.Counter.GetValue())
}

// OnSendBatch is a function that increments the block sent total and calls the latency tracker.
func (c *PerfMetrics) OnSendBatch(txIDs []string) {
	if len(txIDs) == 0 {
		return
	}
	promutil.AddToCounter(c.blockSentTotal, 1)
	promutil.AddToCounter(c.transactionSentTotal, len(txIDs))
	for _, txID := range txIDs {
		c.latencyTracker.onSendTransaction(txID)
	}
}

// OnReceiveBatch increments the transaction received total and calls the latency tracker.
func (c *PerfMetrics) OnReceiveBatch(batch []TxStatus) {
	if len(batch) == 0 {
		return
	}
	promutil.AddToCounter(c.blockReceivedTotal, 1)
	promutil.AddToCounter(c.transactionReceivedTotal, len(batch))
	successCount := 0
	for _, b := range batch {
		success := b.Status == committerpb.Status_COMMITTED
		if success {
			successCount++
		}
		tx := c.latencyTracker.onReceiveTransaction(b.TxID)
		if tx == nil {
			continue
		}

		logger.Debugf("Tracked transaction %s returned with status: %v", b.TxID, success)
		duration := time.Since(tx.created).Seconds()
		if success {
			c.validLatency.Observe(duration)
		} else {
			c.invalidLatency.Observe(duration)
		}
	}
	promutil.AddToCounter(c.transactionCommittedTotal, successCount)
	promutil.AddToCounter(c.transactionAbortedTotal, len(batch)-successCount)
}

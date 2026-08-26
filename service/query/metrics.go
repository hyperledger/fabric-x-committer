/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const (
	namespace = "queryservice"

	subsystemGRPC     = "grpc"
	subsystemDatabase = "database"

	sessionViews              = "active_views"
	sessionProcessingQueries  = "processing_queries"
	sessionWaitingQueries     = "waiting_queries"
	sessionInExecutionQueries = "in_execution_queries"
	sessionTransactions       = "transactions"
)

var sizeBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1 << 10, 1 << 11, 1 << 12, 1 << 13, 1 << 14, 1 << 15}

type perfMetrics struct {
	*monitoring.Provider

	serverMetrics                   *serve.ServerMetrics
	keysRequested                   prometheus.Counter
	keysResponded                   prometheus.Counter
	processingSessions              *prometheus.GaugeVec
	batchQueuingTimeSeconds         prometheus.Histogram
	batchQuerySize                  prometheus.Histogram
	batchResponseSize               prometheus.Histogram
	requestAssignmentLatencySeconds prometheus.Histogram
	queryLatencySeconds             prometheus.Histogram
}

func newQueryServiceMetrics() *perfMetrics {
	p := monitoring.NewProvider()

	return &perfMetrics{
		Provider: p,
		serverMetrics: serve.NewServerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
		}),
		keysRequested: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
			Name:      "key_requested_total",
			Help:      "Number of keys requested by the service",
		}),
		keysResponded: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
			Name:      "key_responded_total",
			Help:      "Number of keys responded by the service",
		}),
		processingSessions: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "processing_sessions",
			Help:      "Number of processing sessions in the service",
		}, []string{"session"}),
		batchQueuingTimeSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "batch_queueing_time_seconds",
			Help:      "The time batches waits for execution",
			Buckets:   monitoring.LatencyBuckets,
		}),
		batchQuerySize: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "batch_query_size",
			Help:      "The size of submitted batches",
			Buckets:   sizeBuckets,
		}),
		batchResponseSize: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "batch_response_size",
			Help:      "The size of response for batch queries",
			Buckets:   sizeBuckets,
		}),
		requestAssignmentLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "request_assignment_latency_seconds",
			Help:      "The latency of the query request assignment to the queue",
			Buckets:   monitoring.LatencyBuckets,
		}),
		queryLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemDatabase,
			Name:      "query_latency_seconds",
			Help:      "The latency of the queries' batches",
			Buckets:   monitoring.LatencyBuckets,
		}),
	}
}

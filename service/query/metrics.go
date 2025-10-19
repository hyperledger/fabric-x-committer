/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

const (
	grpcBeginView             = "begin_view"
	grpcEndView               = "end_view"
	grpcGetRows               = "get_rows"
	grpcGetTxStatus           = "get_tx_status"
	sessionViews              = "active_views"
	sessionProcessingQueries  = "processing_queries"
	sessionWaitingQueries     = "waiting_queries"
	sessionInExecutionQueries = "in_execution_queries"
	sessionTransactions       = "transactions"
)

var (
	timeBuckets = []float64{.0001, .001, .002, .003, .004, .005, .01, .03, .05, .1, .3, .5, 1, 2, 3, 4, 5, 10}
	sizeBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1 << 10, 1 << 11, 1 << 12, 1 << 13, 1 << 14, 1 << 15}
)

type perfMetrics struct {
	*monitoring.Provider

	requests                        *prometheus.CounterVec
	requestsLatency                 *prometheus.HistogramVec
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
		requests: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: "queryservice",
			Subsystem: "grpc",
			Name:      "requests_total",
			Help:      "Number of requests by the service",
		}, []string{"method"}),
		requestsLatency: p.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "queryservice",
			Subsystem: "grpc",
			Name:      "requests_latency_seconds",
			Help:      "The latency (seconds) of requests by the service",
			Buckets:   timeBuckets,
		}, []string{"method"}),
		keysRequested: p.NewCounter(prometheus.CounterOpts{
			Namespace: "queryservice",
			Subsystem: "grpc",
			Name:      "key_requested_total",
			Help:      "Number of keys requested by the service",
		}),
		keysResponded: p.NewCounter(prometheus.CounterOpts{
			Namespace: "queryservice",
			Subsystem: "grpc",
			Name:      "key_responded_total",
			Help:      "Number of keys responded by the service",
		}),
		processingSessions: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "queryservice",
			Subsystem: "database",
			Name:      "processing_sessions",
			Help:      "Number of processing sessions in the service",
		}, []string{"session"}),
		batchQueuingTimeSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: "queryservice",
			Subsystem: "database",
			Name:      "batch_queueing_time_seconds",
			Help:      "The time batches waits for execution",
			Buckets:   timeBuckets,
		}),
		batchQuerySize: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: "queryservice",
			Subsystem: "database",
			Name:      "batch_query_size",
			Help:      "The size of submitted batches",
			Buckets:   sizeBuckets,
		}),
		batchResponseSize: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: "queryservice",
			Subsystem: "database",
			Name:      "batch_response_size",
			Help:      "The size of response for batch queries",
			Buckets:   sizeBuckets,
		}),
		requestAssignmentLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: "queryservice",
			Subsystem: "database",
			Name:      "request_assignment_latency_seconds",
			Help:      "The latency of the query request assignment to the queue",
			Buckets:   timeBuckets,
		}),
		queryLatencySeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: "queryservice",
			Subsystem: "database",
			Name:      "query_latency_seconds",
			Help:      "The latency of the queries' batches",
			Buckets:   timeBuckets,
		}),
	}
}

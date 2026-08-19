/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshothasher

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const (
	namespace = "snapshothasher"

	subsystemHash = "hash"
	subsystemPoll = "poll"
	subsystemGRPC = "grpc"
)

// hashDurationBuckets spans seconds to tens of minutes: hashing a clone is a
// full table scan of the committed state, so the latency of interest is orders of
// magnitude above the per-transaction buckets the other services use.
var hashDurationBuckets = []float64{1, 5, 15, 30, 60, 300, 600, 1800, 3600}

type perfMetrics struct {
	*monitoring.Provider

	hashJobsCompletedTotal prometheus.Counter
	hashJobsFailedTotal    prometheus.Counter
	hashDurationSeconds    prometheus.Histogram

	// hashInProgress and hashStartedTimestampSeconds describe a job while it runs,
	// which every other metric here can only describe afterwards: hashDurationSeconds
	// is observed once a hash returns, so for the many minutes a full clone scan takes,
	// a busy service and an idle one publish identical numbers. Nothing else fills that
	// gap either -- this service exposes no RPCs and holds no queue whose depth could
	// be read. The timestamp is what makes "hashing for too long" alertable, since a
	// stuck job keeps the boolean at 1 and never reaches the duration histogram.
	hashInProgress              prometheus.Gauge
	hashStartedTimestampSeconds prometheus.Gauge

	// pollErrorsTotal counts ticks that could not even determine whether there is
	// work, which the hash-job counters cannot express: a tick that fails to read
	// the record completes no job and fails none, so with only those two counters a
	// service whose state database is unreachable looks identical to an idle one --
	// SERVING health check, flat counters -- for as long as the outage lasts.
	pollErrorsTotal prometheus.Counter

	// serverMetrics reports the RPC-level metrics every service exposes through the
	// shared stats handler. This service serves only health checks, so the value here
	// is uniformity: a probe that starts failing is visible the same way as for any
	// other service, without a special case for this one.
	serverMetrics *serve.ServerMetrics
}

func newSnapshotHasherMetrics() *perfMetrics {
	p := monitoring.NewProvider()
	return &perfMetrics{
		Provider: p,
		hashJobsCompletedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemHash,
			Name:      "jobs_completed_total",
			Help:      "Number of snapshot hash jobs that published a digest.",
		}),
		hashJobsFailedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemHash,
			Name:      "jobs_failed_total",
			Help:      "Number of snapshot hash jobs that ended without publishing a digest.",
		}),
		hashInProgress: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemHash,
			Name:      "in_progress",
			Help:      "1 while a snapshot clone is being hashed, 0 otherwise.",
		}),
		hashStartedTimestampSeconds: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemHash,
			Name:      "started_timestamp_seconds",
			Help: "Unix time at which the in-progress hash started, or 0 when none is " +
				"running; subtract from the current time to alert on a long-running hash.",
		}),
		pollErrorsTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemPoll,
			Name:      "errors_total",
			Help:      "Number of polls that failed before a hash job could be started or skipped.",
		}),
		hashDurationSeconds: p.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystemHash,
			Name:      "duration_seconds",
			Help:      "Time taken to hash a snapshot clone database.",
			Buckets:   hashDurationBuckets,
		}),
		serverMetrics: serve.NewServerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
		}),
	}
}

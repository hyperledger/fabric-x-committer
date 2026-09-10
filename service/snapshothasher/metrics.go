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

	// hashStartedTotal counts hash attempts that began, which nothing else here records:
	// hashDurationSeconds is observed only once a scan returns, and a scan can run for
	// many minutes, so a hash in flight would otherwise be indistinguishable from an idle
	// service. Its gap against the terminal counters is the in-flight signal. A hash lost
	// to a killed process is visible only after the fact, from the stored series: an
	// in-process counter resets to zero along with every other, so a fresh start looks
	// idle rather than divergent.
	hashStartedTotal prometheus.Counter

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
		hashStartedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystemHash,
			Name:      "started_total",
			Help: "Number of snapshot hash attempts that began; its gap against " +
				"duration_seconds_count plus jobs_failed_total is a hash in flight.",
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

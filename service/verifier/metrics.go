/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

type metrics struct {
	Provider             *monitoring.MetricsProvider
	VerifierServerInTxs  prometheus.Counter
	VerifierServerOutTxs prometheus.Counter
	ActiveStreams        prometheus.Gauge
	ActiveRequests       prometheus.Gauge
}

func newMonitoring(mp *monitoring.MetricsProvider) *metrics {
	return &metrics{
		Provider:             mp,
		VerifierServerInTxs:  mp.NewThroughputCounter("verifier_server", "tx", monitoring.In),
		VerifierServerOutTxs: mp.NewThroughputCounter("verifier_server", "tx", monitoring.Out),
		ActiveStreams: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "verifier_server",
			Subsystem: "grpc",
			Name:      "active_streams",
			Help:      "The total number of started streams",
		}),
		ActiveRequests: mp.NewGauge(prometheus.GaugeOpts{
			Namespace: "verifier_server",
			Subsystem: "parallel_executor",
			Name:      "active_requests",
			Help:      "The total number of active requests",
		}),
	}
}

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
	*monitoring.Provider
	VerifierServerInTxs  prometheus.Counter
	VerifierServerOutTxs prometheus.Counter
	ActiveStreams        prometheus.Gauge
	ActiveRequests       prometheus.Gauge
}

func newMonitoring() *metrics {
	p := monitoring.NewProvider()
	return &metrics{
		Provider:             p,
		VerifierServerInTxs:  p.NewThroughputCounter("verifier_server", "tx", monitoring.In),
		VerifierServerOutTxs: p.NewThroughputCounter("verifier_server", "tx", monitoring.Out),
		ActiveStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "verifier_server",
			Subsystem: "grpc",
			Name:      "active_streams",
			Help:      "The total number of started streams",
		}),
		ActiveRequests: p.NewGauge(prometheus.GaugeOpts{
			Namespace: "verifier_server",
			Subsystem: "parallel_executor",
			Name:      "active_requests",
			Help:      "The total number of active requests",
		}),
	}
}

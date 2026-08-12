/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

const namespace = "verifier_server"

type metrics struct {
	*monitoring.Provider
	VerifierServerTxs *monitoring.ThroughputMetrics
	ActiveStreams     prometheus.Gauge
	ActiveRequests    prometheus.Gauge
}

func newMonitoring() *metrics {
	p := monitoring.NewProvider()
	return &metrics{
		Provider: p,
		VerifierServerTxs: monitoring.NewThroughputMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: "tx",
		}),
		ActiveStreams: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "grpc",
			Name:      "active_streams",
			Help:      "The total number of started streams",
		}),
		ActiveRequests: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "parallel_executor",
			Name:      "active_requests",
			Help:      "The total number of active requests",
		}),
	}
}

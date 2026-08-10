/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

type metrics struct {
	*monitoring.Provider
	VerifierServerTxs *monitoring.ThroughputMetrics
	serverMetrics     *serve.ServerMetrics
	ActiveRequests    prometheus.Gauge
}

func newMonitoring() *metrics {
	p := monitoring.NewProvider()
	return &metrics{
		Provider: p,
		VerifierServerTxs: monitoring.NewThroughputMetrics(p, monitoring.MetricsParameters{
			Namespace: "verifier_server",
			Subsystem: "tx",
		}),
		serverMetrics: serve.NewServerMetrics(p, monitoring.MetricsParameters{
			Namespace: "verifier_server",
			Subsystem: "grpc",
		}),
		ActiveRequests: p.NewGauge(prometheus.GaugeOpts{
			Namespace: "verifier_server",
			Subsystem: "parallel_executor",
			Name:      "active_requests",
			Help:      "The total number of active requests",
		}),
	}
}

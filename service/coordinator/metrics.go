/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/servicemanager"
)

type perfMetrics struct {
	*monitoring.Provider

	// received and processed transactions
	transactionReceivedTotal  prometheus.Counter
	transactionCommittedTotal *prometheus.CounterVec

	verifiers *servicemanager.Metrics
	vcs       *servicemanager.Metrics
}

func newPerformanceMetrics() *perfMetrics {
	p := monitoring.NewProvider()

	return &perfMetrics{
		Provider: p,
		transactionReceivedTotal: p.NewCounter(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "grpc",
			Name:      "received_transaction_total",
			Help:      "Total number of transactions received by the coordinator service from the client.",
		}),
		transactionCommittedTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: "coordinator",
			Subsystem: "grpc",
			Name:      "committed_transaction_total",
			Help:      "Total number of transactions committed status sent by the coordinator service to the client.",
		}, []string{"status"}),
		verifiers: servicemanager.NewMetrics(p, monitoring.MetricsParameters{
			Namespace: "coordinator",
			Subsystem: "verifier",
		}),
		vcs: servicemanager.NewMetrics(p, monitoring.MetricsParameters{
			Namespace: "coordinator",
			Subsystem: "vcservice",
		}),
	}
}

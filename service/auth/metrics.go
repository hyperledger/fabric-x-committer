/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const (
	namespace = "authservice"

	subsystemGRPC   = "grpc"
	subsystemConfig = "config"
)

type perfMetrics struct {
	*monitoring.Provider

	serverMetrics     *serve.ServerMetrics
	configLastRefresh prometheus.Gauge
}

func newAuthServiceMetrics() *perfMetrics {
	p := monitoring.NewProvider()

	return &perfMetrics{
		Provider: p,
		serverMetrics: serve.NewServerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: subsystemGRPC,
		}),
		configLastRefresh: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemConfig,
			Name:      "last_refresh_timestamp_seconds",
			Help:      "Unix time of the last successful channel-configuration refresh.",
		}),
	}
}

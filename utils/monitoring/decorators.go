/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	// ThroughputDirection can in or out.
	ThroughputDirection = string

	// ConnectionMetricsOpts describes connection metrics parameters.
	ConnectionMetricsOpts struct {
		Namespace       string
		RemoteNamespace string
	}

	// ConnectionMetrics supports common connection metrics.
	ConnectionMetrics struct {
		Status       *prometheus.GaugeVec
		FailureTotal *prometheus.CounterVec
		connected    sync.Map // tracks connected grpc targets using map[string]any
	}
)

// Direction constants.
const (
	In  ThroughputDirection = "in"
	Out ThroughputDirection = "out"
)

// Connected observed connected.
func (m *ConnectionMetrics) Connected(grpcTarget string) {
	promutil.SetGaugeVec(m.Status, []string{grpcTarget}, connection.Connected)
	m.connected.Store(grpcTarget, nil)
}

// Disconnected observe disconnected. The failure count is increased only if the status was connected.
func (m *ConnectionMetrics) Disconnected(grpcTarget string) {
	promutil.SetGaugeVec(m.Status, []string{grpcTarget}, connection.Disconnected)
	if _, loaded := m.connected.LoadAndDelete(grpcTarget); loaded {
		promutil.AddToCounterVec(m.FailureTotal, []string{grpcTarget}, 1)
	}
}

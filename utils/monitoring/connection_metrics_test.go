/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestConnectionMetrics(t *testing.T) {
	t.Parallel()

	newConnectionMetrics := func() *ConnectionMetrics {
		p := NewProvider()
		return p.NewConnectionMetrics(ConnectionMetricsOpts{})
	}
	target := "localhost:7051"

	t.Run("Connected", func(t *testing.T) {
		t.Parallel()
		m := newConnectionMetrics()

		m.Connected(target)
		requireStatus(t, m, target, int(connection.Connected))
	})

	t.Run("Disconnected_AfterConnected", func(t *testing.T) {
		t.Parallel()
		m := newConnectionMetrics()

		m.Connected(target)
		m.Disconnected(target)

		requireStatus(t, m, target, int(connection.Disconnected))
		requireFailureTotal(t, m, target, 1)
	})

	t.Run("Disconnected_WithoutPriorConnect", func(t *testing.T) {
		t.Parallel()
		m := newConnectionMetrics()

		m.Disconnected(target)

		requireStatus(t, m, target, int(connection.Disconnected))
		requireFailureTotal(t, m, target, 0)
	})

	t.Run("Disconnected_Twice", func(t *testing.T) {
		t.Parallel()
		m := newConnectionMetrics()

		m.Connected(target)
		m.Disconnected(target)
		m.Disconnected(target)

		requireFailureTotal(t, m, target, 1)
	})

	t.Run("MultipleTargets", func(t *testing.T) {
		t.Parallel()
		m := newConnectionMetrics()
		target2 := "localhost:7052"

		m.Connected(target)
		m.Connected(target2)
		// Disconnect only target; target2 failure count must stay at 0.
		m.Disconnected(target)

		requireFailureTotal(t, m, target, 1)
		requireStatus(t, m, target2, int(connection.Connected))
		requireFailureTotal(t, m, target2, 0)
	})

	t.Run("Reconnect", func(t *testing.T) {
		t.Parallel()
		m := newConnectionMetrics()

		m.Connected(target)
		m.Disconnected(target)
		m.Connected(target)
		m.Disconnected(target)

		requireFailureTotal(t, m, target, 2)
	})
}

func requireStatus(t *testing.T, m *ConnectionMetrics, target string, expected int) {
	t.Helper()
	metric, err := m.Status.GetMetricWithLabelValues(target)
	require.NoError(t, err)
	test.RequireIntMetricValue(t, expected, metric)
}

func requireFailureTotal(t *testing.T, m *ConnectionMetrics, target string, expected int) {
	t.Helper()
	metric, err := m.FailureTotal.GetMetricWithLabelValues(target)
	require.NoError(t, err)
	test.RequireIntMetricValue(t, expected, metric)
}

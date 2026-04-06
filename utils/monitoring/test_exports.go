/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// ExpectedConn is used to describe the expected connection state.
type ExpectedConn struct {
	Status       int
	FailureTotal int
}

// WaitForConnections waits for a connection metric to have the required number of connected labels.
func WaitForConnections(t *testing.T, p *Provider, name string, requiredCount int) {
	t.Helper()
	require.Eventually(t, func() bool {
		gather, err := p.Registry().Gather()
		require.NoError(t, err)
		connectedCount := 0
		for _, mf := range gather {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				val := m.GetGauge().GetValue()
				if math.Abs(val-connection.Connected) < 1e-10 {
					connectedCount++
				}
			}
		}
		return connectedCount >= requiredCount
	}, time.Minute, 10*time.Millisecond)
}

// RequireConnectionMetrics waits for a connection status and a specified number of failures.
func RequireConnectionMetrics(
	t *testing.T,
	label string,
	connMetrics *ConnectionMetrics,
	expected ExpectedConn,
) {
	t.Helper()
	connStatus, err := connMetrics.Status.GetMetricWithLabelValues(label)
	require.NoError(t, err)
	connFailure, err := connMetrics.FailureTotal.GetMetricWithLabelValues(label)
	require.NoError(t, err)

	test.EventuallyIntMetric(t, expected.Status, connStatus, 30*time.Second, 200*time.Millisecond)
	test.RequireIntMetricValue(t, expected.FailureTotal, connFailure)
	test.RequireIntMetricValue(t, expected.Status, connStatus)
}

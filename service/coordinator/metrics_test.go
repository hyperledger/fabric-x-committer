/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

// TestManagerMetricsSubsystem asserts on the exported series names rather than on the
// perfMetrics fields. The two managers previously reported each other's retries because the
// counters were registered under each other's subsystem, and the tests that covered them
// asserted on the Go field, so they passed while the exported series was wrong.
func TestManagerMetricsSubsystem(t *testing.T) {
	t.Parallel()
	m := newTestPerfMetrics()

	promutil.AddToCounter(m.verifiers.processedTotal, 3)
	promutil.AddToCounter(m.verifiers.retriedTotal, 5)
	promutil.AddToCounter(m.vcs.processedTotal, 7)
	promutil.AddToCounter(m.vcs.retriedTotal, 11)

	for name, expected := range map[string]int{
		"coordinator_verifier_transaction_processed_total":  3,
		"coordinator_verifier_transaction_retried_total":    5,
		"coordinator_vcservice_transaction_processed_total": 7,
		"coordinator_vcservice_transaction_retried_total":   11,
	} {
		require.Equal(t, expected, gatherCounter(t, m, name), "series %s", name)
	}
}

// newTestPerfMetrics builds the metric set over a minimal set of queues. newPerformanceMetrics
// registers gauge functions over them, and a scrape reads every one, so the status queue must be
// constructed: it is a pointer, unlike the channels, whose nil length is zero.
func newTestPerfMetrics() *perfMetrics {
	return newPerformanceMetrics(&channels{
		vcServiceToCoordinatorTxStatus: newTxStatusQueue(1),
	})
}

// gatherCounter returns the value of the named counter series as the registry exports it,
// rounded to the nearest integer as the counters here all count transactions.
func gatherCounter(t *testing.T, m *perfMetrics, name string) int {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		require.Len(t, family.GetMetric(), 1)
		return int(math.Round(family.GetMetric()[0].GetCounter().GetValue()))
	}
	require.Failf(t, "metric not found", "no series named %s is registered", name)
	return 0
}

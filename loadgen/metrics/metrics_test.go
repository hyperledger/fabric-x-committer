/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestTransactionRateLimitGauge covers the reason the gauge reads the stream instead of being
// written by SetRateLimit: a rate changed at runtime has to show up without the setter knowing
// about the metric.
func TestTransactionRateLimitGauge(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		rateLimit uint64
		expected  int
	}{
		{name: "limited rate is reported", rateLimit: 4200, expected: 4200},
		{name: "unlimited rate is reported as zero", rateLimit: 0, expected: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTestMetrics(t, tc.rateLimit)
			test.RequireIntMetricValue(t, tc.expected, m.transactionRateLimit)
		})
	}

	t.Run("gauge follows a rate changed after construction", func(t *testing.T) {
		t.Parallel()
		stream := newTestStream(t, 1000)
		m := NewLoadgenServiceMetrics(&Config{}, workload.NewTxCounter(
			workload.DefaultProfile(1).Transaction,
		), stream)
		test.RequireIntMetricValue(t, 1000, m.transactionRateLimit)

		stream.SetRate(25_000)
		test.RequireIntMetricValue(t, 25_000, m.transactionRateLimit)

		stream.SetRate(0)
		test.RequireIntMetricValue(t, 0, m.transactionRateLimit)
	})
}

func newTestMetrics(t *testing.T, rateLimit uint64) *PerfMetrics {
	t.Helper()
	profile := workload.DefaultProfile(1)
	return NewLoadgenServiceMetrics(
		&Config{}, workload.NewTxCounter(profile.Transaction), newTestStream(t, rateLimit),
	)
}

func newTestStream(t *testing.T, rateLimit uint64) *workload.TxStream {
	t.Helper()
	profile := workload.DefaultProfile(1)
	stream, err := workload.NewTxStream(
		profile,
		&workload.StreamOptions{GenBatch: 1, BuffersSize: 1, RateLimit: rateLimit},
		workload.NewTxCounter(profile.Transaction),
	)
	require.NoError(t, err)
	return stream
}

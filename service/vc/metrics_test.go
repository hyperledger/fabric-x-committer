/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestQueueSizeMetricsAreWired fills each pipeline queue and asserts the gauge that reports it
// follows. The gauges are evaluated on scrape, so they cannot go stale, but they can still be
// attached to the wrong channel — the four subsystems all export a series named
// input_queue_size / output_queue_size, so a copy-paste would be silent.
func TestQueueSizeMetricsAreWired(t *testing.T) {
	t.Parallel()

	q := &queues{
		receivedTxBatch: make(chan *servicepb.VcBatch, 4),
		toPrepareTxs:    make(chan *servicepb.VcBatch, 4),
		preparedTxs:     make(chan *preparedTransactions, 4),
		validatedTxs:    make(chan *validatedTransactions, 4),
		txsStatus:       make(chan *servicepb.TxStatusBatch, 4),
	}
	m := newVCServiceMetrics(q)

	for _, tc := range []struct {
		name  string
		fill  func()
		gauge prometheus.Metric
	}{
		{
			name:  "batcher input",
			fill:  func() { q.receivedTxBatch <- &servicepb.VcBatch{} },
			gauge: m.batcherInputQueueSize,
		},
		{
			name:  "preparer input",
			fill:  func() { q.toPrepareTxs <- &servicepb.VcBatch{} },
			gauge: m.preparerInputQueueSize,
		},
		{
			name:  "validator input",
			fill:  func() { q.preparedTxs <- &preparedTransactions{} },
			gauge: m.validatorInputQueueSize,
		},
		{
			name:  "committer input",
			fill:  func() { q.validatedTxs <- &validatedTransactions{} },
			gauge: m.committerInputQueueSize,
		},
		{
			name:  "tx status output",
			fill:  func() { q.txsStatus <- &servicepb.TxStatusBatch{} },
			gauge: m.txStatusOutputQueueSize,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, 0, test.GetIntMetricValue(t, tc.gauge))
			tc.fill()
			tc.fill()
			require.Equal(t, 2, test.GetIntMetricValue(t, tc.gauge))
		})
	}
}

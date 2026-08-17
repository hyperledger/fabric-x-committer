/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestQueueSizeMetricsAreWired fills each stage's input queue and asserts the gauge that reports
// it follows. Both stages export a series named input_tx_batch_queue_size and differ only by
// subsystem, so a gauge attached to the wrong queue would otherwise go unnoticed.
func TestQueueSizeMetricsAreWired(t *testing.T) {
	t.Parallel()

	ldgInput := make(chan *TransactionBatch, 4)
	gdgInput := make(chan *transactionNodeBatch, 4)
	m := newPerformanceMetrics(monitoring.NewProvider(), &managerQueues{
		ldgInput: ldgInput,
		gdgInput: gdgInput,
	})

	test.RequireIntMetricValue(t, 0, m.ldgInputTxBatchQueueSize)
	test.RequireIntMetricValue(t, 0, m.gdgInputTxBatchQueueSize)

	ldgInput <- &TransactionBatch{}
	gdgInput <- &transactionNodeBatch{}
	gdgInput <- &transactionNodeBatch{}

	test.RequireIntMetricValue(t, 1, m.ldgInputTxBatchQueueSize)
	test.RequireIntMetricValue(t, 2, m.gdgInputTxBatchQueueSize)

	<-gdgInput
	test.RequireIntMetricValue(t, 1, m.ldgInputTxBatchQueueSize)
	test.RequireIntMetricValue(t, 1, m.gdgInputTxBatchQueueSize)
}

// TestQueueSizeMetricsUnsetQueue covers SimpleManager's construction, which leaves the global
// dependency graph queue unset because its pre-processing queue holds a different element type.
// A nil queue must report zero rather than panic on the scrape path.
func TestQueueSizeMetricsUnsetQueue(t *testing.T) {
	t.Parallel()

	m := newPerformanceMetrics(monitoring.NewProvider(), &managerQueues{})

	require.NotPanics(t, func() {
		test.RequireIntMetricValue(t, 0, m.ldgInputTxBatchQueueSize)
		test.RequireIntMetricValue(t, 0, m.gdgInputTxBatchQueueSize)
	})
}

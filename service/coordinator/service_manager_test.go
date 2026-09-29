/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/coordinator/dependencygraph"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestWorkerAddAndRecoverPendingTxs covers the tracking invariant that receiveResultsAndForward
// depends on when forwarding a result fails: every transaction returned to the tracker must be
// recoverable.
func TestWorkerAddAndRecoverPendingTxs(t *testing.T) {
	t.Parallel()
	p := monitoring.NewProvider()
	w := &serviceWorker{params: &serviceManagerParams{
		metrics: newManagerMetrics(p, monitoring.MetricsParameters{}, nil, nil),
	}}

	txsNode := dependencygraph.TxNodeBatch{
		{VCTx: &servicepb.VcTx{Ref: committerpb.NewTxRef("tx 1", 1, 0)}},
		{VCTx: &servicepb.VcTx{Ref: committerpb.NewTxRef("tx 2", 2, 0)}},
		{VCTx: &servicepb.VcTx{Ref: committerpb.NewTxRef("tx 3", 1, 1)}},
	}
	w.addTxsBeingProcessed(txsNode)
	require.Equal(t, len(txsNode), w.txBeingProcessed.Count())

	recovered := make(chan dependencygraph.TxNodeBatch, 1)
	w.recoverPendingTransactions(channel.NewWriter(t.Context(), recovered))

	require.ElementsMatch(t, txsNode, <-recovered)
	require.Zero(t, w.txBeingProcessed.Count())
	test.RequireIntMetricValue(t, len(txsNode), w.params.metrics.retriedTotal)
}

// TestManagerQueueSizeMetrics covers the queue gauges, which report the channel length when
// scraped rather than being sampled on a timer.
func TestManagerQueueSizeMetrics(t *testing.T) {
	t.Parallel()
	incomingTxs := make(chan dependencygraph.TxNodeBatch, 10)
	outgoingTxs := make(chan dependencygraph.TxNodeBatch, 10)
	m := newManagerMetrics(
		monitoring.NewProvider(), monitoring.MetricsParameters{}, incomingTxs, outgoingTxs,
	)

	test.RequireIntMetricValue(t, 0, m.inputQueueSize)
	test.RequireIntMetricValue(t, 0, m.outputQueueSize)

	incomingTxs <- nil
	outgoingTxs <- nil
	test.RequireIntMetricValue(t, 1, m.inputQueueSize)
	test.RequireIntMetricValue(t, 1, m.outputQueueSize)

	<-incomingTxs
	<-outgoingTxs
	test.RequireIntMetricValue(t, 0, m.inputQueueSize)
	test.RequireIntMetricValue(t, 0, m.outputQueueSize)
}

// allActiveConnections returns all the manager's active connections.
func allActiveConnections(m *serviceManager) []*grpc.ClientConn {
	workers := m.workers.Load()
	if workers == nil {
		return nil
	}
	conns := make([]*grpc.ClientConn, len(*workers))
	for i, w := range *workers {
		conns[i] = w.conn
	}
	return conns
}

// allPendingTxCount returns the number of transactions in flight at each worker.
func allPendingTxCount(m *serviceManager) []int {
	workers := m.workers.Load()
	if workers == nil {
		return nil
	}
	counts := make([]int, len(*workers))
	for i, w := range *workers {
		counts[i] = w.txBeingProcessed.Count()
	}
	return counts
}

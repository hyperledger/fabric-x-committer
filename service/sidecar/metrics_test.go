/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestQueueSizeMetricsAreWired fills each queue and asserts the gauge that reports it follows.
// A gauge evaluated on scrape cannot go stale, but it can still observe the wrong channel or no
// channel at all: the sidecar shipped for three months with a sampler that was never called, so
// sidecar_relay_input_block_queue_size and sidecar_relay_output_committed_block_queue_size read a
// flat zero. This asserts every queue gauge is attached to the queue its name claims.
func TestQueueSizeMetricsAreWired(t *testing.T) {
	t.Parallel()

	q := newQueues(4)
	m := newPerformanceMetrics(q)

	for _, tc := range []struct {
		name  string
		fill  func()
		gauge func() int
	}{
		{
			name:  "relay output committed block",
			fill:  func() { q.committedBlock <- &common.Block{} },
			gauge: func() int { return test.GetIntMetricValue(t, m.committedBlocksQueueSize) },
		},
		{
			name:  "notifier input block",
			fill:  func() { q.committedBlockWithTxs <- &committedBlockWithTxs{} },
			gauge: func() int { return test.GetIntMetricValue(t, m.notifierInputBlockQueueSize) },
		},
		{
			name:  "notifier input status",
			fill:  func() { q.statusQueue <- []*committerpb.TxStatus{{}} },
			gauge: func() int { return test.GetIntMetricValue(t, m.notifierInputStatusQueueSize) },
		},
		{
			name:  "notifier request",
			fill:  func() { q.notifierRequests <- &notificationRequest{} },
			gauge: func() int { return test.GetIntMetricValue(t, m.notifierRequestQueueSize) },
		},
		{
			name:  "notifier timeout",
			fill:  func() { q.notifierTimeouts <- &notificationRequest{} },
			gauge: func() int { return test.GetIntMetricValue(t, m.notifierTimeoutQueueSize) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, 0, tc.gauge())
			tc.fill()
			tc.fill()
			require.Equal(t, 2, tc.gauge())
		})
	}
}

// TestRelaySessionQueueMetricsAreWired covers the queues that live for one coordinator session.
// They are reported through an atomic pointer that the session installs, so a gauge read before
// any session, or after a session was replaced, must report the current queue and not the one the
// previous session left behind.
func TestRelaySessionQueueMetricsAreWired(t *testing.T) {
	t.Parallel()

	q := newQueues(4)
	m := newPerformanceMetrics(q)

	// No session has started, so every session-scoped gauge reports zero.
	test.RequireIntMetricValue(t, 0, m.yetToBeCommittedBlocksQueueSize)
	test.RequireIntMetricValue(t, 0, m.mappedBlocksQueueSize)
	test.RequireIntMetricValue(t, 0, m.statusBatchQueueSize)

	// reset is what a session calls, so it is also what publishes the queues to the gauges.
	q.reset()
	inputBlock, mappedBlock := *q.relayInputBlock.Load(), *q.relayMappedBlock.Load()
	statusBatch := *q.relayStatusBatch.Load()
	inputBlock <- &common.Block{}
	mappedBlock <- &blockMappingResult{}
	mappedBlock <- &blockMappingResult{}
	statusBatch <- &committerpb.TxStatusBatch{}

	test.RequireIntMetricValue(t, 1, m.yetToBeCommittedBlocksQueueSize)
	test.RequireIntMetricValue(t, 2, m.mappedBlocksQueueSize)
	test.RequireIntMetricValue(t, 1, m.statusBatchQueueSize)

	// The next session resets all three. Every gauge must follow the fresh queue and drop the
	// previous session's reading rather than latching it.
	q.reset()
	nextInputBlock, nextMappedBlock := *q.relayInputBlock.Load(), *q.relayMappedBlock.Load()
	nextStatusBatch := *q.relayStatusBatch.Load()
	test.RequireIntMetricValue(t, 0, m.yetToBeCommittedBlocksQueueSize)
	test.RequireIntMetricValue(t, 0, m.mappedBlocksQueueSize)
	test.RequireIntMetricValue(t, 0, m.statusBatchQueueSize)

	// reset installs fresh queues rather than reusing the ones it replaced.
	require.NotEqual(t, inputBlock, nextInputBlock)
	nextMappedBlock <- &blockMappingResult{}
	test.RequireIntMetricValue(t, 1, m.mappedBlocksQueueSize)
	require.Len(t, mappedBlock, 2)

	// Every session queue is sized like the fixed ones.
	require.Equal(t, cap(q.committedBlock), cap(nextInputBlock))
	require.Equal(t, cap(q.committedBlock), cap(nextMappedBlock))
	require.Equal(t, cap(q.committedBlock), cap(nextStatusBatch))
}

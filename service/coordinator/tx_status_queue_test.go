/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
)

func TestTxStatusQueueCount(t *testing.T) {
	t.Parallel()

	queue := newTxStatusQueue(2)
	batch1 := txStatusBatch("tx-1", "tx-2")
	batch2 := txStatusBatch("tx-3", "tx-4", "tx-5")

	require.Zero(t, queue.readyCount())
	require.Zero(t, queue.len())

	require.True(t, queue.write(t.Context(), batch1))
	require.Equal(t, int32(2), queue.readyCount())
	require.Equal(t, 1, queue.len())

	require.True(t, queue.write(t.Context(), batch2))
	require.Equal(t, int32(5), queue.readyCount())
	require.Equal(t, 2, queue.len())

	readBatch, ok := queue.read(t.Context())
	require.True(t, ok)
	require.Same(t, batch1, readBatch)
	require.Equal(t, int32(3), queue.readyCount())
	require.Equal(t, 1, queue.len())

	readBatch, ok = queue.read(t.Context())
	require.True(t, ok)
	require.Same(t, batch2, readBatch)
	require.Zero(t, queue.readyCount())
	require.Zero(t, queue.len())
}

func TestTxStatusQueueCanceledContext(t *testing.T) {
	t.Parallel()

	t.Run("write does not increment", func(t *testing.T) {
		t.Parallel()

		queue := newTxStatusQueue(1)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		require.False(t, queue.write(ctx, txStatusBatch("tx-1", "tx-2")))
		require.Zero(t, queue.readyCount())
		require.Zero(t, queue.len())
	})

	t.Run("read does not decrement", func(t *testing.T) {
		t.Parallel()

		queue := newTxStatusQueue(1)
		batch := txStatusBatch("tx-1", "tx-2")
		require.True(t, queue.write(t.Context(), batch))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		readBatch, ok := queue.read(ctx)
		require.False(t, ok)
		require.Nil(t, readBatch)
		require.Equal(t, int32(2), queue.readyCount())
		require.Equal(t, 1, queue.len())
	})
}

func TestTxStatusQueueCanceledBlockedWriteRollsBackCount(t *testing.T) {
	t.Parallel()

	queue := newTxStatusQueue(1)
	require.True(t, queue.write(t.Context(), txStatusBatch("tx-1")))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan bool, 1)

	// The second write blocks because the queue is full. Its statuses are counted
	// before enqueueing so a reader cannot decrement before the writer increments.
	// Canceling the context should roll that provisional count back.
	go func() {
		done <- queue.write(ctx, txStatusBatch("tx-2", "tx-3"))
	}()

	require.Eventually(t, func() bool {
		return queue.readyCount() == 3 && queue.len() == 1
	}, time.Second, 10*time.Millisecond)

	cancel()
	require.False(t, <-done)
	require.Equal(t, int32(1), queue.readyCount())
	require.Equal(t, 1, queue.len())
}

func TestTxStatusQueueDrain(t *testing.T) {
	t.Parallel()

	t.Run("empty queue drains nothing", func(t *testing.T) {
		t.Parallel()

		queue := newTxStatusQueue(2)
		require.Zero(t, queue.drain())
		require.Zero(t, queue.readyCount())
		require.Zero(t, queue.len())
	})

	t.Run("drain removes all queued statuses and reports the count", func(t *testing.T) {
		t.Parallel()

		queue := newTxStatusQueue(2)
		require.True(t, queue.write(t.Context(), txStatusBatch("tx-1", "tx-2")))
		require.True(t, queue.write(t.Context(), txStatusBatch("tx-3", "tx-4", "tx-5")))
		require.Equal(t, int32(5), queue.readyCount())
		require.Equal(t, 2, queue.len())

		require.Equal(t, int32(5), queue.drain())
		require.Zero(t, queue.readyCount())
		require.Zero(t, queue.len())

		// Draining again is a no-op.
		require.Zero(t, queue.drain())
	})
}

func txStatusBatch(txIDs ...string) *committerpb.TxStatusBatch {
	statuses := make([]*committerpb.TxStatus, len(txIDs))
	for i, txID := range txIDs {
		statuses[i] = committerpb.NewTxStatus(committerpb.Status_COMMITTED, txID, 1, uint32(i))
	}
	return &committerpb.TxStatusBatch{Status: statuses}
}

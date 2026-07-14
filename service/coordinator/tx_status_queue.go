/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"context"
	"sync/atomic"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
)

type txStatusQueue struct {
	ch    chan *committerpb.TxStatusBatch
	count atomic.Int32
}

func newTxStatusQueue(size int) *txStatusQueue {
	return &txStatusQueue{
		ch: make(chan *committerpb.TxStatusBatch, size),
	}
}

func (q *txStatusQueue) write(ctx context.Context, batch *committerpb.TxStatusBatch) bool {
	if ctx.Err() != nil {
		return false
	}

	// Count before enqueueing so a fast reader cannot receive and decrement this
	// batch before it has been counted. If the enqueue is canceled while blocked,
	// roll the count back below.
	count := int32(len(batch.Status)) //nolint:gosec
	q.count.Add(count)
	select {
	case <-ctx.Done():
		q.count.Add(-count)
		return false
	case q.ch <- batch:
		return true
	}
}

func (q *txStatusQueue) read(ctx context.Context) (*committerpb.TxStatusBatch, bool) {
	if ctx.Err() != nil {
		return nil, false
	}

	select {
	case <-ctx.Done():
		return nil, false
	case batch := <-q.ch:
		q.count.Add(-int32(len(batch.Status))) //nolint:gosec
		return batch, true
	}
}

func (q *txStatusQueue) readyCount() int32 {
	return q.count.Load()
}

// drain removes and discards every batch currently buffered in the queue,
// decrementing the ready count accordingly, and returns the total number of
// statuses discarded. It never blocks: it stops as soon as the channel is empty.
//
// This is used only while the sidecar stream is inactive (see
// Service.NoPendingTransactionProcessing). Discarding is safe because a status
// is produced by the VC only after the transaction has been durably committed to
// the state DB, and the sidecar recovers statuses from the DB (never from this
// queue). It is also necessary: the VC keeps producing statuses into this queue
// regardless of the stream, so without a drain a large backlog fills the queue,
// blocks the VC writers, and deadlocks the idle handshake.
//
// The caller must hold streamActive so that this does not race the queue's other
// reader, sendTxStatus.
func (q *txStatusQueue) drain() int32 {
	var discarded int32
	for {
		select {
		case batch := <-q.ch:
			discarded += int32(len(batch.Status)) //nolint:gosec
		default:
			q.count.Add(-discarded)
			return discarded
		}
	}
}

func (q *txStatusQueue) len() int {
	return len(q.ch)
}

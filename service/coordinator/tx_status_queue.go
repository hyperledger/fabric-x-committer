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

// Write implements the servicemanager.ResultWriter interface so the generic
// service manager can forward VC results through this queue. Wrapping the
// status slice in a TxStatusBatch keeps the count-before-enqueue accounting
// (see readyCount) used by NoPendingTransactionProcessing.
func (q *txStatusQueue) Write(ctx context.Context, results []*committerpb.TxStatus) bool {
	return q.write(ctx, &committerpb.TxStatusBatch{Status: results})
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

func (q *txStatusQueue) len() int {
	return len(q.ch)
}

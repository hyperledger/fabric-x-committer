/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"sync"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	// SimpleManager is the simpler version of the dependency graph module.
	// It uses only 3 go routines, and a single regular map.
	SimpleManager struct {
		in                              <-chan *TransactionBatch
		out                             chan<- TxNodeBatch
		val                             <-chan TxNodeBatch
		waitingTxsLimit                 int
		preProcessedTxBatchQueue        chan TxNodeBatch
		preProcessedValidatedBatchQueue chan validatedBatch
		keyToWaitingTXs                 map[string]*waiting
		waitingTXs                      int
		metrics                         *perfMetrics
	}

	validatedBatch struct {
		txCount int
		waiting []*waiting
	}

	waiting struct {
		key   string
		queue []*waiterGroup
	}

	waiterGroup struct {
		group     []*TransactionNode
		doneCount int
		writer    bool
	}
)

// NewSimpleManager create a simple dependency graph manager.
func NewSimpleManager(p *Parameters) *SimpleManager {
	return &SimpleManager{
		in:                              p.IncomingTxs,
		out:                             p.OutgoingDepFreeTxsNode,
		val:                             p.IncomingValidatedTxsNode,
		waitingTxsLimit:                 p.WaitingTxsLimit,
		preProcessedTxBatchQueue:        make(chan TxNodeBatch, cap(p.IncomingTxs)),
		preProcessedValidatedBatchQueue: make(chan validatedBatch, cap(p.IncomingValidatedTxsNode)),
		keyToWaitingTXs:                 make(map[string]*waiting),
		metrics:                         newPerformanceMetrics(p.PrometheusMetricsProvider),
	}
}

// Run starts the dependency graph manager.
func (m *SimpleManager) Run(ctx context.Context) {
	wg := sync.WaitGroup{}
	wg.Add(3)
	go func() {
		// This manager must have a single incoming pre-processing
		// worker to maintain the original TX order.
		defer wg.Done()
		m.preProcessIn(ctx)
	}()
	go func() {
		defer wg.Done()
		m.preProcessVal(ctx)
	}()
	go func() {
		defer wg.Done()
		m.taskProcessing(ctx)
	}()
	wg.Wait()
}

// preProcessIn maps the data.
// - in  (TransactionBatch) -> preProcessedTxBatchQueue (TxNodeBatch).
func (m *SimpleManager) preProcessIn(ctx context.Context) {
	in := channel.NewReader(ctx, m.in)
	batchQueue := channel.NewWriter(ctx, m.preProcessedTxBatchQueue)
	for ctx.Err() == nil {
		batch, ok := in.Read()
		if !ok {
			return
		}
		depTX := make([]*TransactionNode, len(batch.Txs))
		for i, tx := range batch.Txs {
			node := newTransactionNode(tx)
			node.waitingKeys = make([]*waiting, 0, node.rwKeys.size())
			depTX[i] = node
		}
		batchQueue.Write(depTX)
	}
}

// preProcessVal maps the data.
// - val (TxNodeBatch)      -> preProcessedValidatedBatchQueue   (validated).
func (m *SimpleManager) preProcessVal(ctx context.Context) {
	val := channel.NewReader(ctx, m.val)
	valQueue := channel.NewWriter(ctx, m.preProcessedValidatedBatchQueue)
	for ctx.Err() == nil {
		batch, ok := val.Read()
		if !ok {
			return
		}
		var ws []*waiting
		for _, node := range batch {
			ws = append(ws, node.waitingKeys...)
		}
		valQueue.Write(validatedBatch{
			txCount: len(batch),
			waiting: ws,
		})
	}
}

// taskProcessing -- taskQueue (TxNodeBatch/keys) -> out (TxNodeBatch).
func (m *SimpleManager) taskProcessing(ctx context.Context) {
	out := channel.NewWriter(ctx, m.out)
	for ctx.Err() == nil {
		batchQueue := m.preProcessedTxBatchQueue
		if m.waitingTXs > m.waitingTxsLimit {
			// When we passed the waiting TX limit, we only fetch from the validation queue.
			batchQueue = nil
		}

		var depFree TxNodeBatch
		select {
		case <-ctx.Done():
			return
		case batch := <-batchQueue:
			depFree = m.processTxBatch(batch)
			promutil.AddToCounter(m.metrics.gdgTxProcessedTotal, len(batch))
			promutil.AddToGauge(m.metrics.dependentTransactionsQueueSize, len(batch)-len(depFree))
		case batch := <-m.preProcessedValidatedBatchQueue:
			depFree = m.processValidatedBatch(batch)
			promutil.AddToCounter(m.metrics.gdgValidatedTxProcessedTotal, batch.txCount)
			promutil.SubFromGauge(m.metrics.dependentTransactionsQueueSize, len(depFree))
		}
		promutil.SetGauge(m.metrics.gdgWaitingTxQueueSize, m.waitingTXs)
		if len(depFree) > 0 {
			out.Write(depFree)
		}
	}
}

func (m *SimpleManager) processTxBatch(batch TxNodeBatch) TxNodeBatch {
	m.waitingTXs += len(batch)
	depFree := make(TxNodeBatch, 0, len(batch))
	for _, depTX := range batch {
		// With writes.
		for _, rw := range [][]string{depTX.rwKeys.readsAndWrites, depTX.rwKeys.writesOnly} {
			for _, k := range rw {
				m.checkTXFree(depTX, k, true)
			}
		}
		// Read only.
		for _, k := range depTX.rwKeys.readsOnly {
			m.checkTXFree(depTX, k, false)
		}

		if depTX.waitForKeysCount == 0 {
			depFree = append(depFree, depTX)
		}
	}
	return depFree
}

func (m *SimpleManager) processValidatedBatch(val validatedBatch) TxNodeBatch {
	m.waitingTXs -= val.txCount
	depFree := make(TxNodeBatch, 0, len(val.waiting))
	for _, w := range val.waiting {
		depFree = m.appendFree(depFree, w)
	}
	return depFree
}

// checkTXFree check there are active TXs that are using a key, and if so, it adds the TX to thw wait queue.
//
//nolint:revive // false positive: control flag.
func (m *SimpleManager) checkTXFree(tx *TransactionNode, k string, writer bool) {
	w, loaded := m.keyToWaitingTXs[k]
	if loaded {
		if w.add(tx, writer) {
			tx.waitForKeysCount++
		}
	} else {
		w = &waiting{
			key:   k,
			queue: []*waiterGroup{{writer: writer, group: []*TransactionNode{tx}}},
		}
		m.keyToWaitingTXs[k] = w
	}
	tx.waitingKeys = append(tx.waitingKeys, w)
}

// appendFree indicate a key processing item is done, and appends free nodes if applicable.
func (m *SimpleManager) appendFree(out TxNodeBatch, w *waiting) TxNodeBatch {
	nextWaiters, noMoreWait := w.popAndGetNext()
	if noMoreWait {
		delete(m.keyToWaitingTXs, w.key)
		return out
	}

	for _, txWait := range nextWaiters {
		txWait.waitForKeysCount--
		if txWait.waitForKeysCount == 0 {
			out = append(out, txWait)
		}
	}
	return out
}

// add append to the waiting queue.
// returns true if the wait is needed.
func (w *waiting) add(tx *TransactionNode, writer bool) bool { //nolint:revive // false positive: control flag.
	sz := len(w.queue)

	// When there are no other items, or the previous item or this item is a writer,
	// we should add a new group.
	if sz == 0 || writer || w.queue[sz-1].writer {
		w.queue = append(w.queue, &waiterGroup{group: []*TransactionNode{tx}, writer: writer})
		// We can return true if the item is not the first in the queue.
		return sz != 0
	}

	// In this case, the latest group are readers and this item is also a reader.
	// We can append this reader.
	lastQueue := w.queue[sz-1]
	lastQueue.group = append(lastQueue.group, tx)
	// We can return true if the reader group is not the first one.
	return sz != 1
}

// popAndGetNext removes a TX from the waiters.
// Returns the next waiter group to release.
// Returns true if no other TX is waiting.
func (w *waiting) popAndGetNext() ([]*TransactionNode, bool) {
	prev := w.queue[0]
	prev.doneCount++
	if prev.doneCount < len(prev.group) {
		return nil, false
	}

	w.queue = w.queue[1:]
	if len(w.queue) == 0 {
		return nil, true
	}
	return w.queue[0].group, false
}

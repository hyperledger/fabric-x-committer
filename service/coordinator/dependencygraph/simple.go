/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"slices"
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

	// waiting tracks the transactions that access key: the in-flight group and the groups queued
	// behind it. The in-flight group is only a count, so released transactions are not held in memory.
	waiting struct {
		key string
		// inFlightCount is the number of transactions released on this key that are not validated
		// yet. The key is free once it reaches zero.
		inFlightCount int
		// inFlightIsWriter records whether the in-flight group is a writer, since a reader may join
		// only a group of readers.
		inFlightIsWriter bool
		// queue holds the groups waiting behind the in-flight group, in arrival order. The in-flight
		// group is not in it: popAndGetNext removes a group from the queue when it is released.
		queue []*waiterGroup
	}

	waiterGroup struct {
		group  []*TransactionNode
		writer bool
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
		metrics: newPerformanceMetrics(p.PrometheusMetricsProvider, &managerQueues{
			ldgInput: p.IncomingTxs,
		}),
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
		txCount := 0
		for _, node := range batch {
			if node.rejected {
				continue
			}
			txCount++
			ws = append(ws, node.waitingKeys...)
		}
		valQueue.Write(validatedBatch{
			txCount: txCount,
			waiting: ws,
		})
	}
}

// taskProcessing -- taskQueue (TxNodeBatch/keys) -> out (TxNodeBatch).
func (m *SimpleManager) taskProcessing(ctx context.Context) {
	// pending holds released transactions that the output channel has not taken yet.
	var pending TxNodeBatch
	for ctx.Err() == nil {
		batchQueue := m.preProcessedTxBatchQueue
		// We stop fetching new transactions when we passed the waiting TX limit, or while released
		// ones wait for the output (pending). New transactions would only grow pending while the
		// output is full, so pausing them gives the backpressure a blocking write would. The
		// validated queue stays open: consuming it is what lets the verifiers and the vcservices
		// accept our output again (see below), and anything it releases is already counted in
		// waitingTXs, so pending stays bounded by it.
		if m.waitingTXs > m.waitingTxsLimit || len(pending) > 0 {
			batchQueue = nil
		}

		// The output is a case of the select, not a blocking write, because this goroutine is also
		// the only consumer of the validated queue: blocking on a full output closes a cycle through
		// the verifiers and the vcservices, and the pipeline halts silently. So transactions released
		// below are not sent right away; they are added to pending and sent by this case in a later
		// iteration, alongside the validated queue. A nil channel disables the case when there is
		// nothing to send.
		var outQueue chan<- TxNodeBatch
		if len(pending) > 0 {
			outQueue = m.out
		}

		select {
		case <-ctx.Done():
			return
		case outQueue <- pending:
			pending = nil //nolint:prealloc // refilled by appends of unknown size.
		case batch := <-batchQueue:
			depFree := m.processTxBatch(batch)
			pending = append(pending, depFree...)
			promutil.AddToCounter(m.metrics.gdgTxProcessedTotal, len(batch))
			promutil.AddToGauge(m.metrics.dependentTxCount, len(batch)-len(depFree))
		case batch := <-m.preProcessedValidatedBatchQueue:
			depFree := m.processValidatedBatch(batch)
			pending = append(pending, depFree...)
			promutil.AddToCounter(m.metrics.gdgValidatedTxProcessedTotal, batch.txCount)
			promutil.SubFromGauge(m.metrics.dependentTxCount, len(depFree))
		}
		promutil.SetGauge(m.metrics.gdgWaitingTxCount, m.waitingTXs)
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
	// A transaction may repeat a key, e.g. a namespace lifecycle transaction writes _meta:<ns> and
	// also gets the implicit read of it. Registering it again would queue the transaction behind
	// its own group forever. Writers are registered first, so the one kept is the stricter one.
	if loaded && slices.Contains(tx.waitingKeys, w) {
		return
	}
	if loaded {
		if w.add(tx, writer) {
			tx.waitForKeysCount++
		}
	} else {
		w = &waiting{
			key:              k,
			inFlightCount:    1,
			inFlightIsWriter: writer,
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
	// A reader joins the group of readers in front of it: the in-flight group when nothing is
	// queued, so it proceeds at once, or else the last queued group. Anything else starts a group.
	if len(w.queue) == 0 {
		if !writer && !w.inFlightIsWriter {
			w.inFlightCount++
			return false
		}
	} else if last := w.queue[len(w.queue)-1]; !writer && !last.writer {
		last.group = append(last.group, tx)
		return true
	}
	w.queue = append(w.queue, &waiterGroup{group: []*TransactionNode{tx}, writer: writer})
	return true
}

// popAndGetNext removes a validated TX from the in-flight group.
// It returns nil while other TXs of the in-flight group are not validated yet. Once the last of them
// is, it releases the next queued group and returns it, or returns nil and true if none is queued,
// meaning the key is free.
func (w *waiting) popAndGetNext() ([]*TransactionNode, bool) {
	w.inFlightCount--
	if w.inFlightCount > 0 {
		return nil, false
	}
	if len(w.queue) == 0 {
		return nil, true
	}

	next := w.queue[0]
	w.queue = w.queue[1:]
	w.inFlightCount = len(next.group)
	w.inFlightIsWriter = next.writer
	return next.group, false
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	globalDependencyManager struct {
		// incomingTransactionsNode is one of the two inputs to the globalDependencyManager.
		// These transactions are added to the global dependency graph.
		// The local dependency detector sends transactions node on this channel.
		incomingTransactionsNode <-chan *transactionNodeBatch

		// outgoingDepFreeTransactionsNode is the output of the globalDependencyManager.
		// These transactions are ready to be validated as they are dependency free.
		outgoingDepFreeTransactionsNode chan<- TxNodeBatch

		// validatedTransactionsNode is the second input to the dependencyManagement.
		// These transactions are removed from the dependency graph as they are
		// validated and either committed or aborted.
		validatedTransactionsNode <-chan TxNodeBatch

		// TODO: add a text figure explaining how the input and output channels
		//       are being used.

		// dependencyDetector holds mapping between reads/writes and transactions.
		dependencyDetector *dependencyDetector

		mu sync.Mutex

		// The freedTransactionsSet holds transactions that have been released from the dependency
		// graph. They are added to this set once the pending transactions on which they depend
		// have been validated. The transactions in this set are sent on the
		// outgoingDepFreeTransactionsNode channel.
		freedTransactionsSet *dependencyFreedTransactions

		// waitingTxsSlots is employed to restrict the number of transactions awaiting validation.
		// This slot helps prevent the dependency graph from becoming too extensive. The number of
		// transactions within transactionNodeBatch (an input for the globalDependencyManager)
		// must be lower than the maximum allowed number of slots. This is not guaranteed by the
		// localDependencyConstructor, but rather by the input provided to it. Typically, the
		// maximum allowed number of slots would be considerably larger than the batch size.
		waitingTxsSlots *utils.Slots
		waitingTxsLimit int

		metrics *perfMetrics
	}

	dependencyFreedTransactions struct {
		txsNode  TxNodeBatch
		mu       sync.Mutex
		nonEmpty *atomic.Bool
		cond     *sync.Cond
	}

	globalDepConfig struct {
		incomingTxsNode        <-chan *transactionNodeBatch
		outgoingDepFreeTxsNode chan<- TxNodeBatch
		validatedTxsNode       <-chan TxNodeBatch
		waitingTxsLimit        int
		metrics                *perfMetrics
	}
)

func newGlobalDependencyManager(c *globalDepConfig) *globalDependencyManager {
	logger.Info("Initializing newGlobalDependencyManager")

	return &globalDependencyManager{
		incomingTransactionsNode:        c.incomingTxsNode,
		outgoingDepFreeTransactionsNode: c.outgoingDepFreeTxsNode,
		validatedTransactionsNode:       c.validatedTxsNode,
		dependencyDetector:              newDependencyDetector(),
		mu:                              sync.Mutex{},
		freedTransactionsSet: &dependencyFreedTransactions{
			txsNode:  TxNodeBatch{},
			mu:       sync.Mutex{},
			nonEmpty: &atomic.Bool{},
			cond:     sync.NewCond(&sync.Mutex{}),
		},
		waitingTxsSlots: utils.NewSlots(int64(c.waitingTxsLimit)),
		waitingTxsLimit: c.waitingTxsLimit,
		metrics:         c.metrics,
	}
}

func (dm *globalDependencyManager) run(ctx context.Context) {
	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		dm.constructDependencyGraph(gCtx)
		return nil
	})

	g.Go(func() error {
		logger.Debug("Starting processValidatedTransactions")
		dm.processValidatedTransactions(gCtx)
		return nil
	})

	g.Go(func() error {
		logger.Debug("Starting outputFreedExistingTransactions")
		dm.outputFreedExistingTransactions(gCtx)
		return nil
	})

	_ = g.Wait()
}

func (dm *globalDependencyManager) constructDependencyGraph(ctx context.Context) {
	done := context.AfterFunc(ctx, dm.waitingTxsSlots.Broadcast)
	defer done()
	m := dm.metrics
	var txsNode TxNodeBatch
	incomingTransactionsNode := channel.NewReader(ctx, dm.incomingTransactionsNode)
	outgoingDepFreeTransactionsNode := channel.NewWriter(ctx, dm.outgoingDepFreeTransactionsNode)
	for {
		txsNodeBatch, ok := incomingTransactionsNode.Read()
		if !ok {
			return
		}

		constructionStart := time.Now()

		txsNode = txsNodeBatch.txsNode
		dm.waitingTxsSlots.Acquire(ctx, int64(len(txsNode)))
		if ctx.Err() != nil {
			return
		}

		promutil.AddToGauge(m.gdgWaitingTxQueueSize, len(txsNode))
		depFreeTxs := make(TxNodeBatch, 0, len(txsNode))

		start := time.Now()
		dm.mu.Lock()
		promutil.Observe(m.gdgConstructorWaitForLockSeconds, time.Since(start))

		// Step 1: Detect dependencies of each transaction with the transactions
		//         that are already in the dependency graph. After detection,
		//         the dependencies are added to the transactionNode. If the
		//         transaction has no dependencies, it is added to the depFreeTxs
		//         slice.
		start = time.Now()
		for _, txNode := range txsNode {
			dependsOnTx := dm.dependencyDetector.getDependenciesOf(txNode)
			if len(dependsOnTx) > 0 {
				promutil.AddToGauge(m.dependentTransactionsQueueSize, 1)
				txNode.addDependenciesAndUpdateDependents(dependsOnTx)
			} else if len(txNode.dependsOnTxs) == 0 {
				depFreeTxs = append(depFreeTxs, txNode)
			}
		}
		promutil.Observe(m.gdgAddTxToGraphSeconds, time.Since(start))

		// Step 2: Add reads and writes of each input transaction to the global dependency
		// 	       detector so that they can be used to detect correct dependencies
		// 		   for future input transactions. As the local dependency detector
		//         already has the reads and writes in required format, we are just
		// 	       merging it with the global dependency detector.
		start = time.Now()
		dm.dependencyDetector.mergeWaitingTx(txsNodeBatch.localDepDetector)
		promutil.Observe(m.gdgUpdateDependencyDetectorSeconds, time.Since(start))

		dm.mu.Unlock()

		// Step 3: Send the transactions that are free of dependencies to the
		// 	       output channel.
		if len(depFreeTxs) > 0 {
			outgoingDepFreeTransactionsNode.Write(depFreeTxs)
		}
		promutil.AddToCounter(m.gdgTxProcessedTotal, len(txsNode))
		promutil.Observe(m.gdgConstructionSeconds, time.Since(constructionStart))
	}
}

func (dm *globalDependencyManager) processValidatedTransactions(ctx context.Context) {
	m := dm.metrics
	var fullyFreedDependents TxNodeBatch
	validatedTransactionsNode := channel.NewReader(ctx, dm.validatedTransactionsNode)
	for {
		txsNode, ok := validatedTransactionsNode.Read()
		if !ok {
			return
		}
		processValidatedStart := time.Now()
		dm.waitingTxsSlots.Release(int64(len(txsNode)))
		promutil.SubFromGauge(m.gdgWaitingTxQueueSize, len(txsNode))

		start := time.Now()
		dm.mu.Lock()
		promutil.Observe(m.gdgValidatedTxProcessorWaitForLockSeconds, time.Since(start))

		// Step 1: Remove the validated transactions from the dependency graph.
		//         When a transaction becomes free of dependencies, it is added
		// 	       to the fullyFreedDependents list so that it can be sent to
		//         the outgoingDepFreeTransactionsNode.
		start = time.Now()
		for _, txNode := range txsNode {
			fullyFreedDependents = append(fullyFreedDependents, txNode.freeDependents()...)
		}
		dm.dependencyDetector.removeWaitingTx(txsNode)
		promutil.Observe(m.gdgRemoveDependentsOfValidatedTxSeconds, time.Since(start))
		dm.mu.Unlock()

		// Step 2: Send the fullyFreedDependents to the outgoingDepFreeTransactionsNode.
		start = time.Now()
		if len(fullyFreedDependents) > 0 {
			promutil.SubFromGauge(m.dependentTransactionsQueueSize, len(fullyFreedDependents))
			dm.freedTransactionsSet.add(fullyFreedDependents)
			fullyFreedDependents = nil
		}
		promutil.Observe(m.gdgAddFreedTxSeconds, time.Since(start))

		promutil.AddToCounter(dm.metrics.gdgValidatedTxProcessedTotal, len(txsNode))
		promutil.Observe(m.gdgValidatedTxProcessingSeconds, time.Since(processValidatedStart))
	}
}

func (dm *globalDependencyManager) outputFreedExistingTransactions(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() {
		dm.freedTransactionsSet.nonEmpty.Store(true)
		dm.freedTransactionsSet.cond.Signal()
	})
	defer stop()

	for ctx.Err() == nil {
		start := time.Now()
		txsNode := dm.freedTransactionsSet.waitAndRemove()

		if len(txsNode) > 0 {
			dm.outgoingDepFreeTransactionsNode <- txsNode
		}
		promutil.Observe(dm.metrics.gdgOutputFreedTxSeconds, time.Since(start))
	}
}

func (f *dependencyFreedTransactions) add(txsNode TxNodeBatch) {
	f.mu.Lock()
	f.txsNode = append(
		f.txsNode,
		txsNode...,
	)
	f.mu.Unlock()

	f.nonEmpty.Store(true)
	f.cond.Signal()
}

func (f *dependencyFreedTransactions) waitAndRemove() TxNodeBatch {
	f.cond.L.Lock()
	for !f.nonEmpty.CompareAndSwap(true, false) {
		f.cond.Wait()
	}
	f.cond.L.Unlock()

	// NOTE: We have changed the nonEmpty to false before fetching txsNode
	//       and making the list empty. By the time, we remove the txsNode
	//       from the list, the nonEmpty might have been set to true by
	//       processValidatedTransactions. As a result, the next call to
	//       waitAndRemove may return an empty list. This is fine as long
	//       as the caller is able to handle empty list.
	f.mu.Lock()
	txsNode := f.txsNode
	f.txsNode = nil
	f.mu.Unlock()

	return txsNode
}

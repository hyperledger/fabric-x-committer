/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type globalDependencyTestEnv struct {
	incomingTxs  chan *transactionNodeBatch
	outgoingTxs  chan TxNodeBatch
	validatedTxs chan TxNodeBatch
	metrics      *perfMetrics
	dm           *globalDependencyManager
}

func newGlobalDependencyTestEnv(t *testing.T) *globalDependencyTestEnv {
	t.Helper()
	env := &globalDependencyTestEnv{
		incomingTxs:  make(chan *transactionNodeBatch, 10),
		outgoingTxs:  make(chan TxNodeBatch, 10),
		validatedTxs: make(chan TxNodeBatch, 10),
		metrics:      newPerformanceMetrics(monitoring.NewProvider()),
	}

	dm := newGlobalDependencyManager(
		&globalDepConfig{
			incomingTxsNode:        env.incomingTxs,
			outgoingDepFreeTxsNode: env.outgoingTxs,
			validatedTxsNode:       env.validatedTxs,
			waitingTxsLimit:        10,
			metrics:                env.metrics,
		},
	)

	env.dm = dm
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		dm.run(ctx)
		return nil
	}, nil)
	return env
}

func TestGlobalDependencyManagerDependencyFreeTxs(t *testing.T) {
	t.Parallel()
	env := newGlobalDependencyTestEnv(t)
	keys := makeTestKeys(t, 10)

	t.Run("dependency free txs", func(t *testing.T) {
		noDepsTxs := TxNodeBatch{
			createTxNode(t, [][]byte{keys[0]}, [][]byte{keys[1]}, [][]byte{keys[2]}),
			createTxNode(t, [][]byte{keys[3]}, [][]byte{keys[4]}, [][]byte{keys[5]}),
			createTxNode(t, [][]byte{keys[6]}, [][]byte{keys[7]}, [][]byte{keys[8]}),
		}

		env.incomingTxs <- createTxsNodeBatch(t, noDepsTxs)
		depFreeTxs := <-env.outgoingTxs
		// as there are no dependencies, all three transactions should be returned
		require.Equal(t, noDepsTxs, depFreeTxs)
		env.validatedTxs <- depFreeTxs
		// once we validate the transactions, the dependency manager should remove
		// the reads and writes performed by these transactions from the dependency detector
		// so that the new transaction won't get a wrong dependency.
		ensureProcessedAndValidatedMetrics(t, env.metrics, 3, 3)
		ensureEmptyDetector(t, env.dm.dependencyDetector)
	})
}

func TestGlobalDependencyManagerNoGlobalDependency(t *testing.T) {
	t.Parallel()
	env := newGlobalDependencyTestEnv(t)
	keys := makeTestKeys(t, 10)

	t.Run("local dependency but no global dependency", func(t *testing.T) {
		// t3 depends on t2 depends on t1
		t1 := createTxNode(t, [][]byte{keys[0]}, [][]byte{keys[1]}, nil)
		t2 := createTxNode(t, [][]byte{keys[1]}, [][]byte{keys[2]}, nil)
		t3 := createTxNode(t, [][]byte{keys[2]}, [][]byte{keys[3]}, nil)

		t2.dependsOnTxs = append(t2.dependsOnTxs, t1)
		t1.dependentTxs.Store(t2, nil)

		t3.dependsOnTxs = append(t3.dependsOnTxs, t2)
		t2.dependentTxs.Store(t3, nil)

		env.incomingTxs <- createTxsNodeBatch(t, TxNodeBatch{t1, t2, t3})
		depFreeTxs := <-env.outgoingTxs
		// only dependency free tx is t1
		require.Equal(t, TxNodeBatch{t1}, depFreeTxs)

		test.RequireIntMetricValue(t, 3, env.metrics.gdgWaitingTxQueueSize)

		env.validatedTxs <- TxNodeBatch{t1}

		test.EventuallyIntMetric(t, 2, env.metrics.gdgWaitingTxQueueSize, 2*time.Second, 200*time.Millisecond)

		depFreeTxs = <-env.outgoingTxs
		// after validating t1, t2 becomes dependency free
		require.Equal(t, TxNodeBatch{t2}, depFreeTxs)
		require.Empty(t, t2.dependsOnTxs)

		env.validatedTxs <- TxNodeBatch{t2}
		depFreeTxs = <-env.outgoingTxs
		// after validating t2, t3 becomes dependency free
		require.Equal(t, TxNodeBatch{t3}, depFreeTxs)
		require.Empty(t, t3.dependsOnTxs)

		env.validatedTxs <- TxNodeBatch{t3}

		ensureProcessedAndValidatedMetrics(t, env.metrics, 3, 3)
		// after validating t3, there is no more txs
		ensureEmptyDetector(t, env.dm.dependencyDetector)

		// as we are not updating the dependentsTxs of validated txs,
		// the number of dependents is non-zero for t1.
		require.Equal(t, 1, t1.dependentTxs.Count())
	})
}

func TestGlobalDependencyManagerBothLocalAndGlobalDependency(t *testing.T) {
	t.Parallel()
	env := newGlobalDependencyTestEnv(t)
	keys := makeTestKeys(t, 10)

	t.Run("both local and global dependency", func(t *testing.T) {
		// t2 depends on t1
		t1 := createTxNode(t, [][]byte{keys[0], keys[1]}, [][]byte{keys[2], keys[3]}, [][]byte{keys[4], keys[5]})
		t2 := createTxNode(t, [][]byte{keys[4], keys[5]}, [][]byte{keys[2], keys[6]}, [][]byte{keys[3], keys[7]})

		t2.dependsOnTxs = append(t2.dependsOnTxs, t1)
		t1.dependentTxs.Store(t2, nil)

		env.incomingTxs <- createTxsNodeBatch(t, TxNodeBatch{t1, t2})

		// t3 depends on t2 and t1
		t3 := createTxNode(t, [][]byte{keys[7], keys[3]}, [][]byte{keys[2], keys[3]}, [][]byte{keys[8], keys[5]})
		// t4 depends on t2 and t1
		t4 := createTxNode(t, [][]byte{keys[7], keys[6]}, [][]byte{keys[4], keys[1]}, [][]byte{keys[0], keys[9]})

		env.incomingTxs <- createTxsNodeBatch(t, TxNodeBatch{t3, t4})

		// only t1 is dependency free
		depFreeTxs := <-env.outgoingTxs
		require.Equal(t, TxNodeBatch{t1}, depFreeTxs)

		// t1 has three dependents: t2, t3, and t4
		require.Eventually(t, func() bool {
			return t1.dependentTxs.Count() == 3
		}, 2*time.Second, 200*time.Millisecond)
		for _, txNode := range []*TransactionNode{t2, t3, t4} {
			_, exist := t1.dependentTxs.Load(txNode)
			require.True(t, exist)
		}

		env.validatedTxs <- TxNodeBatch{t1}

		// after validating t1, t2 becomes dependency free
		depFreeTxs = <-env.outgoingTxs
		require.Equal(t, TxNodeBatch{t2}, depFreeTxs)

		// t2 has two dependents: t3 and t4
		require.Equal(t, 2, t2.dependentTxs.Count())
		for _, txNode := range []*TransactionNode{t3, t4} {
			_, exist := t2.dependentTxs.Load(txNode)
			require.True(t, exist)
		}

		env.validatedTxs <- TxNodeBatch{t2}

		// after validating t2, both t3 and t4 become dependency free
		depFreeTxs = <-env.outgoingTxs
		require.Len(t, depFreeTxs, 2)
		require.ElementsMatch(t, TxNodeBatch{t3, t4}, depFreeTxs)

		env.validatedTxs <- TxNodeBatch{t3, t4}

		ensureProcessedAndValidatedMetrics(t, env.metrics, 4, 4)
		// after validating t3 and t4, there is no more txs
		ensureEmptyDetector(t, env.dm.dependencyDetector)
	})
}

func TestGlobalDependencyManagerWithLimit(t *testing.T) {
	t.Parallel()
	env := newGlobalDependencyTestEnv(t)
	keys := makeTestKeys(t, 10)

	t.Run("both local and global dependency with waiting due to limit", func(t *testing.T) {
		env.dm.waitingTxsSlots.Store(t, 2)
		// t2 depends on t1
		t1 := createTxNode(t, [][]byte{keys[0], keys[1]}, [][]byte{keys[2], keys[3]}, [][]byte{keys[4], keys[5]})
		t2 := createTxNode(t, [][]byte{keys[4], keys[5]}, [][]byte{keys[2], keys[6]}, [][]byte{keys[3], keys[7]})

		t2.dependsOnTxs = append(t2.dependsOnTxs, t1)
		t1.dependentTxs.Store(t2, struct{}{})

		env.incomingTxs <- createTxsNodeBatch(t, []*TransactionNode{t1, t2})

		// t3 depends on t2 and t1
		t3 := createTxNode(t, [][]byte{keys[7], keys[3]}, [][]byte{keys[2], keys[3]}, [][]byte{keys[8], keys[5]})

		env.incomingTxs <- createTxsNodeBatch(t, []*TransactionNode{t3})

		// only t1 is dependency free
		depFreeTxs := <-env.outgoingTxs
		require.Equal(t, TxNodeBatch{t1}, depFreeTxs)

		// t1 has two dependents: t2, and t3 but t2 is waiting due to the limit and not processed yet.
		// Hence, t1 should have only one dependent which is t2.
		require.Eventually(t, func() bool {
			return t1.dependentTxs.Count() == 1
		}, 2*time.Second, 200*time.Millisecond)
		_, exist := t1.dependentTxs.Load(t2)
		require.True(t, exist)

		env.validatedTxs <- []*TransactionNode{t1}

		// after validating t1, t2 becomes dependency free
		depFreeTxs = <-env.outgoingTxs
		require.Equal(t, TxNodeBatch{t2}, depFreeTxs)

		// t2 has one dependent: t3. As t3 was waiting due to the limit, it might not have been added to
		// the dependency graph yet. However, now, t3 should not be waiting given t1 is removed. Hence,
		// we need to wait until t3 is added to the dependency graph.
		require.Eventually(t, func() bool {
			return t2.dependentTxs.Count() == 1
		}, 2*time.Second, 200*time.Millisecond)

		env.validatedTxs <- []*TransactionNode{t2}

		// after validating t2, t2 becomes dependency free
		// as t1 and t2 are already validated and removed.
		depFreeTxs = <-env.outgoingTxs
		require.Len(t, depFreeTxs, 1)
		require.ElementsMatch(t, []*TransactionNode{t3}, depFreeTxs)

		env.validatedTxs <- []*TransactionNode{t3}

		ensureProcessedAndValidatedMetrics(t, env.metrics, 3, 3)
		// after validating t3, there is no more txs
		ensureEmptyDetector(t, env.dm.dependencyDetector)
	})
}

func createTxsNodeBatch(_ *testing.T, txsNode TxNodeBatch) *transactionNodeBatch {
	localDepDetect := newDependencyDetector()
	for _, tx := range txsNode {
		localDepDetect.addWaitingTx(tx)
	}

	return &transactionNodeBatch{
		txsNode:          txsNode,
		localDepDetector: localDepDetect,
	}
}

func ensureProcessedAndValidatedMetrics(t *testing.T, metrics *perfMetrics, processed, validated int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, metrics.gdgTxProcessedTotal) == processed &&
			test.GetIntMetricValue(t, metrics.gdgValidatedTxProcessedTotal) == validated
	}, 2*time.Second, 200*time.Millisecond)
}

func ensureEmptyDetector(t *testing.T, d *dependencyDetector) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(d.readOnlyKeyToWaitingTxs) == 0 && len(d.writeOnlyKeyToWaitingTxs) == 0 &&
			len(d.readWriteKeyToWaitingTxs) == 0
	}, 2*time.Second, 100*time.Millisecond)
}

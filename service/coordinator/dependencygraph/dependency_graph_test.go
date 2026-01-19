/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestDependencyGraph(t *testing.T) {
	t.Parallel()

	localDepIncomingTxs := make(chan *TransactionBatch, 10)
	localDepOutgoingTxs := make(chan *transactionNodeBatch, 10)

	metrics := newPerformanceMetrics(monitoring.NewProvider())
	ldc := newLocalDependencyConstructor(localDepIncomingTxs, localDepOutgoingTxs, metrics)

	globalDepOutgoingTxs := make(chan TxNodeBatch, 10)
	validatedTxs := make(chan TxNodeBatch, 10)
	dm := newGlobalDependencyManager(
		&globalDepConfig{
			incomingTxsNode:        localDepOutgoingTxs,
			outgoingDepFreeTxsNode: globalDepOutgoingTxs,
			validatedTxsNode:       validatedTxs,
			waitingTxsLimit:        20,
			metrics:                metrics,
		},
	)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		ldc.run(ctx, 2)
		return nil
	}, nil)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		dm.run(ctx)
		return nil
	}, nil)

	t.Log("check reads and writes dependency tracking")
	keys := makeTestKeys(t, 10)

	test.RequireIntMetricValue(t, 0, metrics.dependentTransactionsQueueSize)

	// t2 depends on t1
	t1 := createTxForTest(
		t, 0, nsID1ForTest, [][]byte{keys[0], keys[1]}, [][]byte{keys[2], keys[3]}, [][]byte{keys[4], keys[5]},
	)
	t2 := createTxForTest(
		t, 1, nsID1ForTest, [][]byte{keys[4], keys[5]}, [][]byte{keys[2], keys[6]}, [][]byte{keys[3], keys[7]},
	)

	localDepIncomingTxs <- &TransactionBatch{
		ID:  1,
		Txs: []*servicepb.TxWithRef{t1, t2},
	}

	test.EventuallyIntMetric(t, 1, metrics.dependentTransactionsQueueSize, 5*time.Second, 100*time.Millisecond)

	// t3 depends on t2 and t1
	t3 := createTxForTest(
		t, 0, nsID1ForTest, [][]byte{keys[7], keys[3]}, [][]byte{keys[2], keys[3]}, [][]byte{keys[8], keys[5]},
	)
	// t4 depends on t2 and t1
	t4 := createTxForTest(
		t, 1, nsID1ForTest, [][]byte{keys[7], keys[6]}, [][]byte{keys[4], keys[1]}, [][]byte{keys[0], keys[9]},
	)

	localDepIncomingTxs <- &TransactionBatch{
		ID:  2,
		Txs: []*servicepb.TxWithRef{t3, t4},
	}

	test.EventuallyIntMetric(t, 3, metrics.dependentTransactionsQueueSize, 5*time.Second, 100*time.Millisecond)

	// only t1 is dependency free
	depFreeTxs := <-globalDepOutgoingTxs
	require.Len(t, depFreeTxs, 1)
	actualT1 := depFreeTxs[0]
	test.RequireProtoEqual(t, t1.Ref, actualT1.Tx.Ref)

	// t1 has 3 dependent transactions, t2, t3, and t4
	require.Eventually(t, func() bool {
		return actualT1.dependentTxs.Count() == 3
	}, 2*time.Second, 200*time.Millisecond)

	validatedTxs <- TxNodeBatch{actualT1}

	// after t1 is validated, t2 is dependency free
	depFreeTxs = <-globalDepOutgoingTxs
	require.Len(t, depFreeTxs, 1)
	actualT2 := depFreeTxs[0]
	test.RequireProtoEqual(t, t2.Ref, actualT2.Tx.Ref)

	test.RequireIntMetricValue(t, 2, metrics.dependentTransactionsQueueSize)

	// t2 has 2 dependent transactions, t3 and t4
	require.Equal(t, 2, actualT2.dependentTxs.Count())

	validatedTxs <- TxNodeBatch{actualT2}

	// after t2 is validated, both t3 and t4 are dependency free
	depFreeTxs = <-globalDepOutgoingTxs
	require.Len(t, depFreeTxs, 2)
	var actualT3, actualT4 *TransactionNode
	if t3.Ref.TxId == depFreeTxs[0].Tx.Ref.TxId {
		actualT3 = depFreeTxs[0]
		actualT4 = depFreeTxs[1]
	} else {
		actualT3 = depFreeTxs[1]
		actualT4 = depFreeTxs[0]
	}
	test.RequireProtoEqual(t, t3.Ref, actualT3.Tx.Ref)
	test.RequireProtoEqual(t, t4.Ref, actualT4.Tx.Ref)

	test.RequireIntMetricValue(t, 0, metrics.dependentTransactionsQueueSize)

	validatedTxs <- TxNodeBatch{actualT3, actualT4}

	ensureProcessedAndValidatedMetrics(t, metrics, 4, 4)
	// after validating all txs, the dependency detector should be empty
	ensureEmptyDetector(t, dm.dependencyDetector)

	t.Log("check dependency in namespace")
	keys = makeTestKeys(t, 10)
	// t2 depends on t1
	t1 = createTxForTest(
		t, 1, committerpb.MetaNamespaceID, nil, [][]byte{[]byte(nsID1ForTest)}, nil,
	)
	t2 = createTxForTest(
		t, 2, nsID1ForTest, [][]byte{keys[4], keys[5]}, [][]byte{keys[2], keys[6]}, [][]byte{keys[3], keys[7]},
	)

	localDepIncomingTxs <- &TransactionBatch{
		ID:  3,
		Txs: []*servicepb.TxWithRef{t1, t2},
	}

	// t3 depends on t2, and t1
	t3 = createTxForTest(
		t, 0, committerpb.MetaNamespaceID, nil, [][]byte{[]byte(nsID1ForTest)}, nil,
	)
	// t4 depends on t3, t2 and t1
	t4 = createTxForTest(
		t, 1, nsID1ForTest, [][]byte{keys[7], keys[6]}, [][]byte{keys[4], keys[1]}, [][]byte{keys[0], keys[9]},
	)

	localDepIncomingTxs <- &TransactionBatch{
		ID:  4,
		Txs: []*servicepb.TxWithRef{t3, t4},
	}

	// only t1 is dependency free
	depFreeTxs = <-globalDepOutgoingTxs
	require.Len(t, depFreeTxs, 1)
	actualT1 = depFreeTxs[0]
	test.RequireProtoEqual(t, t1.Ref, actualT1.Tx.Ref)

	// t1 has 3 dependent transactions: t2, t3 and t4
	require.Eventually(t, func() bool {
		return actualT1.dependentTxs.Count() == 3
	}, 2*time.Second, 200*time.Millisecond)

	validatedTxs <- TxNodeBatch{actualT1}

	// only t2 is dependency free
	depFreeTxs = <-globalDepOutgoingTxs
	require.Len(t, depFreeTxs, 1)
	actualT2 = depFreeTxs[0]
	test.RequireProtoEqual(t, t2.Ref, actualT2.Tx.Ref)

	// t2 has 2 dependent transactions, t3 and t4
	require.Equal(t, 2, actualT2.dependentTxs.Count())

	validatedTxs <- TxNodeBatch{actualT2}

	// after t2 is validated, t3 becomes dependency free
	depFreeTxs = <-globalDepOutgoingTxs
	require.Len(t, depFreeTxs, 1)
	actualT3 = depFreeTxs[0]
	test.RequireProtoEqual(t, t3.Ref, actualT3.Tx.Ref)

	validatedTxs <- TxNodeBatch{actualT3}

	// after t3 is validated, t4 becomes dependency free
	depFreeTxs = <-globalDepOutgoingTxs
	require.Len(t, depFreeTxs, 1)
	actualT4 = depFreeTxs[0]
	test.RequireProtoEqual(t, t4.Ref, actualT4.Tx.Ref)

	validatedTxs <- TxNodeBatch{actualT4}

	ensureProcessedAndValidatedMetrics(t, metrics, 8, 8)
	// after validating all txs, the dependency detector should be empty
	ensureEmptyDetector(t, dm.dependencyDetector)
}

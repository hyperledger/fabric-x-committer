/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type localDependencyConstructorTestEnv struct {
	inComingTxs chan *TransactionBatch
	outGoingTxs chan *transactionNodeBatch
	metrics     *perfMetrics
}

func newLocalDependencyConstructorTestEnv(t *testing.T) *localDependencyConstructorTestEnv {
	t.Helper()
	inComingTxs := make(chan *TransactionBatch, 5)
	outGoingTxs := make(chan *transactionNodeBatch, 5)

	metrics := newPerformanceMetrics(monitoring.NewProvider())
	ldc := newLocalDependencyConstructor(inComingTxs, outGoingTxs, metrics)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		ldc.run(ctx, 5)
		return nil
	}, nil)

	return &localDependencyConstructorTestEnv{
		inComingTxs: inComingTxs,
		outGoingTxs: outGoingTxs,
		metrics:     metrics,
	}
}

func TestLocalDependencyConstructorWithDependencies(t *testing.T) { //nolint:gocognit
	t.Parallel()

	keys := makeTestKeys(t, 24)

	t.Run("no dependencies between transactions", func(t *testing.T) {
		t.Parallel()
		env := newLocalDependencyConstructorTestEnv(t)
		noDepsTxs := &TransactionBatch{
			ID: 1,
			Txs: []*protocoordinatorservice.Tx{
				createTxForTest(
					t,
					0,
					nsID1ForTest,
					[][]byte{keys[0], keys[1]},
					[][]byte{keys[2], keys[3]},
					[][]byte{keys[4], keys[5]},
				),
				createTxForTest(
					t,
					1,
					nsID1ForTest,
					[][]byte{keys[6], keys[7]},
					[][]byte{keys[8], keys[9]},
					[][]byte{keys[10], keys[11]},
				),
				createTxForTest(
					t,
					2,
					nsID1ForTest,
					[][]byte{keys[12], keys[13]},
					[][]byte{keys[14], keys[15]},
					[][]byte{keys[16], keys[17]},
				),
				createTxForTest(
					t,
					3,
					nsID1ForTest,
					[][]byte{keys[18], keys[19]},
					[][]byte{keys[20], keys[21]},
					[][]byte{keys[22], keys[23]},
				),
				createTxForTest(
					t,
					4,
					committerpb.MetaNamespaceID,
					nil,
					[][]byte{[]byte("2")},
					nil,
				),
			},
		}
		env.inComingTxs <- noDepsTxs

		txsNodeBatch := <-env.outGoingTxs
		txsNode := txsNodeBatch.txsNode
		require.Len(t, txsNode, 5)
		for _, txNode := range txsNode {
			// as there are no dependencies, both the dependsOnTxs and dependents list should be empty
			require.Empty(t, txNode.dependsOnTxs)
			require.Equal(t, 0, txNode.dependentTxs.Count())
		}

		test.EventuallyIntMetric(t, 5, env.metrics.ldgTxProcessedTotal, 2*time.Second, 200*time.Millisecond)
		test.RequireIntMetricValue(t, 0, env.metrics.dependentTransactionsQueueSize)
	})

	t.Run("linear dependency i and i+1 transaction", func(t *testing.T) {
		t.Parallel()
		env := newLocalDependencyConstructorTestEnv(t)
		noDepsTxs := &TransactionBatch{
			ID: 1,
			Txs: []*protocoordinatorservice.Tx{
				createTxForTest(t, 0, nsID1ForTest, [][]byte{keys[1]}, [][]byte{keys[2]}, nil),
				createTxForTest(t, 1, nsID1ForTest, [][]byte{keys[2]}, [][]byte{keys[3]}, nil),
				createTxForTest(t, 2, nsID1ForTest, [][]byte{keys[3]}, [][]byte{keys[4]}, nil),
				createTxForTest(t, 3, nsID1ForTest, [][]byte{keys[4]}, [][]byte{keys[5]}, nil),
			},
		}
		env.inComingTxs <- noDepsTxs

		txsNodeBatch := <-env.outGoingTxs
		txsNode := txsNodeBatch.txsNode
		require.Len(t, txsNode, 4)
		for i, txNode := range txsNode {
			switch i {
			case 0:
				// tx1 should not have any dependencies
				require.Empty(t, txNode.dependsOnTxs)
			default:
				// other transactions should be dependent on the previous transaction
				require.Len(t, txNode.dependsOnTxs, 1)
				require.Equal(t, TxNodeBatch{txsNode[i-1]}, txNode.dependsOnTxs)
			}

			if i == len(txsNode)-1 {
				// last transaction should not have any dependents
				require.Equal(t, 0, txNode.dependentTxs.Count())
			} else {
				// other transactions should have the next transaction as dependent
				require.Equal(t, 1, txNode.dependentTxs.Count())
				_, exist := txNode.dependentTxs.Load(txsNode[i+1])
				require.True(t, exist)
			}
		}
		test.RequireIntMetricValue(t, 3, env.metrics.dependentTransactionsQueueSize)
	})

	t.Run("all txs depends on the metaNamespace tx", func(t *testing.T) {
		t.Parallel()
		env := newLocalDependencyConstructorTestEnv(t)
		noDepsTxs := &TransactionBatch{
			ID: 1,
			Txs: []*protocoordinatorservice.Tx{
				createTxForTest(
					t, 0, committerpb.MetaNamespaceID, nil, [][]byte{[]byte(nsID1ForTest)}, nil,
				),
				createTxForTest(t, 1, nsID1ForTest, [][]byte{keys[2]}, nil, nil),
				createTxForTest(t, 2, nsID1ForTest, [][]byte{keys[3]}, nil, nil),
				createTxForTest(t, 3, nsID1ForTest, [][]byte{keys[4]}, nil, nil),
			},
		}
		env.inComingTxs <- noDepsTxs

		txsNodeBatch := <-env.outGoingTxs
		txsNode := txsNodeBatch.txsNode
		require.Len(t, txsNode, 4)
		for i, txNode := range txsNode {
			switch i {
			case 0:
				// tx1 should not have any dependencies
				require.Empty(t, txNode.dependsOnTxs)
			default:
				// other transactions should be dependent on the first transaction
				require.Len(t, txNode.dependsOnTxs, 1)
				require.Equal(t, TxNodeBatch{txsNode[0]}, txNode.dependsOnTxs)
			}
		}
		test.RequireIntMetricValue(t, 3, env.metrics.dependentTransactionsQueueSize)
	})

	t.Run("metaNamespace tx depends on all other txs", func(t *testing.T) {
		t.Parallel()
		env := newLocalDependencyConstructorTestEnv(t)
		noDepsTxs := &TransactionBatch{
			ID: 1,
			Txs: []*protocoordinatorservice.Tx{
				createTxForTest(t, 0, nsID1ForTest, [][]byte{keys[2]}, nil, nil),
				createTxForTest(t, 1, nsID1ForTest, [][]byte{keys[3]}, nil, nil),
				createTxForTest(t, 2, nsID1ForTest, [][]byte{keys[4]}, nil, nil),
				createTxForTest(
					t, 3, committerpb.MetaNamespaceID, nil, [][]byte{[]byte(nsID1ForTest)}, nil,
				),
			},
		}
		env.inComingTxs <- noDepsTxs

		txsNodeBatch := <-env.outGoingTxs
		txsNode := txsNodeBatch.txsNode
		require.Len(t, txsNode, 4)
		lastElementIndex := len(txsNode) - 1
		for i, txNode := range txsNode {
			switch i {
			case lastElementIndex:
				// last transaction, i.e., metaNamespace tx should be dependent on all other transactions.
				require.Len(t, txNode.dependsOnTxs, lastElementIndex)
				require.ElementsMatch(t, txsNode[0:lastElementIndex], txNode.dependsOnTxs)
			default:
				// other transactions should not have any dependencies
				require.Empty(t, txNode.dependsOnTxs)
			}
		}
		test.RequireIntMetricValue(t, 1, env.metrics.dependentTransactionsQueueSize)
	})
}

func TestLocalDependencyConstructorWithOrder(t *testing.T) {
	t.Parallel()

	env := newLocalDependencyConstructorTestEnv(t)

	keys := makeTestKeys(t, 9)

	// send the transactions in reverse order
	noDepsTxs := &TransactionBatch{
		ID: 3,
		Txs: []*protocoordinatorservice.Tx{
			createTxForTest(t, 0, nsID1ForTest, nil, [][]byte{keys[4]}, nil),
			createTxForTest(t, 1, nsID1ForTest, nil, [][]byte{keys[5]}, nil),
			createTxForTest(t, 2, nsID1ForTest, nil, [][]byte{keys[6]}, nil),
			createTxForTest(t, 3, nsID1ForTest, nil, [][]byte{keys[7]}, nil),
			createTxForTest(t, 4, nsID1ForTest, nil, [][]byte{keys[8]}, nil),
		},
	}
	env.inComingTxs <- noDepsTxs

	noDepsTxs = &TransactionBatch{
		ID: 2,
		Txs: []*protocoordinatorservice.Tx{
			createTxForTest(t, 0, nsID1ForTest, nil, [][]byte{keys[1]}, nil),
			createTxForTest(t, 1, nsID1ForTest, nil, [][]byte{keys[2]}, nil),
			createTxForTest(t, 2, nsID1ForTest, nil, [][]byte{keys[3]}, nil),
		},
	}
	env.inComingTxs <- noDepsTxs

	select {
	case txsNode := <-env.outGoingTxs:
		t.Fatal("should not have received txsNode: %w", txsNode)
	case <-time.After(1 * time.Second):
	}

	noDepsTxs = &TransactionBatch{
		ID: 1,
		Txs: []*protocoordinatorservice.Tx{
			createTxForTest(t, 0, nsID1ForTest, nil, [][]byte{keys[0]}, nil),
		},
	}
	env.inComingTxs <- noDepsTxs

	// id 1, 2, 3 should be received in order though we have sent id 3, 2, 1 in the order
	tests := []struct {
		txsNodeBatch *transactionNodeBatch
		len          int
	}{
		{
			txsNodeBatch: <-env.outGoingTxs,
			len:          1,
		},
		{
			txsNodeBatch: <-env.outGoingTxs,
			len:          3,
		},
		{
			txsNodeBatch: <-env.outGoingTxs,
			len:          5,
		},
	}

	for _, tc := range tests {
		txsNode := tc.txsNodeBatch.txsNode
		require.Len(t, txsNode, tc.len)
		for _, txNode := range txsNode {
			require.Empty(t, txNode.dependsOnTxs)
			require.Equal(t, 0, txNode.dependentTxs.Count())
		}
	}
}

func makeTestKeys(_ *testing.T, numKeys int) [][]byte {
	keys := make([][]byte, numKeys)
	for i := range numKeys {
		keys[i] = fmt.Appendf(nil, "%d", i)
	}
	return keys
}

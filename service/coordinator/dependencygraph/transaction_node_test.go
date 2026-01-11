/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

var nsID1ForTest = "1"

func TestTransactionNode(t *testing.T) {
	t.Parallel()

	keys := makeTestKeys(t, 7)

	tx1Node := createTxNode(
		t,
		[][]byte{keys[0], keys[1]}, // readsOnly
		[][]byte{keys[2], keys[3]}, // readWrites
		[][]byte{keys[4], keys[5]}, // blindWrites
	)

	tx2Node := createTxNode(
		t,
		[][]byte{keys[2]},          // readsOnly
		[][]byte{keys[0]},          // readWrites
		[][]byte{keys[4], keys[6]}, // blindWrites
	)

	tx2DependsOnTx := TxNodeBatch{
		tx1Node,
	}
	tx2Node.addDependenciesAndUpdateDependents(tx2DependsOnTx)
	require.False(t, tx2Node.isDependencyFree())
	require.Equal(t, tx2DependsOnTx, tx2Node.dependsOnTxs)
	checkDependentTxs(
		t,
		TxNodeBatch{ // expectedDependentTxs
			tx2Node,
		},
		&tx1Node.dependentTxs, // actualDependentTxs
	)

	tx3Node := createTxNode(
		t,
		[][]byte{keys[5]}, // readsOnly
		[][]byte{keys[3]}, // readWrites
		[][]byte{keys[6]}, // blindWrites
	)

	tx3DependsOnTx := TxNodeBatch{
		tx1Node,
		tx2Node,
	}
	tx3Node.addDependenciesAndUpdateDependents(tx3DependsOnTx)
	require.False(t, tx2Node.isDependencyFree())
	require.Equal(t, tx3DependsOnTx, tx3Node.dependsOnTxs)
	checkDependentTxs(
		t,
		TxNodeBatch{ // expectedDependentTxs
			tx2Node,
			tx3Node,
		},
		&tx1Node.dependentTxs, // actualDependentTxs
	)
	checkDependentTxs(
		t,
		TxNodeBatch{ // expectedDependentTxs
			tx3Node,
		},
		&tx2Node.dependentTxs, // actualDependentTxs
	)

	freedTxs := tx1Node.freeDependents()
	require.Equal(t, TxNodeBatch{tx2Node}, freedTxs)
	require.Empty(t, tx2Node.dependsOnTxs)
}

func createTxNode(t *testing.T, readOnly, readWrite, blindWrite [][]byte) *TransactionNode {
	t.Helper()
	tx := createTxForTest(t, 0, nsID1ForTest, readOnly, readWrite, blindWrite)
	txNode := newTransactionNode(tx)

	expectedReads := make([]string, 0, len(readOnly))
	expectedWrites := make([]string, 0, len(blindWrite))
	expectedReadsAndWrites := make([]string, 0, len(readWrite))

	for _, k := range readOnly {
		expectedReads = append(expectedReads, constructCompositeKey(nsID1ForTest, k))
	}
	expectedReads = append(
		expectedReads,
		constructCompositeKey(committerpb.MetaNamespaceID, []byte(nsID1ForTest)),
	)

	for _, k := range readWrite {
		expectedReadsAndWrites = append(expectedReadsAndWrites, constructCompositeKey(nsID1ForTest, k))
	}

	for _, k := range blindWrite {
		expectedWrites = append(expectedWrites, constructCompositeKey(nsID1ForTest, k))
	}

	checkNewTxNode(
		t,
		tx,
		&readWriteKeys{
			expectedReads,
			expectedWrites,
			expectedReadsAndWrites,
		},
		txNode,
	)

	return txNode
}

func createTxForTest( //nolint: revive
	_ *testing.T, txNum int, nsID string, readOnly, readWrite, blindWrite [][]byte,
) *servicepb.TxWithRef {
	reads := make([]*applicationpb.Read, len(readOnly))
	for i, k := range readOnly {
		reads[i] = &applicationpb.Read{Key: k}
	}

	readWrites := make([]*applicationpb.ReadWrite, len(readWrite))
	for i, k := range readWrite {
		readWrites[i] = &applicationpb.ReadWrite{Key: k}
	}

	blindWrites := make([]*applicationpb.Write, len(blindWrite))
	for i, k := range blindWrite {
		blindWrites[i] = &applicationpb.Write{Key: k}
	}

	return &servicepb.TxWithRef{
		Ref: committerpb.NewTxRef(uuid.New().String(), 0, uint32(txNum)), //nolint:gosec // int -> uint32.
		Content: &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:        nsID,
				ReadsOnly:   reads,
				ReadWrites:  readWrites,
				BlindWrites: blindWrites,
			}},
		},
	}
}

func checkNewTxNode(
	t *testing.T,
	tx *servicepb.TxWithRef,
	readsWrites *readWriteKeys,
	txNode *TransactionNode,
) {
	t.Helper()
	test.RequireProtoEqual(t, tx.Ref, txNode.Tx.Ref)
	test.RequireProtoElementsMatch(t, tx.Content.Namespaces, txNode.Tx.Namespaces)
	require.True(t, txNode.isDependencyFree())
	require.ElementsMatch(t, readsWrites.readsOnly, txNode.rwKeys.readsOnly)
	require.ElementsMatch(t, readsWrites.writesOnly, txNode.rwKeys.writesOnly)
	require.Equal(t, 0, txNode.dependentTxs.Count())
}

func checkDependentTxs(
	t *testing.T, expectedTransactionList TxNodeBatch, dependentTxs *utils.SyncMap[*TransactionNode, any],
) {
	t.Helper()
	actualTransactionList := slices.Collect(dependentTxs.IterKeys())
	require.Len(t, expectedTransactionList, len(actualTransactionList))
	require.ElementsMatch(t, expectedTransactionList, actualTransactionList)
}

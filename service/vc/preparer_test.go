/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type prepareTestEnv struct {
	preparer    *transactionPreparer
	txBatch     chan *servicepb.VcBatch
	preparedTxs chan *preparedTransactions
}

func newPrepareTestEnv(t *testing.T) *prepareTestEnv {
	t.Helper()
	txBatch := make(chan *servicepb.VcBatch, 10)
	preparedTxs := make(chan *preparedTransactions, 10)
	metrics := newVCServiceMetrics()
	p := newPreparer(txBatch, preparedTxs, metrics)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return p.run(ctx, 1)
	}, nil)

	return &prepareTestEnv{
		preparer:    p,
		txBatch:     txBatch,
		preparedTxs: preparedTxs,
	}
}

func TestPrepareTxWithReadsOnly(t *testing.T) {
	t.Parallel()

	env := newPrepareTestEnv(t)

	k1 := []byte("key1")
	k2 := []byte("key2")
	k3 := []byte("key3")
	k4 := []byte("key4")
	k5 := []byte("key5")
	k6 := []byte("key6")

	// Shortcut for version pointer.
	v := applicationpb.NewVersion

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("tx1", 1, 1),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k1, Version: v(1)},
							{Key: k2, Version: v(1)},
							{Key: k3, Version: nil},
						},
					},
					{
						NsId:      "2",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k4, Version: v(0)},
							{Key: k5, Version: nil},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef("tx2", 4, 2),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k1, Version: v(1)},
							{Key: k4, Version: v(1)},
							{Key: k5, Version: nil},
						},
					},
					{
						NsId:      "2",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k4, Version: v(1)},
							{Key: k5, Version: v(0)},
							{Key: k6, Version: nil},
						},
					},
				},
			},
		},
	}

	expectedPreparedTxs := &preparedTransactions{
		nsToReads: namespaceToReads{
			"1": &reads{
				keys:     [][]byte{k1, k2, k3, k4, k5},
				versions: []*uint64{v(1), v(1), nil, v(1), nil},
			},
			"2": &reads{
				keys:     [][]byte{k4, k5, k4, k5, k6},
				versions: []*uint64{v(0), nil, v(1), v(0), nil},
			},
			committerpb.MetaNamespaceID: &reads{
				keys:     [][]byte{[]byte("1"), []byte("2")},
				versions: []*uint64{v(1), v(1)},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead("1", k1, v(1)):                                  []TxID{"tx1", "tx2"},
			newCmpRead("1", k2, v(1)):                                  []TxID{"tx1"},
			newCmpRead("1", k3, nil):                                   []TxID{"tx1"},
			newCmpRead("1", k4, v(1)):                                  []TxID{"tx2"},
			newCmpRead("1", k5, nil):                                   []TxID{"tx2"},
			newCmpRead("2", k4, v(0)):                                  []TxID{"tx1"},
			newCmpRead("2", k5, nil):                                   []TxID{"tx1"},
			newCmpRead("2", k4, v(1)):                                  []TxID{"tx2"},
			newCmpRead("2", k5, v(0)):                                  []TxID{"tx2"},
			newCmpRead("2", k6, nil):                                   []TxID{"tx2"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), v(1)): []TxID{"tx1", "tx2"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("2"), v(1)): []TxID{"tx1", "tx2"},
		},
		txIDToNsNonBlindWrites: transactionToWrites{},
		txIDToNsBlindWrites:    transactionToWrites{},
		txIDToNsNewWrites:      transactionToWrites{},
		invalidTxIDStatus:      make(map[TxID]committerpb.Status),
		txIDToHeight: transactionIDToHeight{
			"tx1": servicepb.NewHeight(1, 1),
			"tx2": servicepb.NewHeight(4, 2),
		},
	}

	env.txBatch <- tx
	preparedTxs := <-env.preparedTxs
	ensurePreparedTx(t, expectedPreparedTxs, preparedTxs)
}

func TestPrepareTxWithBlindWritesOnly(t *testing.T) {
	t.Parallel()

	env := newPrepareTestEnv(t)

	k1 := []byte("key1")
	k2 := []byte("key2")
	k3 := []byte("key3")
	k4 := []byte("key4")
	k5 := []byte("key5")

	// Shortcut for version pointer.
	v := applicationpb.NewVersion

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("tx1", 10, 5),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 1,
						BlindWrites: []*applicationpb.Write{
							{Key: k1, Value: []byte("1")},
							{Key: k2, Value: []byte("1")},
							{Key: k3, Value: nil},
						},
					},
					{
						NsId:      "2",
						NsVersion: 1,
						BlindWrites: []*applicationpb.Write{{
							Key:   k1,
							Value: []byte("5"),
						}},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef("tx2", 6, 3),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 2,
						BlindWrites: []*applicationpb.Write{
							{Key: k4, Value: []byte("1")},
							{Key: k5, Value: nil},
						},
					},
				},
			},
		},
	}

	expectedPreparedTxs := &preparedTransactions{
		nsToReads: namespaceToReads{
			committerpb.MetaNamespaceID: &reads{
				keys: [][]byte{
					[]byte("1"), []byte("2"), []byte("1"),
				},
				versions: []*uint64{v(1), v(1), v(2)},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), v(1)): []TxID{"tx1"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("2"), v(1)): []TxID{"tx1"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), v(2)): []TxID{"tx2"},
		},
		txIDToNsNonBlindWrites: transactionToWrites{},
		txIDToNsBlindWrites: transactionToWrites{
			"tx1": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k1, k2, k3},
					values:   [][]byte{[]byte("1"), []byte("1"), nil},
					versions: []uint64{0, 0, 0},
				},
				"2": &namespaceWrites{
					keys:     [][]byte{k1},
					values:   [][]byte{[]byte("5")},
					versions: []uint64{0},
				},
			},
			"tx2": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k4, k5},
					values:   [][]byte{[]byte("1"), nil},
					versions: []uint64{0, 0},
				},
			},
		},
		txIDToNsNewWrites: transactionToWrites{},
		invalidTxIDStatus: make(map[TxID]committerpb.Status),
		txIDToHeight: transactionIDToHeight{
			"tx1": servicepb.NewHeight(10, 5),
			"tx2": servicepb.NewHeight(6, 3),
		},
	}

	env.txBatch <- tx
	preparedTxs := <-env.preparedTxs
	ensurePreparedTx(t, expectedPreparedTxs, preparedTxs)
}

func TestPrepareTxWithReadWritesOnly(t *testing.T) {
	t.Parallel()

	env := newPrepareTestEnv(t)

	k1 := []byte("key1")
	k2 := []byte("key2")
	k3 := []byte("key3")
	k4 := []byte("key4")
	k5 := []byte("key5")
	k6 := []byte("key6")
	k7 := []byte("key7")

	// Shortcut for version pointer.
	v := applicationpb.NewVersion

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("tx1", 7, 4),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 2,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k1, Version: v(1), Value: []byte("v1")},
							{Key: k2, Version: v(1), Value: []byte("v2")},
							{Key: k3, Version: nil, Value: []byte("v3")},
						},
					},
					{
						NsId:      "2",
						NsVersion: 2,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k4, Version: v(0), Value: []byte("v4")},
							{Key: k5, Version: nil, Value: []byte("v5")},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef("tx2", 7, 5),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 2,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k4, Version: v(1), Value: []byte("v4")},
							{Key: k5, Version: nil, Value: []byte("v5")},
						},
					},
					{
						NsId:      "2",
						NsVersion: 2,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k6, Version: nil, Value: []byte("v6")},
							{Key: k7, Version: nil, Value: nil},
						},
					},
				},
			},
		},
	}

	expectedPreparedTxs := &preparedTransactions{
		nsToReads: namespaceToReads{
			"1": &reads{
				keys:     [][]byte{k1, k2, k4},
				versions: []*uint64{v(1), v(1), v(1)},
			},
			"2": &reads{
				keys:     [][]byte{k4},
				versions: []*uint64{v(0)},
			},
			committerpb.MetaNamespaceID: &reads{
				keys:     [][]byte{[]byte("1"), []byte("2")},
				versions: []*uint64{v(2), v(2)},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead("1", k1, v(1)):                                  []TxID{"tx1"},
			newCmpRead("1", k2, v(1)):                                  []TxID{"tx1"},
			newCmpRead("1", k3, nil):                                   []TxID{"tx1"},
			newCmpRead("1", k4, v(1)):                                  []TxID{"tx2"},
			newCmpRead("1", k5, nil):                                   []TxID{"tx2"},
			newCmpRead("2", k4, v(0)):                                  []TxID{"tx1"},
			newCmpRead("2", k5, nil):                                   []TxID{"tx1"},
			newCmpRead("2", k6, nil):                                   []TxID{"tx2"},
			newCmpRead("2", k7, nil):                                   []TxID{"tx2"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), v(2)): []TxID{"tx1", "tx2"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("2"), v(2)): []TxID{"tx1", "tx2"},
		},
		txIDToNsNonBlindWrites: transactionToWrites{
			"tx1": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k1, k2},
					values:   [][]byte{[]byte("v1"), []byte("v2")},
					versions: []uint64{2, 2},
				},
				"2": &namespaceWrites{
					keys:     [][]byte{k4},
					values:   [][]byte{[]byte("v4")},
					versions: []uint64{1},
				},
			},
			"tx2": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k4},
					values:   [][]byte{[]byte("v4")},
					versions: []uint64{2},
				},
			},
		},
		txIDToNsBlindWrites: transactionToWrites{},
		txIDToNsNewWrites: transactionToWrites{
			"tx1": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k3},
					values:   [][]byte{[]byte("v3")},
					versions: []uint64{0},
				},
				"2": &namespaceWrites{
					keys:     [][]byte{k5},
					values:   [][]byte{[]byte("v5")},
					versions: []uint64{0},
				},
			},
			"tx2": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k5},
					values:   [][]byte{[]byte("v5")},
					versions: []uint64{0},
				},
				"2": &namespaceWrites{
					keys:     [][]byte{k6, k7},
					values:   [][]byte{[]byte("v6"), nil},
					versions: []uint64{0, 0},
				},
			},
		},
		invalidTxIDStatus: make(map[TxID]committerpb.Status),
		txIDToHeight: transactionIDToHeight{
			"tx1": servicepb.NewHeight(7, 4),
			"tx2": servicepb.NewHeight(7, 5),
		},
	}

	env.txBatch <- tx
	preparedTxs := <-env.preparedTxs
	ensurePreparedTx(t, expectedPreparedTxs, preparedTxs)
}

func TestPrepareTx(t *testing.T) { //nolint:maintidx // cannot improve.
	t.Parallel()

	env := newPrepareTestEnv(t)

	k1 := []byte("key1")
	k2 := []byte("key2")
	k3 := []byte("key3")
	k4 := []byte("key4")
	k5 := []byte("key5")
	k6 := []byte("key6")
	k7 := []byte("key7")
	k8 := []byte("key8")
	k9 := []byte("key9")
	k10 := []byte("key10")

	// Shortcut for version pointer.
	v := applicationpb.NewVersion

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("tx1", 8, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k1, Version: v(1)},
							{Key: k2, Version: v(2)},
						},
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k3, Version: v(3), Value: []byte("v3")},
						},
						BlindWrites: []*applicationpb.Write{
							{Key: k4, Value: []byte("v4")},
						},
					},
					{
						NsId:      "2",
						NsVersion: 2,
						ReadsOnly: []*applicationpb.Read{
							{Key: k5, Version: nil},
						},
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k6, Version: nil, Value: []byte("v6")},
						},
						BlindWrites: []*applicationpb.Write{
							{Key: k7, Value: []byte("v7")},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef("tx2", 9, 3),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 3,
						ReadsOnly: []*applicationpb.Read{
							{Key: k8, Version: v(8)},
						},
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k9, Version: v(9), Value: []byte("v9")},
						},
						BlindWrites: []*applicationpb.Write{
							{Key: k10, Value: []byte("v10")},
						},
					},
				},
			},
			{
				// We ensure no duplicate reads with duplicated TX.
				Ref: committerpb.NewTxRef("tx2.2", 9, 4),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 3,
						ReadsOnly: []*applicationpb.Read{
							{Key: k8, Version: v(8)},
						},
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k9, Version: v(9), Value: []byte("v9")},
						},
						BlindWrites: []*applicationpb.Write{
							{Key: k10, Value: []byte("v10")},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef("tx3", 6, 2),
				PrelimInvalidTxStatus: &servicepb.InvalidTxStatus{
					Code: committerpb.Status_MALFORMED_NO_WRITES,
				},
			},
			{
				Ref: committerpb.NewTxRef("tx4", 5, 2),
				PrelimInvalidTxStatus: &servicepb.InvalidTxStatus{
					Code: committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE,
				},
			},
			{
				Ref: committerpb.NewTxRef("tx5", 6, 2),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      committerpb.MetaNamespaceID,
						NsVersion: 4,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: []byte("1"), Version: v(1), Value: []byte("meta")},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef("tx6", 6, 2),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId: committerpb.ConfigNamespaceID,
						BlindWrites: []*applicationpb.Write{
							{Key: []byte(committerpb.ConfigKey), Value: []byte("config")},
						},
					},
				},
			},
		},
	}

	expectedPreparedTxs := &preparedTransactions{
		nsToReads: namespaceToReads{
			"1": &reads{
				keys: [][]byte{k1, k2, k3, k8, k9},
				versions: []*uint64{
					v(1), v(2), v(3),
					v(8), v(9),
				},
			},
			"2": &reads{
				keys:     [][]byte{k5},
				versions: []*uint64{nil},
			},
			committerpb.MetaNamespaceID: &reads{
				keys: [][]byte{
					[]byte("1"), []byte("2"), []byte("1"),
				},
				versions: []*uint64{v(1), v(2), v(3)},
			},
			committerpb.ConfigNamespaceID: &reads{
				keys:     [][]byte{[]byte(committerpb.ConfigKey)},
				versions: []*uint64{v(4)},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead("1", k1, v(1)):                                                      []TxID{"tx1"},
			newCmpRead("1", k2, v(2)):                                                      []TxID{"tx1"},
			newCmpRead("1", k3, v(3)):                                                      []TxID{"tx1"},
			newCmpRead("1", k8, v(8)):                                                      []TxID{"tx2", "tx2.2"},
			newCmpRead("1", k9, v(9)):                                                      []TxID{"tx2", "tx2.2"},
			newCmpRead("2", k5, nil):                                                       []TxID{"tx1"},
			newCmpRead("2", k6, nil):                                                       []TxID{"tx1"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), v(1)):                     []TxID{"tx1", "tx5"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("2"), v(2)):                     []TxID{"tx1"},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), v(3)):                     []TxID{"tx2", "tx2.2"},
			newCmpRead(committerpb.ConfigNamespaceID, []byte(committerpb.ConfigKey), v(4)): []TxID{"tx5"},
		},
		txIDToNsNonBlindWrites: transactionToWrites{
			"tx1": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k3},
					values:   [][]byte{[]byte("v3")},
					versions: []uint64{4},
				},
			},
			"tx2": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k9},
					values:   [][]byte{[]byte("v9")},
					versions: []uint64{10},
				},
			},
			"tx2.2": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k9},
					values:   [][]byte{[]byte("v9")},
					versions: []uint64{10},
				},
			},
			"tx5": namespaceToWrites{
				committerpb.MetaNamespaceID: &namespaceWrites{
					keys:     [][]byte{[]byte("1")},
					values:   [][]byte{[]byte("meta")},
					versions: []uint64{2},
				},
			},
		},
		txIDToNsBlindWrites: transactionToWrites{
			"tx1": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k4},
					values:   [][]byte{[]byte("v4")},
					versions: []uint64{0},
				},
				"2": &namespaceWrites{
					keys:     [][]byte{k7},
					values:   [][]byte{[]byte("v7")},
					versions: []uint64{0},
				},
			},
			"tx2": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k10},
					values:   [][]byte{[]byte("v10")},
					versions: []uint64{0},
				},
			},
			"tx2.2": namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k10},
					values:   [][]byte{[]byte("v10")},
					versions: []uint64{0},
				},
			},
			"tx6": namespaceToWrites{
				committerpb.ConfigNamespaceID: &namespaceWrites{
					keys:     [][]byte{[]byte(committerpb.ConfigKey)},
					values:   [][]byte{[]byte("config")},
					versions: []uint64{0},
				},
			},
		},
		txIDToNsNewWrites: transactionToWrites{
			"tx1": namespaceToWrites{
				"2": &namespaceWrites{
					keys:     [][]byte{k6},
					values:   [][]byte{[]byte("v6")},
					versions: []uint64{0},
				},
			},
		},
		invalidTxIDStatus: map[TxID]committerpb.Status{
			"tx3": committerpb.Status_MALFORMED_NO_WRITES,
			"tx4": committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE,
		},
		txIDToHeight: transactionIDToHeight{
			"tx1":   servicepb.NewHeight(8, 0),
			"tx2":   servicepb.NewHeight(9, 3),
			"tx2.2": servicepb.NewHeight(9, 4),
			"tx3":   servicepb.NewHeight(6, 2),
			"tx4":   servicepb.NewHeight(5, 2),
		},
	}

	env.txBatch <- tx
	preparedTxs := <-env.preparedTxs
	ensurePreparedTx(t, expectedPreparedTxs, preparedTxs)
}

func ensurePreparedTx(t *testing.T, expectedPreparedTxs, actualPreparedTxs *preparedTransactions) {
	t.Helper()
	require.Equal(t, expectedPreparedTxs.nsToReads, actualPreparedTxs.nsToReads)
	requireEqualMapOfLists(t, expectedPreparedTxs.readToTxIDs, actualPreparedTxs.readToTxIDs)
	require.Equal(t, expectedPreparedTxs.txIDToNsNonBlindWrites, actualPreparedTxs.txIDToNsNonBlindWrites)
	require.Equal(t, expectedPreparedTxs.txIDToNsBlindWrites, actualPreparedTxs.txIDToNsBlindWrites)
	require.Equal(t, expectedPreparedTxs.txIDToNsNewWrites, actualPreparedTxs.txIDToNsNewWrites)
	require.Equal(t, expectedPreparedTxs.invalidTxIDStatus, actualPreparedTxs.invalidTxIDStatus)
}

func requireEqualMapOfLists[K comparable, V any](t *testing.T, expected, actual map[K][]V) {
	t.Helper()
	for k, expectedV := range expected {
		actualV, ok := actual[k]
		require.True(t, ok, k)
		require.ElementsMatch(t, expectedV, actualV, k)
	}
}

//nolint:gocognit // single method for simplicity.
func BenchmarkPrepare(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})

	// Parameters
	batchSize := 1024

	for _, w := range []int{1, 2, 4, 8} {
		w := w
		b.Run(fmt.Sprintf("w=%d", w), func(b *testing.B) {
			txBatch := make(chan *servicepb.VcBatch, 8)
			preparedTxs := make(chan *preparedTransactions, 8)
			metrics := newVCServiceMetrics()
			p := newPreparer(txBatch, preparedTxs, metrics)
			test.RunServiceForTest(b.Context(), b, func(ctx context.Context) error {
				return p.run(ctx, w)
			}, nil)

			txPoll := workload.GenerateTransactions(b, workload.DefaultProfile(8), max(b.N*3, batchSize*3))

			inCtx := channel.NewWriter(b.Context(), txBatch)
			outCtx := channel.NewReader(b.Context(), preparedTxs)
			b.ResetTimer()
			// Generates the load to the preparer's queue.
			go func() {
				var i uint64
				for b.Context().Err() == nil && len(txPoll) > 0 {
					take := min(batchSize, len(txPoll))
					inCtx.Write(workload.MapToVcBatch(i, txPoll[:take]))
					txPoll = txPoll[take:]
					i++
				}
			}()
			// Reads the output of preparer.
			var total int
			for total < b.N {
				batch, ok := outCtx.Read()
				if !ok {
					return
				}
				total += len(batch.txIDToHeight)
			}
			b.StopTimer()
		})
	}
}

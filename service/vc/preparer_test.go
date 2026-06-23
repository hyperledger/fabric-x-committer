/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type prepareTestEnv struct {
	preparer    *transactionPreparer
	txBatch     chan *servicepb.VcBatch
	preparedTxs chan *preparedTransactions
}

var txs = []TxID{"tx1", "tx2", "tx3", "tx4", "tx5", "tx6", "tx7", "tx8"}

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

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef(string(txs[0]), 1, 1),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k1, Version: new(uint64(1))},
							{Key: k2, Version: new(uint64(1))},
							{Key: k3, Version: nil},
						},
					},
					{
						NsId:      "2",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k4, Version: new(uint64(0))},
							{Key: k5, Version: nil},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef(string(txs[1]), 4, 2),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k1, Version: new(uint64(1))},
							{Key: k4, Version: new(uint64(1))},
							{Key: k5, Version: nil},
						},
					},
					{
						NsId:      "2",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k4, Version: new(uint64(1))},
							{Key: k5, Version: new(uint64(0))},
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
				versions: []*uint64{new(uint64(1)), new(uint64(1)), nil, new(uint64(1)), nil},
			},
			"2": &reads{
				keys:     [][]byte{k4, k5, k4, k5, k6},
				versions: []*uint64{new(uint64(0)), nil, new(uint64(1)), new(uint64(0)), nil},
			},
			committerpb.MetaNamespaceID: &reads{
				keys:     [][]byte{[]byte("1"), []byte("2")},
				versions: []*uint64{new(uint64(1)), new(uint64(1))},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead("1", k1, new(uint64(1))):                                  []TxID{txs[0], txs[1]},
			newCmpRead("1", k2, new(uint64(1))):                                  []TxID{txs[0]},
			newCmpRead("1", k3, nil):                                             []TxID{txs[0]},
			newCmpRead("1", k4, new(uint64(1))):                                  []TxID{txs[1]},
			newCmpRead("1", k5, nil):                                             []TxID{txs[1]},
			newCmpRead("2", k4, new(uint64(0))):                                  []TxID{txs[0]},
			newCmpRead("2", k5, nil):                                             []TxID{txs[0]},
			newCmpRead("2", k4, new(uint64(1))):                                  []TxID{txs[1]},
			newCmpRead("2", k5, new(uint64(0))):                                  []TxID{txs[1]},
			newCmpRead("2", k6, nil):                                             []TxID{txs[1]},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), new(uint64(1))): []TxID{txs[0], txs[1]},
			newCmpRead(committerpb.MetaNamespaceID, []byte("2"), new(uint64(1))): []TxID{txs[0], txs[1]},
		},
		txIDToNsNonBlindWrites: transactionToWrites{},
		txIDToNsBlindWrites:    transactionToWrites{},
		txIDToNsNewWrites:      transactionToWrites{},
		invalidTxIDStatus:      make(map[TxID]committerpb.Status),
		txIDToHeight: transactionIDToHeight{
			txs[0]: servicepb.NewHeight(1, 1),
			txs[1]: servicepb.NewHeight(4, 2),
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

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef(string(txs[0]), 10, 5),
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
				Ref: committerpb.NewTxRef(string(txs[1]), 6, 3),
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
				versions: []*uint64{new(uint64(1)), new(uint64(1)), new(uint64(2))},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), new(uint64(1))): []TxID{txs[0]},
			newCmpRead(committerpb.MetaNamespaceID, []byte("2"), new(uint64(1))): []TxID{txs[0]},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), new(uint64(2))): []TxID{txs[1]},
		},
		txIDToNsNonBlindWrites: transactionToWrites{},
		txIDToNsBlindWrites: transactionToWrites{
			txs[0]: namespaceToWrites{
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
			txs[1]: namespaceToWrites{
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
			txs[0]: servicepb.NewHeight(10, 5),
			txs[1]: servicepb.NewHeight(6, 3),
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

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef(string(txs[0]), 7, 4),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 2,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k1, Version: new(uint64(1)), Value: []byte("v1")},
							{Key: k2, Version: new(uint64(1)), Value: []byte("v2")},
							{Key: k3, Version: nil, Value: []byte("v3")},
						},
					},
					{
						NsId:      "2",
						NsVersion: 2,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k4, Version: new(uint64(0)), Value: []byte("v4")},
							{Key: k5, Version: nil, Value: []byte("v5")},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef(string(txs[1]), 7, 5),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 2,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k4, Version: new(uint64(1)), Value: []byte("v4")},
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
				versions: []*uint64{new(uint64(1)), new(uint64(1)), new(uint64(1))},
			},
			"2": &reads{
				keys:     [][]byte{k4},
				versions: []*uint64{new(uint64(0))},
			},
			committerpb.MetaNamespaceID: &reads{
				keys:     [][]byte{[]byte("1"), []byte("2")},
				versions: []*uint64{new(uint64(2)), new(uint64(2))},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead("1", k1, new(uint64(1))):                                  []TxID{txs[0]},
			newCmpRead("1", k2, new(uint64(1))):                                  []TxID{txs[0]},
			newCmpRead("1", k3, nil):                                             []TxID{txs[0]},
			newCmpRead("1", k4, new(uint64(1))):                                  []TxID{txs[1]},
			newCmpRead("1", k5, nil):                                             []TxID{txs[1]},
			newCmpRead("2", k4, new(uint64(0))):                                  []TxID{txs[0]},
			newCmpRead("2", k5, nil):                                             []TxID{txs[0]},
			newCmpRead("2", k6, nil):                                             []TxID{txs[1]},
			newCmpRead("2", k7, nil):                                             []TxID{txs[1]},
			newCmpRead(committerpb.MetaNamespaceID, []byte("1"), new(uint64(2))): []TxID{txs[0], txs[1]},
			newCmpRead(committerpb.MetaNamespaceID, []byte("2"), new(uint64(2))): []TxID{txs[0], txs[1]},
		},
		txIDToNsNonBlindWrites: transactionToWrites{
			txs[0]: namespaceToWrites{
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
			txs[1]: namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k4},
					values:   [][]byte{[]byte("v4")},
					versions: []uint64{2},
				},
			},
		},
		txIDToNsBlindWrites: transactionToWrites{},
		txIDToNsNewWrites: transactionToWrites{
			txs[0]: namespaceToWrites{
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
			txs[1]: namespaceToWrites{
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
			txs[0]: servicepb.NewHeight(7, 4),
			txs[1]: servicepb.NewHeight(7, 5),
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

	tx := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef(string(txs[0]), 8, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 1,
						ReadsOnly: []*applicationpb.Read{
							{Key: k1, Version: new(uint64(1))},
							{Key: k2, Version: new(uint64(2))},
						},
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k3, Version: new(uint64(3)), Value: []byte("v3")},
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
				Ref: committerpb.NewTxRef(string(txs[1]), 9, 3),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 3,
						ReadsOnly: []*applicationpb.Read{
							{Key: k8, Version: new(uint64(8))},
						},
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k9, Version: new(uint64(9)), Value: []byte("v9")},
						},
						BlindWrites: []*applicationpb.Write{
							{Key: k10, Value: []byte("v10")},
						},
					},
				},
			},
			{
				// We ensure no duplicate reads with duplicated TX.
				Ref: committerpb.NewTxRef(string(txs[6]), 9, 4),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 3,
						ReadsOnly: []*applicationpb.Read{
							{Key: k8, Version: new(uint64(8))},
						},
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: k9, Version: new(uint64(9)), Value: []byte("v9")},
						},
						BlindWrites: []*applicationpb.Write{
							{Key: k10, Value: []byte("v10")},
						},
					},
				},
			},
			{
				Ref:                   committerpb.NewTxRef(string(txs[2]), 6, 2),
				PrelimInvalidTxStatus: new(committerpb.Status_MALFORMED_NO_WRITES),
			},
			{
				Ref:                   committerpb.NewTxRef(string(txs[3]), 5, 2),
				PrelimInvalidTxStatus: new(committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE),
			},
			{
				Ref: committerpb.NewTxRef(string(txs[4]), 6, 2),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:      committerpb.MetaNamespaceID,
						NsVersion: 4,
						ReadWrites: []*applicationpb.ReadWrite{
							{Key: []byte("1"), Version: new(uint64(1)), Value: []byte("meta")},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef(string(txs[5]), 6, 2),
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

	meta := committerpb.MetaNamespaceID
	conf := committerpb.ConfigNamespaceID

	expectedPreparedTxs := &preparedTransactions{
		nsToReads: namespaceToReads{
			"1": &reads{
				keys: [][]byte{k1, k2, k3, k8, k9},
				versions: []*uint64{
					new(uint64(1)), new(uint64(2)), new(uint64(3)), new(uint64(8)), new(uint64(9)),
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
				versions: []*uint64{new(uint64(1)), new(uint64(2)), new(uint64(3))},
			},
			committerpb.ConfigNamespaceID: &reads{
				keys:     [][]byte{[]byte(committerpb.ConfigKey)},
				versions: []*uint64{new(uint64(4))},
			},
		},
		readToTxIDs: readToTransactions{
			newCmpRead("1", k1, new(uint64(1))):                             []TxID{txs[0]},
			newCmpRead("1", k2, new(uint64(2))):                             []TxID{txs[0]},
			newCmpRead("1", k3, new(uint64(3))):                             []TxID{txs[0]},
			newCmpRead("1", k8, new(uint64(8))):                             []TxID{txs[1], txs[6]},
			newCmpRead("1", k9, new(uint64(9))):                             []TxID{txs[1], txs[6]},
			newCmpRead("2", k5, nil):                                        []TxID{txs[0]},
			newCmpRead("2", k6, nil):                                        []TxID{txs[0]},
			newCmpRead(meta, []byte("1"), new(uint64(1))):                   []TxID{txs[0], txs[4]},
			newCmpRead(meta, []byte("2"), new(uint64(2))):                   []TxID{txs[0]},
			newCmpRead(meta, []byte("1"), new(uint64(3))):                   []TxID{txs[1], txs[6]},
			newCmpRead(conf, []byte(committerpb.ConfigKey), new(uint64(4))): []TxID{txs[4]},
		},
		txIDToNsNonBlindWrites: transactionToWrites{
			txs[0]: namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k3},
					values:   [][]byte{[]byte("v3")},
					versions: []uint64{4},
				},
			},
			txs[1]: namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k9},
					values:   [][]byte{[]byte("v9")},
					versions: []uint64{10},
				},
			},
			txs[6]: namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k9},
					values:   [][]byte{[]byte("v9")},
					versions: []uint64{10},
				},
			},
			txs[4]: namespaceToWrites{
				committerpb.MetaNamespaceID: &namespaceWrites{
					keys:     [][]byte{[]byte("1")},
					values:   [][]byte{[]byte("meta")},
					versions: []uint64{2},
				},
			},
		},
		txIDToNsBlindWrites: transactionToWrites{
			txs[0]: namespaceToWrites{
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
			txs[1]: namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k10},
					values:   [][]byte{[]byte("v10")},
					versions: []uint64{0},
				},
			},
			txs[6]: namespaceToWrites{
				"1": &namespaceWrites{
					keys:     [][]byte{k10},
					values:   [][]byte{[]byte("v10")},
					versions: []uint64{0},
				},
			},
			txs[5]: namespaceToWrites{
				committerpb.ConfigNamespaceID: &namespaceWrites{
					keys:     [][]byte{[]byte(committerpb.ConfigKey)},
					values:   [][]byte{[]byte("config")},
					versions: []uint64{0},
				},
			},
		},
		txIDToNsNewWrites: transactionToWrites{
			txs[0]: namespaceToWrites{
				"2": &namespaceWrites{
					keys:     [][]byte{k6},
					values:   [][]byte{[]byte("v6")},
					versions: []uint64{0},
				},
			},
		},
		invalidTxIDStatus: map[TxID]committerpb.Status{
			txs[2]: committerpb.Status_MALFORMED_NO_WRITES,
			txs[3]: committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE,
		},
		txIDToHeight: transactionIDToHeight{
			txs[0]: servicepb.NewHeight(8, 0),
			txs[1]: servicepb.NewHeight(9, 3),
			txs[6]: servicepb.NewHeight(9, 4),
			txs[2]: servicepb.NewHeight(6, 2),
			txs[3]: servicepb.NewHeight(5, 2),
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
	flogging.ActivateSpec("fatal")

	// Parameters
	batchSize := 1024

	for _, w := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("w=%d", w), func(b *testing.B) {
			txBatch := make(chan *servicepb.VcBatch, 8)
			preparedTxs := make(chan *preparedTransactions, 8)
			metrics := newVCServiceMetrics()
			p := newPreparer(txBatch, preparedTxs, metrics)
			test.RunServiceForTest(b.Context(), b, func(ctx context.Context) error {
				return p.run(ctx, w)
			}, nil)

			txPoll := workload.GenerateTransactions(b, nil, max(b.N*3, batchSize*3))

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

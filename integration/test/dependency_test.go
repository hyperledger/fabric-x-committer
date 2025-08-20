/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
)

func testSetup(t *testing.T) *runner.CommitterRuntime {
	t.Helper()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockSize:    5,
		BlockTimeout: 2 * time.Second,
	})
	c.Start(t, runner.FullTxPath)
	c.CreateNamespacesAndCommit(t, "1")

	c.MakeAndSendTransactionsToOrderer(t, [][]*protoblocktx.TxNamespace{{{
		// blind write keys to ns1.
		NsId:      "1",
		NsVersion: 0,
		BlindWrites: []*protoblocktx.Write{{
			Key: []byte("k1"),
		}},
	}}}, []protoblocktx.Status{protoblocktx.Status_COMMITTED})

	return c
}

func TestDependentHappyPath(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name     string
		txs      []*protoblocktx.TxNamespace
		expected []protoblocktx.Status
	}{
		{
			name: "valid transactions: second tx waits due to write-write conflict",
			txs: []*protoblocktx.TxNamespace{
				{ // performs read only, read-write, and blind-write.
					ReadsOnly: []*protoblocktx.Read{
						{
							Key:     []byte("k3"),
							Version: nil,
						},
					},
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:     []byte("k1"),
							Version: types.Version(0),
							Value:   []byte("v2"),
						},
					},
					BlindWrites: []*protoblocktx.Write{
						{
							Key: []byte("k4"),
						},
					},
				},
				{ // performs only blind-write.
					BlindWrites: []*protoblocktx.Write{
						{
							Key: []byte("k4"),
						},
					},
				},
			},
			expected: []protoblocktx.Status{protoblocktx.Status_COMMITTED, protoblocktx.Status_COMMITTED},
		},
		{
			name: "valid transactions: second tx waits due to read-write but uses the updated version",
			txs: []*protoblocktx.TxNamespace{
				{ // performs read-write and blind-write.
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:     []byte("k1"),
							Version: types.Version(1),
							Value:   []byte("v3"),
						},
					},
					BlindWrites: []*protoblocktx.Write{
						{
							Key: []byte("k4"),
						},
					},
				},
				{ // performs only read-write.
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:     []byte("k1"),
							Version: types.Version(2),
							Value:   []byte("v4"),
						},
					},
				},
			},
			expected: []protoblocktx.Status{protoblocktx.Status_COMMITTED, protoblocktx.Status_COMMITTED},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			txs := make([]*protoloadgen.TX, len(tt.txs))
			for i, tx := range tt.txs {
				txs[i] = c.TxBuilder.MakeTx(&protoblocktx.Tx{
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:        "1",
							NsVersion:   0,
							ReadWrites:  tx.ReadWrites,
							ReadsOnly:   tx.ReadsOnly,
							BlindWrites: tx.BlindWrites,
						},
					},
				})
			}

			c.SendTransactionsToOrderer(t, txs, tt.expected)
		})
	}
}

func TestReadOnlyConflictsWithCommittedStates(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name      string
		readsOnly []*protoblocktx.Read
		expected  []protoblocktx.Status
	}{
		{
			name: "readOnly version is nil but the committed version is not nil, i.e., state exist",
			readsOnly: []*protoblocktx.Read{
				{
					Key: []byte("k1"),
				},
			},
			expected: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
		},
		{
			name: "readOnly version is not nil but the committed version is nil, i.e., state does not exist",
			readsOnly: []*protoblocktx.Read{
				{
					Key:     []byte("k2"),
					Version: types.Version(0),
				},
			},
			expected: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
		},
		{
			name: "readOnly version is different from the committed version",
			readsOnly: []*protoblocktx.Read{
				{
					Key:     []byte("k1"),
					Version: types.Version(1),
				},
			},
			expected: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
		},
		{
			name: "valid",
			readsOnly: []*protoblocktx.Read{
				{
					Key:     []byte("k1"),
					Version: types.Version(0),
				},
			},
			expected: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			txs := []*protoloadgen.TX{
				c.TxBuilder.MakeTx(&protoblocktx.Tx{
					Namespaces: []*protoblocktx.TxNamespace{{
						NsId:      "1",
						NsVersion: 0,
						ReadsOnly: tt.readsOnly,
						BlindWrites: []*protoblocktx.Write{{
							Key: []byte("k3"),
						}},
					}},
				}),
			}
			c.SendTransactionsToOrderer(t, txs, tt.expected)
		})
	}
}

func TestReadWriteConflictsWithCommittedStates(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name     string
		txs      [][]*protoblocktx.TxNamespace
		expected []protoblocktx.Status
	}{
		{
			name: "readWrite version is nil but the committed version is not nil, i.e., state exist",
			txs: [][]*protoblocktx.TxNamespace{{{
				ReadWrites: []*protoblocktx.ReadWrite{{
					Key:     []byte("k1"),
					Version: nil,
				}},
			}}},
			expected: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
		},
		{
			name: "readWrite version is not nil but the committed version is nil, i.e., state does not exist",
			txs: [][]*protoblocktx.TxNamespace{{{
				ReadWrites: []*protoblocktx.ReadWrite{{
					Key:     []byte("k2"),
					Version: types.Version(0),
				}},
			}}},
			expected: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
		},
		{
			name: "readWrite version is different from the committed version",
			txs: [][]*protoblocktx.TxNamespace{{{
				ReadWrites: []*protoblocktx.ReadWrite{{
					Key:     []byte("k1"),
					Version: types.Version(1),
				}},
			}}},
			expected: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
		},
		{
			name: "valid",
			txs: [][]*protoblocktx.TxNamespace{{{
				ReadWrites: []*protoblocktx.ReadWrite{{
					Key:     []byte("k1"),
					Version: types.Version(0),
				}},
			}}},
			expected: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, txs := range tt.txs {
				for _, ns := range txs {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
			}
			c.MakeAndSendTransactionsToOrderer(t, tt.txs, tt.expected)
		})
	}
}

func TestReadWriteConflictsAmongActiveTransactions(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name     string
		txs      [][]*protoblocktx.TxNamespace
		expected []protoblocktx.Status
	}{
		{
			name: "first transaction invalidates the second",
			txs: [][]*protoblocktx.TxNamespace{
				{{ // "read-write k1".
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("k1"),
						Version: types.Version(0),
					}},
				}},
				{{ // read-write k1 but invalid due to the previous tx.
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("k1"),
						Version: types.Version(0),
					}},
				}},
			},
			expected: []protoblocktx.Status{
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_MVCC_CONFLICT,
			},
		},
		{
			name: "as first and second transactions are invalid, the third succeeds",
			txs: [][]*protoblocktx.TxNamespace{
				{{ // read-write k1 but wrong version v0.
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("k1"),
						Version: types.Version(0),
					}},
				}},
				{{ // read-write k1 but wrong version v2.
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("k1"),
						Version: types.Version(2),
					}},
				}},
				{{ // read-write k1 with correct version.
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("k1"),
						Version: types.Version(1),
					}},
				}},
			},
			expected: []protoblocktx.Status{
				protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				protoblocktx.Status_COMMITTED,
			},
		},
		{
			name: "first transaction writes non-existing key before the second transaction",
			txs: [][]*protoblocktx.TxNamespace{
				{{ // read-write k2.
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("k2"),
						Version: nil,
					}},
				}},
				{{ // read-write k2 but invalid due to the previous tx.
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("k2"),
						Version: nil,
					}},
				}},
			},
			expected: []protoblocktx.Status{
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_MVCC_CONFLICT,
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, txs := range tt.txs {
				for _, ns := range txs {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
			}
			c.MakeAndSendTransactionsToOrderer(t, tt.txs, tt.expected)
		})
	}
}

func TestWriteWriteConflictsAmongActiveTransactions(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name     string
		txs      [][]*protoblocktx.TxNamespace
		expected []protoblocktx.Status
	}{
		{
			name: "first transaction invalidates the second",
			txs: [][]*protoblocktx.TxNamespace{
				{{ // blind-write k1.
					BlindWrites: []*protoblocktx.Write{
						{
							Key: []byte("k1"),
						},
					},
				}},
				{{ // read-write k1 but invalid due to the previous tx.
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:     []byte("k1"),
							Version: types.Version(0),
						},
					},
				}},
			},
			expected: []protoblocktx.Status{
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_MVCC_CONFLICT,
			},
		},
		{
			name: "first transaction writes non-existing key before the second transaction",
			txs: [][]*protoblocktx.TxNamespace{
				{{ // blind-write k2.
					BlindWrites: []*protoblocktx.Write{
						{
							Key: []byte("k2"),
						},
					},
				}},
				{{ // read-write k2 but invalid due to the previous tx.
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:     []byte("k2"),
							Version: nil,
						},
					},
				}},
			},
			expected: []protoblocktx.Status{
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_MVCC_CONFLICT,
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, txs := range tt.txs {
				for _, ns := range txs {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
			}
			c.MakeAndSendTransactionsToOrderer(t, tt.txs, tt.expected)
		})
	}
}

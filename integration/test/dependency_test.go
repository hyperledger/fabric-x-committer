/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
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

	initTx := &protoblocktx.Tx{
		Id: "blind write keys to ns1",
		Namespaces: []*protoblocktx.TxNamespace{
			{
				NsId:      "1",
				NsVersion: 0,
				BlindWrites: []*protoblocktx.Write{
					{
						Key: []byte("k1"),
					},
				},
			},
		},
	}
	c.AddSignatures(t, initTx)
	c.SendTransactionsToOrderer(t, []*protoblocktx.Tx{initTx})
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		TxIDs: []string{"blind write keys to ns1"}, Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
	})

	return c
}

func TestDependentHappyPath(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name            string
		txs             []*protoblocktx.Tx
		expectedResults *runner.ExpectedStatusInBlock
	}{
		{
			name: "valid transactions: second tx waits due to write-write conflict",
			txs: []*protoblocktx.Tx{
				{
					Id: "performs read only, read-write, and blind-write",
					Namespaces: []*protoblocktx.TxNamespace{
						{
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
					},
				},
				{
					Id: "performs only blind-write",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("k4"),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"performs read only, read-write, and blind-write", "performs only blind-write"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED, protoblocktx.Status_COMMITTED},
			},
		},
		{
			name: "valid transactions: second tx waits due to read-write but uses the updated version",
			txs: []*protoblocktx.Tx{
				{
					Id: "performs read-write and blind-write",
					Namespaces: []*protoblocktx.TxNamespace{
						{
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
					},
				},
				{
					Id: "performs only read-write",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k1"),
									Version: types.Version(2),
									Value:   []byte("v4"),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"performs read-write and blind-write", "performs only read-write"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED, protoblocktx.Status_COMMITTED},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, tx := range tt.txs {
				for _, ns := range tx.Namespaces {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
				c.AddSignatures(t, tx)
			}

			c.SendTransactionsToOrderer(t, tt.txs)
			c.ValidateExpectedResultsInCommittedBlock(t, tt.expectedResults)
		})
	}
	fmt.Println("done")
}

func TestReadOnlyConflictsWithCommittedStates(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name            string
		txID            string
		readsOnly       []*protoblocktx.Read
		expectedResults *runner.ExpectedStatusInBlock
	}{
		{
			name: "readOnly version is nil but the committed version is not nil, i.e., state exist",
			txID: "readOnly stale k1",
			readsOnly: []*protoblocktx.Read{
				{
					Key: []byte("k1"),
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"readOnly stale k1"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
			},
		},
		{
			name: "readOnly version is not nil but the committed version is nil, i.e., state does not exist",
			txID: "readsOnly stale k2",
			readsOnly: []*protoblocktx.Read{
				{
					Key:     []byte("k2"),
					Version: types.Version(0),
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"readsOnly stale k2"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
			},
		},
		{
			name: "readOnly version is different from the committed version",
			txID: "readsOnly different version of k1",
			readsOnly: []*protoblocktx.Read{
				{
					Key:     []byte("k1"),
					Version: types.Version(1),
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"readsOnly different version of k1"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
			},
		},
		{
			name: "valid",
			txID: "valid readOnly",
			readsOnly: []*protoblocktx.Read{
				{
					Key:     []byte("k1"),
					Version: types.Version(0),
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"valid readOnly"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.name)
			tx := &protoblocktx.Tx{
				Id: tt.txID,
				Namespaces: []*protoblocktx.TxNamespace{
					{
						NsId:      "1",
						NsVersion: 0,
						ReadsOnly: tt.readsOnly,
						BlindWrites: []*protoblocktx.Write{
							{
								Key: []byte("k3"),
							},
						},
					},
				},
			}

			c.AddSignatures(t, tx)
			c.SendTransactionsToOrderer(t, []*protoblocktx.Tx{tx})
			c.ValidateExpectedResultsInCommittedBlock(t, tt.expectedResults)
		})
	}
}

func TestReadWriteConflictsWithCommittedStates(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name            string
		txID            string
		readWrites      []*protoblocktx.ReadWrite
		expectedResults *runner.ExpectedStatusInBlock
	}{
		{
			name: "readWrite version is nil but the committed version is not nil, i.e., state exist",
			txID: "readWrite stale k1",
			readWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte("k1"),
					Version: nil,
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"readWrite stale k1"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
			},
		},
		{
			name: "readWrite version is not nil but the committed version is nil, i.e., state does not exist",
			txID: "readWrites stale k2",
			readWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte("k2"),
					Version: types.Version(0),
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"readWrites stale k2"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
			},
		},
		{
			name: "readWrite version is different from the committed version",
			txID: "readWrites different version of k1",
			readWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte("k1"),
					Version: types.Version(1),
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"readWrites different version of k1"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_ABORTED_MVCC_CONFLICT},
			},
		},
		{
			name: "valid",
			txID: "valid readWrite",
			readWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte("k1"),
					Version: types.Version(0),
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"valid readWrite"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.name)
			tx := &protoblocktx.Tx{
				Id: tt.txID,
				Namespaces: []*protoblocktx.TxNamespace{
					{
						NsId:       "1",
						NsVersion:  0,
						ReadWrites: tt.readWrites,
					},
				},
			}

			c.AddSignatures(t, tx)
			c.SendTransactionsToOrderer(t, []*protoblocktx.Tx{tx})
			c.ValidateExpectedResultsInCommittedBlock(t, tt.expectedResults)
		})
	}
}

func TestReadWriteConflictsAmongActiveTransactions(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name            string
		txs             []*protoblocktx.Tx
		expectedResults *runner.ExpectedStatusInBlock
	}{
		{
			name: "first transaction invalidates the second",
			txs: []*protoblocktx.Tx{
				{
					Id: "read-write k1",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k1"),
									Version: types.Version(0),
								},
							},
						},
					},
				},
				{
					Id: "read-write k1 but invalid due to the previous tx",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k1"),
									Version: types.Version(0),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs: []string{
					"read-write k1",
					"read-write k1 but invalid due to the previous tx",
				},
				Statuses: []protoblocktx.Status{
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
			},
		},
		{
			name: "as first and second transactions are invalid, the third succeeds",
			txs: []*protoblocktx.Tx{
				{
					Id: "read-write k1 but wrong version v0",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k1"),
									Version: types.Version(0),
								},
							},
						},
					},
				},
				{
					Id: "read-write k1 but wrong version v2",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k1"),
									Version: types.Version(2),
								},
							},
						},
					},
				},
				{
					Id: "read-write k1 with correct version",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k1"),
									Version: types.Version(1),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs: []string{
					"read-write k1 but wrong version v0",
					"read-write k1 but wrong version v2",
					"read-write k1 with correct version",
				},
				Statuses: []protoblocktx.Status{
					protoblocktx.Status_ABORTED_MVCC_CONFLICT,
					protoblocktx.Status_ABORTED_MVCC_CONFLICT,
					protoblocktx.Status_COMMITTED,
				},
			},
		},
		{
			name: "first transaction writes non-existing key before the second transaction",
			txs: []*protoblocktx.Tx{
				{
					Id: "read-write k2",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k2"),
									Version: nil,
								},
							},
						},
					},
				},
				{
					Id: "read-write k2 but invalid due to the previous tx",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k2"),
									Version: nil,
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs: []string{
					"read-write k2",
					"read-write k2 but invalid due to the previous tx",
				},
				Statuses: []protoblocktx.Status{
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, tx := range tt.txs {
				for _, ns := range tx.Namespaces {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
				c.AddSignatures(t, tx)
			}

			c.SendTransactionsToOrderer(t, tt.txs)
			c.ValidateExpectedResultsInCommittedBlock(t, tt.expectedResults)
		})
	}
}

func TestWriteWriteConflictsAmongActiveTransactions(t *testing.T) {
	t.Parallel()
	c := testSetup(t)

	tests := []struct {
		name            string
		txs             []*protoblocktx.Tx
		expectedResults *runner.ExpectedStatusInBlock
	}{
		{
			name: "first transaction invalidates the second",
			txs: []*protoblocktx.Tx{
				{
					Id: "blind-write k1",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("k1"),
								},
							},
						},
					},
				},
				{
					Id: "read-write k1 but invalid due to the previous tx",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k1"),
									Version: types.Version(0),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs: []string{
					"blind-write k1",
					"read-write k1 but invalid due to the previous tx",
				},
				Statuses: []protoblocktx.Status{
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
			},
		},
		{
			name: "first transaction writes non-existing key before the second transaction",
			txs: []*protoblocktx.Tx{
				{
					Id: "blind-write k2",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("k2"),
								},
							},
						},
					},
				},
				{
					Id: "read-write k2 but invalid due to the previous tx",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("k2"),
									Version: nil,
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs: []string{
					"blind-write k2",
					"read-write k2 but invalid due to the previous tx",
				},
				Statuses: []protoblocktx.Status{
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, tx := range tt.txs {
				for _, ns := range tx.Namespaces {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
				c.AddSignatures(t, tx)
			}

			c.SendTransactionsToOrderer(t, tt.txs)
			c.ValidateExpectedResultsInCommittedBlock(t, tt.expectedResults)
		})
	}
}

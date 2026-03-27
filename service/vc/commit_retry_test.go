/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestCommitRetryOnInsertConflict(t *testing.T) {
	t.Parallel()
	dbEnv := newDatabaseTestEnvWithTablesSetup(t)
	metrics := newVCServiceMetrics()
	c := newCommitter(dbEnv.DB, nil, nil, metrics)

	// Seed namespace "r" with keyr.1 so the insert below causes a conflict.
	dbEnv.populateData(t, []string{"r"}, namespaceToWrites{
		"r": {
			keys:     [][]byte{[]byte("keyr.1")},
			values:   [][]byte{[]byte("valuer.1.0")},
			versions: []uint64{0},
		},
	},
		&committerpb.TxStatusBatch{
			Status: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "seed-tx", 0, 0),
			},
		},
		transactionIDToHeight{"seed-tx": servicepb.NewHeight(0, 0)},
	)

	// tx-conflict inserts keyr.1 (already exists), tx-clean inserts keyr.2.
	vTx := &validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{},
		validTxBlindWrites:    transactionToWrites{},
		newWrites: transactionToWrites{
			"tx-conflict": {
				"r": {
					keys:     [][]byte{[]byte("keyr.1")},
					values:   [][]byte{[]byte("conflict-value")},
					versions: []uint64{0},
				},
			},
			"tx-clean": {
				"r": {
					keys:     [][]byte{[]byte("keyr.2")},
					values:   [][]byte{[]byte("valuer.2.0")},
					versions: []uint64{0},
				},
			},
		},
		invalidTxStatus: map[TxID]committerpb.Status{},
		readToTxIDs: readToTransactions{
			newCmpRead("r", []byte("keyr.1"), nil): {"tx-conflict"},
		},
		txIDToHeight: transactionIDToHeight{
			"tx-conflict": servicepb.NewHeight(5, 0),
			"tx-clean":    servicepb.NewHeight(5, 1),
		},
	}

	txsStatus, err := c.commitTransactions(t.Context(), vTx)
	require.NoError(t, err)
	require.NotNil(t, txsStatus)

	expectedStatuses := []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict", 5, 0),
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-clean", 5, 1),
	}
	test.RequireProtoElementsMatch(t, expectedStatuses, txsStatus.Status)

	dbEnv.rowExists(t, "r", namespaceWrites{
		keys:     [][]byte{[]byte("keyr.1")},
		values:   [][]byte{[]byte("valuer.1.0")},
		versions: []uint64{0},
	})
	dbEnv.rowExists(t, "r", namespaceWrites{
		keys:     [][]byte{[]byte("keyr.2")},
		values:   [][]byte{[]byte("valuer.2.0")},
		versions: []uint64{0},
	})
}

func TestCommitRetryOnDuplicateTxID(t *testing.T) {
	t.Parallel()
	dbEnv := newDatabaseTestEnvWithTablesSetup(t)
	metrics := newVCServiceMetrics()
	c := newCommitter(dbEnv.DB, nil, nil, metrics)

	dbEnv.populateData(t, []string{"d"}, namespaceToWrites{
		"d": {
			keys:     [][]byte{[]byte("keyd.1")},
			values:   [][]byte{[]byte("valued.1.0")},
			versions: []uint64{0},
		},
	},
		&committerpb.TxStatusBatch{
			Status: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-already-done", 2, 0),
			},
		},
		transactionIDToHeight{"tx-already-done": servicepb.NewHeight(2, 0)},
	)

	// Resubmit "tx-already-done" (duplicate TxID) alongside a fresh transaction.
	vTx := &validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{
			"tx-already-done": {
				"d": {
					keys:     [][]byte{[]byte("keyd.1")},
					values:   [][]byte{[]byte("valued.1.1")},
					versions: []uint64{1},
				},
			},
		},
		validTxBlindWrites: transactionToWrites{},
		newWrites: transactionToWrites{
			"tx-fresh": {
				"d": {
					keys:     [][]byte{[]byte("keyd.2")},
					values:   [][]byte{[]byte("valued.2.0")},
					versions: []uint64{0},
				},
			},
		},
		invalidTxStatus: map[TxID]committerpb.Status{},
		readToTxIDs:     readToTransactions{},
		txIDToHeight: transactionIDToHeight{
			"tx-already-done": servicepb.NewHeight(2, 0),
			"tx-fresh":        servicepb.NewHeight(3, 0),
		},
	}

	txsStatus, err := c.commitTransactions(t.Context(), vTx)
	require.NoError(t, err)
	require.NotNil(t, txsStatus)

	expectedStatuses := []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-already-done", 2, 0),
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-fresh", 3, 0),
	}
	test.RequireProtoElementsMatch(t, expectedStatuses, txsStatus.Status)

	// keyd.1 must retain its original value — the duplicate's writes are discarded.
	dbEnv.rowExists(t, "d", namespaceWrites{
		keys:     [][]byte{[]byte("keyd.1")},
		values:   [][]byte{[]byte("valued.1.0")},
		versions: []uint64{0},
	})
	dbEnv.rowExists(t, "d", namespaceWrites{
		keys:     [][]byte{[]byte("keyd.2")},
		values:   [][]byte{[]byte("valued.2.0")},
		versions: []uint64{0},
	})
}

func TestCommitRetryOnInsertConflictAndDuplicateTxID(t *testing.T) {
	t.Parallel()
	dbEnv := newDatabaseTestEnvWithTablesSetup(t)
	metrics := newVCServiceMetrics()
	c := newCommitter(dbEnv.DB, nil, nil, metrics)

	dbEnv.populateData(t, []string{"m"}, namespaceToWrites{
		"m": {
			keys:     [][]byte{[]byte("keym.1")},
			values:   [][]byte{[]byte("valuem.1.0")},
			versions: []uint64{0},
		},
	},
		&committerpb.TxStatusBatch{
			Status: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-dup", 1, 0),
			},
		},
		transactionIDToHeight{"tx-dup": servicepb.NewHeight(1, 0)},
	)

	vTx := &validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{
			"tx-dup": {
				"m": {
					keys:     [][]byte{[]byte("keym.1")},
					values:   [][]byte{[]byte("valuem.1.1")},
					versions: []uint64{1},
				},
			},
		},
		validTxBlindWrites: transactionToWrites{},
		newWrites: transactionToWrites{
			"tx-insert-conflict": {
				"m": {
					keys:     [][]byte{[]byte("keym.1")},
					values:   [][]byte{[]byte("conflict-value")},
					versions: []uint64{0},
				},
			},
			"tx-ok": {
				"m": {
					keys:     [][]byte{[]byte("keym.2")},
					values:   [][]byte{[]byte("valuem.2.0")},
					versions: []uint64{0},
				},
			},
		},
		invalidTxStatus: map[TxID]committerpb.Status{},
		readToTxIDs: readToTransactions{
			newCmpRead("m", []byte("keym.1"), nil): {"tx-insert-conflict"},
		},
		txIDToHeight: transactionIDToHeight{
			"tx-dup":             servicepb.NewHeight(1, 0),
			"tx-insert-conflict": servicepb.NewHeight(4, 0),
			"tx-ok":              servicepb.NewHeight(4, 1),
		},
	}

	txsStatus, err := c.commitTransactions(t.Context(), vTx)
	require.NoError(t, err)
	require.NotNil(t, txsStatus)

	expectedStatuses := []*committerpb.TxStatus{
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-dup", 1, 0),
		committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-insert-conflict", 4, 0),
		committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-ok", 4, 1),
	}
	test.RequireProtoElementsMatch(t, expectedStatuses, txsStatus.Status)

	dbEnv.rowExists(t, "m", namespaceWrites{
		keys:     [][]byte{[]byte("keym.1")},
		values:   [][]byte{[]byte("valuem.1.0")},
		versions: []uint64{0},
	})
	dbEnv.rowExists(t, "m", namespaceWrites{
		keys:     [][]byte{[]byte("keym.2")},
		values:   [][]byte{[]byte("valuem.2.0")},
		versions: []uint64{0},
	})
}

func TestCommitRetryOnDuplicateTxIDWithDifferentHeight(t *testing.T) {
	t.Parallel()
	dbEnv := newDatabaseTestEnvWithTablesSetup(t)
	metrics := newVCServiceMetrics()
	c := newCommitter(dbEnv.DB, nil, nil, metrics)

	dbEnv.populateData(t, []string{"h"}, nil,
		&committerpb.TxStatusBatch{
			Status: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-height-mismatch", 10, 0),
			},
		},
		transactionIDToHeight{"tx-height-mismatch": servicepb.NewHeight(10, 0)},
	)

	// Same TxID resubmitted from a different block height.
	vTx := &validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{},
		validTxBlindWrites:    transactionToWrites{},
		newWrites: transactionToWrites{
			"tx-height-mismatch": {
				"h": {
					keys:     [][]byte{[]byte("keyh.1")},
					values:   [][]byte{[]byte("valueh.1.0")},
					versions: []uint64{0},
				},
			},
		},
		invalidTxStatus: map[TxID]committerpb.Status{},
		readToTxIDs:     readToTransactions{},
		txIDToHeight: transactionIDToHeight{
			"tx-height-mismatch": servicepb.NewHeight(99, 0),
		},
	}

	txsStatus, err := c.commitTransactions(t.Context(), vTx)
	require.NoError(t, err)
	require.NotNil(t, txsStatus)
	require.Len(t, txsStatus.Status, 1)

	assert.Equal(t, committerpb.Status_REJECTED_DUPLICATE_TX_ID, txsStatus.Status[0].Status)
	assert.Equal(t, "tx-height-mismatch", txsStatus.Status[0].Ref.TxId)
}

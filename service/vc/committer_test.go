/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type committerTestEnv struct {
	c            *transactionCommitter
	validatedTxs chan *validatedTransactions
	txStatus     chan *protoblocktx.TransactionsStatus
	dbEnv        *DatabaseTestEnv
}

func newCommitterTestEnv(t *testing.T) *committerTestEnv {
	t.Helper()
	validatedTxs := make(chan *validatedTransactions, 10)
	txStatus := make(chan *protoblocktx.TransactionsStatus, 10)

	dbEnv := newDatabaseTestEnvWithTablesSetup(t)
	metrics := newVCServiceMetrics()
	c := newCommitter(dbEnv.DB, validatedTxs, txStatus, metrics)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return c.run(ctx, 1)
	}, nil)
	return &committerTestEnv{
		c:            c,
		validatedTxs: validatedTxs,
		txStatus:     txStatus,
		dbEnv:        dbEnv,
	}
}

type state struct {
	namespace      string
	keySuffix      int
	updateSequence uint64
}

func writes(isBlind bool, allWrites ...state) namespaceToWrites { //nolint: revive
	ntw := make(namespaceToWrites)
	for _, ww := range allWrites {
		nw := ntw.getOrCreate(ww.namespace)
		var ver uint64
		if !isBlind {
			ver = ww.updateSequence
		}
		nw.append(
			fmt.Appendf(nil, "key%s.%d", ww.namespace, ww.keySuffix),
			fmt.Appendf(nil, "value%s.%d.%d", ww.namespace, ww.keySuffix, ww.updateSequence),
			ver,
		)
	}
	return ntw
}

func TestCommit(t *testing.T) { //nolint:maintidx // cannot improve.
	t.Parallel()
	env := newCommitterTestEnv(t)

	env.dbEnv.populateData(
		t,
		[]string{"1", "2"},
		writes(
			false,
			state{"1", 1, 1},
			state{"1", 2, 1},
			state{"1", 3, 2},
			state{"1", 4, 2},
			state{"2", 1, 0},
			state{"2", 2, 0},
			state{"2", 3, 1},
			state{"2", 4, 1},
		),
		&protoblocktx.TransactionsStatus{
			Status: map[string]*protoblocktx.StatusWithHeight{
				"tx1": types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 1, 1),
				"tx2": types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 1, 2),
			},
		},
		transactionIDToHeight{
			"tx1": types.NewHeight(1, 1),
			"tx2": types.NewHeight(1, 2),
		},
	)

	// Note: the order of the subtest is important
	tests := []struct {
		name                      string
		txs                       *validatedTransactions
		expectedTxStatuses        map[string]*protoblocktx.StatusWithHeight
		expectedNsRows            namespaceToWrites
		unexpectedNsRows          namespaceToWrites
		expectedMaxSeenBlocNumber uint64
	}{
		{
			name: "new writes",
			txs: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{},
				validTxBlindWrites:    transactionToWrites{},
				newWrites: transactionToWrites{
					"tx-new-1": writes(
						true,
						state{"1", 10, 0},
						state{"1", 11, 0},
						state{"2", 10, 0},
						state{"2", 11, 0},
					),
					"tx-new-2": writes(
						true,
						state{"1", 20, 0},
						state{"1", 21, 0},
						state{"2", 20, 0},
						state{"2", 21, 0},
					),
				},
				invalidTxStatus: map[TxID]protoblocktx.Status{},
				txIDToHeight: transactionIDToHeight{
					"tx-new-1": types.NewHeight(1, 1),
					"tx-new-2": types.NewHeight(244, 2),
				},
			},
			expectedTxStatuses: map[string]*protoblocktx.StatusWithHeight{
				"tx-new-1": types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 1, 1),
				"tx-new-2": types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 244, 2),
			},
			expectedNsRows: writes(
				false,
				state{"1", 10, 0},
				state{"1", 11, 0},
				state{"2", 10, 0},
				state{"2", 11, 0},
				state{"1", 20, 0},
				state{"1", 21, 0},
				state{"2", 20, 0},
				state{"2", 21, 0},
			),
			expectedMaxSeenBlocNumber: 244,
		},
		{
			name: "non-blind writes",
			txs: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{
					"tx-non-blind-1": writes(
						false,
						state{"1", 1, 2},
						state{"1", 2, 2},
						state{"2", 1, 1},
						state{"2", 2, 1},
					),
				},
				validTxBlindWrites: transactionToWrites{},
				newWrites:          transactionToWrites{},
				invalidTxStatus:    map[TxID]protoblocktx.Status{},
				txIDToHeight: transactionIDToHeight{
					"tx-non-blind-1": types.NewHeight(239, 1),
				},
			},
			expectedTxStatuses: map[string]*protoblocktx.StatusWithHeight{
				"tx-non-blind-1": types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 239, 1),
			},
			expectedNsRows: writes(
				false,
				state{"1", 1, 2},
				state{"1", 2, 2},
				state{"2", 1, 1},
				state{"2", 2, 1},
			),
			expectedMaxSeenBlocNumber: 244,
		},
		{
			name: "blind writes",
			txs: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{},
				validTxBlindWrites: transactionToWrites{
					"tx-blind-1": writes(
						true,
						state{"1", 1, 3},
						state{"1", 2, 3},
						state{"2", 1, 2},
						state{"2", 2, 2},
					),
				},
				newWrites:       transactionToWrites{},
				invalidTxStatus: map[TxID]protoblocktx.Status{},
				txIDToHeight: transactionIDToHeight{
					"tx-blind-1": types.NewHeight(1024, 1),
				},
			},
			expectedTxStatuses: map[string]*protoblocktx.StatusWithHeight{
				"tx-blind-1": types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 1024, 1),
			},
			expectedNsRows: writes(
				false,
				state{"1", 1, 3},
				state{"1", 2, 3},
				state{"2", 1, 2},
				state{"2", 2, 2},
			),
			expectedMaxSeenBlocNumber: 1024,
		},
		{
			name: "blind, non-blind, and new writes",
			txs: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{
					"tx-all-1": writes(
						false,
						state{"1", 1, 4},
						state{"1", 2, 4},
					),
					"tx-all-2": writes(
						false,
						state{"1", 3, 3},
						state{"1", 4, 3},
					),
				},
				validTxBlindWrites: transactionToWrites{
					"tx-all-1": writes(
						true,
						state{"2", 1, 3},
						state{"2", 2, 3},
						state{"2", 5, 0},
					),
					"tx-all-2": writes(
						true,
						state{"2", 3, 2},
						state{"2", 4, 2},
						state{"2", 6, 0},
					),
				},
				newWrites: transactionToWrites{
					"tx-all-1": writes(
						true,
						state{"2", 30, 0},
					),
					"tx-all-2": writes(
						true,
						state{"2", 31, 0},
					),
				},
				invalidTxStatus: map[TxID]protoblocktx.Status{
					"tx-conflict-1": protoblocktx.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-2": protoblocktx.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-3": protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
				txIDToHeight: transactionIDToHeight{
					"tx-all-1":      types.NewHeight(5, 1),
					"tx-all-2":      types.NewHeight(200, 2),
					"tx-conflict-1": types.NewHeight(1, 1),
					"tx-conflict-2": types.NewHeight(396, 2),
					"tx-conflict-3": types.NewHeight(396, 3),
				},
			},
			expectedTxStatuses: map[string]*protoblocktx.StatusWithHeight{
				"tx-all-1":      types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 5, 1),
				"tx-all-2":      types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 200, 2),
				"tx-conflict-1": types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 1, 1),
				"tx-conflict-2": types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 396, 2),
				"tx-conflict-3": types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 396, 3),
			},
			expectedNsRows: writes(
				false,
				state{"1", 1, 4},
				state{"1", 2, 4},
				state{"1", 3, 3},
				state{"1", 4, 3},
				state{"2", 1, 3},
				state{"2", 2, 3},
				state{"2", 3, 2},
				state{"2", 4, 2},
				state{"2", 5, 0},
				state{"2", 6, 0},
				state{"2", 30, 0},
				state{"2", 31, 0},
			),
			expectedMaxSeenBlocNumber: 1024,
		},
		{
			name: "new writes with violating",
			txs: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{
					"tx-not-violate-1": writes(
						false,
						state{"1", 1, 5},
					),
					"tx-violate-1": writes(
						true,
						state{"1", 3, 4},
					),
				},
				validTxBlindWrites: transactionToWrites{
					"tx-not-violate-1": writes(
						false,
						state{"1", 2, 5},
					),
					"tx-violate-1": writes(
						true,
						state{"1", 4, 4},
					),
				},
				newWrites: transactionToWrites{
					"tx-not-violate-1": writes(
						true,
						state{"1", 22, 0}, // not violate
					),
					"tx-violate-1": writes(
						true,
						state{"1", 10, 0}, // violate
						state{"1", 12, 0}, // not violate
					),
				},
				invalidTxStatus: map[TxID]protoblocktx.Status{
					"tx-conflict-4": protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
				readToTxIDs: map[comparableRead][]TxID{
					newCmpRead("1", []byte("key1.10"), nil): {"tx-violate-1"},
				},
				txIDToHeight: transactionIDToHeight{
					"tx-violate-1":     types.NewHeight(1, 1),
					"tx-not-violate-1": types.NewHeight(4, 2),
					"tx-conflict-4":    types.NewHeight(1000, 3),
				},
			},
			expectedTxStatuses: map[string]*protoblocktx.StatusWithHeight{
				"tx-violate-1":     types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 1, 1),
				"tx-not-violate-1": types.CreateStatusWithHeight(protoblocktx.Status_COMMITTED, 4, 2),
				"tx-conflict-4":    types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 1000, 3),
			},
			expectedNsRows: writes(
				false,
				state{"1", 1, 5},
				state{"1", 2, 5},
				state{"1", 3, 3},
				state{"1", 4, 3},
				state{"1", 10, 0},
				state{"1", 22, 0},
			),
			unexpectedNsRows: writes(
				false,
				state{"1", 12, 0},
			),
			expectedMaxSeenBlocNumber: 10000,
		},
		{
			name: "all invalid txs",
			txs: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{
					"tx1": writes(false, state{"2", 7, 1}),
				},
				validTxBlindWrites: transactionToWrites{
					"tx1": writes(true, state{"1", 1, 6}),
				},
				newWrites: transactionToWrites{
					"tx1": writes(true, state{"1", 40, 0}),
				},
				invalidTxStatus: map[TxID]protoblocktx.Status{
					"tx-conflict-10": protoblocktx.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-11": protoblocktx.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-12": protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
				txIDToHeight: transactionIDToHeight{
					"tx1":            types.NewHeight(1, 5),
					"tx-conflict-10": types.NewHeight(1, 1),
					"tx-conflict-11": types.NewHeight(4, 2),
					"tx-conflict-12": types.NewHeight(66000, 3),
				},
			},
			expectedTxStatuses: map[string]*protoblocktx.StatusWithHeight{
				"tx1":            types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_DUPLICATE_TXID, 1, 5),
				"tx-conflict-10": types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 1, 1),
				"tx-conflict-11": types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 4, 2),
				"tx-conflict-12": types.CreateStatusWithHeight(protoblocktx.Status_ABORTED_MVCC_CONFLICT, 66000, 3),
			},
			expectedNsRows: writes(
				false,
				state{"1", 1, 5},
			),
			unexpectedNsRows: writes(
				false,
				state{"2", 7, 0},
				state{"1", 40, 0},
			),
			expectedMaxSeenBlocNumber: 66000,
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	for _, tt := range tests { //nolint:paralleltest // each test case depends on the previous test case.
		t.Run(tt.name, func(t *testing.T) {
			channel.NewWriter(ctx, env.validatedTxs).Write(tt.txs)
			txStatus, ok := channel.NewReader(ctx, env.txStatus).Read()
			require.True(t, ok)
			require.Equal(t, &protoblocktx.TransactionsStatus{Status: tt.expectedTxStatuses}, txStatus)
			for nsID, expectedRows := range tt.expectedNsRows {
				env.dbEnv.rowExists(t, nsID, *expectedRows)
			}
			for nsID, expectedRows := range tt.unexpectedNsRows {
				env.dbEnv.rowNotExists(t, nsID, expectedRows.keys)
			}
			env.dbEnv.StatusExistsForNonDuplicateTxID(t, tt.expectedTxStatuses)
		})
	}
}

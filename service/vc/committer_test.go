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

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type committerTestEnv struct {
	c            *transactionCommitter
	validatedTxs chan *validatedTransactions
	txStatus     chan *committerpb.TxStatusBatch
	dbEnv        *DatabaseTestEnv
}

func newCommitterTestEnv(t *testing.T) *committerTestEnv {
	t.Helper()
	validatedTxs := make(chan *validatedTransactions, 10)
	txStatus := make(chan *committerpb.TxStatusBatch, 10)

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
		&committerpb.TxStatusBatch{
			Status: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx1", 1, 1),
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx2", 1, 2),
			},
		},
		transactionIDToHeight{
			"tx1": servicepb.NewHeight(1, 1),
			"tx2": servicepb.NewHeight(1, 2),
		},
	)

	// Note: the order of the subtest is important
	tests := []struct {
		name                      string
		txs                       *validatedTransactions
		expectedTxStatuses        []*committerpb.TxStatus
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
				invalidTxStatus: map[TxID]committerpb.Status{},
				txIDToHeight: transactionIDToHeight{
					"tx-new-1": servicepb.NewHeight(1, 1),
					"tx-new-2": servicepb.NewHeight(244, 2),
				},
			},
			expectedTxStatuses: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-new-1", 1, 1),
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-new-2", 244, 2),
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
				invalidTxStatus:    map[TxID]committerpb.Status{},
				txIDToHeight: transactionIDToHeight{
					"tx-non-blind-1": servicepb.NewHeight(239, 1),
				},
			},
			expectedTxStatuses: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-non-blind-1", 239, 1),
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
				invalidTxStatus: map[TxID]committerpb.Status{},
				txIDToHeight: transactionIDToHeight{
					"tx-blind-1": servicepb.NewHeight(1024, 1),
				},
			},
			expectedTxStatuses: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-blind-1", 1024, 1),
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
				invalidTxStatus: map[TxID]committerpb.Status{
					"tx-conflict-1": committerpb.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-2": committerpb.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-3": committerpb.Status_ABORTED_MVCC_CONFLICT,
				},
				txIDToHeight: transactionIDToHeight{
					"tx-all-1":      servicepb.NewHeight(5, 1),
					"tx-all-2":      servicepb.NewHeight(200, 2),
					"tx-conflict-1": servicepb.NewHeight(1, 1),
					"tx-conflict-2": servicepb.NewHeight(396, 2),
					"tx-conflict-3": servicepb.NewHeight(396, 3),
				},
			},
			expectedTxStatuses: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-all-1", 5, 1),
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-all-2", 200, 2),
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict-1", 1, 1),
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict-2", 396, 2),
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict-3", 396, 3),
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
				invalidTxStatus: map[TxID]committerpb.Status{
					"tx-conflict-4": committerpb.Status_ABORTED_MVCC_CONFLICT,
				},
				readToTxIDs: map[comparableRead][]TxID{
					newCmpRead("1", []byte("key1.10"), nil): {"tx-violate-1"},
				},
				txIDToHeight: transactionIDToHeight{
					"tx-violate-1":     servicepb.NewHeight(1, 1),
					"tx-not-violate-1": servicepb.NewHeight(4, 2),
					"tx-conflict-4":    servicepb.NewHeight(1000, 3),
				},
			},
			expectedTxStatuses: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-violate-1", 1, 1),
				committerpb.NewTxStatus(committerpb.Status_COMMITTED, "tx-not-violate-1", 4, 2),
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict-4", 1000, 3),
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
				invalidTxStatus: map[TxID]committerpb.Status{
					"tx-conflict-10": committerpb.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-11": committerpb.Status_ABORTED_MVCC_CONFLICT,
					"tx-conflict-12": committerpb.Status_ABORTED_MVCC_CONFLICT,
				},
				txIDToHeight: transactionIDToHeight{
					"tx1":            servicepb.NewHeight(1, 5),
					"tx-conflict-10": servicepb.NewHeight(1, 1),
					"tx-conflict-11": servicepb.NewHeight(4, 2),
					"tx-conflict-12": servicepb.NewHeight(66000, 3),
				},
			},
			expectedTxStatuses: []*committerpb.TxStatus{
				committerpb.NewTxStatus(committerpb.Status_REJECTED_DUPLICATE_TX_ID, "tx1", 1, 5),
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict-10", 1, 1),
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict-11", 4, 2),
				committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "tx-conflict-12", 66000, 3),
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
			test.RequireProtoElementsMatch(t, tt.expectedTxStatuses, txStatus.Status)
			for nsID, expectedRows := range tt.expectedNsRows {
				env.dbEnv.rowExists(t, nsID, *expectedRows)
			}
			for nsID, expectedRows := range tt.unexpectedNsRows {
				env.dbEnv.rowNotExists(t, nsID, expectedRows.keys)
			}
			env.dbEnv.StatusExistsForNonDuplicateTxID(ctx, t, tt.expectedTxStatuses)
		})
	}
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

type validatorTestEnv struct {
	v            *transactionValidator
	preparedTxs  chan *preparedTransactions
	validatedTxs chan *validatedTransactions
	txStatus     chan *servicepb.TxStatusBatch
	dbEnv        *DatabaseTestEnv
}

// newValidatorTestEnv starts a validator wired to a fresh DB. When withCommitter is
// true, a committer is chained onto validatedTxs too, so tests can exercise gating
// logic that spans both (e.g. the snapshot gate, which the validator applies before
// the committer's createSnapshotIfPresent runs) via txStatus. withCommitter must be
// false for tests that read validatedTxs directly (e.g. TestValidate): a running
// committer would otherwise drain that channel out from under the test.
//
//nolint:revive // flag-parameter: withCommitter selects test topology, not runtime behavior.
func newValidatorTestEnv(t *testing.T, withCommitter bool) *validatorTestEnv {
	t.Helper()
	preparedTxs := make(chan *preparedTransactions, 10)
	validatedTxs := make(chan *validatedTransactions, 10)
	var txStatus chan *servicepb.TxStatusBatch

	dbEnv := NewDatabaseTestEnv(t)
	metrics := newVCServiceMetrics(&queues{preparedTxs: preparedTxs, validatedTxs: validatedTxs})
	v := newValidator(preparedTxs, validatedTxs, metrics)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return v.run(ctx, dbEnv.DB, 1)
	}, nil)

	if withCommitter {
		txStatus = make(chan *servicepb.TxStatusBatch, 10)
		c := newCommitter(validatedTxs, txStatus, metrics)
		test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
			return c.run(ctx, dbEnv.DB, 1)
		}, nil)
	}

	return &validatorTestEnv{
		v:            v,
		preparedTxs:  preparedTxs,
		validatedTxs: validatedTxs,
		txStatus:     txStatus,
		dbEnv:        dbEnv,
	}
}

func TestValidate(t *testing.T) { //nolint:maintidx // cannot improve.
	t.Parallel()

	env := newValidatorTestEnv(t, false)

	k1_1 := []byte("key1.1")
	k1_2 := []byte("key1.2")
	k1_3 := []byte("key1.3")
	k1_4 := []byte("key1.4")
	k1_5 := []byte("key1.5")
	k1_6 := []byte("key1.6")
	k2_1 := []byte("key2.1")
	k2_2 := []byte("key2.2")
	k2_3 := []byte("key2.3")
	k2_4 := []byte("key2.4")
	k2_5 := []byte("key2.5")

	env.dbEnv.populateData(
		t,
		[]string{"1", "2"},
		namespaceToWrites{
			"1": {
				keys:     [][]byte{k1_1, k1_2, k1_3, k1_4},
				values:   [][]byte{[]byte("value1.1"), []byte("value1.2"), []byte("value1.3"), []byte("value1.4")},
				versions: []uint64{1, 1, 2, 2},
			},
			"2": {
				keys:     [][]byte{k2_1, k2_2, k2_3, k2_4},
				values:   [][]byte{[]byte("value2.1"), []byte("value2.2"), []byte("value2.3"), []byte("value2.4")},
				versions: []uint64{0, 0, 1, 1},
			},
		},
		nil,
		nil,
	)

	tx1NonBlindWrites := namespaceToWrites{
		"1": {
			keys:     [][]byte{k1_1},
			values:   [][]byte{[]byte("value1.1.1")},
			versions: []uint64{2},
		},
		"2": {
			keys:     [][]byte{k2_1},
			values:   [][]byte{[]byte("value1.1.1")},
			versions: []uint64{2},
		},
	}
	tx2NonBlindWrites := namespaceToWrites{
		"1": {
			keys:     [][]byte{k1_5},
			values:   [][]byte{[]byte("value1.5.1")},
			versions: []uint64{0},
		},
	}
	tx3NonBlindWrites := namespaceToWrites{
		"2": {
			keys:     [][]byte{k2_2},
			values:   [][]byte{[]byte("value2.2.1")},
			versions: []uint64{2},
		},
	}
	tx3BlindWrites := namespaceToWrites{
		"1": {
			keys:   [][]byte{k1_6},
			values: [][]byte{[]byte("value1.6")},
			// This version value will not be used because we do not assign the version
			// when inserting a new key. We use the DB default value instead, which is 0.
			versions: []uint64{0},
		},
	}

	// Note: the order of the sub-test is important
	tests := []struct {
		name                string
		preparedTx          *preparedTransactions
		expectedValidatedTx *validatedTransactions
	}{
		{
			name: "all valid tx",
			preparedTx: &preparedTransactions{
				nsToReads: namespaceToReads{
					"1": &reads{
						keys:     [][]byte{k1_1, k1_2, k1_5},
						versions: []*uint64{new(uint64(1)), new(uint64(1)), nil},
					},
					"2": &reads{
						keys:     [][]byte{k2_1, k2_2, k2_5},
						versions: []*uint64{new(uint64(0)), new(uint64(0)), nil},
					},
					committerpb.MetaNamespaceID: &reads{
						keys:     [][]byte{[]byte("1"), []byte("2")},
						versions: []*uint64{new(uint64(0)), new(uint64(0))},
					},
				},
				readToTxIDs: readToTransactions{
					newCmpRead("1", k1_1, new(uint64(1))):                                []TxID{txs[0]},
					newCmpRead("1", k1_2, new(uint64(1))):                                []TxID{txs[0]},
					newCmpRead("1", k1_5, nil):                                           []TxID{txs[1]},
					newCmpRead("2", k2_1, new(uint64(0))):                                []TxID{txs[0]},
					newCmpRead("2", k2_2, new(uint64(0))):                                []TxID{txs[2]},
					newCmpRead("2", k2_5, nil):                                           []TxID{txs[2]},
					newCmpRead(committerpb.MetaNamespaceID, []byte("1"), new(uint64(0))): []TxID{txs[0], txs[1]},
					newCmpRead(committerpb.MetaNamespaceID, []byte("2"), new(uint64(0))): []TxID{txs[0], txs[2]},
				},
				txIDToNsNonBlindWrites: transactionToWrites{
					txs[0]: tx1NonBlindWrites,
					txs[1]: tx2NonBlindWrites,
					txs[2]: tx3NonBlindWrites,
				},
				txIDToNsBlindWrites: transactionToWrites{
					txs[2]: tx3BlindWrites,
				},
				invalidTxIDStatus: make(map[TxID]committerpb.Status),
				txIDToHeight: transactionIDToHeight{
					txs[0]: servicepb.NewHeight(1, 1),
					txs[1]: servicepb.NewHeight(4, 2),
					txs[2]: servicepb.NewHeight(4, 3),
				},
			},
			expectedValidatedTx: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{
					txs[0]: tx1NonBlindWrites,
					txs[1]: tx2NonBlindWrites,
					txs[2]: tx3NonBlindWrites,
				},
				validTxBlindWrites: transactionToWrites{
					txs[2]: tx3BlindWrites,
				},
				invalidTxStatus: map[TxID]committerpb.Status{},
				txIDToHeight: transactionIDToHeight{
					txs[0]: servicepb.NewHeight(1, 1),
					txs[1]: servicepb.NewHeight(4, 2),
					txs[2]: servicepb.NewHeight(4, 3),
				},
			},
		},
		{
			name: "all invalid tx",
			preparedTx: &preparedTransactions{
				nsToReads: namespaceToReads{
					"1": &reads{
						keys:     [][]byte{k1_1, k1_2, k1_5},
						versions: []*uint64{new(uint64(0)), new(uint64(0)), new(uint64(1))},
					},
					"2": &reads{
						keys:     [][]byte{k2_1, k2_2, k2_5},
						versions: []*uint64{nil, nil, nil},
					},
					committerpb.MetaNamespaceID: &reads{
						keys:     [][]byte{[]byte("1"), []byte("2")},
						versions: []*uint64{new(uint64(1)), new(uint64(1))},
					},
				},
				readToTxIDs: readToTransactions{
					newCmpRead("1", k1_1, new(uint64(0))):                                []TxID{txs[0]},
					newCmpRead("1", k1_2, new(uint64(0))):                                []TxID{txs[0]},
					newCmpRead("1", k1_5, new(uint64(1))):                                []TxID{txs[1]},
					newCmpRead("2", k2_1, nil):                                           []TxID{txs[0]},
					newCmpRead("2", k2_2, nil):                                           []TxID{txs[2]},
					newCmpRead("2", k2_5, nil):                                           []TxID{txs[2]},
					newCmpRead(committerpb.MetaNamespaceID, []byte("1"), new(uint64(1))): txs[:2],
					newCmpRead(committerpb.MetaNamespaceID, []byte("2"), new(uint64(1))): txs[:2],
				},
				txIDToNsNonBlindWrites: transactionToWrites{
					txs[0]: tx1NonBlindWrites,
					txs[1]: tx2NonBlindWrites,
					txs[2]: tx3NonBlindWrites,
				},
				txIDToNsBlindWrites: transactionToWrites{
					txs[2]: tx3BlindWrites,
				},
				invalidTxIDStatus: make(map[TxID]committerpb.Status),
				txIDToHeight: transactionIDToHeight{
					txs[0]: servicepb.NewHeight(1, 1),
					txs[1]: servicepb.NewHeight(5, 2),
					txs[2]: servicepb.NewHeight(5, 3),
				},
			},
			expectedValidatedTx: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{},
				validTxBlindWrites:    transactionToWrites{},
				invalidTxStatus: map[TxID]committerpb.Status{
					txs[0]: committerpb.Status_ABORTED_MVCC_CONFLICT,
					txs[1]: committerpb.Status_ABORTED_MVCC_CONFLICT,
					txs[2]: committerpb.Status_ABORTED_MVCC_CONFLICT,
				},
				txIDToHeight: transactionIDToHeight{
					txs[0]: servicepb.NewHeight(1, 1),
					txs[1]: servicepb.NewHeight(5, 2),
					txs[2]: servicepb.NewHeight(5, 3),
				},
			},
		},
		{
			name: "valid and invalid tx",
			preparedTx: &preparedTransactions{
				nsToReads: namespaceToReads{
					"1": &reads{
						keys:     [][]byte{k1_1, k1_2, k1_5},
						versions: []*uint64{new(uint64(1)), new(uint64(1)), nil},
					},
					"2": &reads{
						keys:     [][]byte{k2_1, k2_2, k2_5},
						versions: []*uint64{nil, nil, nil},
					},
					committerpb.MetaNamespaceID: &reads{
						keys: [][]byte{
							[]byte("1"),
							[]byte("2"),
							[]byte("2"),
						},
						versions: []*uint64{new(uint64(0)), new(uint64(0)), new(uint64(1))},
					},
				},
				readToTxIDs: readToTransactions{
					newCmpRead("1", k1_1, new(uint64(1))):                                []TxID{txs[0]},
					newCmpRead("1", k1_2, new(uint64(1))):                                []TxID{txs[0]},
					newCmpRead("1", k1_5, nil):                                           []TxID{txs[1]},
					newCmpRead("2", k2_1, nil):                                           []TxID{txs[0]},
					newCmpRead("2", k2_2, nil):                                           []TxID{txs[2]},
					newCmpRead("2", k2_5, nil):                                           []TxID{txs[2]},
					newCmpRead(committerpb.MetaNamespaceID, []byte("1"), new(uint64(0))): []TxID{txs[0], txs[1]},
					newCmpRead(committerpb.MetaNamespaceID, []byte("2"), new(uint64(0))): []TxID{txs[0], txs[2]},
					newCmpRead(committerpb.MetaNamespaceID, []byte("2"), new(uint64(1))): []TxID{txs[3]},
				},
				txIDToNsNonBlindWrites: transactionToWrites{
					txs[0]: tx1NonBlindWrites,
					txs[1]: tx2NonBlindWrites,
					txs[2]: tx3NonBlindWrites,
				},
				txIDToNsBlindWrites: transactionToWrites{
					txs[2]: tx3BlindWrites,
				},
				invalidTxIDStatus: map[TxID]committerpb.Status{
					txs[4]: committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE,
					txs[5]: committerpb.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED,
				},
				txIDToHeight: transactionIDToHeight{
					txs[0]: servicepb.NewHeight(1, 1),
					txs[1]: servicepb.NewHeight(4, 2),
					txs[2]: servicepb.NewHeight(4, 3),
					txs[3]: servicepb.NewHeight(6, 3),
					txs[4]: servicepb.NewHeight(7, 3),
					txs[5]: servicepb.NewHeight(7, 4),
				},
			},
			expectedValidatedTx: &validatedTransactions{
				validTxNonBlindWrites: transactionToWrites{
					txs[1]: tx2NonBlindWrites,
				},
				validTxBlindWrites: transactionToWrites{},
				invalidTxStatus: map[TxID]committerpb.Status{
					txs[0]: committerpb.Status_ABORTED_MVCC_CONFLICT,
					txs[2]: committerpb.Status_ABORTED_MVCC_CONFLICT,
					txs[3]: committerpb.Status_ABORTED_MVCC_CONFLICT,
					txs[4]: committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE,
					txs[5]: committerpb.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED,
				},
				txIDToHeight: transactionIDToHeight{
					txs[0]: servicepb.NewHeight(1, 1),
					txs[1]: servicepb.NewHeight(4, 2),
					txs[2]: servicepb.NewHeight(4, 3),
					txs[3]: servicepb.NewHeight(6, 3),
					txs[4]: servicepb.NewHeight(7, 3),
					txs[5]: servicepb.NewHeight(7, 4),
				},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // each test case depends on the previous test.
		t.Run(tt.name, func(t *testing.T) {
			channel.NewWriter(t.Context(), env.preparedTxs).Write(tt.preparedTx)
			validatedTxs, ok := channel.NewReader(t.Context(), env.validatedTxs).Read()
			require.True(t, ok)
			require.Equal(t, tt.expectedValidatedTx.validTxNonBlindWrites, validatedTxs.validTxNonBlindWrites)
			require.Equal(t, tt.expectedValidatedTx.validTxBlindWrites, validatedTxs.validTxBlindWrites)
			require.Equal(t, tt.expectedValidatedTx.invalidTxStatus, validatedTxs.invalidTxStatus)
		})
	}
}

func TestSnapshotGateEndToEnd(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)
	writer := channel.NewWriter(ctx, env.preparedTxs)
	reader := channel.NewReader(ctx, env.txStatus)

	// First snapshot commits (PENDING, not yet checkpointed).
	prepTx1, _ := newIncomingSnapshotPreparedTx(t, env.dbEnv.DB, 700001, "gate-first")
	writer.Write(prepTx1)
	s1, ok := reader.Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s1.Status[0].Status)

	// Second, different snapshot is rejected because first is not CHECKPOINTED.
	secondTxID := "gate-second"
	prepTx2, secondName := newIncomingSnapshotPreparedTx(t, env.dbEnv.DB, 700002, secondTxID)
	writer.Write(prepTx2)
	s2, ok := reader.Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_REJECTED_SNAPSHOT_IN_PROGRESS, s2.Status[0].Status)

	// No database and no _snapshot record for the rejected request.
	require.False(t, cloneExists(t, env.dbEnv.DB, secondName))
	rows := env.dbEnv.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{[]byte(secondTxID)})
	require.Empty(t, rows)
}

func newIncomingSnapshotPreparedTx(
	t *testing.T, db *database, blockNum uint64, txID string,
) (*preparedTransactions, string) {
	t.Helper()
	ref := &committerpb.TxRef{BlockNum: blockNum, TxNum: 0, TxId: txID}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, db, name)

	value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
	require.NoError(t, err)

	nws := make(transactionToWrites)
	nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)

	prepTxs := &preparedTransactions{
		nsToReads:              namespaceToReads{},
		readToTxIDs:            readToTransactions{},
		txIDToNsNonBlindWrites: transactionToWrites{},
		txIDToNsBlindWrites:    transactionToWrites{},
		txIDToNsNewWrites:      nws,
		invalidTxIDStatus:      map[TxID]committerpb.Status{},
		txIDToHeight:           transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
	}
	return prepTxs, name
}

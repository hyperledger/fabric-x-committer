/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestSnapshotDatabaseName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		blockNum uint64
		want     string
	}{
		{name: "zero", blockNum: 0, want: "snapshot_0"},
		{name: "typical", blockNum: 42, want: "snapshot_42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, snapshotDatabaseName(&committerpb.TxRef{BlockNum: tc.blockNum}))
		})
	}
}

func TestCreateSnapshotDatabase(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.DBConf.Database)
	ctx, _ := createContext(t)
	ref := &committerpb.TxRef{BlockNum: 1234567, TxNum: 0, TxId: "snap-clone-1"}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, env.DB, name)

	// First creation succeeds and snapshot database exists.
	require.NoError(t, env.DB.createSnapshotDatabase(ctx, name))
	require.True(t, cloneExists(t, env.DB, name))

	// Second creation over existing database is a no-op success (reuse), not drop+recreate.
	require.NoError(t, env.DB.createSnapshotDatabase(ctx, name))
	require.True(t, cloneExists(t, env.DB, name))
}

func cloneExists(t *testing.T, db *database, name string) bool {
	t.Helper()
	// createSnapshotDatabase's PostgreSQL path terminates all connections to source
	// database (see createPostgresSnapshotDatabase), which can include this pool's own connection
	// to db.config.Database, not just the clone name being polled for; the very next
	// pool acquisition can hit that severed connection (SQLSTATE 57P01) before pgxpool
	// notices and redials. Production callers tolerate this via db.retryProfile
	// (e.g. commit's post-clone follow-up query); do the same here instead of a bare query.
	exists, err := retry.ExecuteWithResult(t.Context(), db.retryProfile, func() (bool, error) {
		var exists bool
		err := db.pool.QueryRow(t.Context(),
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
		return exists, err
	})
	require.NoError(t, err)
	return exists
}

func dropCloneCleanup(t *testing.T, db *database, name string) {
	t.Helper()
	t.Cleanup(func() {
		sql := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pgx.Identifier{name}.Sanitize())
		_ = db.adminExec(context.Background(), sql)
	})
}

func TestCommitSnapshotTxCreatesCloneAndPendingRow(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	ref := &committerpb.TxRef{BlockNum: 987654, TxNum: 1, TxId: "snap-e2e-1"}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, env.dbEnv.DB, name)

	// Preparer routes _snapshot record as a new write: key=txId, value=SnapshotState{TxRef}.
	value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
	require.NoError(t, err)

	newWrites := make(transactionToWrites)
	nw := newWrites.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID)
	nw.append([]byte(ref.TxId), value, 0)

	vTx := &validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{},
		validTxBlindWrites:    transactionToWrites{},
		newWrites:             newWrites,
		readToTxIDs:           readToTransactions{},
		invalidTxStatus:       map[TxID]committerpb.Status{},
		txIDToHeight:          transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
	}

	channel.NewWriter(ctx, env.validatedTxs).Write(vTx)

	// Committed status is returned.
	status, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	require.Len(t, status.Status, 1)
	require.Equal(t, committerpb.Status_COMMITTED, status.Status[0].Status)

	// Snapshot database exists.
	require.True(t, cloneExists(t, env.dbEnv.DB, name))

	// Committed _snapshot record is PENDING with clone_database set.
	rows := env.dbEnv.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{[]byte(ref.TxId)})
	stored := rows[ref.TxId]
	require.NotNil(t, stored)
	var got committerpb.SnapshotState
	require.NoError(t, proto.Unmarshal(stored.Value, &got))
	require.Equal(t, committerpb.SnapshotState_PENDING, got.Status)
	require.Equal(t, name, got.CloneDatabase)
	require.Equal(t, ref.TxId, got.TxRef.TxId)
}

func TestSnapshotDatabaseFailureReturnsError(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)
	ctx, _ := createContext(t)

	// Force database-creation failure by pointing source DB name at nonexistent DB.
	env.DB.config.Database = "definitely_not_a_real_source_db_name"

	ref := &committerpb.TxRef{BlockNum: 555, TxNum: 0, TxId: "snap-fail-1"}
	value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
	require.NoError(t, err)
	nws := make(transactionToWrites)
	nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)

	err = env.DB.createSnapshotIfPresent(ctx, nws)
	require.ErrorContains(t, err, "failed to create snapshot database")
	require.False(t, cloneExists(t, env.DB, snapshotDatabaseName(ref)))
}

func TestSnapshotDuplicateTxIDIsIdempotent(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)
	writer := channel.NewWriter(ctx, env.validatedTxs)
	reader := channel.NewReader(ctx, env.txStatus)

	ref := &committerpb.TxRef{BlockNum: 222333, TxNum: 0, TxId: "snap-dup-1"}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, env.dbEnv.DB, name)

	build := func() *validatedTransactions {
		value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
		require.NoError(t, err)
		nws := make(transactionToWrites)
		nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)
		return &validatedTransactions{
			validTxNonBlindWrites: transactionToWrites{},
			validTxBlindWrites:    transactionToWrites{},
			newWrites:             nws,
			readToTxIDs:           readToTransactions{},
			invalidTxStatus:       map[TxID]committerpb.Status{},
			txIDToHeight:          transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
		}
	}

	// First submission: COMMITTED, row present.
	writer.Write(build())
	s1, ok := reader.Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s1.Status[0].Status)

	// Second submission of the same tx_id at the same height: the commit path
	// detects the duplicate txID, but setCorrectStatusForDuplicateTxID recognizes
	// it as a resubmission (same TX, same height) and returns the real committed
	// status, not a duplicate-rejection. Exactly one row remains (no re-insert).
	writer.Write(build())
	s2, ok := reader.Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s2.Status[0].Status)

	require.True(t, cloneExists(t, env.dbEnv.DB, name))
	rows := env.dbEnv.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{[]byte(ref.TxId)})
	require.Len(t, rows, 1)
}

func TestSnapshotResubmissionSkipsReclone(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)
	writer := channel.NewWriter(ctx, env.validatedTxs)
	reader := channel.NewReader(ctx, env.txStatus)

	ref := &committerpb.TxRef{BlockNum: 444555, TxNum: 0, TxId: "snap-resubmit-1"}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, env.dbEnv.DB, name)

	build := func() *validatedTransactions {
		value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
		require.NoError(t, err)
		nws := make(transactionToWrites)
		nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)
		return &validatedTransactions{
			validTxNonBlindWrites: transactionToWrites{},
			validTxBlindWrites:    transactionToWrites{},
			newWrites:             nws,
			readToTxIDs:           readToTransactions{},
			invalidTxStatus:       map[TxID]committerpb.Status{},
			txIDToHeight:          transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
		}
	}

	// First submission commits the snapshot (clone + PENDING row + txID).
	writer.Write(build())
	s1, ok := reader.Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s1.Status[0].Status)
	require.True(t, cloneExists(t, env.dbEnv.DB, name))

	// Drop the clone out-of-band to prove the resubmission does NOT re-create it:
	// because txID is already committed, createSnapshotIfPresent must skip database creation.
	require.NoError(t, env.dbEnv.DB.adminExec(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", pgx.Identifier{name}.Sanitize())))
	require.False(t, cloneExists(t, env.dbEnv.DB, name))

	// Resubmit the same snapshot TX (block re-delivered after combined failure).
	// setCorrectStatusForDuplicateTxID recognizes this as a resubmission (same TX,
	// same height) and returns the real committed status, not a duplicate-rejection.
	writer.Write(build())
	s2, ok := reader.Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s2.Status[0].Status)

	// The clone was NOT re-created — the resubmission short-circuited on the
	// already-committed txID and returned the committed status.
	require.False(t, cloneExists(t, env.dbEnv.DB, name))
}

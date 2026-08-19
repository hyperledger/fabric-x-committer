/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/jackc/pgerrcode"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
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

func TestYugabyteCloneStateError(t *testing.T) {
	t.Parallel()

	require.NoError(t, yugabyteCloneStateError("snapshot_1", "COMPLETE", ""))

	for _, tc := range []struct {
		name               string
		state              string
		failureReason      string
		expectedError      string
		expectNonRetryable bool
	}{
		{
			name:          "aborted clone reports server reason",
			state:         "ABORTED",
			failureReason: "tablet limit exceeded",
			expectedError: "YugabyteDB clone snapshot_1 aborted; " +
				"failure reason: tablet limit exceeded",
			expectNonRetryable: true,
		},
		{
			name:  "aborted clone reports absent reason",
			state: "ABORTED",
			expectedError: "YugabyteDB clone snapshot_1 aborted; " +
				"failure reason: not reported",
			expectNonRetryable: true,
		},
		{
			name:          "schema creation remains retryable",
			state:         "CLONE_SCHEMA_STARTED",
			expectedError: "YugabyteDB clone snapshot_1 is not ready: state CLONE_SCHEMA_STARTED",
		},
		{
			name:          "restoring clone remains retryable",
			state:         "RESTORING",
			expectedError: "YugabyteDB clone snapshot_1 is not ready: state RESTORING",
		},
		{
			name:          "unknown clone state remains retryable",
			state:         "FUTURE_STATE",
			expectedError: "YugabyteDB clone snapshot_1 is not ready: state FUTURE_STATE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := yugabyteCloneStateError("snapshot_1", tc.state, tc.failureReason)
			require.ErrorContains(t, err, tc.expectedError)
			if tc.expectNonRetryable {
				require.ErrorIs(t, err, retry.ErrNonRetryable)
			} else {
				require.NotErrorIs(t, err, retry.ErrNonRetryable)
			}
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

	// Distinguishable data written to the source, across TWO namespaces, BEFORE
	// cloning; the clone must carry both rows so a later reader observes the exact
	// source state rather than a partial/single-table copy.
	cloneRows := []cloneRow{
		{ns: ns1, key: []byte("clone-check-key-1"), value: []byte("clone-check-value-1")},
		{ns: ns2, key: []byte("clone-check-key-2"), value: []byte("clone-check-value-2")},
	}
	env.populateData(t, []string{ns1, ns2}, namespaceToWrites{
		ns1: {keys: [][]byte{cloneRows[0].key}, values: [][]byte{cloneRows[0].value}, versions: []uint64{0}},
		ns2: {keys: [][]byte{cloneRows[1].key}, values: [][]byte{cloneRows[1].value}, versions: []uint64{0}},
	}, nil, nil)

	// First creation succeeds and snapshot database exists.
	require.NoError(t, env.DB.createSnapshotDatabase(ctx, name))
	require.True(t, cloneExists(t, env.DB, name))
	for _, row := range cloneRows {
		requireCloneHasRow(t, env.DB, name, row)
	}

	// Second creation over existing database is a no-op success (reuse), not drop+recreate.
	require.NoError(t, env.DB.createSnapshotDatabase(ctx, name))
	require.True(t, cloneExists(t, env.DB, name))
	for _, row := range cloneRows {
		requireCloneHasRow(t, env.DB, name, row)
	}
}

// cloneRow identifies a single key/value expectation in a namespace, used by
// requireCloneHasRow to keep its argument count within the linter's limit.
type cloneRow struct {
	ns    string
	key   []byte
	value []byte
}

// requireCloneHasRow opens a short-lived pool against the clone database (not
// the source pool) and asserts it contains row's key/value in row's namespace,
// proving the clone's content matches the source instead of merely existing.
func requireCloneHasRow(t *testing.T, db *database, cloneName string, row cloneRow) {
	t.Helper()
	cloneConfig := *db.config
	cloneConfig.Database = cloneName
	clonePool, err := statedb.NewPool(t.Context(), &cloneConfig)
	require.NoError(t, err)
	defer clonePool.Close()

	var gotValue []byte
	err = retry.Execute(t.Context(), db.retryProfile, func() error {
		query := fmt.Sprintf("SELECT value FROM %s WHERE key = $1", statedb.TableName(row.ns))
		return clonePool.QueryRow(t.Context(), query, row.key).Scan(&gotValue)
	})
	require.NoError(t, err)
	require.Equal(t, row.value, gotValue)
}

func cloneExists(t *testing.T, db *database, name string) bool {
	t.Helper()
	// PostgreSQL clone creation terminates source connections, so catalog queries
	// need retries while pgxpool redials. YugabyteDB readiness requires both a
	// database catalog entry and an asynchronous clone state of COMPLETE.
	exists, err := retry.ExecuteWithResult(t.Context(), db.retryProfile, func() (bool, error) {
		isYuga, err := statedb.IsYugabyteDB(t.Context(), db.pool)
		if err != nil {
			return false, err
		}

		query := "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)"
		if isYuga {
			query += " AND EXISTS(" +
				"SELECT 1 FROM yb_database_clones() WHERE db_name = $1 AND state = 'COMPLETE')"
		}

		var exists bool
		err = db.pool.QueryRow(t.Context(), query, name).Scan(&exists)
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
	require.Contains(t, []committerpb.SnapshotState_Status{
		committerpb.SnapshotState_PENDING,
		committerpb.SnapshotState_IN_PROGRESS,
		committerpb.SnapshotState_COMPLETED,
	}, got.Status)
	require.Equal(t, name, got.CloneDatabase)
	require.Equal(t, ref.TxId, got.TxRef.TxId)
}

func TestUpdateSnapshotState(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	ref := &committerpb.TxRef{BlockNum: 700200, TxNum: 0, TxId: "snap-update-1"}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, env.dbEnv.DB, name)

	// Commit a PENDING _snapshot row through the normal path.
	value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
	require.NoError(t, err)
	nws := make(transactionToWrites)
	nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)
	channel.NewWriter(ctx, env.validatedTxs).Write(&validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{},
		validTxBlindWrites:    transactionToWrites{},
		newWrites:             nws,
		readToTxIDs:           readToTransactions{},
		invalidTxStatus:       map[TxID]committerpb.Status{},
		txIDToHeight:          transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
	})
	s, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s.Status[0].Status)

	// Move PENDING -> IN_PROGRESS.
	require.NoError(t, env.dbEnv.DB.updateSnapshotState(ctx, ref, snapshotStateUpdate{
		Status: committerpb.SnapshotState_IN_PROGRESS,
	}))

	rows := env.dbEnv.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{[]byte(ref.TxId)})
	stored := rows[ref.TxId]
	require.NotNil(t, stored)
	require.EqualValues(t, 1, stored.Version) // version incremented from 0.
	var got committerpb.SnapshotState
	require.NoError(t, proto.Unmarshal(stored.Value, &got))
	require.Equal(t, committerpb.SnapshotState_IN_PROGRESS, got.Status)
	require.Equal(t, name, got.CloneDatabase)
}

func TestIgnoreDuplicateDatabase(t *testing.T) {
	t.Parallel()
	require.NoError(t, ignoreDuplicateDatabase(nil))
	require.NoError(t, ignoreDuplicateDatabase(&pgconn.PgError{Code: pgerrcode.DuplicateDatabase}))
	require.ErrorContains(t, ignoreDuplicateDatabase(errors.New("create failed")), "create failed")
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

	w, ok := snapshotWriteInBatch(nws)
	require.True(t, ok)
	_, err = env.DB.createSnapshotDatabaseAndRewriteRecord(ctx, w.keys[0], w.values[0])
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

	// The commit path starts no hashing; the scheduler owns that, and it is not
	// running in this env, so the record stays where the commit left it.
	requireSnapshotStatus(t, env.dbEnv.DB, ref.TxId, expectedSnapshotState{
		status: committerpb.SnapshotState_PENDING,
	})

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

	// The commit path starts no hashing; the scheduler owns that, and it is not
	// running in this env, so the record stays where the commit left it.
	requireSnapshotStatus(t, env.dbEnv.DB, ref.TxId, expectedSnapshotState{
		status: committerpb.SnapshotState_PENDING,
	})

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

func TestRejectSnapshotIfPriorNotCheckpointed(t *testing.T) {
	t.Parallel()
	inProgress := committerpb.Status_REJECTED_SNAPSHOT_IN_PROGRESS
	noCheckpoint := committerpb.Status_REJECTED_SNAPSHOT_NO_CHECKPOINT
	tests := []struct {
		name       string
		block      uint64
		priorState committerpb.SnapshotState_Status
		wantStatus committerpb.Status
		accepted   bool
	}{
		{"checkpointed accepts", 1000, committerpb.SnapshotState_CHECKPOINTED, 0, true},
		{"unspecified blocks 119", 1001, committerpb.SnapshotState_STATUS_UNSPECIFIED, inProgress, false},
		{"pending blocks 119", 1002, committerpb.SnapshotState_PENDING, inProgress, false},
		{"in_progress blocks 119", 1003, committerpb.SnapshotState_IN_PROGRESS, inProgress, false},
		{"failed blocks 119", 1004, committerpb.SnapshotState_FAILED, inProgress, false},
		{"completed blocks 120", 1005, committerpb.SnapshotState_COMPLETED, noCheckpoint, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCommitterTestEnv(t)
			testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
			ctx, _ := createContext(t)

			prior := &committerpb.TxRef{BlockNum: tc.block, TxNum: 0, TxId: fmt.Sprintf("prior-%d", tc.block)}
			// Seed a prior _snapshot row directly through the normal commit path (bypassing
			// the gate), so its state is set up for the gate to react to.
			priorValue, err := proto.Marshal(&committerpb.SnapshotState{TxRef: prior, Status: tc.priorState})
			require.NoError(t, err)

			priorNWs := make(transactionToWrites)
			priorNWs.getOrCreate(TxID(prior.TxId), committerpb.SnapshotNamespaceID).
				append([]byte(prior.TxId), priorValue, 0)

			_, err = env.dbEnv.DB.commit(ctx, &statesToBeCommitted{
				newWrites: groupWritesByNamespace(priorNWs),
				batchStatus: &committerpb.TxStatusBatch{Status: []*committerpb.TxStatus{
					servicepb.NewHeightFromTxRef(prior).WithStatus(prior.TxId, committerpb.Status_COMMITTED),
				}},
				txIDToHeight: transactionIDToHeight{TxID(prior.TxId): servicepb.NewHeightFromTxRef(prior)},
			})
			require.NoError(t, err)

			incomingTxID := fmt.Sprintf("incoming-%d", tc.block)
			vTx, name := newIncomingSnapshotVTx(t, env.dbEnv.DB, tc.block+1000, incomingTxID)

			require.NoError(t, env.dbEnv.DB.rejectSnapshotIfPriorNotCheckpointed(ctx, vTx))

			// The gate itself never creates a clone (that happens later, in
			// createSnapshotIfPresent), so no clone should exist regardless of
			// accept/reject outcome.
			require.False(t, cloneExists(t, env.dbEnv.DB, name))

			if tc.accepted {
				require.Empty(t, vTx.invalidTxStatus)
				require.NotEmpty(t, vTx.newWrites)
				return
			}
			require.Equal(t, tc.wantStatus, vTx.invalidTxStatus[TxID(incomingTxID)])
			require.Empty(t, vTx.newWrites) // incoming _snapshot write removed
		})
	}
}

// TestRejectSnapshotIfPriorNotCheckpointedMalformedRecord verifies that a
// latest _snapshot record which fails to decode is a hard error (data
// corruption / invariant violation), not a soft rejection status.
func TestRejectSnapshotIfPriorNotCheckpointedMalformedRecord(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	// Seed a prior _snapshot record whose value is not a valid SnapshotState.
	prior := &committerpb.TxRef{BlockNum: 2000, TxNum: 0, TxId: "prior-malformed"}
	nws := make(transactionToWrites)
	nws.getOrCreate(TxID(prior.TxId), committerpb.SnapshotNamespaceID).
		append([]byte(prior.TxId), []byte("not a valid protobuf message"), 0)
	info := &statesToBeCommitted{
		newWrites: groupWritesByNamespace(nws),
		batchStatus: &committerpb.TxStatusBatch{Status: []*committerpb.TxStatus{
			servicepb.NewHeightFromTxRef(prior).WithStatus(prior.TxId, committerpb.Status_COMMITTED),
		}},
		txIDToHeight: transactionIDToHeight{TxID(prior.TxId): servicepb.NewHeightFromTxRef(prior)},
	}
	_, err := env.dbEnv.DB.commit(ctx, info)
	require.NoError(t, err)

	// An incoming, different snapshot request must see a hard error, not a
	// silent conservative rejection status.
	vTx, name := newIncomingSnapshotVTx(t, env.dbEnv.DB, 2001, "incoming-malformed")

	err = env.dbEnv.DB.rejectSnapshotIfPriorNotCheckpointed(ctx, vTx)
	require.ErrorContains(t, err, "failed to decode latest _snapshot record")
	require.False(t, cloneExists(t, env.dbEnv.DB, name))
}

// newIncomingSnapshotVTx builds the validatedTransactions batch for a single,
// standalone incoming snapshot TX targeting (blockNum, txID), as
// rejectSnapshotIfPriorNotCheckpointed expects: exactly one transaction with one
// unstamped (status-UNSPECIFIED) _snapshot write. It also registers cleanup for
// the snapshot's clone database (named after ref), so callers get that for free.
// Returns the built vTx and the clone database name.
func newIncomingSnapshotVTx(t *testing.T, db *database, blockNum uint64, txID string) (*validatedTransactions, string) {
	t.Helper()
	ref := &committerpb.TxRef{BlockNum: blockNum, TxNum: 0, TxId: txID}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, db, name)

	value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
	require.NoError(t, err)

	nws := make(transactionToWrites)
	nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)

	vTx := &validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{},
		validTxBlindWrites:    transactionToWrites{},
		newWrites:             nws,
		readToTxIDs:           readToTransactions{},
		invalidTxStatus:       map[TxID]committerpb.Status{},
		txIDToHeight:          transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
	}
	return vTx, name
}

// TestCreateSnapshotIfPresentIgnoresDuplicateHeight covers a txID already
// committed at a DIFFERENT height: the normal duplicate-status path rejects it,
// so no clone is created and no hash job is queued.
func TestCreateSnapshotIfPresentIgnoresDuplicateHeight(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	committed := &committerpb.TxRef{BlockNum: 800700, TxNum: 0, TxId: "snap-prepare-duplicate"}
	seedSnapshotRowAtStatus(
		t.Context(), t, env.dbEnv.DB, committed, committerpb.SnapshotState_PENDING, snapshotDatabaseName(committed),
	)
	vTx, name := newIncomingSnapshotVTx(t, env.dbEnv.DB, committed.BlockNum+1, committed.TxId)

	require.NoError(t, env.dbEnv.DB.createSnapshotIfPresent(t.Context(), vTx.newWrites))
	require.False(t, cloneExists(t, env.dbEnv.DB, name))
}

// TestSnapshotResubmissionLeavesHashStartToScheduler covers a VC that committed a
// snapshot's row and clone but crashed before acking the coordinator. From the
// coordinator's side that is indistinguishable from a transaction whose outcome it
// never learned, so it resubmits the same tx.
//
// A resubmission creates no clone and starts no hash itself; the scheduler owns
// starting hashes. The two halves of that are what this test walks:
//
//   - while a live worker still holds the lease, neither the resubmission nor a
//     scheduler tick may start a competing hash (the duplicate the lease prevents);
//   - once that worker is gone, its lease having lapsed unrenewed, the scheduler
//     MUST hash the existing clone, so the crash does not lose the hash.
func TestSnapshotResubmissionLeavesHashStartToScheduler(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t) // no scheduler: drive each tick explicitly.
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	writer := channel.NewWriter(t.Context(), env.validatedTxs)
	reader := channel.NewReader(t.Context(), env.txStatus)

	const txID = "snap-resubmit-after-crash"

	// Step 1: the original submission commits the row and the clone. The commit path
	// starts no hashing, so the record is left PENDING for the scheduler.
	t.Log("Step 1: original snapshot tx commits, leaving a PENDING record")
	vTx1, name := newIncomingSnapshotVTx(t, env.dbEnv.DB, 800600, txID)
	writer.Write(vTx1)
	s1, ok := reader.Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s1.Status[0].Status)

	rows := env.dbEnv.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{[]byte(txID)})
	require.NotNil(t, rows[txID])
	requireSnapshotStatus(t, env.dbEnv.DB, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_PENDING,
	})

	// Step 2: a VC claims the job and is hashing. Its live lease is what the
	// resubmission in Step 3 must respect.
	t.Log("Step 2: a worker claims the hash job and holds a live lease")
	claim, err := env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.NotNil(t, claim)

	// Resubmit the same tx (simulating the coordinator's plain-tx-resubmission
	// retry path after a VC disconnect it never got a COMMITTED reply from).
	// newIncomingSnapshotVTx rebuilds an equivalent vTx for the same
	// (block, tx_id) pair — same shape the real coordinator retry path
	// resends — registering a second (harmless, deduped by t.Cleanup) clone
	// cleanup for the same name.
	resubmit := func() {
		vTx, _ := newIncomingSnapshotVTx(t, env.dbEnv.DB, 800600, txID)
		writer.Write(vTx)
		s, readOK := reader.Read()
		require.True(t, readOK)
		require.Equal(t, committerpb.Status_COMMITTED, s.Status[0].Status)
	}

	// Step 3: the worker is still alive and holds the lease, so neither the
	// resubmission nor a scheduler tick may start a competing hash.
	t.Log("Step 3: resubmission and a scheduler tick both back off from the live lease")
	resubmit()
	require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(t.Context()))
	requireSnapshotHashLeaseEquals(t, env.dbEnv.DB, claim)
	requireSnapshotStatus(t, env.dbEnv.DB, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_PENDING,
	})

	// Step 4: the worker crashes. A crash leaves the lease row *present but expired*
	// -- nobody clears it, which is exactly how a dead worker is detected -- so the
	// recovery below has to reason about expiry, not absence.
	t.Log("Step 4: the worker crashes, leaving its lease behind to expire")
	seedExpiredSnapshotHashLease(t, env.dbEnv.DB, claim)

	// Step 5: a resubmission alone still must not hash, even with no live worker.
	t.Log("Step 5: resubmission alone still starts no hash")
	resubmit()
	requireSnapshotStatus(t, env.dbEnv.DB, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_PENDING,
	})

	// Step 6: the scheduler takes the expired lease over and hashes the existing
	// clone to completion, so the crash did not lose the hash.
	t.Log("Step 6: the scheduler takes over the expired lease and completes the hash")
	require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(t.Context()))
	record, found := snapshotRecordForPolling(t.Context(), env.dbEnv.DB, txID)
	require.True(t, found)
	require.Equal(t, committerpb.SnapshotState_COMPLETED, record.state.Status)
	require.NotEmpty(t, record.state.Hash)

	// No second clone/row was created — exactly one row, same clone.
	require.True(t, cloneExists(t, env.dbEnv.DB, name))
	rows = env.dbEnv.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{[]byte(txID)})
	require.Len(t, rows, 1)
}

// commitFreshSnapshotTx builds and submits a single-write _snapshot batch for
// ref through the real committer path (writer -> commit -> COMMITTED status),
// exactly as a first-ever snapshot submission would arrive, and asserts the
// resulting status is COMMITTED. Shared by every test in this file (and
// snapshot_hash_test.go) that just needs "a snapshot tx is already
// committed" as setup, so that shape is not re-typed per test.
func commitFreshSnapshotTx(
	ctx context.Context, t *testing.T, env *committerTestEnv, ref *committerpb.TxRef,
) {
	t.Helper()
	value, err := proto.Marshal(&committerpb.SnapshotState{TxRef: ref})
	require.NoError(t, err)
	nws := make(transactionToWrites)
	nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)
	channel.NewWriter(ctx, env.validatedTxs).Write(&validatedTransactions{
		validTxNonBlindWrites: transactionToWrites{},
		validTxBlindWrites:    transactionToWrites{},
		newWrites:             nws,
		readToTxIDs:           readToTransactions{},
		invalidTxStatus:       map[TxID]committerpb.Status{},
		txIDToHeight:          transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
	})
	s, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, s.Status[0].Status)
}

// seedSnapshotRowAtStatus commits a _snapshot row for ref directly at status
// with cloneDB as its clone_database, bypassing the normal
// PENDING-then-worker-advances flow. Used when a test needs to set up
// IN_PROGRESS/FAILED/CHECKPOINTED or a deliberately-empty clone_database
// without racing a running hash worker or the commit path's own database
// creation. Shared by Task 4/5/6-derived tests in snapshot_hash_test.go.
func seedSnapshotRowAtStatus( //nolint:revive // 6 args; reused by later tasks.
	ctx context.Context, t *testing.T, db *database, ref *committerpb.TxRef,
	status committerpb.SnapshotState_Status, cloneDB string,
) {
	t.Helper()
	value, err := proto.Marshal(&committerpb.SnapshotState{
		TxRef: ref, Status: status, CloneDatabase: cloneDB,
	})
	require.NoError(t, err)
	nws := make(transactionToWrites)
	nws.getOrCreate(TxID(ref.TxId), committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)
	states := &statesToBeCommitted{
		newWrites: groupWritesByNamespace(nws),
		batchStatus: &committerpb.TxStatusBatch{Status: []*committerpb.TxStatus{
			servicepb.NewHeightFromTxRef(ref).WithStatus(ref.TxId, committerpb.Status_COMMITTED),
		}},
		txIDToHeight: transactionIDToHeight{TxID(ref.TxId): servicepb.NewHeightFromTxRef(ref)},
	}
	// Retry exactly as the production commit path does (see committer.go): on
	// PostgreSQL, creating a snapshot clone runs pg_terminate_backend against the
	// source database, which kills this pool's pooled connections. A commit that
	// happens to pick up a killed connection fails with SQLSTATE 57P01
	// ("terminating connection due to administrator command") until the pool
	// replaces it. Calling db.commit bare here made the seed flaky whenever it ran
	// after a clone; Yugabyte clones do not terminate backends, so only PostgreSQL
	// showed it.
	_, err = retry.ExecuteWithResult(ctx, db.retryProfile, func() (*commitResult, error) {
		return db.commit(ctx, states)
	})
	require.NoError(t, err)
}

func TestUpdateSnapshotStateSetsErrorMessage(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	ref := &committerpb.TxRef{BlockNum: 800200, TxNum: 0, TxId: "snap-errmsg-1"}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, env.dbEnv.DB, name)
	commitFreshSnapshotTx(ctx, t, env, ref)

	require.NoError(t, env.dbEnv.DB.updateSnapshotState(ctx, ref, snapshotStateUpdate{
		Status: committerpb.SnapshotState_FAILED,
		ErrMsg: "missing clone_database",
	}))

	rows := env.dbEnv.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{[]byte(ref.TxId)})
	stored := rows[ref.TxId]
	require.NotNil(t, stored)
	var got committerpb.SnapshotState
	require.NoError(t, proto.Unmarshal(stored.Value, &got))
	require.Equal(t, committerpb.SnapshotState_FAILED, got.Status)
	require.Equal(t, "missing clone_database", got.Error)
	require.Equal(t, name, got.CloneDatabase) // preserved, not clobbered.
}

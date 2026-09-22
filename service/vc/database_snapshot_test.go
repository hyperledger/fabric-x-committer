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
	dropSnapshotCloneOnCleanup(t, env.DB, name)

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

func TestCommitSnapshotTxCreatesCloneAndPendingRow(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	ref := &committerpb.TxRef{BlockNum: 987654, TxNum: 1, TxId: "snap-e2e-1"}
	name := snapshotDatabaseName(ref)
	dropSnapshotCloneOnCleanup(t, env.dbEnv.DB, name)

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
	dropSnapshotCloneOnCleanup(t, env.dbEnv.DB, name)

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
	require.NoError(t, env.dbEnv.DB.snapshotState.Update(ctx, ref, statedb.SnapshotUpdate{
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
	dropSnapshotCloneOnCleanup(t, env.dbEnv.DB, name)

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

	// The commit path starts no hashing at all -- the snapshot service does, from its
	// own process -- so the record stays exactly where the commit left it.
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_PENDING)

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
	dropSnapshotCloneOnCleanup(t, env.dbEnv.DB, name)

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

	// The commit path starts no hashing at all -- the snapshot service does, from its
	// own process -- so the record stays exactly where the commit left it.
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_PENDING)

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
		// The stall this fixes: a snapshot that can never be hashed rests at ABORTED, and a
		// later request must be admitted exactly as it is after a checkpoint.
		{"aborted accepts", 1006, committerpb.SnapshotState_ABORTED, 0, true},
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
				batchStatus: &servicepb.TxStatusBatch{Status: []*committerpb.TxStatus{
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
		batchStatus: &servicepb.TxStatusBatch{Status: []*committerpb.TxStatus{
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
	require.ErrorContains(t, err, "failed to decode the latest _snapshot record")
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
	dropSnapshotCloneOnCleanup(t, db, name)

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
	env.dbEnv.SeedSnapshotRecord(t, SnapshotFixture{
		Ref:           committed,
		Status:        committerpb.SnapshotState_PENDING,
		CloneDatabase: snapshotDatabaseName(committed),
	})
	vTx, name := newIncomingSnapshotVTx(t, env.dbEnv.DB, committed.BlockNum+1, committed.TxId)

	require.NoError(t, env.dbEnv.DB.createSnapshotIfPresent(t.Context(), vTx.newWrites))
	require.False(t, cloneExists(t, env.dbEnv.DB, name))
}

// commitFreshSnapshotTx builds and submits a single-write _snapshot batch for
// ref through the real committer path (writer -> commit -> COMMITTED status),
// exactly as a first-ever snapshot submission would arrive, and asserts the
// resulting status is COMMITTED. Shared by every test in this file that just needs
// "a snapshot tx is already committed" as setup, so that shape is not re-typed per
// test.
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

// requireSnapshotStatus asserts the committed _snapshot record for txID is at the
// wanted status. An optional wantErrSubstring asserts the record's error field too: an
// empty string requires no error is recorded, so a case that expects a clean record
// cannot pass while carrying a stale divergence.
func requireSnapshotStatus(
	t *testing.T, env *DatabaseTestEnv, txID string, want committerpb.SnapshotState_Status,
	wantErrSubstring ...string,
) {
	t.Helper()
	record, found := env.ReadSnapshotRecord(t.Context(), txID)
	require.True(t, found)
	require.Equal(t, want, record.State.Status)
	if len(wantErrSubstring) == 0 {
		return
	}
	if wantErrSubstring[0] == "" {
		require.Empty(t, record.State.Error)
		return
	}
	require.Contains(t, record.State.Error, wantErrSubstring[0])
}

func TestUpdateSnapshotStateSetsErrorMessage(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	ref := &committerpb.TxRef{BlockNum: 800200, TxNum: 0, TxId: "snap-errmsg-1"}
	name := snapshotDatabaseName(ref)
	dropSnapshotCloneOnCleanup(t, env.dbEnv.DB, name)
	commitFreshSnapshotTx(ctx, t, env, ref)

	require.NoError(t, env.dbEnv.DB.snapshotState.Update(ctx, ref, statedb.SnapshotUpdate{
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

// TestCommitSnapshotAbort walks every status an abort may close. Everything short of a
// closed lifecycle is abortable, COMPLETED included: a digest that diverged from the
// other organizations is exactly a snapshot nobody will checkpoint, so refusing to
// abort it would leave that stall with no exit.
func TestCommitSnapshotAbort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		blockNum uint64
		// held is the status the snapshot record is in when the abort arrives.
		held   committerpb.SnapshotState_Status
		digest []byte
	}{
		{name: "pending", blockNum: 920100, held: committerpb.SnapshotState_PENDING},
		{name: "in progress", blockNum: 920101, held: committerpb.SnapshotState_IN_PROGRESS},
		{name: "failed", blockNum: 920102, held: committerpb.SnapshotState_FAILED},
		{
			name:     "completed with a digest",
			blockNum: 920103,
			held:     committerpb.SnapshotState_COMPLETED,
			digest:   []byte("diverged-digest"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, true)
			ctx, _ := createContext(t)

			ref := env.seedSnapshotHeldAt(t, snapshotHold{
				txID: "abort-" + tc.name, blockNum: tc.blockNum, status: tc.held, digest: tc.digest,
			})
			abortRef := committerpb.NewTxRef("abort-tx-"+tc.name, tc.blockNum+1, 0)

			status := env.submitSnapshotAbort(ctx, t, newSnapshotAbortPreparedTx(abortRef, tc.blockNum))
			require.Len(t, status.Status, 1)
			require.Equal(t, committerpb.Status_COMMITTED, status.Status[0].Status)

			// Both must be durable: the row is the fact a replay reproduces, the status is
			// what admits the next snapshot.
			requireSnapshotAbortRow(t, env.dbEnv, tc.blockNum)
			requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_ABORTED)

			// The pointer still names the record, now ABORTED, never the abort row.
			state, err := env.dbEnv.DB.snapshotState.ReadLatest(ctx)
			require.NoError(t, err)
			require.Equal(t, ref.TxId, state.TxRef.TxId)
		})
	}
}

// TestRejectSnapshotAbortForClosedOrUnknownSnapshot covers every abort that names no
// abortable snapshot. All are per-TX rejections rather than failures: the submitter
// chose the block number, and local state is not in question.
func TestRejectSnapshotAbortForClosedOrUnknownSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// snapshotBlock is the block of the seeded snapshot, or 0 to seed none.
		snapshotBlock uint64
		seededStatus  committerpb.SnapshotState_Status
		// abortBlock is the block the abort names.
		abortBlock uint64
		// wantStatus is the rejection the submitter must be able to tell apart.
		wantStatus committerpb.Status
	}{
		{
			name:          "already checkpointed",
			snapshotBlock: 920200,
			seededStatus:  committerpb.SnapshotState_CHECKPOINTED,
			abortBlock:    920200,
			wantStatus:    committerpb.Status_REJECTED_SNAPSHOT_ALREADY_CHECKPOINTED,
		},
		{
			name:          "already aborted",
			snapshotBlock: 920201,
			seededStatus:  committerpb.SnapshotState_ABORTED,
			abortBlock:    920201,
			wantStatus:    committerpb.Status_REJECTED_SNAPSHOT_ALREADY_ABORTED,
		},
		{
			name:          "names a block that is not the latest snapshot",
			snapshotBlock: 920202,
			seededStatus:  committerpb.SnapshotState_PENDING,
			abortBlock:    920999,
			wantStatus:    committerpb.Status_REJECTED_NO_SUCH_SNAPSHOT,
		},
		{
			name:       "no snapshot was ever accepted",
			abortBlock: 920203,
			wantStatus: committerpb.Status_REJECTED_NO_SUCH_SNAPSHOT,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, true)
			ctx, _ := createContext(t)

			if tc.snapshotBlock != 0 {
				env.seedSnapshotHeldAt(t, snapshotHold{
					txID: "abort-rej-" + tc.name, blockNum: tc.snapshotBlock, status: tc.seededStatus,
				})
			}
			abortRef := committerpb.NewTxRef("abort-rej-tx-"+tc.name, tc.abortBlock+1, 0)

			status := env.submitSnapshotAbort(ctx, t, newSnapshotAbortPreparedTx(abortRef, tc.abortBlock))
			require.Len(t, status.Status, 1)
			require.Equal(t, tc.wantStatus, status.Status[0].Status)

			// No row is left behind, so a replay cannot reproduce an abort we never accepted.
			requireNoSnapshotAbortRow(t, env.dbEnv, tc.abortBlock)
		})
	}
}

// TestSnapshotAbortDoesNotGateOnPriorSnapshot pins the interaction that makes an abort
// usable at all: the admission gate rejects a new snapshot until the latest one is
// lifecycle-closed, and an abort is submitted precisely when it is not. Were the abort
// gated like a request, the stuck snapshot could never be aborted.
func TestSnapshotAbortDoesNotGateOnPriorSnapshot(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, false)
	ctx, _ := createContext(t)

	env.seedSnapshotHeldAt(t, snapshotHold{
		txID: "abort-not-gated", blockNum: 920300, status: committerpb.SnapshotState_FAILED,
	})

	abortRef := committerpb.NewTxRef("abort-not-gated-tx", 920301, 0)
	prepTx := newSnapshotAbortPreparedTx(abortRef, 920300)
	vTx := newValidatedTxsFromPrepared(prepTx)

	require.NoError(t, env.dbEnv.DB.rejectSnapshotIfPriorNotCheckpointed(ctx, vTx))
	require.Empty(t, vTx.invalidTxStatus, "the admission gate must ignore an abort TX")
	require.NotEmpty(t, vTx.newWrites)
}

// TestSnapshotAbortBatchHelpers covers the two batch-level rules that keep an abort out of
// the snapshot-request path: it must never be mistaken for a request (which would clone a
// database for a snapshot being abandoned), and two aborts in one batch must fail the batch
// rather than let an unverified one commit.
func TestSnapshotAbortBatchHelpers(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	ctx, _ := createContext(t)

	prepTx := newSnapshotAbortPreparedTx(committerpb.NewTxRef("abort-no-clone-tx", 920400, 0), 920399)
	_, found := snapshotWriteInBatch(prepTx.txIDToNsNewWrites)
	require.False(t, found, "an abort write must not be seen as a snapshot request")
	require.NoError(t, env.dbEnv.DB.createSnapshotIfPresent(ctx, prepTx.txIDToNsNewWrites))

	twoAborts := make(transactionToWrites)
	twoAborts.getOrCreate("abort-a", committerpb.SnapshotNamespaceID).
		append(committerpb.SnapshotAbortKey(1), nil, 0)
	twoAborts.getOrCreate("abort-b", committerpb.SnapshotNamespaceID).
		append(committerpb.SnapshotAbortKey(2), nil, 0)
	_, err := snapshotAbortWriteInBatch(twoAborts)
	require.ErrorContains(t, err, "at most one snapshot abort")
}

// submitSnapshotAbort sends an abort TX through the real validator-committer pipeline
// and returns the resulting status batch, so every case asserts the verdict the
// pipeline actually produces rather than one a direct call would fabricate.
func (env *validatorTestEnv) submitSnapshotAbort(
	ctx context.Context, t *testing.T, abort *preparedTransactions,
) *servicepb.TxStatusBatch {
	t.Helper()
	channel.NewWriter(ctx, env.preparedTxs).Write(abort)
	status, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	return status
}

// snapshotHold is the state an abort finds a `_snapshot` record in.
type snapshotHold struct {
	txID     string
	blockNum uint64
	status   committerpb.SnapshotState_Status
	digest   []byte
}

// seedSnapshotHeldAt commits a `_snapshot` record for hold.blockNum and leaves it at hold.status.
func (env *validatorTestEnv) seedSnapshotHeldAt(t *testing.T, hold snapshotHold) *committerpb.TxRef {
	t.Helper()
	ref := env.seedSnapshotRecord(t, hold.txID, hold.blockNum, hold.digest)
	if hold.status != committerpb.SnapshotState_COMPLETED {
		require.NoError(t, env.dbEnv.DB.snapshotState.Update(t.Context(), ref, statedb.SnapshotUpdate{
			Status: hold.status,
		}))
	}
	return ref
}

// newSnapshotAbortPreparedTx builds what the preparer produces for an abort TX: a
// transaction whose single nil-version ReadWrite becomes one new write, key = the abort
// key for blockNum, value = empty.
func newSnapshotAbortPreparedTx(ref *committerpb.TxRef, blockNum uint64) *preparedTransactions {
	key := committerpb.SnapshotAbortKey(blockNum)
	txID := TxID(ref.TxId)
	prepTx := newEmptyPreparedTransactions()
	prepTx.txIDToNsNewWrites.getOrCreate(txID, committerpb.SnapshotNamespaceID).append(key, nil, 0)
	prepTx.readToTxIDs[newCmpRead(committerpb.SnapshotNamespaceID, key, nil)] = []TxID{txID}
	prepTx.txIDToHeight[txID] = servicepb.NewHeightFromTxRef(ref)
	return prepTx
}

func requireSnapshotAbortRow(t *testing.T, env *DatabaseTestEnv, blockNum uint64) {
	t.Helper()
	key := committerpb.SnapshotAbortKey(blockNum)
	rows := env.FetchKeys(t, committerpb.SnapshotNamespaceID, [][]byte{key})
	require.NotNil(t, rows[string(key)], "no abort row for block %d", blockNum)
}

func requireNoSnapshotAbortRow(t *testing.T, env *DatabaseTestEnv, blockNum uint64) {
	t.Helper()
	env.rowNotExists(t, committerpb.SnapshotNamespaceID, [][]byte{committerpb.SnapshotAbortKey(blockNum)})
}

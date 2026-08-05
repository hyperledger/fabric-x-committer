/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestEnqueueSnapshotHashJobReturnsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	db := &database{snapshotHashJobs: make(chan snapshotHashJob, 1)}

	job := snapshotHashJob{cloneDatabase: "snapshot_1", ref: &committerpb.TxRef{TxId: "snapshot-tx"}}
	err := db.enqueueSnapshotHashJob(ctx, job)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, db.snapshotHashJobs)
}

func TestSnapshotHashJobFromWritesOnlyReturnsPendingRecord(t *testing.T) {
	t.Parallel()

	// A committed batch carries at most one _snapshot write (key = tx_id, one
	// ReadWrite), per the preparer's invariant. Each case below is its own
	// single-pair batch, matching that real shape.
	singleWrite := func(key string, value []byte) transactionToWrites {
		writes := make(transactionToWrites)
		writes.getOrCreate(TxID(key), committerpb.SnapshotNamespaceID).append([]byte(key), value, 0)
		return writes
	}

	ref := &committerpb.TxRef{TxId: "snapshot-tx"}
	const cloneDB = "snapshot_1"

	tests := []struct {
		name   string
		state  *committerpb.SnapshotState
		wantOK bool
	}{
		{
			name: "valid PENDING record",
			state: &committerpb.SnapshotState{
				TxRef: ref, Status: committerpb.SnapshotState_PENDING, CloneDatabase: cloneDB,
			},
			wantOK: true,
		},
		{
			name: "non-PENDING record must not enqueue a job",
			state: &committerpb.SnapshotState{
				TxRef: ref, Status: committerpb.SnapshotState_COMPLETED, CloneDatabase: cloneDB,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value, err := encodeSnapshotState(tc.state)
			require.NoError(t, err)

			job, ok := snapshotHashJobFromWrites(singleWrite(ref.TxId, value))
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.Equal(t, cloneDB, job.cloneDatabase)
				require.Equal(t, ref.TxId, job.ref.TxId)
			}
		})
	}
}

func TestSnapshotHashReEnqueueIsIdempotent(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnvWithHashWorker(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	// Step 1: submit a snapshot TX through the real committer path so the worker
	// hashes it exactly as production would (createSnapshotIfPresent -> commit ->
	// enqueueSnapshotHashJob -> runSnapshotHashWorker).
	ref := &committerpb.TxRef{BlockNum: 700400, TxNum: 0, TxId: "snap-reenqueue-1"}
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, env.dbEnv.DB, name)

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

	status, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	require.Equal(t, committerpb.Status_COMMITTED, status.Status[0].Status)

	// Step 2: wait for the background worker to finish the first hash pass
	// (IN_PROGRESS -> COMPLETED with a non-empty hash), and record that hash and
	// version as the baseline the re-enqueue must reproduce.
	var firstHash []byte
	var firstVersion int64
	require.Eventually(t, func() bool {
		pollCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		record, found := snapshotRecordForPolling(pollCtx, env.dbEnv.DB, ref.TxId)
		if !found || record.state.Status != committerpb.SnapshotState_COMPLETED || len(record.state.Hash) == 0 {
			return false
		}
		firstHash = append([]byte(nil), record.state.Hash...)
		firstVersion = record.version
		return true
	}, 30*time.Second, 100*time.Millisecond)

	// Step 3: manually re-enqueue the same clone (simulating a recovery re-drive,
	// see the enqueueSnapshotHashJob doc comment), then wait for a second
	// IN_PROGRESS -> COMPLETED pass over the same immutable clone.
	require.NoError(t, env.dbEnv.DB.enqueueSnapshotHashJob(ctx, snapshotHashJob{cloneDatabase: name, ref: ref}))

	// Re-enqueue must drive IN_PROGRESS then COMPLETED. Version grows by two, so
	// this cannot pass by observing first completed state before worker runs.
	require.Eventually(t, func() bool {
		pollCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		record, found := snapshotRecordForPolling(pollCtx, env.dbEnv.DB, ref.TxId)
		return found && record.recordCount == 1 &&
			record.state.Status == committerpb.SnapshotState_COMPLETED &&
			bytes.Equal(firstHash, record.state.Hash) &&
			record.version >= firstVersion+2
	}, 30*time.Second, 100*time.Millisecond)
}

func TestSnapshotHashDeterministic(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.DBConf.Database)
	ctx, _ := createContext(t)

	// Seed three namespaces with several keys each, plus committed tx statuses, so
	// the digest covers multiple ns_<id> tables AND tx_status. populateData commits
	// the namespace IDs into ns__meta (so listHashedTables discovers ns_1..ns_3),
	// inserts the rows, and commits the given tx statuses.
	nsIDs := []string{"1", "2", "3"}
	allStates := make([]state, 0, len(nsIDs)*5)
	for _, ns := range nsIDs {
		for k := 1; k <= 5; k++ {
			allStates = append(allStates, state{namespace: ns, keySuffix: k, updateSequence: 0})
		}
	}

	statusBatch := &committerpb.TxStatusBatch{}
	txIDToHeight := transactionIDToHeight{}
	for i := 1; i <= 3; i++ {
		txID := fmt.Sprintf("snap-hash-seed-tx-%d", i)
		ref := &committerpb.TxRef{BlockNum: 700000, TxNum: uint32(i), TxId: txID}
		statusBatch.Status = append(statusBatch.Status,
			committerpb.NewTxStatusFromRef(ref, committerpb.Status_COMMITTED))
		txIDToHeight[TxID(txID)] = servicepb.NewHeightFromTxRef(ref)
	}

	env.populateData(t, nsIDs, writes(false, allStates...), statusBatch, txIDToHeight)

	ref := &committerpb.TxRef{BlockNum: 700100, TxNum: 0, TxId: "snap-hash-1"}
	// dropCloneCleanup registers a t.Cleanup internally that drops the clone
	// database — do not add a second t.Cleanup.
	h1 := createAndHashSnapshotClone(ctx, t, env.DB, ref)
	require.NotEmpty(t, h1)

	// Re-hashing the same immutable clone yields the identical digest.
	h2, err := env.DB.hasher.hashSnapshotDatabase(ctx, snapshotDatabaseName(ref))
	require.NoError(t, err)
	require.Equal(t, h1, h2)

	// DIFFERENT state -> DIFFERENT hash. Commit an additional row into a user
	// namespace, then a fresh clone (new snapshot name) must hash differently.
	env.populateData(t, nil, writes(false, state{namespace: "1", keySuffix: 99, updateSequence: 0}),
		&committerpb.TxStatusBatch{}, transactionIDToHeight{})

	ref2 := &committerpb.TxRef{BlockNum: 700110, TxNum: 0, TxId: "snap-hash-2"}
	h3 := createAndHashSnapshotClone(ctx, t, env.DB, ref2)
	require.NotEqual(t, h1, h3)
}

// createAndHashSnapshotClone creates the snapshot clone for ref (registering its
// t.Cleanup drop) and returns its content hash, collapsing the repeated
// ref -> name -> dropCloneCleanup -> createSnapshotDatabase -> hashSnapshotDatabase
// sequence shared by the snapshot-hash tests in this file.
func createAndHashSnapshotClone(ctx context.Context, t *testing.T, db *database, ref *committerpb.TxRef) []byte {
	t.Helper()
	name := snapshotDatabaseName(ref)
	dropCloneCleanup(t, db, name) //nolint:contextcheck // cleanup must run after test ctx ends; see dropCloneCleanup.
	require.NoError(t, db.createSnapshotDatabase(ctx, name))
	hash, err := db.hasher.hashSnapshotDatabase(ctx, name)
	require.NoError(t, err)
	return hash
}

// TestSnapshotHashReflectsStateAndExclusions proves that rows in the _snapshot
// and _checkpoint system namespaces are EXCLUDED from the digest: those tables
// are not registered in ns__meta, so listHashedTables never hashes them.
func TestSnapshotHashReflectsStateAndExclusions(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.DBConf.Database)
	ctx, _ := createContext(t)

	// Seed a user namespace so the digest covers real hashed content.
	env.populateData(t, []string{"1"},
		writes(
			false,
			state{namespace: "1", keySuffix: 1, updateSequence: 0},
			state{namespace: "1", keySuffix: 2, updateSequence: 0},
		),
		&committerpb.TxStatusBatch{}, transactionIDToHeight{})

	// Baseline clone + hash.
	baselineRef := &committerpb.TxRef{BlockNum: 710000, TxNum: 0, TxId: "snap-excl-base"}
	baselineHash := createAndHashSnapshotClone(ctx, t, env.DB, baselineRef)
	require.NotEmpty(t, baselineHash)

	// Write rows ONLY into the excluded system namespaces (ns__snapshot,
	// ns__checkpoint). These tables exist (bootstrapped by
	// SetupSystemTablesAndNamespaces) but are not registered in ns__meta, so a
	// fresh clone's digest must be unchanged. No user-namespace rows are added
	// here, keeping this property independent of the different-state property.
	insertRawRow(t, env.DB, committerpb.SnapshotNamespaceID,
		nsRow{Key: []byte("excl-snap-key"), Value: []byte("excl-snap-val")})
	insertRawRow(t, env.DB, committerpb.CheckpointNamespaceID,
		nsRow{Key: []byte("excl-ckpt-key"), Value: []byte("excl-ckpt-val")})

	newRef := &committerpb.TxRef{BlockNum: 710100, TxNum: 0, TxId: "snap-excl-new"}
	newHash := createAndHashSnapshotClone(ctx, t, env.DB, newRef)

	// Excluded-namespace rows do not affect the digest.
	require.Equal(t, baselineHash, newHash)
}

// insertRawRow inserts a single row directly into ns_<nsID> on the source DB,
// bypassing ns__meta registration. Used to populate excluded system namespaces
// (_snapshot, _checkpoint) whose tables already exist.
func insertRawRow(t *testing.T, db *database, nsID string, row nsRow) {
	t.Helper()
	query := statedb.FmtNsID(
		"INSERT INTO ns_${NAMESPACE_ID} (key, value, version) VALUES ($1, $2, 0)", nsID,
	)
	require.NoError(t, retry.ExecuteSQL(t.Context(), db.retryProfile, db.pool, query, row.Key, row.Value))
}

type snapshotPollingRecord struct {
	state       *committerpb.SnapshotState
	version     int64
	recordCount int
}

// snapshotRecordForPolling reads a snapshot record, its version, and total
// snapshot-record count with retry protection. Snapshot cloning can briefly
// sever source-pool connections, so polling must not use FetchKeys directly.
func snapshotRecordForPolling(ctx context.Context, db *database, txID string) (*snapshotPollingRecord, bool) {
	record, err := retry.ExecuteWithResult(ctx, db.retryProfile, func() (*snapshotPollingRecord, error) {
		var raw []byte
		record := &snapshotPollingRecord{}
		err := db.pool.QueryRow(ctx, `
SELECT value, version, (SELECT COUNT(*) FROM ns__snapshot)
FROM ns__snapshot
WHERE key = $1`, []byte(txID)).Scan(&raw, &record.version, &record.recordCount)
		if err != nil {
			return nil, err
		}

		var state committerpb.SnapshotState
		if err := proto.Unmarshal(raw, &state); err != nil {
			return nil, err
		}

		record.state = &state
		return record, nil
	})
	if err != nil {
		return nil, false
	}

	return record, true
}

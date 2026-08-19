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

	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

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

func TestHashLatestSnapshotIfNeededReturnsContextCancellation(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	const txID = "snap-sched-canceled"
	seedSnapshotRow(t, env.dbEnv.DB, txID, committerpb.SnapshotState_PENDING)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(ctx), context.Canceled)
	requireSnapshotStatus(t, env.dbEnv.DB, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_PENDING,
	})
}

// TestSnapshotHashReEnqueueOnCompletedIsDeclined proves a finished snapshot is
// never re-hashed. Re-enqueueing one is not an error - callers legitimately try
// it on a scheduler tick, without knowing the snapshot already finished - it simply
// does nothing.
//
// This is a deliberate change from the original behavior, where a re-enqueue
// recomputed and rewrote the same deterministic digest. Redoing that work is
// pointless (the clone is immutable, so the digest cannot differ), and allowing
// it meant a COMPLETED row could be dragged back through IN_PROGRESS, which
// the scheduler then had to interpret. The hash-job lease treats COMPLETED and
// CHECKPOINTED as terminal instead (see acquireSnapshotHashLease).
func TestSnapshotHashReEnqueueOnCompletedIsDeclined(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	// Step 1: submit a snapshot TX through the real committer path, then let
	// the scheduler start the hash exactly as production would (createSnapshotIfPresent
	// -> commit -> hashLatestSnapshotIfNeeded).
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

	// The commit path starts no hash; the scheduler does. Stand in for the periodic
	// tick so the test does not have to wait out a whole lease TTL.
	require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(ctx))

	// Step 2: wait for the background worker to finish hashing (IN_PROGRESS ->
	// COMPLETED with a non-empty hash), and record the hash and version.
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

	// Step 3: another scheduler tick sees the COMPLETED record. This reports success
	// (nothing went wrong) but must not queue any work.
	require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(ctx))

	// The record is untouched: same hash, same version, no extra state transitions.
	require.Never(t, func() bool {
		pollCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		record, found := snapshotRecordForPolling(pollCtx, env.dbEnv.DB, ref.TxId)
		return found && (record.version != firstVersion || !bytes.Equal(firstHash, record.state.Hash))
	}, 3*time.Second, 200*time.Millisecond)
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

// TestHashLatestSnapshotIfNeededBacksOffForLiveWorker is the regression test for
// duplicate scheduling: two VCs polling on the same tick can both try to start the
// same job. Before the lease both did, because IN_PROGRESS could not distinguish a
// running job from an orphan. It also shows the database alone prevents duplicate
// scheduling -- no coordinator broadcast and no elected VC -- and that the loser
// leaves the winner's lease and the snapshot status untouched.
func TestHashLatestSnapshotIfNeededBacksOffForLiveWorker(t *testing.T) {
	t.Parallel()

	// A live lease must be respected whatever the row says: PENDING is a job the
	// winner has not started yet, IN_PROGRESS one it is running.
	for _, status := range []committerpb.SnapshotState_Status{
		committerpb.SnapshotState_PENDING,
		committerpb.SnapshotState_IN_PROGRESS,
	} {
		t.Run(status.String(), func(t *testing.T) {
			t.Parallel()
			env := newCommitterTestEnv(t)
			txID := "snap-sched-live-" + status.String()
			seedSnapshotRow(t, env.dbEnv.DB, txID, status)

			// Stand in for the VC that wins the race: it holds the lease while hashing.
			claim, err := env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
			require.NoError(t, err)
			require.NotNil(t, claim)

			// A second VC polling the same record must decline and change nothing.
			require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(t.Context()))
			requireSnapshotHashLeaseEquals(t, env.dbEnv.DB, claim)
			requireSnapshotStatus(t, env.dbEnv.DB, txID, expectedSnapshotState{status: status})
		})
	}
}

func TestHashLatestSnapshotIfNeededRejectsMissingTxRef(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	const txID = "snap-vc-sched-nil-ref"
	value, err := encodeSnapshotState(&committerpb.SnapshotState{
		Status: committerpb.SnapshotState_PENDING, CloneDatabase: "snapshot_corrupt",
	})
	require.NoError(t, err)
	nws := make(namespaceToWrites)
	nws.getOrCreate(committerpb.SnapshotNamespaceID).append([]byte(txID), value, 0)
	_, err = env.dbEnv.DB.commit(t.Context(), &statesToBeCommitted{newWrites: nws})
	require.NoError(t, err)

	err = env.dbEnv.DB.hashLatestSnapshotIfNeeded(t.Context())
	require.ErrorContains(t, err, "corrupt latest _snapshot record: missing TxRef")
}

// TestRunSnapshotHashScheduler proves the long-running loop is the single start
// path for hashing, by running the real loop against each lease state a tick can
// find, with no coordinator involvement:
//
//   - new: a freshly committed record with no lease row at all, which is what the
//     commit path leaves behind since it never starts a hash itself;
//   - expired: a lease row still present but past its deadline, which is what a VC
//     that died mid-hash leaves behind. The successor must overwrite a live-looking
//     row carrying somebody else's token, so this is the case the fencing turns on,
//     and it is distinct from the absent-lease case: acquireSnapshotHashLease
//     rejects an absent lease on `l != nil` and never reaches the deadline compare;
//   - completed: a terminal record, which the loop must leave untouched forever
//     rather than re-hash.
//
// Scheduling is periodic only: the first check happens one TTL after start, so a
// restart waits out at most one interval. With several VCs all running this loop,
// they may race on the same tick; acquireSnapshotHashLease's FOR UPDATE row lock
// makes one win and the rest back off.
func TestRunSnapshotHashScheduler(t *testing.T) {
	t.Parallel()
	// A TTL this short makes the tick interval, and hence the takeover, observable
	// within the test rather than after a production-length wait.
	const leaseTTL = 2 * time.Second

	for _, tc := range []struct {
		name string
		// blockNum must differ per case: the clone database name derives from it and
		// is cluster-global, so sharing one would make these parallel cases collide
		// on CREATE DATABASE and drop each other's clone on cleanup.
		blockNum uint64
		// status is the record state the tick finds.
		status committerpb.SnapshotState_Status
		// seedLease writes the lease row the tick finds; nil means no lease row,
		// standing in for a snapshot that has never been claimed.
		seedLease func(t *testing.T, db *database, txID string)
		// wantHashed is false for a terminal record, which must never be re-hashed.
		wantHashed bool
	}{{
		name:       "new",
		blockNum:   730200,
		status:     committerpb.SnapshotState_PENDING,
		wantHashed: true,
	}, {
		name:     "expired-lease",
		blockNum: 730201,
		status:   committerpb.SnapshotState_IN_PROGRESS,
		seedLease: func(t *testing.T, db *database, txID string) {
			t.Helper()
			// A token unrelated to any live worker: the successor has to replace it,
			// not match it.
			seedExpiredSnapshotHashLease(t, db, &snapshotHashLease{
				TxID:  txID,
				Token: uuid.MustParse("cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa"),
			})
		},
		wantHashed: true,
	}, {
		name:       "completed",
		blockNum:   730202,
		status:     committerpb.SnapshotState_COMPLETED,
		wantHashed: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCommitterTestEnv(t)
			testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
			ref := &committerpb.TxRef{BlockNum: tc.blockNum, TxNum: 0, TxId: "snap-vc-sched-" + tc.name}
			seedSnapshotRowWithClone(t, env.dbEnv.DB, ref, tc.status)
			if tc.seedLease != nil {
				tc.seedLease(t, env.dbEnv.DB, ref.TxId)
			}
			env.dbEnv.DB.resourceLimits.SnapshotHashLeaseTTL = leaseTTL

			// A COMPLETED record is seeded with no digest, so "hashed" stays a
			// question about this loop's writes rather than about the seeded state.
			hashed := func() bool {
				pollCtx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				record, found := snapshotRecordForPolling(pollCtx, env.dbEnv.DB, ref.TxId)
				return found && record.state.Status == committerpb.SnapshotState_COMPLETED &&
					len(record.state.Hash) > 0
			}

			// Step 1: start the loop against the record and lease seeded above.
			t.Log("Step 1: start the scheduler loop")
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- env.dbEnv.DB.runSnapshotHashScheduler(ctx) }()

			// Step 2: scheduling is periodic only, so nothing may happen before the
			// first tick. This is what bounds restart latency at one interval.
			t.Log("Step 2: nothing is hashed before the first tick")
			require.Never(t, hashed, leaseTTL/2, 100*time.Millisecond)

			if tc.wantHashed {
				// Step 3: the first tick claims the job and hashes it to completion,
				// taking the lease over from whatever the seeded state left behind.
				t.Log("Step 3: a tick claims the job and completes the hash")
				require.Eventually(t, hashed, 30*time.Second, 100*time.Millisecond)
				// The winner releases the lease on the way out, so the row is free for
				// the next snapshot instead of holding out the rest of its TTL.
				requireSnapshotHashLeaseEquals(t, env.dbEnv.DB, nil)
			} else {
				// Step 3: a terminal record is left alone across several ticks, so
				// "no re-hash" is a steady-state property, not a slow first tick.
				t.Log("Step 3: a terminal record is never hashed")
				require.Never(t, hashed, 3*leaseTTL, 100*time.Millisecond)
			}

			// Step 4: cancelling the context must stop the loop cleanly, not error.
			t.Log("Step 4: the loop stops cleanly on context cancellation")
			cancel()
			require.NoError(t, <-done)
		})
	}
}

func TestHashLatestSnapshotIfNeeded(t *testing.T) {
	t.Parallel()

	// Success cases: PENDING/IN_PROGRESS/FAILED all (re-)enqueue and eventually
	// reach COMPLETED with a non-empty hash; CHECKPOINTED and COMPLETED are
	// left untouched (no-op).
	for _, tc := range []struct {
		name          string
		initialStatus committerpb.SnapshotState_Status
		expectEnqueue bool
	}{
		{name: "PENDING enqueues and completes", initialStatus: committerpb.SnapshotState_PENDING, expectEnqueue: true},
		{
			name: "FAILED re-enqueues and completes", initialStatus: committerpb.SnapshotState_FAILED,
			expectEnqueue: true,
		},
		{name: "COMPLETED is a no-op", initialStatus: committerpb.SnapshotState_COMPLETED, expectEnqueue: false},
		{name: "CHECKPOINTED is a no-op", initialStatus: committerpb.SnapshotState_CHECKPOINTED, expectEnqueue: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCommitterTestEnv(t)
			testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
			ctx, _ := createContext(t)

			ref := &committerpb.TxRef{
				//nolint:gosec // enum values are small, non-negative, and bounded (0-5).
				BlockNum: 800300 + uint64(tc.initialStatus), TxNum: uint32(tc.initialStatus),
				TxId: fmt.Sprintf("snap-restart-%d", tc.initialStatus),
			}
			name := snapshotDatabaseName(ref)
			dropCloneCleanup(t, env.dbEnv.DB, name)
			require.NoError(t, env.dbEnv.DB.createSnapshotDatabase(ctx, name))

			// Seed a committed row directly at the target status (bypassing the
			// normal PENDING-then-worker-advances flow, so IN_PROGRESS/FAILED/
			// CHECKPOINTED can be set up without racing the running worker).
			seedSnapshotRowAtStatus(ctx, t, env.dbEnv.DB, ref, tc.initialStatus, name)

			require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(ctx))

			if !tc.expectEnqueue {
				// No-op: status must remain exactly what it was seeded to.
				record, found := snapshotRecordForPolling(ctx, env.dbEnv.DB, ref.TxId)
				require.True(t, found)
				require.Equal(t, tc.initialStatus, record.state.Status)
				return
			}

			require.Eventually(t, func() bool {
				pollCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				record, found := snapshotRecordForPolling(pollCtx, env.dbEnv.DB, ref.TxId)
				return found && record.state.Status == committerpb.SnapshotState_COMPLETED && len(record.state.Hash) > 0
			}, 30*time.Second, 100*time.Millisecond)
		})
	}
}

func TestHashLatestSnapshotIfNeededMissingCloneRecordsFailedAndHalts(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	testdb.EnsureSnapshotSchedule(t, env.dbEnv.DBConf.Database)
	ctx, _ := createContext(t)

	ref := &committerpb.TxRef{BlockNum: 800400, TxNum: 0, TxId: "snap-restart-missing-clone"}

	// Seed a committed row with an EMPTY clone_database: the row exists and is
	// committed, but there is nothing to hash and re-cloning a committed txID is
	// not safe, so nothing can repair it.
	seedSnapshotRowAtStatus(ctx, t, env.dbEnv.DB, ref, committerpb.SnapshotState_PENDING, "")

	require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(ctx))

	record, found := snapshotRecordForPolling(ctx, env.dbEnv.DB, ref.TxId)
	require.True(t, found)
	require.Equal(t, committerpb.SnapshotState_FAILED, record.state.Status)
	require.NotEmpty(t, record.state.Error)

	// FAILED is not terminal for the scheduler, so this record is re-read on every
	// tick. Later ticks must leave it alone: the unchanged version proves the row is
	// not rewritten once per TTL forever, which would also re-log the halt each time.
	require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(ctx))
	after, found := snapshotRecordForPolling(ctx, env.dbEnv.DB, ref.TxId)
	require.True(t, found)
	require.Equal(t, record.version, after.version, "a re-tick must not rewrite the FAILED record")
}

func TestHashLatestSnapshotIfNeededWithoutAnySnapshotIsNoOp(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	ctx, _ := createContext(t)

	require.NoError(t, env.dbEnv.DB.hashLatestSnapshotIfNeeded(ctx))
}

// seedSnapshotRow writes a _snapshot row directly, so a lease test can set up a
// given status without driving a whole commit through the pipeline. The named
// clone is not created: callers that let the scheduler hash the record use
// seedSnapshotRowWithClone instead.
func seedSnapshotRow(t *testing.T, db *database, txID string, status committerpb.SnapshotState_Status) {
	t.Helper()
	value, err := proto.Marshal(&committerpb.SnapshotState{
		TxRef:         &committerpb.TxRef{TxId: txID},
		Status:        status,
		CloneDatabase: "snapshot_lease_test",
	})
	require.NoError(t, err)

	nws := make(namespaceToWrites)
	nws.getOrCreate(committerpb.SnapshotNamespaceID).append([]byte(txID), value, 0)
	_, err = db.commit(t.Context(), &statesToBeCommitted{newWrites: nws})
	require.NoError(t, err)
}

// seedSnapshotRowWithClone is seedSnapshotRow plus a real clone database, for
// tests that let the scheduler hash the seeded record end to end. blockNum must be
// unique per test, because the clone name is derived from it and these tests run
// in parallel against the same server.
func seedSnapshotRowWithClone(
	t *testing.T, db *database, ref *committerpb.TxRef, status committerpb.SnapshotState_Status,
) {
	t.Helper()
	clone := snapshotDatabaseName(ref)
	dropCloneCleanup(t, db, clone)
	require.NoError(t, db.createSnapshotDatabase(t.Context(), clone))

	value, err := proto.Marshal(&committerpb.SnapshotState{
		TxRef: ref, Status: status, CloneDatabase: clone,
	})
	require.NoError(t, err)
	nws := make(namespaceToWrites)
	nws.getOrCreate(committerpb.SnapshotNamespaceID).append([]byte(ref.TxId), value, 0)
	// Retried for the same reason as seedSnapshotRowAtStatus: createSnapshotDatabase
	// above terminates this pool's backends on PostgreSQL, so the following commit
	// can hit a killed connection (SQLSTATE 57P01).
	_, err = retry.ExecuteWithResult(t.Context(), db.retryProfile, func() (*commitResult, error) {
		return db.commit(t.Context(), &statesToBeCommitted{newWrites: nws})
	})
	require.NoError(t, err)
}

type expectedSnapshotState struct {
	status committerpb.SnapshotState_Status
	digest []byte
}

func requireSnapshotStatus(t *testing.T, db *database, txID string, want expectedSnapshotState) {
	t.Helper()
	record, found := snapshotRecordForPolling(t.Context(), db, txID)
	require.True(t, found)
	require.Equal(t, want.status, record.state.Status)
	require.Equal(t, want.digest, record.state.Hash)
}

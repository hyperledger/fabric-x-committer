/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshothasher

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

// TestHashStartedCounter pins the only signal that a hash which never returns leaves
// behind. Every other metric here is after-the-fact -- the duration histogram is
// observed once a hash returns, and the failure counter only once a failure is
// classified -- so a scan still running, or a process killed mid-hash, would otherwise
// be indistinguishable from an idle service. Counting starts separately makes that
// visible as a gap between this counter and the two terminal ones.
func TestHashStartedCounter(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx := createContext(t)

	started := func() int {
		return int(test.GetMetricValue(t, env.metrics.hashStartedTotal))
	}
	require.Zero(t, started(), "no hash has started yet")

	env.dbEnv.SeedState(t, seededState([]string{"1", "2"}))
	ref := &committerpb.TxRef{BlockNum: 740000, TxNum: 0, TxId: "snap-counter-hashed"}
	env.seedRecord(t, ref, committerpb.SnapshotState_PENDING)

	require.NoError(t, env.scheduler.hashLatestSnapshotIfNeeded(ctx))
	require.Equal(t, 1, started(), "a hash that ran must be counted as started")
	require.Equal(t, 1, int(test.GetMetricValue(t, env.metrics.hashJobsCompletedTotal)))

	// A tick that finds terminal work must not look like a started hash: the record is
	// now COMPLETED, so the next tick has nothing to do. Were this counted, the gap
	// against the terminal counters would grow on every idle tick and the alert built on
	// it would fire on a healthy service.
	require.NoError(t, env.scheduler.hashLatestSnapshotIfNeeded(ctx))
	require.Equal(t, 1, started(), "a tick with nothing to hash must not count a start")
}

// TestHashSnapshotRejectsRecordTakenOverMidHash covers the two ways a record can move
// while a hash runs: a checkpoint transaction commits for a slow organization, or a
// second hasher (a deployment error) publishes first. Either way this job's write was
// decided against a status the record no longer holds, so it must be rejected rather
// than applied -- publishing COMPLETED over a CHECKPOINTED record would undo the
// checkpoint.
//
// Both writes hashSnapshot makes are covered, because they fail differently. Rejecting
// the COMPLETED write protects the digest directly. Rejecting the opening IN_PROGRESS
// write matters more subtly: were it unguarded it would overwrite CHECKPOINTED with
// IN_PROGRESS, and the COMPLETED write would then find its own value, match, and
// publish -- so the record would be un-checkpointed through a guard that looks like it
// is protecting it.
func TestHashSnapshotRejectsRecordTakenOverMidHash(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		blockNum uint64
		// held is the status this job read before the takeover, and therefore the premise
		// its writes are decided against. PENDING exercises the opening IN_PROGRESS write;
		// IN_PROGRESS skips it and exercises the publishing one.
		held    committerpb.SnapshotState_Status
		takenTo committerpb.SnapshotState_Status
	}{{
		name:     "checkpointed while publishing",
		blockNum: 750100,
		held:     committerpb.SnapshotState_IN_PROGRESS,
		takenTo:  committerpb.SnapshotState_CHECKPOINTED,
	}, {
		name:     "completed by another hasher",
		blockNum: 750101,
		held:     committerpb.SnapshotState_IN_PROGRESS,
		takenTo:  committerpb.SnapshotState_COMPLETED,
	}, {
		name:     "checkpointed before the job started",
		blockNum: 750102,
		held:     committerpb.SnapshotState_PENDING,
		takenTo:  committerpb.SnapshotState_CHECKPOINTED,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			ctx := createContext(t)
			env.dbEnv.SeedState(t, seededState([]string{"1"}))

			ref := &committerpb.TxRef{BlockNum: tc.blockNum, TxNum: 0, TxId: "snap-takeover-" + tc.name}
			env.seedRecord(t, ref, tc.held)

			// The record is moved out from under the job before it writes, which is what a
			// concurrent writer does. Moving it up front reproduces the interleaving
			// deterministically, without racing a real hash.
			require.NoError(t, env.state.Update(ctx, ref, statedb.SnapshotUpdate{Status: tc.takenTo}))

			state, err := env.state.ReadLatest(ctx)
			require.NoError(t, err)
			// The premise this job holds in memory: it read the record before the takeover.
			state.Status = tc.held

			pool, err := env.hasher.openClonePool(ctx, state.CloneDatabase)
			require.NoError(t, err)
			t.Cleanup(pool.Close)

			require.ErrorIs(t, env.scheduler.hashSnapshot(ctx, state, pool),
				statedb.ErrUnexpectedSnapshotStatus)

			// The new owner's status stands, and no digest from this job is published over it.
			record, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
			require.True(t, found)
			require.Equal(t, tc.takenTo, record.State.Status)
			require.Empty(t, record.State.Hash)
		})
	}
}

// TestHashLatestSnapshotIfNeeded walks every status a tick can find on the latest
// record. PENDING, IN_PROGRESS, and FAILED all still need hashing, so each reaches
// COMPLETED with a digest; a record that was orphaned mid-hash by a restart is
// exactly the IN_PROGRESS case, and a FAILED one is a retry. COMPLETED and
// CHECKPOINTED are terminal and must be left byte-for-byte alone, because
// re-hashing a checkpointed snapshot could only ever undo the checkpoint.
func TestHashLatestSnapshotIfNeeded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		status     committerpb.SnapshotState_Status
		blockNum   uint64
		wantHashed bool
	}{
		{name: "PENDING", status: committerpb.SnapshotState_PENDING, blockNum: 800300, wantHashed: true},
		{name: "IN_PROGRESS", status: committerpb.SnapshotState_IN_PROGRESS, blockNum: 800301, wantHashed: true},
		{name: "FAILED", status: committerpb.SnapshotState_FAILED, blockNum: 800302, wantHashed: true},
		{name: "COMPLETED", status: committerpb.SnapshotState_COMPLETED, blockNum: 800303, wantHashed: false},
		{name: "CHECKPOINTED", status: committerpb.SnapshotState_CHECKPOINTED, blockNum: 800304, wantHashed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			ctx := createContext(t)

			ref := &committerpb.TxRef{BlockNum: tc.blockNum, TxNum: 0, TxId: "snap-sched-" + tc.name}
			before := env.seedRecord(t, ref, tc.status)

			require.NoError(t, env.scheduler.hashLatestSnapshotIfNeeded(ctx))

			record, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
			require.True(t, found)
			if !tc.wantHashed {
				// An untouched row version is the strong assertion: the status alone would
				// still pass if the tick rewrote the same value.
				require.Equal(t, before.Version, record.Version)
				require.Empty(t, record.State.Hash)
				return
			}
			require.Equal(t, committerpb.SnapshotState_COMPLETED, record.State.Status)
			require.NotEmpty(t, record.State.Hash)
		})
	}
}

// TestSchedulerRun proves the long-running loop is the single start path for
// hashing: nothing happens before the first tick, which is what bounds restart
// latency at one poll interval, and a terminal record stays terminal across
// several ticks rather than merely surviving a slow first one.
func TestSchedulerRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// blockNum must differ per case: the clone name derives from it and is
		// cluster-global, so sharing one would make these parallel cases collide on
		// CREATE DATABASE and drop each other's clone on cleanup.
		blockNum   uint64
		status     committerpb.SnapshotState_Status
		wantHashed bool
	}{
		{name: "pending", blockNum: 730200, status: committerpb.SnapshotState_PENDING, wantHashed: true},
		{name: "orphaned", blockNum: 730201, status: committerpb.SnapshotState_IN_PROGRESS, wantHashed: true},
		{name: "completed", blockNum: 730202, status: committerpb.SnapshotState_COMPLETED, wantHashed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			ref := &committerpb.TxRef{BlockNum: tc.blockNum, TxNum: 0, TxId: "snap-run-" + tc.name}
			env.seedRecord(t, ref, tc.status)

			// A COMPLETED record is seeded with no digest, so "hashed" stays a question
			// about this loop's writes rather than about the seeded state.
			hashed := func() bool {
				pollCtx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				record, found := env.dbEnv.ReadSnapshotRecord(pollCtx, ref.TxId)
				return found && record.State.Status == committerpb.SnapshotState_COMPLETED &&
					len(record.State.Hash) > 0
			}

			t.Log("Step 1: start the scheduler loop")
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- env.scheduler.run(ctx) }()

			t.Log("Step 2: nothing is hashed before the first tick")
			require.Never(t, hashed, testPollInterval/2, 100*time.Millisecond)

			if tc.wantHashed {
				t.Log("Step 3: a tick hashes the record to completion")
				require.Eventually(t, hashed, 30*time.Second, 100*time.Millisecond)
			} else {
				t.Log("Step 3: a terminal record is never hashed")
				require.Never(t, hashed, 3*testPollInterval, 100*time.Millisecond)
			}

			t.Log("Step 4: the loop stops cleanly on context cancellation")
			cancel()
			require.NoError(t, <-done)
		})
	}
}

// TestHashLatestSnapshotIfNeededCorruptCloneStops covers a committed record whose
// clone is not there to hash. The clone is created before its snapshot transaction
// commits, so a committed record always names an existing clone; neither shape of
// absence -- no clone_database recorded, or a recorded name whose database is gone --
// can be produced by this system, which leaves external interference. Nothing can
// repair it, so the tick reports ErrCorruptSnapshotState and leaves the record
// untouched rather than recording an attempt that will never succeed.
func TestHashLatestSnapshotIfNeededCorruptCloneStops(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		blockNum   uint64
		clone      string
		wantReason string
	}{
		{
			name:       "unrecorded",
			blockNum:   800400,
			clone:      "",
			wantReason: "has no clone_database to hash",
		},
		{
			name:       "dropped",
			blockNum:   800401,
			clone:      "snapshot_dropped_800401",
			wantReason: "does not exist",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			ctx := createContext(t)

			ref := &committerpb.TxRef{BlockNum: tc.blockNum, TxNum: 0, TxId: "snap-corrupt-clone-" + tc.name}
			// No CreateSnapshotClone call: the clone this record names must not exist.
			env.dbEnv.SeedSnapshotRecord(t, vc.SnapshotFixture{
				Ref:           ref,
				Status:        committerpb.SnapshotState_PENDING,
				CloneDatabase: tc.clone,
			})

			// Bounded well under the database retry budget: a missing database that was
			// retried instead of reported terminal would exceed this, so that regression
			// fails here rather than merely being slow.
			tickCtx, tickCancel := context.WithTimeout(ctx, 30*time.Second)
			defer tickCancel()

			err := env.scheduler.hashLatestSnapshotIfNeeded(tickCtx)
			require.ErrorIs(t, err, ErrCorruptSnapshotState)
			require.ErrorContains(t, err, tc.wantReason)

			// The record must be left byte-for-byte as it was found: an unrepairable state is
			// not a failed attempt, and neither an error text nor an IN_PROGRESS marker from
			// an attempt that could never run belongs on it. The clone pool is therefore
			// opened before the IN_PROGRESS write, not after it.
			after, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
			require.True(t, found)
			require.Equal(t, committerpb.SnapshotState_PENDING, after.State.Status)
			require.Empty(t, after.State.Error)
			require.Empty(t, after.State.Hash)
		})
	}
}

// TestSchedulerRunStopsOnCorruptState proves the corrupt-state verdict reaches the
// poll loop: a condition no retry repairs must end the service, not be logged once
// per interval forever while the service reports itself healthy.
func TestSchedulerRunStopsOnCorruptState(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx := createContext(t)

	ref := &committerpb.TxRef{BlockNum: 800402, TxNum: 0, TxId: "snap-corrupt-run"}
	env.dbEnv.SeedSnapshotRecord(t, vc.SnapshotFixture{
		Ref:    ref,
		Status: committerpb.SnapshotState_PENDING,
	})

	done := make(chan error, 1)
	go func() { done <- env.scheduler.run(ctx) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrCorruptSnapshotState)
	case <-time.After(4 * testPollInterval):
		t.Fatal("run kept polling a corrupt record instead of stopping")
	}
}

// TestHashLatestSnapshotIfNeededRejectsMissingTxRef keeps a record without a TxRef
// a hard error: the scheduler cannot address a record it cannot name, and treating
// it as "nothing to do" would silently stall hashing forever.
func TestHashLatestSnapshotIfNeededRejectsMissingTxRef(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx := createContext(t)

	env.dbEnv.SeedSnapshotRecordWithoutTxRef(t, "snap-corrupt-no-ref", "snapshot_corrupt")

	require.ErrorContains(t, env.scheduler.hashLatestSnapshotIfNeeded(ctx),
		"corrupt latest _snapshot record: missing TxRef")
}

func TestHashLatestSnapshotIfNeededWithoutAnySnapshotIsNoOp(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx := createContext(t)

	require.NoError(t, env.scheduler.hashLatestSnapshotIfNeeded(ctx))
}

func TestHashLatestSnapshotIfNeededReturnsContextCancellation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ref := &committerpb.TxRef{BlockNum: 800500, TxNum: 0, TxId: "snap-cancelled"}
	before := env.seedRecord(t, ref, committerpb.SnapshotState_PENDING)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, env.scheduler.hashLatestSnapshotIfNeeded(ctx), context.Canceled)
	record, found := env.dbEnv.ReadSnapshotRecord(t.Context(), ref.TxId)
	require.True(t, found)
	require.Equal(t, before.Version, record.Version)
}

// seedRecord commits a `_snapshot` record for ref at status, together with its
// clone, and returns the record as stored so a test can assert a later tick did
// not rewrite it.
func (env *testEnv) seedRecord(
	t *testing.T, ref *committerpb.TxRef, status committerpb.SnapshotState_Status,
) *vc.SnapshotRecord {
	t.Helper()
	clone := vc.SnapshotDatabaseName(ref)
	env.dbEnv.CreateSnapshotClone(t, clone)
	env.dbEnv.SeedSnapshotRecord(t, vc.SnapshotFixture{
		Ref:           ref,
		Status:        status,
		CloneDatabase: clone,
	})
	record, found := env.dbEnv.ReadSnapshotRecord(t.Context(), ref.TxId)
	require.True(t, found)
	return record
}

// testPollInterval keeps a scheduler tick observable inside a test instead of
// after a production-length wait.
const testPollInterval = 2 * time.Second

// testEnv wires a scheduler and a hasher onto the same database a
// validator-committer commits into, which is the whole deployment this service
// assumes: the VC makes records durable, and this process discovers them.
type testEnv struct {
	dbEnv     *vc.DatabaseTestEnv
	config    *Config
	metrics   *perfMetrics
	hasher    *hasher
	state     *statedb.SnapshotStateManager
	scheduler *scheduler
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dbEnv := vc.NewDatabaseTestEnv(t)
	// YugabyteDB's clone prerequisite, made explicit: cloning requires a snapshot
	// schedule on the source keyspace.
	testdb.EnsureSnapshotSchedule(t, dbEnv.DBConf.Database)

	config := &Config{
		Database:     dbEnv.DBConf,
		PollInterval: testPollInterval,
		ResourceLimits: ResourceLimitsConfig{
			MaxWorkersForHash: 4,
			HashBatchSize:     1000,
		},
	}

	pool, err := statedb.NewPool(t.Context(), config.Database)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	state := statedb.NewSnapshotStateManager(pool, config.Database.Retry)
	hasher := &hasher{config: config}
	metrics := newSnapshotHasherMetrics()
	return &testEnv{
		dbEnv:   dbEnv,
		config:  config,
		metrics: metrics,
		hasher:  hasher,
		state:   state,
		scheduler: &scheduler{
			state:        state,
			hasher:       hasher,
			metrics:      metrics,
			pollInterval: config.PollInterval,
		},
	}
}

// createContext bounds a test at the suite's limit. Cancellation is registered as
// cleanup, so a caller needs nothing but the context.
func createContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

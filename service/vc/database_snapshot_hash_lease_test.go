/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

func TestSnapshotHashLeaseHeldBy(t *testing.T) {
	t.Parallel()

	now := time.Now()
	const txID = "snap-held-1"
	token := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	claim := &snapshotHashLease{TxID: txID, Token: token, ExpiresAt: now.Add(time.Minute)}

	for _, tc := range []struct {
		name  string
		lease *snapshotHashLease
		claim *snapshotHashLease
		want  bool
	}{
		{name: "nil lease is not held", claim: claim},
		{name: "nil claim owns nothing", lease: claim},
		{
			name:  "expired lease is not held",
			lease: &snapshotHashLease{TxID: txID, Token: token, ExpiresAt: now.Add(-time.Second)},
			claim: claim,
		},
		{
			name:  "another tx is not ours",
			lease: &snapshotHashLease{TxID: "other-tx", Token: token, ExpiresAt: now.Add(time.Minute)},
			claim: claim,
		},
		{
			name: "another token is not ours",
			lease: &snapshotHashLease{
				TxID:      txID,
				Token:     uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"),
				ExpiresAt: now.Add(time.Minute),
			},
			claim: claim,
		},
		{
			name:  "zero token never owns",
			lease: &snapshotHashLease{TxID: txID, ExpiresAt: now.Add(time.Minute)},
			claim: &snapshotHashLease{TxID: txID},
		},
		{
			name:  "same tx, same token, unexpired is ours",
			lease: claim,
			claim: claim,
			want:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, tc.lease.heldBy(tc.claim, now))
		})
	}
}

// TestSnapshotHashLeaseStaleOwnerCannotMutateSuccessor covers the worker that comes
// back. The lapsed worker's lease expired and a successor took the job over, but the
// lapsed worker is not dead -- it is slow, or was partitioned, and still believes it
// owns the job. Its next lease write lands on a row that is now the successor's.
//
// Both of its possible writes must be refused, for different reasons: a renewal would
// resurrect a job the successor is actively hashing, and a release would strip the
// successor of a live claim and invite a third worker in. The fencing token is what
// makes the two distinguishable, since both hold leases for the same tx.
func TestSnapshotHashLeaseStaleOwnerCannotMutateSuccessor(t *testing.T) {
	t.Parallel()
	after := takeSnapshotHashLeaseOver(t, "snap-lease-stale-owner", committerpb.SnapshotState_IN_PROGRESS)
	lapsedDB, lapsedClaim := after.lapsedDB, after.lapsedClaim
	require.NotEqual(t, lapsedClaim.Token, after.takeoverClaim.Token)

	renewed, err := lapsedDB.updateSnapshotHashLease(
		t.Context(), lapsedClaim, lapsedDB.resourceLimits.SnapshotHashLeaseTTL,
	)
	require.NoError(t, err)
	require.False(t, renewed, "a stale owner must not renew the successor's lease")

	cleared, err := lapsedDB.updateSnapshotHashLease(t.Context(), lapsedClaim, 0)
	require.NoError(t, err)
	require.False(t, cleared, "a stale owner must not clear the successor's lease")

	requireSnapshotHashLeaseEquals(t, after.takeoverDB, after.takeoverClaim)
}

// TestSnapshotHashLeaseCompletionIsIdempotent covers an ambiguous commit inside
// markSnapshotHashCompleted: the database applied the completion, but the commit
// call still returned an error, so its own internal retry runs the transaction
// again. The single caller never retries, so this is the only way a second attempt
// happens. The retry now finds the lease gone, because the attempt it cannot
// confirm is the one that cleared it, and must report success rather than "lost the
// lease", or a finished hash would be treated as failed. acquireSnapshotHashLease
// resolves the same ambiguity the same way.
//
// The evidence for success is that the digest on the row is our own, so the test
// also pins the two cases that must NOT be waved through: somebody else's digest
// under the same cleared lease, and a row that was never completed at all. Steps 2
// to 4 call the method directly to reach the retry branch, which is reachable in
// production only through a lost commit acknowledgement.
func TestSnapshotHashLeaseCompletionIsIdempotent(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	const txID = "snap-lease-repeat-completion"
	seedSnapshotRow(t, env.dbEnv.DB, txID, committerpb.SnapshotState_PENDING)

	requireCompletion := func(claim *snapshotHashLease, digest []byte, want bool) {
		t.Helper()
		completed, err := env.dbEnv.DB.markSnapshotHashCompleted(t.Context(), claim, digest)
		require.NoError(t, err)
		require.Equal(t, want, completed)
	}
	requireRecord := func() *snapshotPollingRecord {
		t.Helper()
		record, found := snapshotRecordForPolling(t.Context(), env.dbEnv.DB, txID)
		require.True(t, found)
		return record
	}

	// Step 1: hash the snapshot under a held lease and complete it normally.
	t.Log("Step 1: complete the hash under a held lease")
	claim, err := env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.NotNil(t, claim)
	requireSnapshotHashInProgress(t, env.dbEnv.DB, claim)

	digest := []byte("expected")
	requireCompletion(claim, digest, true)
	afterFirst := requireRecord()

	// Step 2: stand in for the internal retry after an ambiguous commit. The
	// completion already cleared the lease, so this attempt finds nothing it owns,
	// yet must still report success -- and must not rewrite the row, which the
	// unchanged version proves.
	t.Log("Step 2: re-running the committed completion succeeds without rewriting the row")
	requireCompletion(claim, digest, true)
	require.Equal(t, afterFirst.version, requireRecord().version)
	requireSnapshotHashLeaseEquals(t, env.dbEnv.DB, nil)

	// Step 3: the same cleared-lease state must not wave through a digest that is
	// not the one on the row; that digest belongs to a successor's hash.
	t.Log("Step 3: a different digest under the cleared lease is refused")
	requireCompletion(claim, []byte("different"), false)

	// Step 4: nor may a cleared lease complete a row that never reached COMPLETED,
	// so the retry path cannot be used to publish an unhashed snapshot.
	t.Log("Step 4: a never-completed row is refused")
	const pendingTxID = "snap-lease-completion-wrong-state"
	seedSnapshotRow(t, env.dbEnv.DB, pendingTxID, committerpb.SnapshotState_PENDING)
	requireCompletion(&snapshotHashLease{TxID: pendingTxID, Token: claim.Token}, digest, false)
}

// TestSnapshotHashLeaseStaleCompletion covers the most damaging failure the token
// exists to prevent: a worker's lease lapses, a successor takes the job over and
// re-hashes, and then the lapsed worker finishes its own now-obsolete hash and tries
// to publish it.
//
// The lapsed worker's digest was computed before the takeover and must never reach
// the row -- publishing it would hand callers a digest for a snapshot state nobody
// verified, and would clear the successor's lease while it is still hashing. This
// differs from the idempotent-retry case: there the lease is gone and the digest
// already on the row is ours, whereas here the lease is live and belongs to someone
// else.
func TestSnapshotHashLeaseStaleCompletion(t *testing.T) {
	t.Parallel()
	const txID = "snap-lease-stale-completion"

	// Step 1: one worker's lease lapses and a successor takes the job over.
	t.Log("Step 1: the successor takes over the lapsed lease and starts hashing")
	after := takeSnapshotHashLeaseOver(t, txID, committerpb.SnapshotState_PENDING)
	lapsedDB, takeoverDB := after.lapsedDB, after.takeoverDB
	lapsedClaim, takeoverClaim := after.lapsedClaim, after.takeoverClaim
	requireSnapshotHashInProgress(t, takeoverDB, takeoverClaim)

	// Step 2: the lapsed worker finishes its obsolete hash and tries to publish. The
	// token fences it, leaving both the successor's lease and the IN_PROGRESS row
	// exactly as the successor left them.
	t.Log("Step 2: the lapsed worker's stale completion is fenced, changing nothing")
	completed, err := lapsedDB.markSnapshotHashCompleted(t.Context(), lapsedClaim, []byte("stale"))
	require.NoError(t, err)
	require.False(t, completed)
	requireSnapshotHashLeaseEquals(t, takeoverDB, takeoverClaim)
	requireSnapshotStatus(t, takeoverDB, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_IN_PROGRESS,
	})

	// Step 3: the successor, the actual owner, publishes its digest and frees the lease.
	t.Log("Step 3: the successor publishes its own digest and releases the lease")
	completed, err = takeoverDB.markSnapshotHashCompleted(t.Context(), takeoverClaim, []byte("current"))
	require.NoError(t, err)
	require.True(t, completed)
	requireSnapshotHashLeaseEquals(t, takeoverDB, nil)
	requireSnapshotStatus(t, takeoverDB, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_COMPLETED,
		digest: []byte("current"),
	})
}

// TestDecodeSnapshotHashLeaseRejectsCorruptRow pins the one decision that keeps a
// corrupt lease row from becoming a concurrency bug: a row that fails to decode or
// validate is an error, never "no lease recorded". Reading it as absent would let a
// second worker acquire a lease for a snapshot someone else is already hashing.
//
// Only an empty value is legitimately absent, which is how a released lease is
// stored.
func newSnapshotHashLeaseToken() []byte {
	token := uuid.New()
	return token[:]
}

func TestDecodeSnapshotHashLeaseRejectsCorruptRow(t *testing.T) {
	t.Parallel()
	const txID = "snap-decode-1"

	valid, err := proto.Marshal(&servicepb.SnapshotHashLease{
		TxId:              txID,
		OwnerToken:        newSnapshotHashLeaseToken(),
		ExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano(),
	})
	require.NoError(t, err)

	shortToken, err := proto.Marshal(&servicepb.SnapshotHashLease{
		TxId:              txID,
		OwnerToken:        []byte("too-short"),
		ExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano(),
	})
	require.NoError(t, err)

	noTxID, err := proto.Marshal(&servicepb.SnapshotHashLease{
		OwnerToken:        newSnapshotHashLeaseToken(),
		ExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano(),
	})
	require.NoError(t, err)

	noDeadline, err := proto.Marshal(&servicepb.SnapshotHashLease{
		TxId:       txID,
		OwnerToken: newSnapshotHashLeaseToken(),
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		raw     []byte
		wantErr bool
		wantNil bool
	}{
		{name: "nil value is an absent lease", raw: nil, wantNil: true},
		{name: "empty value is an absent lease", raw: []byte{}, wantNil: true},
		{name: "undecodable bytes are an error", raw: []byte("not-a-proto"), wantErr: true},
		{name: "token of the wrong length is an error", raw: shortToken, wantErr: true},
		{name: "missing tx id is an error", raw: noTxID, wantErr: true},
		{name: "missing deadline is an error", raw: noDeadline, wantErr: true},
		{name: "a complete lease decodes", raw: valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lease, err := decodeSnapshotHashLease(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				require.Nil(t, lease, "a corrupt row must never yield a usable lease")
				return
			}
			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, lease)
				return
			}
			require.NotNil(t, lease)
		})
	}
}

// TestSnapshotHashLeaseStaleStartCannotRegressTerminalState covers the mirror image
// of the stale completion: worker A loses its lease before its *first* write rather
// than its last. A need only stall for one TTL between acquiring the lease and
// marking the row IN_PROGRESS, by which time B may have finished the job.
//
// An unfenced status write here would be the most damaging corruption in the design,
// because it is silent and, for CHECKPOINTED, unrecoverable: re-hashing only ever
// restores COMPLETED, so a clobbered checkpoint cannot be rebuilt by this loop. Both
// terminal states are checked for that reason.
func TestSnapshotHashLeaseStaleStartCannotRegressTerminalState(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		terminal committerpb.SnapshotState_Status
	}{
		{name: "completed", terminal: committerpb.SnapshotState_COMPLETED},
		{name: "checkpointed", terminal: committerpb.SnapshotState_CHECKPOINTED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			txID := "snap-lease-stale-start-" + tc.name

			// Step 1: one worker's lease lapses and a successor takes the job over.
			t.Log("Step 1: the successor takes over the lapsed lease")
			after := takeSnapshotHashLeaseOver(t, txID, committerpb.SnapshotState_PENDING)
			lapsedDB, takeoverDB := after.lapsedDB, after.takeoverDB

			// Step 2: the successor finishes the job, leaving the row terminal. COMPLETED
			// is what it publishes itself; CHECKPOINTED is the later transition a
			// checkpoint makes.
			t.Log("Step 2: the successor drives the row to " + tc.terminal.String())
			requireSnapshotHashInProgress(t, takeoverDB, after.takeoverClaim)
			completed, err := takeoverDB.markSnapshotHashCompleted(
				t.Context(), after.takeoverClaim, []byte("successor-digest"),
			)
			require.NoError(t, err)
			require.True(t, completed)
			if tc.terminal == committerpb.SnapshotState_CHECKPOINTED {
				require.NoError(t, takeoverDB.updateSnapshotState(t.Context(), &committerpb.TxRef{TxId: txID},
					snapshotStateUpdate{Status: committerpb.SnapshotState_CHECKPOINTED}))
			}

			// Step 3: the lapsed worker wakes up and takes its first step, believing it
			// still owns the job. The token must refuse the write and leave the terminal
			// row alone.
			t.Log("Step 3: the lapsed worker's stale start is refused, leaving the terminal row intact")
			started, err := lapsedDB.markSnapshotHashInProgress(
				t.Context(), after.lapsedClaim, &committerpb.TxRef{TxId: txID},
			)
			require.NoError(t, err)
			require.False(t, started, "a worker that lost its lease must not start hashing")
			requireSnapshotStatus(t, lapsedDB, txID, expectedSnapshotState{
				status: tc.terminal,
				digest: []byte("successor-digest"),
			})
		})
	}
}

// TestRenewSnapshotHashLeaseUntilDone covers the renewal loop's three exits, which
// are what actually stop two workers from hashing the same snapshot: a finished hash
// must end renewal quietly, and a lost lease must return an error, because that error
// is what cancels the errgroup and with it the now-obsolete hash.
func TestRenewSnapshotHashLeaseUntilDone(t *testing.T) {
	t.Parallel()

	t.Run("a finished hash stops renewal without an error", func(t *testing.T) {
		t.Parallel()
		const txID = "snap-renew-done"
		env := NewDatabaseTestEnv(t)
		seedSnapshotRow(t, env.DB, txID, committerpb.SnapshotState_PENDING)
		env.DB.resourceLimits.SnapshotHashLeaseTTL = time.Minute
		claim, err := env.DB.acquireSnapshotHashLease(t.Context(), txID)
		require.NoError(t, err)
		require.NotNil(t, claim)

		// Cancelling stands in for the hash goroutine returning, which is the normal,
		// successful stop and must not look like a failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.NoError(t, env.DB.renewSnapshotHashLeaseUntilDone(ctx, claim))
	})

	t.Run("a taken-over lease stops the hash with an error", func(t *testing.T) {
		t.Parallel()
		const txID = "snap-renew-taken-over"
		after := takeSnapshotHashLeaseOver(t, txID, committerpb.SnapshotState_PENDING)

		// The lease now belongs to the successor. A short TTL keeps the renewal tick
		// prompt; the first tick must observe the takeover and give up rather than renew.
		after.lapsedDB.resourceLimits.SnapshotHashLeaseTTL = 200 * time.Millisecond
		err := after.lapsedDB.renewSnapshotHashLeaseUntilDone(t.Context(), after.lapsedClaim)
		require.ErrorContains(t, err, "was taken over")

		// The successor's claim must survive the failed renewal untouched.
		requireSnapshotHashLeaseEquals(t, after.takeoverDB, after.takeoverClaim)
	})
}

// TestSnapshotHashLeaseOwnershipUsesDatabaseClock pins the clock that decides
// ownership. Every VC has its own host clock, and those clocks drift; if expiry were
// judged against them, a VC running fast would consider a live lease expired and
// hash a snapshot another VC is already hashing. Ownership is therefore compared
// only against clock_timestamp(), read in the same statement as the lease row.
//
// The fixture is seeded with a database-clock deadline for exactly that reason,
// and the wait polls the database clock rather than sleeping on the host's.
func TestSnapshotHashLeaseOwnershipUsesDatabaseClock(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	const txID = "snap-lease-database-clock"
	seedSnapshotRow(t, env.dbEnv.DB, txID, committerpb.SnapshotState_IN_PROGRESS)

	var expiresAt time.Time
	require.NoError(t, env.dbEnv.DB.pool.QueryRow(
		t.Context(), "SELECT clock_timestamp() + interval '2 seconds'",
	).Scan(&expiresAt))

	seedSnapshotHashLease(t, env.dbEnv.DB, &snapshotHashLease{
		TxID:      txID,
		Token:     uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"),
		ExpiresAt: expiresAt,
	})

	claim, err := env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.Nil(t, claim, "database clock must keep ownership blocked before expiry")

	require.Eventually(t, func() bool {
		var expired bool
		require.NoError(t, env.dbEnv.DB.pool.QueryRow(
			t.Context(), "SELECT clock_timestamp() >= $1", expiresAt,
		).Scan(&expired))
		return expired
	}, 10*time.Second, 50*time.Millisecond, "database clock must reach fixture expiry")

	claim, err = env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.NotNil(t, claim, "acquire must succeed after database-clock expiry")
}

// TestSnapshotHashLeaseConcurrentAcquireHasOneWinner covers the race the FOR UPDATE
// row lock exists for: two VCs, on separate connections, calling acquire at the same
// instant on a snapshot with no lease. Both read "unheld" at READ COMMITTED unless
// something serializes them, and both would then write themselves in and hash
// concurrently.
//
// The sequential decline tests cannot show this, because there the second caller
// reads an already-committed lease. Here the two transactions genuinely overlap, so
// only the row lock can produce a single winner.
func TestSnapshotHashLeaseConcurrentAcquireHasOneWinner(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)
	// Two interchangeable pollers, not a stale/live pair: neither holds a lease, and
	// the test asserts only that exactly one of them wins.
	firstPoller := env.DB
	secondPoller := newSnapshotHashLeaseTestDatabase(t, env)
	const txID = "snap-lease-concurrent"
	seedSnapshotRow(t, firstPoller, txID, committerpb.SnapshotState_IN_PROGRESS)

	ready := make(chan struct{})
	claims := make(chan *snapshotHashLease, 2)
	errs := make(chan error, 2)
	for _, db := range []*database{firstPoller, secondPoller} {
		go func() {
			<-ready
			claim, err := db.acquireSnapshotHashLease(t.Context(), txID)
			claims <- claim
			errs <- err
		}()
	}
	close(ready)

	winnerCount := 0
	for range 2 {
		require.NoError(t, <-errs)
		if <-claims != nil {
			winnerCount++
		}
	}
	require.Equal(t, 1, winnerCount)
}

// TestSnapshotHashLeaseConcurrentAcquireAndCompletion covers an acquire racing the
// completion of the job it is trying to claim. Both transactions lock the lease row
// and then the snapshot row, so they serialize, and the invariant must hold in either
// order: the owner publishes its digest and the poller walks away empty-handed.
//
// The two orders reach that outcome by different routes, which is the point of racing
// them rather than sequencing one. If the completion commits first it clears the lease
// and leaves COMPLETED, so the acquire finds no lease to block it and is stopped
// instead by the terminal-status check -- the reason acquire locks the snapshot row it
// never writes. If the acquire goes first it finds the owner's lease still live and
// declines immediately, leaving the completion to commit unopposed.
func TestSnapshotHashLeaseConcurrentAcquireAndCompletion(t *testing.T) {
	t.Parallel()
	const txID = "snap-lease-acquire-vs-complete"
	env := NewDatabaseTestEnv(t)
	owner, poller := env.DB, newSnapshotHashLeaseTestDatabase(t, env)
	seedSnapshotRow(t, owner, txID, committerpb.SnapshotState_PENDING)

	// The owner's lease stays live for the whole race, so the poller may never claim
	// the job: a minute is far longer than the race it is contending in.
	owner.resourceLimits.SnapshotHashLeaseTTL = time.Minute
	claim, err := owner.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.NotNil(t, claim)
	requireSnapshotHashInProgress(t, owner, claim)

	digest := []byte("owner-digest")
	ready := make(chan struct{})
	type acquireResult struct {
		claim *snapshotHashLease
		err   error
	}
	acquired := make(chan acquireResult, 1)
	completed := make(chan acquireResult, 1)

	go func() {
		<-ready
		rival, acquireErr := poller.acquireSnapshotHashLease(t.Context(), txID)
		acquired <- acquireResult{claim: rival, err: acquireErr}
	}()
	go func() {
		<-ready
		published, completeErr := owner.markSnapshotHashCompleted(t.Context(), claim, digest)
		var asClaim *snapshotHashLease
		if published {
			asClaim = claim
		}
		completed <- acquireResult{claim: asClaim, err: completeErr}
	}()
	close(ready)

	rival, completion := <-acquired, <-completed
	require.NoError(t, rival.err)
	require.NoError(t, completion.err)
	require.Nil(t, rival.claim, "a live owner's job must never be claimed by a poller")
	require.NotNil(t, completion.claim, "the lease owner must be able to publish its digest")

	// The digest is on the row and the lease is freed, whichever order the two ran in.
	requireSnapshotStatus(t, owner, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_COMPLETED,
		digest: digest,
	})
	requireSnapshotHashLeaseEquals(t, owner, nil)
}

// TestSnapshotHashLeaseConcurrentAcquireAndRelease covers the third pairing of the
// three lease writes: a release racing another VC's acquire. A single VC cannot
// produce this race -- hashSnapshotUnderClaim joins the renewal goroutine before
// completing and only then runs its deferred release -- so both parties here are
// separate VCs contending for the same lease row.
//
// The two subtests are the two reasons a lease gets released, and they must differ:
// a release that follows a published digest must never reopen the job, whereas a
// release after a failed hash must return the job so it can be retried.
func TestSnapshotHashLeaseConcurrentAcquireAndRelease(t *testing.T) {
	t.Parallel()

	// This is the interleaving to be sure of: one VC sees IN_PROGRESS, the owner then
	// publishes its digest and marks the row COMPLETED, and the owner's release races
	// the other VC's acquire. No re-hash may be handed out, because a second hash of a
	// snapshot already published would overwrite a digest callers may have read.
	//
	// It cannot happen, and the reason is that completion is a single transaction:
	// markSnapshotHashCompleted writes the digest, sets COMPLETED and clears the lease
	// together (database_snapshot_hash.go, one tx), so a freed lease is never visible
	// without COMPLETED beside it. The acquire therefore sees one of two states, and
	// declines in both: the lease still live (another worker owns it), or the lease
	// gone and the row terminal. The deferred release is a no-op by then, since the
	// lease it would clear is already cleared.
	t.Run("a release after completion never reopens the job", func(t *testing.T) {
		t.Parallel()
		const txID = "snap-lease-release-vs-acquire-completed"
		env := NewDatabaseTestEnv(t)
		owner, poller := env.DB, newSnapshotHashLeaseTestDatabase(t, env)
		seedSnapshotRow(t, owner, txID, committerpb.SnapshotState_PENDING)

		// The TTL outlives the race, so the acquire can only ever be refused by the
		// terminal-status check, never by an incidental expiry.
		owner.resourceLimits.SnapshotHashLeaseTTL = time.Minute
		claim, err := owner.acquireSnapshotHashLease(t.Context(), txID)
		require.NoError(t, err)
		require.NotNil(t, claim)
		requireSnapshotHashInProgress(t, owner, claim)

		digest := []byte("owner-digest")
		published, err := owner.markSnapshotHashCompleted(t.Context(), claim, digest)
		require.NoError(t, err)
		require.True(t, published)

		// The owner's deferred release now races the other VC's acquire.
		released, acquired := raceSnapshotHashLeaseReleaseAndAcquire(t, owner, poller, claim)
		require.False(t, released, "completion already cleared the lease, so the release is a no-op")
		require.Nil(t, acquired, "a published snapshot must never be handed out for re-hashing")

		// The digest survived the race untouched, and the lease stayed clear.
		requireSnapshotStatus(t, poller, txID, expectedSnapshotState{
			status: committerpb.SnapshotState_COMPLETED,
			digest: digest,
		})
		requireSnapshotHashLeaseEquals(t, poller, nil)
	})

	// The mirror case: hashing failed or was cancelled, so the release runs with the
	// row still IN_PROGRESS and no digest. Here the job must come back, otherwise a
	// failed hash would strand the snapshot until the TTL elapsed.
	//
	// Either commit order is acceptable, so the assertion is the invariant rather than
	// one outcome: the acquire may lose and see the lease still live, but the job is
	// claimable once the release lands.
	t.Run("a release without completion returns the job", func(t *testing.T) {
		t.Parallel()
		const txID = "snap-lease-release-vs-acquire-unfinished"
		env := NewDatabaseTestEnv(t)
		owner, poller := env.DB, newSnapshotHashLeaseTestDatabase(t, env)
		seedSnapshotRow(t, owner, txID, committerpb.SnapshotState_PENDING)

		owner.resourceLimits.SnapshotHashLeaseTTL = time.Minute
		claim, err := owner.acquireSnapshotHashLease(t.Context(), txID)
		require.NoError(t, err)
		require.NotNil(t, claim)
		requireSnapshotHashInProgress(t, owner, claim)

		released, acquired := raceSnapshotHashLeaseReleaseAndAcquire(t, owner, poller, claim)
		require.True(t, released, "the owner still held the lease, so its release must land")

		// If the acquire ran first it saw a live lease and declined; the job must still
		// be free now that the release has committed.
		if acquired == nil {
			acquired, err = poller.acquireSnapshotHashLease(t.Context(), txID)
			require.NoError(t, err)
		}
		require.NotNil(t, acquired, "an unfinished job must be reclaimable after its lease is released")
		require.NotEqual(t, claim.Token, acquired.Token, "a reclaim must carry a fresh fencing token")

		// No digest was published, so the row is still awaiting a hash.
		requireSnapshotStatus(t, poller, txID, expectedSnapshotState{
			status: committerpb.SnapshotState_IN_PROGRESS,
		})
	})
}

// raceSnapshotHashLeaseReleaseAndAcquire starts owner's release of claim and a
// competing acquire by poller at the same instant, and reports whether the release
// rewrote the row and what the acquire was granted.
//
// owner and poller must be distinct *database values, because the contention being
// reproduced is between two VCs on separate connections: a single pool could let the
// two statements share a transaction and never contend on the lease row at all.
func raceSnapshotHashLeaseReleaseAndAcquire(
	t *testing.T, owner, poller *database, claim *snapshotHashLease,
) (released bool, acquired *snapshotHashLease) {
	t.Helper()
	ready := make(chan struct{})
	releases := make(chan bool, 1)
	claims := make(chan *snapshotHashLease, 1)
	errs := make(chan error, 2)

	go func() {
		<-ready
		ok, err := owner.updateSnapshotHashLease(t.Context(), claim, 0)
		releases <- ok
		errs <- err
	}()
	go func() {
		<-ready
		rival, err := poller.acquireSnapshotHashLease(t.Context(), claim.TxID)
		claims <- rival
		errs <- err
	}()
	close(ready)

	released, acquired = <-releases, <-claims
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	return released, acquired
}

// TestSnapshotHashLeaseExpiredOwnerRacingAcquireCannotPublish is the dangerous
// version of the acquire-vs-complete race: the owner's lease has already lapsed when
// it tries to publish, and a poller is acquiring at the same instant.
//
// Neither order may let the lapsed owner publish, because its digest describes a
// snapshot no live lease covered while it was being hashed. If the poller commits
// first, the lease row carries the poller's token and heldBy fails on the token. If
// the lapsed owner commits first, heldBy fails on the deadline, and the retry path
// that forgives a cleared lease does not apply either: that path demands a nil lease
// AND an already-COMPLETED row carrying this digest, whereas here the owner's own
// expired lease is still present and the row is only IN_PROGRESS.
//
// Discarding the digest costs one re-hash, which is the deliberate trade: an expired
// lease is owned by nobody, so work done under it can never be trusted.
func TestSnapshotHashLeaseExpiredOwnerRacingAcquireCannotPublish(t *testing.T) {
	t.Parallel()
	const txID = "snap-lease-expired-vs-acquire"
	after := takeSnapshotHashLeaseOver(t, txID, committerpb.SnapshotState_PENDING)

	// The fixture's successor took the job over; releasing its claim returns the row to
	// the free-but-not-terminal state an expired owner actually races into, and leaves
	// that worker a plain poller rather than an owner. The roles are rebound to say so:
	// here the lapsed worker is the expired owner trying to publish, and the other is
	// simply the next VC to poll.
	requireSnapshotHashLeaseCleared(t, after.takeoverDB, after.takeoverClaim)
	expiredOwner, expiredClaim := after.lapsedDB, after.lapsedClaim
	poller := after.takeoverDB

	ready := make(chan struct{})
	published := make(chan bool, 1)
	publishErrs := make(chan error, 1)
	claims := make(chan *snapshotHashLease, 1)
	acquireErrs := make(chan error, 1)

	go func() {
		<-ready
		ok, err := expiredOwner.markSnapshotHashCompleted(t.Context(), expiredClaim, []byte("stale-digest"))
		published <- ok
		publishErrs <- err
	}()
	go func() {
		<-ready
		claim, err := poller.acquireSnapshotHashLease(t.Context(), txID)
		claims <- claim
		acquireErrs <- err
	}()
	close(ready)

	require.NoError(t, <-publishErrs)
	require.NoError(t, <-acquireErrs)
	require.False(t, <-published, "a worker whose lease expired must never publish its digest")
	require.NotNil(t, <-claims, "a free non-terminal job must remain claimable")

	// The stale digest reached neither the row nor the status.
	requireSnapshotStatus(t, poller, txID, expectedSnapshotState{
		status: committerpb.SnapshotState_PENDING,
	})
}

// TestSnapshotHashLeaseRenewalKeepsJob proves renewal is what protects a hash
// that legitimately outlives the TTL from being taken over mid-run.
func TestSnapshotHashLeaseRenewalKeepsJob(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	const txID = "snap-lease-renew"
	seedSnapshotRow(t, env.dbEnv.DB, txID, committerpb.SnapshotState_IN_PROGRESS)

	claim, err := env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.NotNil(t, claim)

	renewed, err := env.dbEnv.DB.updateSnapshotHashLease(
		t.Context(), claim, env.dbEnv.DB.resourceLimits.SnapshotHashLeaseTTL,
	)
	require.NoError(t, err)
	require.True(t, renewed, "renewing a lease we hold should extend it")

	// Still exclusive after renewal.
	blockedClaim, err := env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.Nil(t, blockedClaim)

	// Once released, renewal must not resurrect the lease: a worker that lost the
	// job has to stop, not push the deadline forward again.
	requireSnapshotHashLeaseCleared(t, env.dbEnv.DB, claim)
	renewed, err = env.dbEnv.DB.updateSnapshotHashLease(
		t.Context(), claim, env.dbEnv.DB.resourceLimits.SnapshotHashLeaseTTL,
	)
	require.NoError(t, err)
	require.False(t, renewed, "renewal must not recreate a lease we no longer hold")

	// The freed row is claimable again, so releasing does not strand the job.
	claim, err = env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.NotNil(t, claim, "acquire should succeed once the lease is released")
}

// TestSnapshotHashLeaseDeclinedWhenFinished proves a finished snapshot is never
// re-hashed, whatever the lease says: COMPLETED and CHECKPOINTED are terminal.
func TestSnapshotHashLeaseDeclinedWhenFinished(t *testing.T) {
	t.Parallel()

	for _, status := range []committerpb.SnapshotState_Status{
		committerpb.SnapshotState_COMPLETED,
		committerpb.SnapshotState_CHECKPOINTED,
	} {
		t.Run(status.String(), func(t *testing.T) {
			t.Parallel()
			env := newCommitterTestEnv(t)
			txID := "snap-lease-done-" + status.String()
			seedSnapshotRow(t, env.dbEnv.DB, txID, status)

			claim, err := env.dbEnv.DB.acquireSnapshotHashLease(t.Context(), txID)
			require.NoError(t, err)
			require.Nil(t, claim, "%s snapshot must not be re-hashed", status)
		})
	}
}

func newSnapshotHashLeaseTestDatabase(t *testing.T, env *DatabaseTestEnv) *database {
	t.Helper()
	db, err := newDatabase(t.Context(), env.DBConf, newVCServiceMetrics(), defaultTestResourceLimits())
	require.NoError(t, err)
	t.Cleanup(db.close)
	return db
}

// snapshotHashLeaseAfterTakeover is the state left behind when one worker's lease
// lapses and a second worker takes the job over. The lapsed worker is the one whose
// writes must now be refused; the takeover worker is the one that legitimately owns
// the job.
//
// There are two databases because each stands for a separate VC worker, and the two
// workers must disagree about the lease TTL: the lapsed worker's is short enough to
// lapse, the takeover worker's is long enough to hold. A single database has one
// resourceLimits pointer and so one TTL, whereas each of these owns its own, letting
// the fixture set them apart. Distinct databases also mean distinct pools, so the
// only thing passing between the workers is the lease row itself -- exactly the
// channel the fencing works over.
//
// The two claims are the stale/live pair the fencing acts on. lapsedClaim expired
// when its worker stopped renewing; takeoverClaim is the live claim, carrying a
// different token. Both differences matter, since heldBy demands a matching token
// AND an unexpired deadline, and lapsedClaim now fails on each.
type snapshotHashLeaseAfterTakeover struct {
	lapsedDB, takeoverDB       *database
	lapsedClaim, takeoverClaim *snapshotHashLease
}

// takeSnapshotHashLeaseOver drives a real takeover rather than seeding one, so the
// stale/live pair a test asserts against is the pair production would produce,
// rather than two hand-built structs that may not match what acquire writes.
//
// The takeover has to be driven by expiry alone, because the snapshot row carries no
// liveness signal: it reads IN_PROGRESS whether the worker is still hashing or died
// mid-hash. acquireSnapshotHashLease therefore never infers takeover from status --
// it grants the job when the lease has lapsed, and refuses only when the status is
// terminal.
func takeSnapshotHashLeaseOver(
	t *testing.T, txID string, status committerpb.SnapshotState_Status,
) snapshotHashLeaseAfterTakeover {
	t.Helper()
	env := NewDatabaseTestEnv(t)
	f := snapshotHashLeaseAfterTakeover{lapsedDB: env.DB, takeoverDB: newSnapshotHashLeaseTestDatabase(t, env)}
	seedSnapshotRow(t, f.lapsedDB, txID, status)

	// A TTL of one millisecond stands in for a worker that stopped renewing.
	f.lapsedDB.resourceLimits.SnapshotHashLeaseTTL = time.Millisecond
	lapsedClaim, err := f.lapsedDB.acquireSnapshotHashLease(t.Context(), txID)
	require.NoError(t, err)
	require.NotNil(t, lapsedClaim)
	f.lapsedClaim = lapsedClaim

	f.takeoverDB.resourceLimits.SnapshotHashLeaseTTL = time.Minute
	require.Eventually(t, func() bool {
		f.takeoverClaim, err = f.takeoverDB.acquireSnapshotHashLease(t.Context(), txID)
		require.NoError(t, err)
		return f.takeoverClaim != nil
	}, 10*time.Second, 50*time.Millisecond, "an expired lease must be takeable")
	return f
}

// requireSnapshotHashLeaseCleared releases claim's lease and asserts the row was
// actually rewritten, i.e. the lease was still claim's to clear.
func requireSnapshotHashLeaseCleared(t *testing.T, db *database, claim *snapshotHashLease) {
	t.Helper()
	cleared, err := db.updateSnapshotHashLease(t.Context(), claim, 0)
	require.NoError(t, err)
	require.True(t, cleared)
}

// requireSnapshotHashInProgress advances a seeded row the way hashSnapshotUnderClaim
// does -- through the fenced transition, under claim -- so a lease test can reach
// IN_PROGRESS without running a real hash.
func requireSnapshotHashInProgress(t *testing.T, db *database, claim *snapshotHashLease) {
	t.Helper()
	started, err := db.markSnapshotHashInProgress(t.Context(), claim, &committerpb.TxRef{TxId: claim.TxID})
	require.NoError(t, err)
	require.True(t, started)
}

// seedSnapshotHashLease writes lease directly through the production write path,
// so a test can set up a given ownership state without racing a real worker.
func seedSnapshotHashLease(t *testing.T, db *database, lease *snapshotHashLease) {
	t.Helper()
	tx, rollBack, err := db.beginTx(t.Context())
	require.NoError(t, err)
	defer rollBack()
	require.NoError(t, writeSnapshotHashLease(t.Context(), tx, lease))
	require.NoError(t, tx.Commit(t.Context()))
}

// seedExpiredSnapshotHashLease rewrites claim's lease row with a deadline already
// in the past, standing in for a worker that died holding it: the row survives the
// crash and simply stops being renewed. The deadline comes from the database clock,
// because that is the only clock lease expiry is ever judged against.
func seedExpiredSnapshotHashLease(t *testing.T, db *database, claim *snapshotHashLease) {
	t.Helper()
	var expired time.Time
	require.NoError(t, db.pool.QueryRow(
		t.Context(), "SELECT clock_timestamp() - interval '1 hour'",
	).Scan(&expired))
	seedSnapshotHashLease(t, db, &snapshotHashLease{
		TxID:      claim.TxID,
		Token:     claim.Token,
		ExpiresAt: expired,
	})
}

// requireSnapshotHashLeaseEquals asserts the persisted lease, read back through
// the production read path so the stored bytes must also decode and validate.
func requireSnapshotHashLeaseEquals(t *testing.T, db *database, want *snapshotHashLease) {
	t.Helper()
	tx, rollBack, err := db.beginTx(t.Context())
	require.NoError(t, err)
	defer rollBack()
	lease, _, err := readAndLockSnapshotHashLease(t.Context(), tx)
	require.NoError(t, err)
	require.Equal(t, want, lease)
}

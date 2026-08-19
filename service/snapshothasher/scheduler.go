/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshothasher

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// ErrCorruptSnapshotState marks a durable `_snapshot` record that contradicts an
// invariant the commit path guarantees. The clone database is created before its
// snapshot transaction commits, so a committed record always names a clone that
// exists; neither a missing name nor a missing database can arise from anything this
// system does, which leaves external interference or storage corruption. No retry
// repairs either, so the service stops instead of logging the same impossible state
// once per interval forever.
var ErrCorruptSnapshotState = errors.New("corrupt durable snapshot state")

// scheduler drives snapshot hashing from durable state alone.
//
// Exactly one instance of this service runs per deployment, so the scheduler needs
// no lease, ownership token, or leader election to keep two workers off the same
// job: hashing runs inline on the polling goroutine, so this process hashes one
// snapshot at a time, and no other process hashes at all. Running a second
// instance is a deployment error, and would show up as both processes writing the
// same deterministic digest for the same clone.
type scheduler struct {
	state        *statedb.SnapshotStateManager
	hasher       *hasher
	metrics      *perfMetrics
	pollInterval time.Duration
}

// run polls the latest `_snapshot` record until ctx ends, hashing it whenever it
// still needs hashing.
//
// The first check happens one interval after start, and hashing runs inline, so a
// job that outlives the interval simply delays the next check rather than starting
// a second hash.
func (s *scheduler) run(ctx context.Context) error {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// A transient failure is logged and retried on the next tick rather than stopping
			// the service: the durable record is still there to be picked up, and there is
			// nothing else this process must do in the meantime. A corrupt record is the
			// exception -- waiting cannot repair it, so it ends the loop and, with it, the
			// service. The counters are incremented where the failure is classified, since
			// only there is a failed poll distinguishable from a failed hash.
			switch err := s.hashLatestSnapshotIfNeeded(ctx); {
			case err == nil:
			case errors.Is(err, ErrCorruptSnapshotState):
				return err
			default:
				logger.Errorf("failed to hash the latest snapshot: %+v", err)
			}
		}
	}
}

// hashLatestSnapshotIfNeeded hashes the latest `_snapshot` record when that record
// still needs it, using the status and clone database stored in the record. There is
// at most one non-terminal snapshot at a time, and the latest-snapshot pointer is
// written atomically with its row, so the latest record is always the one that needs
// hashing.
//
// A CHECKPOINTED or COMPLETED record has nothing to do, so it is left untouched. A
// record that is committed but carries no clone_database, or that names a clone whose
// database is gone, is reported as ErrCorruptSnapshotState, which stops the service: a
// committed txID must always have a matching clone, so a missing one is not a hash
// failure to record and retry.
//
// A failure to establish whether there is work at all -- an unreadable pointer, an
// undecodable record, a status this service does not know -- is counted as a poll
// error, which is the only signal that separates a service that cannot reach its state
// database from an idle one. A hash that starts and then fails is counted by
// hashSnapshot instead, so the two cannot be confused.
func (s *scheduler) hashLatestSnapshotIfNeeded(ctx context.Context) error {
	state, err := s.readRecordNeedingHash(ctx)
	if err != nil || state == nil {
		return err
	}

	// The pool is opened here, before any state is written for this attempt, so a clone
	// that is not there to hash leaves the record exactly as it was found rather than
	// stranded at IN_PROGRESS by an attempt that could never run.
	pool, err := s.hasher.openClonePool(ctx, state.CloneDatabase)
	if err != nil {
		if !errors.Is(err, ErrCorruptSnapshotState) {
			promutil.AddToCounter(s.metrics.pollErrorsTotal, 1)
		}
		return err
	}
	defer pool.Close()

	return s.hashSnapshot(ctx, state, pool)
}

// readRecordNeedingHash returns the latest `_snapshot` record when that record still
// needs hashing, or (nil, nil) when there is nothing to do -- no snapshot was ever
// accepted, or the latest one is already COMPLETED or CHECKPOINTED. Every way of
// failing to decide which of those holds is counted as a poll error, since such a tick
// neither completes nor fails a hash job.
func (s *scheduler) readRecordNeedingHash(ctx context.Context) (*committerpb.SnapshotState, error) {
	state, err := s.state.ReadLatest(ctx)
	if err != nil {
		promutil.AddToCounter(s.metrics.pollErrorsTotal, 1)
		return nil, err
	}
	if state == nil {
		return nil, nil // no snapshot has ever been accepted.
	}
	if state.TxRef == nil {
		promutil.AddToCounter(s.metrics.pollErrorsTotal, 1)
		return nil, errors.New("corrupt latest _snapshot record: missing TxRef")
	}
	txID := state.TxRef.TxId

	switch state.Status {
	case committerpb.SnapshotState_CHECKPOINTED, committerpb.SnapshotState_COMPLETED:
		return nil, nil // terminal / already done -- nothing to hash.
	case committerpb.SnapshotState_PENDING, committerpb.SnapshotState_IN_PROGRESS, committerpb.SnapshotState_FAILED:
		// fall through to the clone check below.
	default:
		promutil.AddToCounter(s.metrics.pollErrorsTotal, 1)
		return nil, errors.Newf("_snapshot record for tx %s has unexpected status %s", txID, state.Status)
	}

	if state.CloneDatabase == "" {
		return nil, errors.Wrapf(ErrCorruptSnapshotState,
			"committed snapshot tx %s has no clone_database to hash", txID)
	}
	return state, nil
}

// hashSnapshot marks the record IN_PROGRESS, hashes the already-open clone pool, and
// publishes the digest.
//
// A failed hash is recorded as FAILED with the cause, which is not terminal: the
// next tick reads the same record and tries again. Re-hashing is always safe,
// because a clone is immutable, so the digest of a given clone cannot change
// between attempts.
//
// TODO: no hash failure is classified as permanent today. The per-page and
// table-discovery retries inside the hasher pass no terminal errors, so a permanent
// failure (a permission change, a dropped table) is retried for the whole retry budget and
// then retried again on the next tick. Wrap those classes with retry.ErrNonRetryable at
// the query sites so the cause reaches the record in seconds rather than after the
// budget.
func (s *scheduler) hashSnapshot(
	ctx context.Context, state *committerpb.SnapshotState, pool *pgxpool.Pool,
) error {
	ref := state.TxRef
	clone := state.CloneDatabase
	if state.Status != committerpb.SnapshotState_IN_PROGRESS {
		if err := s.state.Update(ctx, ref, statedb.SnapshotUpdate{
			Status: committerpb.SnapshotState_IN_PROGRESS,
		}); err != nil {
			return fmt.Errorf("failed to mark snapshot %s IN_PROGRESS: %w", clone, err)
		}
	}

	logger.Infof("hashing snapshot clone [%s] for tx [%s]", clone, ref.TxId)
	start := time.Now()
	// The start timestamp is what makes "hashing for too long" alertable: the duration
	// histogram is only observed once a hash returns, so for the many minutes a clone
	// scan takes, a busy service and an idle one would publish identical numbers. It is
	// cleared however the hash ends, so a stale value cannot outlive the job.
	promutil.SetGauge(s.metrics.hashStartedTimestampSeconds, int(start.Unix()))
	defer promutil.SetGauge(s.metrics.hashStartedTimestampSeconds, 0)
	digest, hashErr := s.hasher.hashSnapshotDatabase(ctx, pool)
	if hashErr != nil {
		return s.failHash(ctx, ref, clone, hashErr)
	}
	promutil.Observe(s.metrics.hashDurationSeconds, time.Since(start))

	if err := s.state.Update(ctx, ref, statedb.SnapshotUpdate{
		Status: committerpb.SnapshotState_COMPLETED,
		Digest: digest,
	}); err != nil {
		return fmt.Errorf("failed to mark snapshot %s COMPLETED: %w", clone, err)
	}
	promutil.AddToCounter(s.metrics.hashJobsCompletedTotal, 1)
	logger.Infof("hashed snapshot clone [%s] for tx [%s] in %s", clone, ref.TxId, time.Since(start))
	return nil
}

// failHash records why a hash attempt ended, so an operator sees the cause on the
// record itself rather than only in this process's log. The original error is
// returned either way; a failure to persist it is joined onto it rather than
// replacing it, since the hash failure is the more informative of the two.
func (s *scheduler) failHash(
	ctx context.Context, ref *committerpb.TxRef, clone string, hashErr error,
) error {
	err := fmt.Errorf("failed to hash snapshot %s: %w", clone, hashErr)

	// A cancelled context is a shutdown, not a bad snapshot: recording FAILED would
	// need a database write we can no longer make, and the record is already in a
	// state the next start re-reads. It is not counted as a failed job either, since
	// a restart mid-hash would otherwise raise the failure count on every deploy.
	if ctx.Err() != nil {
		return err
	}
	promutil.AddToCounter(s.metrics.hashJobsFailedTotal, 1)
	updateErr := s.state.Update(ctx, ref, statedb.SnapshotUpdate{
		Status: committerpb.SnapshotState_FAILED,
		ErrMsg: err.Error(),
	})
	return errors.Join(err, updateErr)
}

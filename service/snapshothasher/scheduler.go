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

	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/snapshotstate"
)

// ErrCorruptSnapshotState marks a durable `_snapshot` record that contradicts an
// invariant the commit path guarantees. The clone database is created before its
// snapshot transaction commits, so a committed record always names a clone that
// exists; neither a missing name nor a missing database can arise from anything this
// system does, which leaves external interference or storage corruption. No retry
// repairs either, so the service stops instead of logging the same impossible state
// once per interval forever.
var ErrCorruptSnapshotState = errors.New("corrupt durable snapshot state")

// schedulerConfig carries the scheduler's collaborators, which exceed the
// four-argument limit as positional parameters.
type schedulerConfig struct {
	state        *snapshotstate.StateManager
	hasher       *hasher
	metrics      *perfMetrics
	pollInterval time.Duration
}

// scheduler drives snapshot hashing from durable state alone.
//
// Exactly one instance of this service runs per deployment, so the scheduler needs
// no lease, ownership token, or leader election to keep two workers off the same
// job: hashing runs inline on the polling goroutine, so this process hashes one
// snapshot at a time, and no other process hashes at all. Running a second
// instance is a deployment error, and would show up as both processes writing the
// same deterministic digest for the same clone.
type scheduler struct {
	schedulerConfig
}

func newScheduler(config *schedulerConfig) *scheduler {
	return &scheduler{schedulerConfig: *config}
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
			// A transient failure is logged, counted, and retried on the next tick rather
			// than stopping the service: the durable record is still there to be picked up,
			// and there is nothing else this process must do in the meantime. Counting it is
			// what separates a service that cannot reach its state database from an idle one.
			// A corrupt record is the exception -- waiting cannot repair it, so it ends the
			// loop and, with it, the service.
			switch err := s.hashLatestSnapshotIfNeeded(ctx); {
			case err == nil:
			case errors.Is(err, ErrCorruptSnapshotState):
				return err
			default:
				promutil.AddToCounter(s.metrics.pollErrorsTotal, 1)
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
// record that is committed but carries no clone_database is reported as
// ErrCorruptSnapshotState, which stops the service: a committed txID must always have
// a matching clone, so a missing one is not a hash failure to record and retry. A
// clone that is named but no longer exists is caught one level down, when the pool on
// it cannot be opened.
func (s *scheduler) hashLatestSnapshotIfNeeded(ctx context.Context) error {
	state, err := s.state.ReadLatest(ctx)
	if err != nil {
		return err
	}
	if state == nil {
		return nil // no snapshot has ever been accepted.
	}
	if state.TxRef == nil {
		return errors.New("corrupt latest _snapshot record: missing TxRef")
	}
	txID := state.TxRef.TxId

	switch state.Status {
	case committerpb.SnapshotState_CHECKPOINTED, committerpb.SnapshotState_COMPLETED:
		return nil // terminal / already done -- nothing to hash.
	case committerpb.SnapshotState_PENDING, committerpb.SnapshotState_IN_PROGRESS, committerpb.SnapshotState_FAILED:
		// fall through to the clone check below.
	default:
		return errors.Newf("_snapshot record for tx %s has unexpected status %s", txID, state.Status)
	}

	if state.CloneDatabase == "" {
		return errors.Wrapf(ErrCorruptSnapshotState,
			"committed snapshot tx %s has no clone_database to hash", txID)
	}
	return s.hashSnapshot(ctx, state)
}

// hashSnapshot marks the record IN_PROGRESS, hashes its clone, and publishes the
// digest.
//
// A failed hash is recorded as FAILED with the cause, which is not terminal: the
// next tick reads the same record and tries again. Re-hashing is always safe,
// because a clone is immutable, so the digest of a given clone cannot change
// between attempts. A corrupt state is the exception and is returned as-is, since
// recording it on the record would be treating an unrepairable condition as a
// retryable attempt.
//
// TODO: only a missing clone database is classified as permanent today (via
// statedb.NewPool). The per-page and table-discovery retries inside the hasher pass no
// terminal errors, so any other permanent failure (a permission change, a decode
// mismatch) is retried for the whole retry budget and then retried again on the next
// tick. Wrap those classes with retry.ErrNonRetryable at the query sites so the cause
// reaches the record in seconds rather than after the budget.
func (s *scheduler) hashSnapshot(ctx context.Context, state *committerpb.SnapshotState) error {
	ref := state.TxRef
	clone := state.CloneDatabase
	if state.Status != committerpb.SnapshotState_IN_PROGRESS {
		if err := s.state.Update(ctx, ref, snapshotstate.Update{
			Status: committerpb.SnapshotState_IN_PROGRESS,
		}); err != nil {
			return fmt.Errorf("failed to mark snapshot %s IN_PROGRESS: %w", clone, err)
		}
	}

	logger.Infof("hashing snapshot clone [%s] for tx [%s]", clone, ref.TxId)
	start := time.Now()
	s.markHashStarted(start)
	defer s.markHashFinished()
	digest, hashErr := s.hasher.hashSnapshotDatabase(ctx, clone)
	if errors.Is(hashErr, ErrCorruptSnapshotState) {
		return hashErr
	}
	if hashErr != nil {
		return s.failHash(ctx, ref, clone, hashErr)
	}
	promutil.Observe(s.metrics.hashDurationSeconds, time.Since(start))

	if err := s.state.Update(ctx, ref, snapshotstate.Update{
		Status: committerpb.SnapshotState_COMPLETED,
		Digest: digest,
	}); err != nil {
		return fmt.Errorf("failed to mark snapshot %s COMPLETED: %w", clone, err)
	}
	promutil.AddToCounter(s.metrics.hashJobsCompletedTotal, 1)
	logger.Infof("hashed snapshot clone [%s] for tx [%s] in %s", clone, ref.TxId, time.Since(start))
	return nil
}

// markHashStarted publishes that a hash is running, and since when. Both are set
// together so a scrape never sees an in-progress hash without a start time.
func (s *scheduler) markHashStarted(start time.Time) {
	promutil.SetGauge(s.metrics.hashInProgress, 1)
	promutil.SetGauge(s.metrics.hashStartedTimestampSeconds, int(start.Unix()))
}

// markHashFinished clears both gauges however the hash ended -- success, failure,
// shutdown, or an unrepairable record -- so a stuck value cannot outlive the job and
// misreport an idle service as busy.
func (s *scheduler) markHashFinished() {
	promutil.SetGauge(s.metrics.hashInProgress, 0)
	promutil.SetGauge(s.metrics.hashStartedTimestampSeconds, 0)
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
	updateErr := s.state.Update(ctx, ref, snapshotstate.Update{
		Status: committerpb.SnapshotState_FAILED,
		ErrMsg: err.Error(),
	})
	return errors.Join(err, updateErr)
}

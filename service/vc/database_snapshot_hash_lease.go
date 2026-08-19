/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

const (
	// snapshotHashLeaseRenewalDivisor derives the renewal interval from the TTL: a
	// worker renews every TTL/divisor, so several renewals are attempted before the
	// lease would expire.
	snapshotHashLeaseRenewalDivisor = 4

	// minSnapshotHashLeaseTTL floors the TTL that drives the scheduler and renewal
	// tickers. Config validation already requires gte=1m, so this only guards a
	// ResourceLimitsConfig built in code that leaves the field at its zero value:
	// time.NewTicker panics on a non-positive interval, which would take down the
	// whole VC goroutine rather than degrade the background hashing it affects.
	minSnapshotHashLeaseTTL = time.Second

	// selectLeaseForUpdateSQL locks the lease row for the duration of the enclosing
	// transaction. The row lock is what makes lease acquisition mutually exclusive:
	// beginTx runs at READ COMMITTED, so without FOR UPDATE two workers could both
	// read "no live lease" and both write themselves in, each believing it won. With
	// FOR UPDATE the second worker blocks until the first commits, then reads the
	// lease the first just took and correctly backs off.
	selectLeaseForUpdateSQL = "SELECT value, clock_timestamp() FROM metadata WHERE key = $1 FOR UPDATE;"

	// selectLeaseSQL reads the lease row without locking it, for the advisory
	// pre-check in snapshotHashLeaseHeldByOther. Both statements read the database
	// clock alongside the value so a lease is never judged against a host clock.
	selectLeaseSQL = "SELECT value, clock_timestamp() FROM metadata WHERE key = $1;"
)

// snapshotHashLeaseKey is the metadata key holding the snapshot hash lease.
var snapshotHashLeaseKey = []byte("snapshot hash lease")

// snapshotHashLease records which snapshot is being hashed, acquisition identity,
// and deadline. Token fences stale workers after same-txID takeover.
type snapshotHashLease struct {
	TxID      string
	Token     uuid.UUID
	ExpiresAt time.Time
}

// held reports whether this lease still keeps other workers out as of now.
func (l *snapshotHashLease) held(now time.Time) bool {
	return l != nil && l.ExpiresAt.After(now)
}

// heldBy reports whether claim exactly owns this unexpired tokenized lease. An
// expired lease is owned by nobody: a successor may take it over at any moment, so
// a worker whose lease lapsed must not renew it, clear it, or publish under it.
func (l *snapshotHashLease) heldBy(claim *snapshotHashLease, now time.Time) bool {
	return l != nil && claim != nil && l.Token != uuid.Nil &&
		l.TxID == claim.TxID && l.Token == claim.Token && l.ExpiresAt.After(now)
}

// renewSnapshotHashLeaseUntilDone keeps lease alive until ctx ends, running in
// the same errgroup as the hash it protects. A ctx that ends means hashing
// finished, which is a normal, successful stop.
//
// Both failure modes stop the hash by returning an error, which cancels the
// group's context:
//
//   - the lease is no longer ours: another worker has taken the job over, and
//     continuing would duplicate its work and race its writes;
//   - renewal itself failed: updateSnapshotHashLease caps a lease write at one
//     TTL, so an exhausted attempt means the lease has certainly lapsed and this
//     worker can no longer prove it owns the job.
func (db *database) renewSnapshotHashLeaseUntilDone(ctx context.Context, lease *snapshotHashLease) error {
	txID := lease.TxID
	// Renew every TTL/divisor so several renewals are attempted while the lease is
	// alive, rather than betting the job on a single one.
	ticker := time.NewTicker(db.snapshotHashLeaseTTL() / snapshotHashLeaseRenewalDivisor)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil // hashing finished; nothing left to protect.
		case <-ticker.C:
			renewed, err := db.updateSnapshotHashLease(ctx, lease, db.resourceLimits.SnapshotHashLeaseTTL)
			if err != nil {
				return fmt.Errorf("failed to renew snapshot hash lease for tx %s: %w", txID, err)
			}
			if !renewed {
				return errors.Newf("snapshot hash lease for tx %s was taken over", txID)
			}
		}
	}
}

// acquireSnapshotHashLease tries to claim the hash job for txID. It is the
// single gate every start-hashing path goes through, so only one worker hashes
// a snapshot at a time.
//
// It returns nil without error when hashing must not start: snapshot is already
// done (COMPLETED/CHECKPOINTED), or another live lease owns the job. Otherwise,
// it returns a claim containing acquisition token used to fence stale workers.
//
// The read and the write happen in one transaction under SELECT ... FOR UPDATE
// (see selectLeaseForUpdateSQL), which is what makes this a genuine
// compare-and-set rather than a check-then-act race: two workers arriving
// together are serialized by the row lock, so exactly one of them observes the
// lease as unheld and wins.
//
// The loser is never handed an error, but the two supported databases get there
// differently, so db.retryProfile below is load-bearing rather than boilerplate for
// transient faults:
//
//   - PostgreSQL: FOR UPDATE blocks rather than failing. The loser waits at the
//     SELECT until the winner commits, and because beginTx runs at READ COMMITTED
//     the unblocked statement re-reads the row as newly committed. It therefore
//     observes the winner's lease, not the unheld row it originally queued on, and
//     backs off with nil.
//   - YugabyteDB: a waiter under contention may instead be aborted with a
//     retryable conflict (SQLSTATE 40001) rather than queueing to completion. That
//     error never reaches the caller: the whole closure is inside
//     retry.ExecuteWithResult with no terminal errors registered, so the attempt is
//     rolled back by the deferred rollback and retried. By then the winner's lease
//     is committed and visible, so the retry backs off with nil as well.
//
// The candidate, including its token, is built once outside the retry loop. A
// retry therefore recognizes a lease it wrote itself in an attempt whose commit
// succeeded but whose result was lost, and reports that claim as its own instead
// of backing off from it.
//
//nolint:gocognit // one transaction: ownership check, terminal check, lease write.
func (db *database) acquireSnapshotHashLease(
	ctx context.Context, txID string,
) (*snapshotHashLease, error) {
	token, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate snapshot hash lease owner token")
	}
	candidate := &snapshotHashLease{TxID: txID, Token: token}

	claim, acquireErr := retry.ExecuteWithResult(ctx, db.retryProfile, func() (*snapshotHashLease, error) {
		tx, rollBackFunc, txErr := db.beginTx(ctx)
		if txErr != nil {
			return nil, txErr
		}
		defer rollBackFunc()

		lease, now, leaseErr := readAndLockSnapshotHashLease(ctx, tx)
		if leaseErr != nil {
			return nil, leaseErr
		}
		if lease.heldBy(candidate, now) {
			return candidate, nil
		}
		if lease.held(now) {
			return nil, nil // another live worker owns the job.
		}

		// The snapshot row is read FOR UPDATE even though this transaction never
		// writes it, because granting a lease is a check-then-act spanning two rows:
		// we decide from the snapshot status here, then write the *lease* row below.
		// Without the row lock, markSnapshotHashCompleted could commit COMPLETED in
		// between, and we would hand out a lease to re-hash a finished snapshot. The
		// lock is taken after the lease row, keeping the lease-before-snapshot order
		// every multi-row lease transaction uses, so two of them cannot deadlock.
		state, stateErr := readAndLockSnapshotState(ctx, tx, txID)
		if stateErr != nil {
			return nil, stateErr
		}
		if state.Status == committerpb.SnapshotState_COMPLETED ||
			state.Status == committerpb.SnapshotState_CHECKPOINTED {
			return nil, nil // hashing already finished; it must never run again.
		}

		candidate.ExpiresAt = now.Add(db.resourceLimits.SnapshotHashLeaseTTL)
		if writeErr := writeSnapshotHashLease(ctx, tx, candidate); writeErr != nil {
			return nil, writeErr
		}
		return candidate, errors.Wrapf(tx.Commit(ctx), "failed to commit snapshot hash lease for tx %s", txID)
	})
	if acquireErr != nil {
		return nil, fmt.Errorf("failed to acquire snapshot hash lease for tx %s: %w", txID, acquireErr)
	}
	return claim, nil
}

// updateSnapshotHashLease rewrites the lease row only while claim still owns it: a
// positive ttl pushes the deadline that far past the database clock (renewal), and
// a zero ttl clears the row (release). Callers must leave a lease alone once it is
// no longer theirs -- a renewal would resurrect a job another worker took over, and
// a release would drop that worker's live claim -- so the ownership check and the
// write happen in one transaction under SELECT ... FOR UPDATE.
//
// It reports whether the row was actually rewritten, which is false exactly when
// the lease was already taken over or cleared. Not rewriting is not corrupting: a
// lease nobody clears simply expires on its own.
//
// A renewal here and an acquire by another worker can run at the same instant. Both
// take the lease row FOR UPDATE first, so they serialize on it, and either order is
// safe:
//
//   - renewal commits first: the successor's acquire then re-reads a lease whose
//     deadline has just moved forward, sees it live, and backs off. Hashing
//     continues undisturbed.
//   - acquire commits first (this worker's lease had genuinely lapsed): the renewal
//     re-reads a row carrying the successor's token, so heldBy fails on the token
//     and this returns false WITHOUT overwriting it. renewSnapshotHashLeaseUntilDone
//     turns that into "taken over" and cancels the now-obsolete hash.
//
// The token comparison is what makes the second case safe, and it is why renewal is
// a compare-and-set under the row lock rather than a blind UPDATE on key: a blind
// write would push the deadline of a lease the successor now owns, leaving two
// workers hashing the same snapshot. Reaching that case at all means this worker
// stalled for a full TTL despite renewing every TTL/snapshotHashLeaseRenewalDivisor,
// so being treated as dead is the intended outcome, not a lost race.
//
// This is the only operation that retries on a shortened budget. A lease write
// must never retry for longer than the lease itself lives: retrying past the
// deadline would either resurrect a job a successor already took over, or leave
// the renewal loop believing it still owns a lease that has lapsed. Bounding the
// budget at one TTL makes an exhausted retry *proof* that the lease is gone, which
// is what lets renewSnapshotHashLeaseUntilDone stop hashing. Every other database
// operation, including acquisition, holds no lease and so uses db.retryProfile.
func (db *database) updateSnapshotHashLease(
	ctx context.Context, claim *snapshotHashLease, ttl time.Duration,
) (bool, error) {
	// WithDefaults returns a clone, so shortening the budget cannot affect the
	// profile shared by the rest of the package. The TTL is copied to a local rather
	// than pointed at the config field, which several components share, so the budget
	// cannot silently follow a later mutation of that field.
	maxRetryTime := db.resourceLimits.SnapshotHashLeaseTTL
	leaseRetry := db.retryProfile.WithDefaults()
	leaseRetry.MaxElapsedTime = &maxRetryTime

	var written bool
	err := retry.Execute(ctx, leaseRetry, func() error {
		written = false
		tx, rollBackFunc, err := db.beginTx(ctx)
		if err != nil {
			return err
		}
		defer rollBackFunc()

		lease, now, err := readAndLockSnapshotHashLease(ctx, tx)
		if err != nil {
			return err
		}
		if !lease.heldBy(claim, now) {
			return nil // not ours anymore; leave it for whoever holds it.
		}

		var next *snapshotHashLease
		if ttl > 0 {
			next = &snapshotHashLease{TxID: claim.TxID, Token: claim.Token, ExpiresAt: now.Add(ttl)}
		}
		if err := writeSnapshotHashLease(ctx, tx, next); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return errors.Wrapf(err, "failed to commit snapshot hash lease write for tx %s", claim.TxID)
		}
		written = true
		return nil
	})
	return written, err //nolint:wrapcheck // already wrapped inside the retried closure.
}

// snapshotHashLeaseHeldByOther reports whether some other worker plainly holds a
// live lease, so this VC can skip the acquire transaction entirely.
//
// This is an advisory fast path, not the gate: acquireSnapshotHashLease still does
// the authoritative compare-and-set under FOR UPDATE. It exists because every VC
// polls once per TTL, and in the steady state all but one of them would otherwise
// open a write transaction and block on the lease row that the working VC holds,
// only to roll back. A stale answer here is harmless: a false "held" costs one
// polling interval, and a false "free" just proceeds to acquire, which decides.
func (db *database) snapshotHashLeaseHeldByOther(ctx context.Context) (bool, error) {
	held, err := retry.ExecuteWithResult(ctx, db.retryProfile, func() (bool, error) {
		var raw []byte
		var now time.Time
		if err := db.pool.QueryRow(ctx, selectLeaseSQL, snapshotHashLeaseKey).Scan(&raw, &now); err != nil {
			return false, errors.Wrap(err, "failed to read snapshot hash lease")
		}
		lease, err := decodeSnapshotHashLease(raw)
		if err != nil {
			return false, err
		}
		return lease.held(now), nil
	})
	return held, err //nolint:wrapcheck // already wrapped inside the retried closure.
}

// readAndLockSnapshotHashLease reads and decodes the lease row, locking it FOR
// UPDATE so the caller's subsequent write cannot be interleaved with another
// worker's.
//
// Every transaction that locks both the lease row and a _snapshot row must take the
// lease FIRST (acquireSnapshotHashLease and markSnapshotHashCompleted both do).
// updateSnapshotState locks only the _snapshot row and updateSnapshotHashLease only
// the lease row, so no transaction takes them in the opposite order and none of them
// can deadlock. A new lease-and-state transaction must keep that order.
func readAndLockSnapshotHashLease(ctx context.Context, tx pgx.Tx) (*snapshotHashLease, time.Time, error) {
	var raw []byte
	var now time.Time
	if err := tx.QueryRow(ctx, selectLeaseForUpdateSQL, snapshotHashLeaseKey).Scan(&raw, &now); err != nil {
		return nil, time.Time{}, errors.Wrap(err, "failed to read snapshot hash lease")
	}
	lease, err := decodeSnapshotHashLease(raw)
	return lease, now, err
}

// decodeSnapshotHashLease decodes a persisted lease. A NULL/empty value means no
// lease is recorded and yields a nil lease.
//
// A row that decodes but fails validation is a hard error, never treated as "no
// lease": silently reading a corrupt lease as absent would let a second worker
// hash the same snapshot concurrently.
func decodeSnapshotHashLease(raw []byte) (*snapshotHashLease, error) {
	if len(raw) == 0 {
		return nil, nil //nolint:nilnil // no lease is recorded.
	}

	var persisted servicepb.SnapshotHashLease
	if err := proto.Unmarshal(raw, &persisted); err != nil {
		return nil, errors.Wrap(err, "failed to decode snapshot hash lease")
	}
	if len(persisted.OwnerToken) != len(uuid.UUID{}) ||
		persisted.TxId == "" || persisted.ExpiresAtUnixNano <= 0 {
		return nil, errors.New("invalid snapshot hash lease")
	}

	return &snapshotHashLease{
		TxID:      persisted.TxId,
		Token:     uuid.UUID(persisted.OwnerToken),
		ExpiresAt: time.Unix(0, persisted.ExpiresAtUnixNano),
	}, nil
}

// writeSnapshotHashLease stores lease with its fencing token, or clears the
// lease row when lease is nil.
func writeSnapshotHashLease(ctx context.Context, tx pgx.Tx, lease *snapshotHashLease) error {
	var raw []byte
	if lease != nil {
		var err error
		raw, err = proto.Marshal(&servicepb.SnapshotHashLease{
			TxId:              lease.TxID,
			OwnerToken:        lease.Token[:],
			ExpiresAtUnixNano: lease.ExpiresAt.UnixNano(),
		})
		if err != nil {
			return errors.Wrap(err, "failed to encode snapshot hash lease")
		}
	}
	_, err := tx.Exec(ctx, setMetadataPrepSQLStmt, snapshotHashLeaseKey, raw)
	return errors.Wrap(err, "failed to write snapshot hash lease")
}

// snapshotHashLeaseTTL returns the configured lease TTL, floored so it can safely
// drive a ticker. See minSnapshotHashLeaseTTL.
func (db *database) snapshotHashLeaseTTL() time.Duration {
	return max(db.resourceLimits.SnapshotHashLeaseTTL, minSnapshotHashLeaseTTL)
}

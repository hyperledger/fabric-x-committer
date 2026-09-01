/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// This file owns the durable `_snapshot` record contract: the latest-record pointer,
// the record encoding, and the read/write paths that mutate a record safely.
//
// It lives here, next to the schema it reads, because two components read and write
// the same record from different processes: the validator-committer accepts or rejects
// a new snapshot from it (and will mark it CHECKPOINTED once checkpointing lands),
// while the snapshot hasher drives it from PENDING to COMPLETED as it hashes the
// clone. Keeping the pointer key, the encoding, and the locking discipline in one
// place is what keeps those two processes agreeing on the record.

package statedb

import (
	"context"
	"fmt"
	"slices"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

const (
	getLatestSnapshotKeySQL = "SELECT value FROM metadata WHERE key = $1;"

	selectSnapshotRecordSQL = "SELECT value FROM ns_" + committerpb.SnapshotNamespaceID + " WHERE key = $1;"

	updateSnapshotRecordSQL = "UPDATE ns_" + committerpb.SnapshotNamespaceID +
		" SET value = $2, version = version + 1 WHERE key = $1;"

	// selectSnapshotRecordForUpdateSQL locks the `_snapshot` row for the duration of the
	// enclosing transaction. The transaction runs at READ COMMITTED, which does not
	// itself serialize concurrent access to the row or fail our commit if another
	// writer changed it after we read it: our later UPDATE is a blind write keyed
	// only on `key`, so at plain READ COMMITTED a concurrent writer could commit
	// between our SELECT and UPDATE and we would still match and overwrite it,
	// succeeding with a stale value (TOCTOU). FOR UPDATE closes that gap by blocking
	// any concurrent writer on this row until we commit or roll back, so no stale
	// read can survive into our write.
	selectSnapshotRecordForUpdateSQL = "SELECT value FROM ns_" + committerpb.SnapshotNamespaceID +
		" WHERE key = $1 FOR UPDATE;"
)

// LatestSnapshotPointerKey is the `metadata`-table key whose value is the tx_id of
// the most recently accepted `_snapshot` record, so a reader can look up that
// record's current status with a single key lookup instead of scanning the (small
// but growing) ns__snapshot table. Pre-seeded (NULL) at DB init and written
// atomically, in the same DB transaction as the `_snapshot` row it points to, by
// the validator-committer's commit path.
var LatestSnapshotPointerKey = []byte("latest snapshot key")

// SnapshotUpdate bundles the fields SnapshotStateManager.Update can change. Status is required. A
// nil Digest keeps the record's existing Hash, because a status move that publishes
// no new digest (IN_PROGRESS) must not wipe one.
//
// ErrMsg, by contrast, is written unconditionally: the diagnostic describes the
// status it arrives with, so a record that reaches COMPLETED after a failed attempt
// must not keep the failed attempt's error text, which would otherwise read as a
// completed snapshot that also failed.
type SnapshotUpdate struct {
	Status committerpb.SnapshotState_Status
	Digest []byte
	ErrMsg string

	// ExpectedStatus, when non-empty, aborts the update unless the locked row's status
	// is one of these; an empty slice writes unconditionally. See Update for why the row
	// lock alone does not give this.
	ExpectedStatus []committerpb.SnapshotState_Status
}

// ErrUnexpectedSnapshotStatus reports that the locked `_snapshot` record was not in
// a status its caller required, so the update was not applied. It is not a fault:
// a concurrent checkpoint reaches it legitimately. It is never retryable, because
// the record has moved on -- re-reading cannot restore the caller's premise, and the
// caller must re-decide what to do from the new status.
var ErrUnexpectedSnapshotStatus = errors.New("unexpected _snapshot record status")

// SnapshotStateManager reads and writes `_snapshot` records over a state-database pool.
// Callers hold one per process; it carries no per-record state, so it is safe to
// share.
type SnapshotStateManager struct {
	pool         *pgxpool.Pool
	retryProfile *retry.Profile
}

// NewSnapshotStateManager returns a SnapshotStateManager over an already-open state-database pool.
// The caller keeps ownership of the pool, including closing it.
func NewSnapshotStateManager(pool *pgxpool.Pool, retryProfile *retry.Profile) *SnapshotStateManager {
	return &SnapshotStateManager{pool: pool, retryProfile: retryProfile}
}

// ReadLatest performs the full pointer-to-row read cycle: it looks up the
// latest-record pointer (LatestSnapshotPointerKey) and, when one is set, reads and
// decodes the `_snapshot` record it names.
//
// Returns (nil, nil) when no snapshot has ever been accepted (pointer unset).
//
// A pointer that names a missing row, and a row whose value does not decode, are
// both hard errors rather than "no snapshot": the pointer is written in the same DB
// transaction as the row it names, so either state is corruption. Reporting it as
// absent would let the validator-committer conclude no snapshot is in flight and
// accept a new one. Both are also non-retryable, because neither a missing row nor
// an undecodable value can resolve itself on a later attempt.
func (s *SnapshotStateManager) ReadLatest(ctx context.Context) (*committerpb.SnapshotState, error) {
	state, err := retry.ExecuteWithResult(ctx, s.retryProfile, func() (*committerpb.SnapshotState, error) {
		var key []byte
		row := s.pool.QueryRow(ctx, getLatestSnapshotKeySQL, LatestSnapshotPointerKey)
		if scanErr := row.Scan(&key); scanErr != nil {
			return nil, errors.Wrap(scanErr, "failed to read the latest snapshot key")
		}
		if len(key) == 0 {
			return nil, nil //nolint:nilnil // no snapshot has ever been accepted.
		}

		var raw []byte
		if scanErr := s.pool.QueryRow(ctx, selectSnapshotRecordSQL, key).Scan(&raw); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return nil, errors.Wrapf(retry.ErrNonRetryable,
					"latest snapshot key %s has no matching _snapshot record", key)
			}
			return nil, errors.Wrapf(scanErr, "failed to read _snapshot record for key %s", key)
		}
		state, decodeErr := DecodeSnapshotState(raw)
		if decodeErr != nil {
			return nil, errors.Wrapf(errors.Join(retry.ErrNonRetryable, decodeErr),
				"failed to decode the latest _snapshot record for key %s", key)
		}
		return state, nil
	}, retry.ErrNonRetryable)
	if err != nil {
		return nil, fmt.Errorf("failed to read the latest _snapshot record: %w", err)
	}
	return state, nil
}

// Update rewrites the `_snapshot` record for ref.TxId per update; TxRef and
// CloneDatabase are preserved because the existing record is decoded, mutated, and
// re-encoded rather than rebuilt.
//
// The read and the write run inside a single DB transaction using SELECT ... FOR
// UPDATE (see selectSnapshotRecordForUpdateSQL), not READ COMMITTED alone: without the row
// lock a concurrent writer could commit between our SELECT and UPDATE, and we would
// still overwrite it with our stale re-encoded value, with no error at any point.
// The whole read-decode-mutate-encode-write sequence is retried as one unit, so a
// transient failure anywhere in it restarts from a fresh, consistent read.
//
// The row lock stops a lost update; it does not stop a wrong-state transition, since
// this UPDATE matches on `key` alone and would apply whatever the locked row now
// holds. update.ExpectedStatus closes that: when set, the locked status must be one
// of the statuses the caller reasoned about, or the update is rejected with
// ErrUnexpectedSnapshotStatus. An empty ExpectedStatus keeps the write
// unconditional.
//
//nolint:gocognit // one transaction: lock, decode, mutate, write, commit.
func (s *SnapshotStateManager) Update(ctx context.Context, ref *committerpb.TxRef, update SnapshotUpdate) error {
	if update.Status == committerpb.SnapshotState_STATUS_UNSPECIFIED {
		return errors.Newf("refusing to update the _snapshot record for tx %s to an unspecified status", ref.TxId)
	}
	err := retry.Execute(ctx, s.retryProfile, func() error {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return errors.Wrap(err, "failed to begin a database transaction")
		}
		// Roll back on a context that is already cancelled, so a failed attempt never
		// leaves a transaction holding the row lock.
		defer func() { //nolint:contextcheck // roll back even when ctx is cancelled.
			if rbErr := tx.Rollback(context.Background()); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				logger.Warnf("failed rolling back _snapshot transaction: %v", rbErr)
			}
		}()

		var raw []byte
		if scanErr := tx.QueryRow(ctx, selectSnapshotRecordForUpdateSQL, []byte(ref.TxId)).Scan(&raw); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				// A record that is not there cannot appear on a later attempt, so this fails now
				// instead of spending the whole retry budget on it.
				return errors.Wrapf(errors.Join(retry.ErrNonRetryable, scanErr),
					"no _snapshot record for tx %s", ref.TxId)
			}
			return errors.Wrapf(scanErr, "failed to read _snapshot record for tx %s", ref.TxId)
		}
		state, err := DecodeSnapshotState(raw)
		if err != nil {
			// A value that does not decode will not decode on a retry either.
			return errors.Wrapf(errors.Join(retry.ErrNonRetryable, err), "tx %s", ref.TxId)
		}
		// Checked against the LOCKED row, so no writer can move the record between this
		// comparison and the UPDATE below.
		if len(update.ExpectedStatus) > 0 && !slices.Contains(update.ExpectedStatus, state.Status) {
			return errors.Wrapf(errors.Join(retry.ErrNonRetryable, ErrUnexpectedSnapshotStatus),
				"_snapshot record for tx %s is %s, expected one of %v",
				ref.TxId, state.Status, update.ExpectedStatus)
		}

		state.Status = update.Status
		if update.Digest != nil {
			state.Hash = update.Digest
		}
		state.Error = update.ErrMsg

		newRaw, err := EncodeSnapshotState(state)
		if err != nil {
			return errors.Wrapf(err, "tx %s", ref.TxId)
		}
		if _, execErr := tx.Exec(ctx, updateSnapshotRecordSQL, []byte(ref.TxId), newRaw); execErr != nil {
			return errors.Wrapf(execErr, "failed to update _snapshot record for tx %s", ref.TxId)
		}
		return errors.Wrapf(tx.Commit(ctx), "failed to commit _snapshot state update for tx %s", ref.TxId)
	}, retry.ErrNonRetryable)
	return err //nolint:wrapcheck // already wrapped inside the retried closure.
}

// MarkSnapshotCheckpointedInTx advances the latest `_snapshot` record to CHECKPOINTED inside
// the caller's transaction, so the record and the `_checkpoint` row that attests to it become
// durable together. Doing it in a later transaction would leave a crash window: a durable
// attestation whose record is still short of CHECKPOINTED reads to the admission gate as
// "a snapshot is still awaiting its checkpoint", which rejects every later snapshot with
// nothing to repair it.
//
// An already-CHECKPOINTED record is left untouched, so a resubmitted checkpoint neither
// rewrites it nor bumps its version. An unset pointer or a record for a different block,
// by contrast, are invariant violations reported as non-retryable: the caller verifies both
// against this same record before the checkpoint reaches a commit (see
// rejectCheckpointIfNotVerified), so reaching here means the record changed underneath a
// verified checkpoint. Retrying cannot fix that, and succeeding silently would leave a
// durable attestation whose record never advances.
func MarkSnapshotCheckpointedInTx(ctx context.Context, tx pgx.Tx, blockNum uint64) error {
	var key []byte
	if err := tx.QueryRow(ctx, getLatestSnapshotKeySQL, LatestSnapshotPointerKey).Scan(&key); err != nil {
		return errors.Wrap(err, "failed to read the latest snapshot key")
	}
	if len(key) == 0 {
		return errors.Wrapf(retry.ErrNonRetryable,
			"no snapshot record to checkpoint for block %d, but its checkpoint was verified", blockNum)
	}

	var raw []byte
	if err := tx.QueryRow(ctx, selectSnapshotRecordForUpdateSQL, key).Scan(&raw); err != nil {
		return errors.Wrapf(err, "failed to read _snapshot record for key %s", key)
	}
	state, err := DecodeSnapshotState(raw)
	if err != nil {
		return errors.Wrapf(err, "failed to decode _snapshot record for key %s", key)
	}
	if state.Status == committerpb.SnapshotState_CHECKPOINTED {
		return nil // already checkpointed: a resubmitted checkpoint must not rewrite it.
	}
	if state.TxRef == nil || state.TxRef.BlockNum != blockNum {
		return errors.Wrapf(retry.ErrNonRetryable,
			"the latest snapshot record is not for block %d, but its checkpoint was verified", blockNum)
	}

	state.Status = committerpb.SnapshotState_CHECKPOINTED
	newRaw, err := EncodeSnapshotState(state)
	if err != nil {
		return errors.Wrapf(err, "failed to encode _snapshot record for key %s", key)
	}
	_, err = tx.Exec(ctx, updateSnapshotRecordSQL, key, newRaw)
	return errors.Wrapf(err, "failed to mark the snapshot for block %d as checkpointed", blockNum)
}

// DecodeSnapshotState unmarshals a `_snapshot` record value.
func DecodeSnapshotState(raw []byte) (*committerpb.SnapshotState, error) {
	var state committerpb.SnapshotState
	if err := proto.Unmarshal(raw, &state); err != nil {
		return nil, errors.Wrap(err, "failed to decode _snapshot record")
	}
	return &state, nil
}

// EncodeSnapshotState marshals a `_snapshot` record value.
func EncodeSnapshotState(state *committerpb.SnapshotState) ([]byte, error) {
	raw, err := proto.Marshal(state)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal _snapshot record")
	}
	return raw, nil
}

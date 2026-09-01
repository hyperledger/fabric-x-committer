/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package snapshotstate owns the durable `_snapshot` record contract: the
// latest-record pointer, the record encoding, and the read/write paths that mutate
// a record safely.
//
// It is shared rather than owned by one service because two components read and
// write the same record from different processes: the validator-committer accepts
// or rejects a new snapshot from it (and will mark it CHECKPOINTED once
// checkpointing lands), while the snapshot service drives it from PENDING to
// COMPLETED as it hashes the clone. Keeping the pointer key, the encoding, and the
// locking discipline in one place is what keeps those two processes agreeing on the
// record.
package snapshotstate

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

var logger = flogging.MustGetLogger("snapshot-state")

const (
	getLatestKeySQL = "SELECT value FROM metadata WHERE key = $1;"

	selectRecordSQL = "SELECT value FROM ns_" + committerpb.SnapshotNamespaceID + " WHERE key = $1;"

	updateRecordSQL = "UPDATE ns_" + committerpb.SnapshotNamespaceID +
		" SET value = $2, version = version + 1 WHERE key = $1;"

	// selectRecordForUpdateSQL locks the `_snapshot` row for the duration of the
	// enclosing transaction. The transaction runs at READ COMMITTED, which does not
	// itself serialize concurrent access to the row or fail our commit if another
	// writer changed it after we read it: our later UPDATE is a blind write keyed
	// only on `key`, so at plain READ COMMITTED a concurrent writer could commit
	// between our SELECT and UPDATE and we would still match and overwrite it,
	// succeeding with a stale value (TOCTOU). FOR UPDATE closes that gap by blocking
	// any concurrent writer on this row until we commit or roll back, so no stale
	// read can survive into our write.
	selectRecordForUpdateSQL = "SELECT value FROM ns_" + committerpb.SnapshotNamespaceID +
		" WHERE key = $1 FOR UPDATE;"
)

// LatestRecordPointerKey is the `metadata`-table key whose value is the tx_id of
// the most recently accepted `_snapshot` record, so a reader can look up that
// record's current status with a single key lookup instead of scanning the (small
// but growing) ns__snapshot table. Pre-seeded (NULL) at DB init and written
// atomically, in the same DB transaction as the `_snapshot` row it points to, by
// the validator-committer's commit path.
var LatestRecordPointerKey = []byte("latest snapshot key")

// Update bundles the fields StateManager.Update can change. It is a struct rather
// than separate parameters because a zero-value field means "leave this part of the
// record unchanged": an empty ErrMsg keeps the record's existing Error, and a nil
// Digest keeps its existing Hash.
type Update struct {
	Status committerpb.SnapshotState_Status
	Digest []byte
	ErrMsg string
}

// StateManager reads and writes `_snapshot` records over a state-database pool.
// Callers hold one per process; it carries no per-record state, so it is safe to
// share.
type StateManager struct {
	pool         *pgxpool.Pool
	retryProfile *retry.Profile
}

// NewStateManager returns a StateManager over an already-open state-database pool.
// The caller keeps ownership of the pool, including closing it.
func NewStateManager(pool *pgxpool.Pool, retryProfile *retry.Profile) *StateManager {
	return &StateManager{pool: pool, retryProfile: retryProfile}
}

// ReadLatest performs the full pointer-to-row read cycle: it looks up the
// latest-record pointer (LatestRecordPointerKey) and, when one is set, reads and
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
func (s *StateManager) ReadLatest(ctx context.Context) (*committerpb.SnapshotState, error) {
	state, err := retry.ExecuteWithResult(ctx, s.retryProfile, func() (*committerpb.SnapshotState, error) {
		var key []byte
		row := s.pool.QueryRow(ctx, getLatestKeySQL, LatestRecordPointerKey)
		if scanErr := row.Scan(&key); scanErr != nil {
			return nil, errors.Wrap(scanErr, "failed to read the latest snapshot key")
		}
		if len(key) == 0 {
			return nil, nil //nolint:nilnil // no snapshot has ever been accepted.
		}

		var raw []byte
		if scanErr := s.pool.QueryRow(ctx, selectRecordSQL, key).Scan(&raw); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return nil, errors.Wrapf(retry.ErrNonRetryable,
					"latest snapshot key %s has no matching _snapshot record", key)
			}
			return nil, errors.Wrapf(scanErr, "failed to read _snapshot record for key %s", key)
		}
		state, decodeErr := Decode(raw)
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
// UPDATE (see selectRecordForUpdateSQL), not READ COMMITTED alone: without the row
// lock a concurrent writer could commit between our SELECT and UPDATE, and we would
// still overwrite it with our stale re-encoded value, with no error at any point.
// The whole read-decode-mutate-encode-write sequence is retried as one unit, so a
// transient failure anywhere in it restarts from a fresh, consistent read.
//
//nolint:gocognit // one transaction: lock, decode, mutate, write, commit.
func (s *StateManager) Update(ctx context.Context, ref *committerpb.TxRef, update Update) error {
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
		if scanErr := tx.QueryRow(ctx, selectRecordForUpdateSQL, []byte(ref.TxId)).Scan(&raw); scanErr != nil {
			return errors.Wrapf(scanErr, "failed to read _snapshot record for tx %s", ref.TxId)
		}
		state, err := Decode(raw)
		if err != nil {
			return errors.Wrapf(err, "tx %s", ref.TxId)
		}

		state.Status = update.Status
		if update.Digest != nil {
			state.Hash = update.Digest
		}
		if update.ErrMsg != "" {
			state.Error = update.ErrMsg
		}

		newRaw, err := Encode(state)
		if err != nil {
			return errors.Wrapf(err, "tx %s", ref.TxId)
		}
		if _, execErr := tx.Exec(ctx, updateRecordSQL, []byte(ref.TxId), newRaw); execErr != nil {
			return errors.Wrapf(execErr, "failed to update _snapshot record for tx %s", ref.TxId)
		}
		return errors.Wrapf(tx.Commit(ctx), "failed to commit _snapshot state update for tx %s", ref.TxId)
	})
	return err //nolint:wrapcheck // already wrapped inside the retried closure.
}

// MarkCheckpointedInTx advances the latest `_snapshot` record to CHECKPOINTED inside the
// caller's transaction, so the record and the `_checkpoint` row that attests to it become
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
//
// Unlike Update, this does not retry and is not a StateManager method: it runs entirely on
// the caller's transaction and pool, so it needs no manager state of its own, and the
// caller retries the transaction as a whole.
func MarkCheckpointedInTx(ctx context.Context, tx pgx.Tx, blockNum uint64) error {
	var key []byte
	if err := tx.QueryRow(ctx, getLatestKeySQL, LatestRecordPointerKey).Scan(&key); err != nil {
		return errors.Wrap(err, "failed to read the latest snapshot key")
	}
	if len(key) == 0 {
		return errors.Wrapf(retry.ErrNonRetryable,
			"no snapshot record to checkpoint for block %d, but its checkpoint was verified", blockNum)
	}

	var raw []byte
	if err := tx.QueryRow(ctx, selectRecordForUpdateSQL, key).Scan(&raw); err != nil {
		return errors.Wrapf(err, "failed to read _snapshot record for key %s", key)
	}
	state, err := Decode(raw)
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
	newRaw, err := Encode(state)
	if err != nil {
		return errors.Wrapf(err, "failed to encode _snapshot record for key %s", key)
	}
	_, err = tx.Exec(ctx, updateRecordSQL, key, newRaw)
	return errors.Wrapf(err, "failed to mark the snapshot for block %d as checkpointed", blockNum)
}

// Decode unmarshals a `_snapshot` record value.
func Decode(raw []byte) (*committerpb.SnapshotState, error) {
	var state committerpb.SnapshotState
	if err := proto.Unmarshal(raw, &state); err != nil {
		return nil, errors.Wrap(err, "failed to decode _snapshot record")
	}
	return &state, nil
}

// Encode marshals a `_snapshot` record value.
func Encode(state *committerpb.SnapshotState) ([]byte, error) {
	raw, err := proto.Marshal(state)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal _snapshot record")
	}
	return raw, nil
}

// TableName returns the `_snapshot` namespace table name, so callers do not have to
// rebuild it from the namespace ID.
func TableName() string {
	return statedb.TableName(committerpb.SnapshotNamespaceID)
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Tests for the durable `_snapshot` record contract (snapshot_state.go).
//
// This file is an external test package, unlike the rest of statedb's tests, because
// it needs utils/testdb to bring up a database and testdb imports statedb -- an
// in-package test would be an import cycle. It therefore exercises only the exported
// API, which is what its two callers (the validator-committer and the snapshot
// hasher) use anyway.
package statedb_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

// snapshotTable is the `_snapshot` namespace table these tests read and write.
var snapshotTable = statedb.TableName(committerpb.SnapshotNamespaceID)

func TestMain(m *testing.M) {
	testdb.RunTestMain(m)
}

// TestReadLatestWithoutPointerIsNoSnapshot covers a fresh system: the pointer row is
// pre-seeded NULL at DB init, and no snapshot has ever been accepted. This must be
// reported as "no record" and not as an error, because it is the normal state every
// reader starts from.
func TestReadLatestWithoutPointerIsNoSnapshot(t *testing.T) {
	t.Parallel()
	env := newSnapshotStateTestEnv(t)

	state, err := env.state.ReadLatest(t.Context())
	require.NoError(t, err)
	require.Nil(t, state)
}

// TestReadLatestFollowsPointer proves the pointer-to-row read cycle returns the record
// the pointer names, including the fields a hash job needs (clone name) and a reader
// gates on (status).
func TestReadLatestFollowsPointer(t *testing.T) {
	t.Parallel()
	env := newSnapshotStateTestEnv(t)

	want := &committerpb.SnapshotState{
		TxRef:         &committerpb.TxRef{BlockNum: 7, TxNum: 0, TxId: "snap-read-latest"},
		Status:        committerpb.SnapshotState_PENDING,
		CloneDatabase: "snapshot_7",
	}
	env.seedRecord(t, want)

	got, err := env.state.ReadLatest(t.Context())
	require.NoError(t, err)
	test.RequireProtoEqual(t, want, got)
}

// TestReadLatestRejectsDanglingPointer keeps a pointer with no matching row a hard
// error rather than "no snapshot". The pointer is written in the same transaction as
// its row, so this state is an invariant violation; reporting it as absent would let a
// caller believe no snapshot is in flight and accept a new one.
//
// The error must also be non-retryable: a missing row will still be missing on a later
// attempt, so retrying only burns the whole retry budget before reporting the same
// corruption. The bounded test context is what would expose a retry loop here.
func TestReadLatestRejectsDanglingPointer(t *testing.T) {
	t.Parallel()
	env := newSnapshotStateTestEnv(t)
	env.setPointer(t, "snap-missing-row")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_, err := env.state.ReadLatest(ctx)
	require.ErrorContains(t, err, "has no matching _snapshot record")
	require.NoError(t, ctx.Err(), "a dangling pointer must fail fast, not retry")
}

// TestReadLatestRejectsCorruptRecord is the same fast-failure requirement for a value
// that does not decode: it cannot start decoding on a retry, so the read must report
// the corruption immediately instead of retrying until the budget runs out.
func TestReadLatestRejectsCorruptRecord(t *testing.T) {
	t.Parallel()
	env := newSnapshotStateTestEnv(t)
	env.insertRawRecord(t, "snap-corrupt", []byte("not a protobuf message"))
	env.setPointer(t, "snap-corrupt")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_, err := env.state.ReadLatest(ctx)
	require.ErrorContains(t, err, "failed to decode the latest _snapshot record")
	require.NoError(t, ctx.Err(), "a corrupt record must fail fast, not retry")
}

// TestUpdate covers the field semantics documented on Update: the status always
// moves, a nil Digest leaves the stored hash alone -- the snapshot service marks a
// record IN_PROGRESS with no digest and must not wipe one -- while ErrMsg is written
// as given, so a status that carries no error clears whatever a previous failed
// attempt recorded.
func TestUpdate(t *testing.T) {
	t.Parallel()
	// Seeded on the record before every case, so a case that leaves Digest or ErrMsg
	// unset asserts the stored values survive rather than that they were never there.
	const seededHash, seededErrMsg = "seeded-hash", "seeded-error"

	for _, tc := range []struct {
		name       string
		update     statedb.SnapshotUpdate
		wantHash   []byte
		wantErrMsg string
	}{{
		name:       "status only preserves the hash and clears the error",
		update:     statedb.SnapshotUpdate{Status: committerpb.SnapshotState_IN_PROGRESS},
		wantHash:   []byte(seededHash),
		wantErrMsg: "",
	}, {
		name: "digest is published",
		update: statedb.SnapshotUpdate{
			Status: committerpb.SnapshotState_COMPLETED,
			Digest: []byte("fresh-digest"),
		},
		wantHash: []byte("fresh-digest"),
		// A COMPLETED record must not keep the previous attempt's diagnostic: it would
		// read as a snapshot that both completed and failed.
		wantErrMsg: "",
	}, {
		name: "error message is recorded",
		update: statedb.SnapshotUpdate{
			Status: committerpb.SnapshotState_FAILED,
			ErrMsg: "fresh-error",
		},
		wantHash:   []byte(seededHash),
		wantErrMsg: "fresh-error",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newSnapshotStateTestEnv(t)
			ref := &committerpb.TxRef{BlockNum: 11, TxNum: 0, TxId: "snap-update"}
			env.seedRecord(t, &committerpb.SnapshotState{
				TxRef:         ref,
				Status:        committerpb.SnapshotState_PENDING,
				CloneDatabase: "snapshot_11",
				Hash:          []byte(seededHash),
				Error:         seededErrMsg,
			})

			require.NoError(t, env.state.Update(t.Context(), ref, tc.update))

			got, err := env.state.ReadLatest(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.update.Status, got.Status)
			require.Equal(t, tc.wantHash, got.Hash)
			require.Equal(t, tc.wantErrMsg, got.Error)
			// TxRef and CloneDatabase survive because Update decodes, mutates, and
			// re-encodes the existing record instead of rebuilding it. Losing the clone
			// name would leave a record nothing can hash.
			test.RequireProtoEqual(t, ref, got.TxRef)
			require.Equal(t, "snapshot_11", got.CloneDatabase)
			// The row version is bumped, which is how a caller distinguishes a record a
			// tick rewrote from one it left alone.
			require.EqualValues(t, 1, env.recordVersion(t, ref.TxId))
		})
	}
}

// TestUpdateWithExpectedStatus covers the conditional write. The row lock stops two
// writers from interleaving a read and a write, but not a write that was decided
// against a status the record no longer holds: the UPDATE matches on `key` alone, so
// without this check a caller that read IN_PROGRESS would still publish COMPLETED
// over a record that has since been CHECKPOINTED. The two cases that motivate it are
// a second hasher (a deployment error) and a checkpoint transaction arriving while
// this organization is still hashing.
func TestUpdateWithExpectedStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		stored   committerpb.SnapshotState_Status
		expected []committerpb.SnapshotState_Status
		wantErr  bool
	}{{
		name:     "matching status is written",
		stored:   committerpb.SnapshotState_IN_PROGRESS,
		expected: []committerpb.SnapshotState_Status{committerpb.SnapshotState_IN_PROGRESS},
	}, {
		name:   "any listed status matches",
		stored: committerpb.SnapshotState_FAILED,
		expected: []committerpb.SnapshotState_Status{
			committerpb.SnapshotState_PENDING,
			committerpb.SnapshotState_IN_PROGRESS,
			committerpb.SnapshotState_FAILED,
		},
	}, {
		// The case the check exists for: a checkpoint landed while this process was
		// hashing, so publishing a digest now would undo the checkpoint.
		name:     "checkpointed record rejects a completing write",
		stored:   committerpb.SnapshotState_CHECKPOINTED,
		expected: []committerpb.SnapshotState_Status{committerpb.SnapshotState_IN_PROGRESS},
		wantErr:  true,
	}, {
		// A second hasher that already published: this one's write is stale.
		name:     "completed record rejects a second completing write",
		stored:   committerpb.SnapshotState_COMPLETED,
		expected: []committerpb.SnapshotState_Status{committerpb.SnapshotState_IN_PROGRESS},
		wantErr:  true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newSnapshotStateTestEnv(t)
			ref := &committerpb.TxRef{BlockNum: 21, TxNum: 0, TxId: "snap-expected-status"}
			env.seedRecord(t, &committerpb.SnapshotState{
				TxRef:         ref,
				Status:        tc.stored,
				CloneDatabase: "snapshot_21",
			})

			// Bounded well under the retry budget: a rejection that was retried instead of
			// reported terminal would exhaust this context, so that regression fails here
			// rather than merely being slow.
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			t.Cleanup(cancel)

			err := env.state.Update(ctx, ref, statedb.SnapshotUpdate{
				Status:         committerpb.SnapshotState_COMPLETED,
				Digest:         []byte("digest"),
				ExpectedStatus: tc.expected,
			})

			got, readErr := env.state.ReadLatest(t.Context())
			require.NoError(t, readErr)

			if !tc.wantErr {
				require.NoError(t, err)
				require.Equal(t, committerpb.SnapshotState_COMPLETED, got.Status)
				require.Equal(t, []byte("digest"), got.Hash)
				return
			}

			require.ErrorIs(t, err, statedb.ErrUnexpectedSnapshotStatus)
			// Never retried: the record moved on, so a later attempt cannot restore the
			// caller's premise, and spending the budget would delay the caller's re-decision.
			require.NoError(t, ctx.Err(), "a status mismatch must fail fast, not retry")
			// The rejected write must leave no trace at all, not even a version bump.
			require.Equal(t, tc.stored, got.Status)
			require.Empty(t, got.Hash)
			require.EqualValues(t, 0, env.recordVersion(t, ref.TxId))
		})
	}
}

// TestUpdateWithoutExpectedStatusIsUnconditional keeps the field opt-in: the
// validator-committer owns the record's whole lifecycle and writes without naming a
// premise, so an empty ExpectedStatus must overwrite whatever is stored -- including
// a terminal status.
func TestUpdateWithoutExpectedStatusIsUnconditional(t *testing.T) {
	t.Parallel()
	env := newSnapshotStateTestEnv(t)
	ref := &committerpb.TxRef{BlockNum: 22, TxNum: 0, TxId: "snap-unconditional"}
	env.seedRecord(t, &committerpb.SnapshotState{
		TxRef:         ref,
		Status:        committerpb.SnapshotState_CHECKPOINTED,
		CloneDatabase: "snapshot_22",
	})

	require.NoError(t, env.state.Update(t.Context(), ref, statedb.SnapshotUpdate{
		Status: committerpb.SnapshotState_COMPLETED,
	}))

	got, err := env.state.ReadLatest(t.Context())
	require.NoError(t, err)
	require.Equal(t, committerpb.SnapshotState_COMPLETED, got.Status)
}

// TestEncodeDecodeRoundTrip pins the record encoding, which two processes depend on
// agreeing about: the validator-committer writes the record and the snapshot service
// rewrites it, so a field lost in a round trip would silently drop state such as the
// clone name a hash job needs.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state *committerpb.SnapshotState
	}{{
		name:  "empty",
		state: &committerpb.SnapshotState{},
	}, {
		name: "pending with clone",
		state: &committerpb.SnapshotState{
			TxRef:         &committerpb.TxRef{BlockNum: 42, TxNum: 1, TxId: "snap-tx"},
			Status:        committerpb.SnapshotState_PENDING,
			CloneDatabase: "snapshot_42",
		},
	}, {
		name: "completed with digest",
		state: &committerpb.SnapshotState{
			TxRef:         &committerpb.TxRef{BlockNum: 43, TxNum: 0, TxId: "snap-tx-done"},
			Status:        committerpb.SnapshotState_COMPLETED,
			CloneDatabase: "snapshot_43",
			Hash:          []byte{0x01, 0x02, 0x03},
		},
	}, {
		name: "failed with error",
		state: &committerpb.SnapshotState{
			TxRef:         &committerpb.TxRef{BlockNum: 44, TxNum: 0, TxId: "snap-tx-failed"},
			Status:        committerpb.SnapshotState_FAILED,
			CloneDatabase: "snapshot_44",
			Error:         "no clone_database to hash",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := statedb.EncodeSnapshotState(tc.state)
			require.NoError(t, err)

			decoded, err := statedb.DecodeSnapshotState(raw)
			require.NoError(t, err)
			test.RequireProtoEqual(t, tc.state, decoded)
		})
	}
}

// TestDecodeRejectsCorruptValue keeps a corrupt record a hard error rather than an
// empty state. Reading it as a zero-value SnapshotState would look like a record with
// no clone and status UNSPECIFIED, which callers treat as actionable state.
func TestDecodeRejectsCorruptValue(t *testing.T) {
	t.Parallel()
	_, err := statedb.DecodeSnapshotState([]byte("not a protobuf message"))
	require.ErrorContains(t, err, "failed to decode _snapshot record")
}

// TestDecodeEmptyValueIsZeroState documents that an empty value decodes to a
// zero-value record rather than failing: proto treats no bytes as no fields set. It
// is not a case any writer produces, and callers reject the resulting missing TxRef.
func TestDecodeEmptyValueIsZeroState(t *testing.T) {
	t.Parallel()
	state, err := statedb.DecodeSnapshotState(nil)
	require.NoError(t, err)
	require.Nil(t, state.TxRef)
	require.Equal(t, committerpb.SnapshotState_STATUS_UNSPECIFIED, state.Status)
}

// snapshotStateTestEnv is a SnapshotStateManager over a freshly initialized state
// database, plus the raw SQL needed to seed records without going through the
// validator-committer (which imports this package, so a test here cannot use its
// fixtures).
type snapshotStateTestEnv struct {
	pool  *pgxpool.Pool
	state *statedb.SnapshotStateManager
}

func newSnapshotStateTestEnv(t *testing.T) *snapshotStateTestEnv {
	t.Helper()
	conn := testdb.PrepareTestEnv(t)
	config := &statedb.Config{
		Endpoints:      conn.Endpoints,
		Username:       conn.User,
		Password:       conn.Password,
		Database:       conn.Database,
		MaxConnections: 10,
		MinConnections: 1,
		LoadBalance:    conn.LoadBalance,
		TLS:            conn.TLS,
		Retry:          testdb.DefaultRetry,
	}
	require.NoError(t, statedb.SetupSystemTablesAndNamespaces(t.Context(), config))

	pool, err := statedb.NewPool(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return &snapshotStateTestEnv{pool: pool, state: statedb.NewSnapshotStateManager(pool, config.Retry)}
}

// seedRecord inserts state as its own `_snapshot` row and points the latest-snapshot
// pointer at it, mirroring what the validator-committer's commit path writes
// atomically.
func (env *snapshotStateTestEnv) seedRecord(t *testing.T, state *committerpb.SnapshotState) {
	t.Helper()
	raw, err := statedb.EncodeSnapshotState(state)
	require.NoError(t, err)
	env.insertRawRecord(t, state.TxRef.TxId, raw)
	env.setPointer(t, state.TxRef.TxId)
}

// insertRawRecord inserts a `_snapshot` row verbatim, so a test can also store a value
// that does not decode.
func (env *snapshotStateTestEnv) insertRawRecord(t *testing.T, txID string, raw []byte) {
	t.Helper()
	query := fmt.Sprintf("INSERT INTO %s (key, value, version) VALUES ($1, $2, 0)", snapshotTable)
	_, err := env.pool.Exec(t.Context(), query, []byte(txID), raw)
	require.NoError(t, err)
}

func (env *snapshotStateTestEnv) setPointer(t *testing.T, txID string) {
	t.Helper()
	_, err := env.pool.Exec(t.Context(), "UPDATE metadata SET value = $2 WHERE key = $1",
		statedb.LatestSnapshotPointerKey, []byte(txID))
	require.NoError(t, err)
}

func (env *snapshotStateTestEnv) recordVersion(t *testing.T, txID string) int64 {
	t.Helper()
	query := fmt.Sprintf("SELECT version FROM %s WHERE key = $1", snapshotTable)
	var version int64
	require.NoError(t, env.pool.QueryRow(t.Context(), query, []byte(txID)).Scan(&version))
	return version
}

// TestMarkSnapshotCheckpointedInTx covers the transaction-scoped CHECKPOINTED advance. The
// advance runs inside the caller's commit transaction so the record and the `_checkpoint`
// row that attests to it become durable together; these cases pin the three outcomes that
// transaction can see.
func TestMarkSnapshotCheckpointedInTx(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		seed          *committerpb.SnapshotState
		blockNum      uint64
		expectedError string
		wantStatus    committerpb.SnapshotState_Status
	}{{
		name: "advances the awaited snapshot",
		seed: &committerpb.SnapshotState{
			TxRef:  &committerpb.TxRef{BlockNum: 21, TxNum: 0, TxId: "snap-cp-ok"},
			Status: committerpb.SnapshotState_COMPLETED,
		},
		blockNum:   21,
		wantStatus: committerpb.SnapshotState_CHECKPOINTED,
	}, {
		// A resubmitted checkpoint must not rewrite the record, so this stays a no-op
		// rather than an error.
		name: "already checkpointed is a no-op",
		seed: &committerpb.SnapshotState{
			TxRef:  &committerpb.TxRef{BlockNum: 22, TxNum: 0, TxId: "snap-cp-dup"},
			Status: committerpb.SnapshotState_CHECKPOINTED,
		},
		blockNum:   22,
		wantStatus: committerpb.SnapshotState_CHECKPOINTED,
	}, {
		// The caller verified the checkpoint against this same record, so a different
		// block here means the record changed underneath a verified checkpoint. Retrying
		// cannot fix that, and succeeding silently would leave a durable attestation whose
		// record never advances.
		name: "different block is a non-retryable invariant violation",
		seed: &committerpb.SnapshotState{
			TxRef:  &committerpb.TxRef{BlockNum: 23, TxNum: 0, TxId: "snap-cp-other"},
			Status: committerpb.SnapshotState_COMPLETED,
		},
		blockNum:      99,
		expectedError: "the latest snapshot record is not for block 99",
		wantStatus:    committerpb.SnapshotState_COMPLETED,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newSnapshotStateTestEnv(t)
			env.seedRecord(t, tc.seed)

			err := env.inTx(t, func(tx pgx.Tx) error {
				return statedb.MarkSnapshotCheckpointedInTx(t.Context(), tx, tc.blockNum)
			})
			if tc.expectedError != "" {
				require.ErrorContains(t, err, tc.expectedError)
				require.ErrorIs(t, err, retry.ErrNonRetryable)
			} else {
				require.NoError(t, err)
			}

			state, err := env.state.ReadLatest(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, state.Status)
		})
	}
}

// TestMarkSnapshotCheckpointedInTxWithoutPointer covers a verified checkpoint whose snapshot
// record has since vanished. Reporting it as success would leave the attestation durable
// with nothing to advance, so it is a non-retryable invariant violation.
func TestMarkSnapshotCheckpointedInTxWithoutPointer(t *testing.T) {
	t.Parallel()
	env := newSnapshotStateTestEnv(t)

	err := env.inTx(t, func(tx pgx.Tx) error {
		return statedb.MarkSnapshotCheckpointedInTx(t.Context(), tx, 31)
	})
	require.ErrorContains(t, err, "no snapshot record to checkpoint for block 31")
	require.ErrorIs(t, err, retry.ErrNonRetryable)
}

// TestMarkSnapshotCheckpointedInTxRollsBackWithCaller pins the reason this is transaction-scoped:
// the advance must not survive a caller that rolls back, or a `_snapshot` record could
// claim a checkpoint whose attestation row never committed.
func TestMarkSnapshotCheckpointedInTxRollsBackWithCaller(t *testing.T) {
	t.Parallel()
	env := newSnapshotStateTestEnv(t)
	ref := &committerpb.TxRef{BlockNum: 41, TxNum: 0, TxId: "snap-cp-rollback"}
	env.seedRecord(t, &committerpb.SnapshotState{
		TxRef:  ref,
		Status: committerpb.SnapshotState_COMPLETED,
	})

	tx, err := env.pool.Begin(t.Context())
	require.NoError(t, err)
	require.NoError(t, statedb.MarkSnapshotCheckpointedInTx(t.Context(), tx, ref.BlockNum))
	require.NoError(t, tx.Rollback(t.Context()))

	state, err := env.state.ReadLatest(t.Context())
	require.NoError(t, err)
	require.Equal(t, committerpb.SnapshotState_COMPLETED, state.Status)
}

// inTx runs op in its own transaction, committing only when op succeeds, so a test sees
// the same visibility rules the commit path gives statedb.MarkSnapshotCheckpointedInTx.
func (env *snapshotStateTestEnv) inTx(t *testing.T, op func(tx pgx.Tx) error) error {
	t.Helper()
	tx, err := env.pool.Begin(t.Context())
	require.NoError(t, err)
	if opErr := op(tx); opErr != nil {
		require.NoError(t, tx.Rollback(t.Context()))
		return opErr
	}
	require.NoError(t, tx.Commit(t.Context()))
	return nil
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshotstate

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestMain(m *testing.M) {
	testdb.RunTestMain(m)
}

// TestReadLatestWithoutPointerIsNoSnapshot covers a fresh system: the pointer row is
// pre-seeded NULL at DB init, and no snapshot has ever been accepted. This must be
// reported as "no record" and not as an error, because it is the normal state every
// reader starts from.
func TestReadLatestWithoutPointerIsNoSnapshot(t *testing.T) {
	t.Parallel()
	env := newStateTestEnv(t)

	state, err := env.state.ReadLatest(t.Context())
	require.NoError(t, err)
	require.Nil(t, state)
}

// TestReadLatestFollowsPointer proves the pointer-to-row read cycle returns the record
// the pointer names, including the fields a hash job needs (clone name) and a reader
// gates on (status).
func TestReadLatestFollowsPointer(t *testing.T) {
	t.Parallel()
	env := newStateTestEnv(t)

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
	env := newStateTestEnv(t)
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
	env := newStateTestEnv(t)
	env.insertRawRecord(t, "snap-corrupt", []byte("not a protobuf message"))
	env.setPointer(t, "snap-corrupt")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_, err := env.state.ReadLatest(ctx)
	require.ErrorContains(t, err, "failed to decode the latest _snapshot record")
	require.NoError(t, ctx.Err(), "a corrupt record must fail fast, not retry")
}

// TestUpdate covers the zero-value semantics documented on Update: the status always
// moves, while a nil Digest and an empty ErrMsg leave the stored hash and error alone.
// Both matter to the snapshot service, which marks a record IN_PROGRESS with neither
// field set and must not wipe a digest or diagnostic already on the record.
func TestUpdate(t *testing.T) {
	t.Parallel()
	// Seeded on the record before every case, so a case that leaves Digest or ErrMsg
	// unset asserts the stored values survive rather than that they were never there.
	const seededHash, seededErrMsg = "seeded-hash", "seeded-error"

	for _, tc := range []struct {
		name       string
		update     Update
		wantHash   []byte
		wantErrMsg string
	}{{
		name:       "status only preserves hash and error",
		update:     Update{Status: committerpb.SnapshotState_IN_PROGRESS},
		wantHash:   []byte(seededHash),
		wantErrMsg: seededErrMsg,
	}, {
		name: "digest is published",
		update: Update{
			Status: committerpb.SnapshotState_COMPLETED,
			Digest: []byte("fresh-digest"),
		},
		wantHash:   []byte("fresh-digest"),
		wantErrMsg: seededErrMsg,
	}, {
		name: "error message is recorded",
		update: Update{
			Status: committerpb.SnapshotState_FAILED,
			ErrMsg: "fresh-error",
		},
		wantHash:   []byte(seededHash),
		wantErrMsg: "fresh-error",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newStateTestEnv(t)
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
			raw, err := Encode(tc.state)
			require.NoError(t, err)

			decoded, err := Decode(raw)
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
	_, err := Decode([]byte("not a protobuf message"))
	require.ErrorContains(t, err, "failed to decode _snapshot record")
}

// TestDecodeEmptyValueIsZeroState documents that an empty value decodes to a
// zero-value record rather than failing: proto treats no bytes as no fields set. It
// is not a case any writer produces, and callers reject the resulting missing TxRef.
func TestDecodeEmptyValueIsZeroState(t *testing.T) {
	t.Parallel()
	state, err := Decode(nil)
	require.NoError(t, err)
	require.Nil(t, state.TxRef)
	require.Equal(t, committerpb.SnapshotState_STATUS_UNSPECIFIED, state.Status)
}

// stateTestEnv is a StateManager over a freshly initialized state database, plus the
// raw SQL needed to seed records without going through the validator-committer (which
// imports this package, so a test here cannot use its fixtures).
type stateTestEnv struct {
	pool  *pgxpool.Pool
	state *StateManager
}

func newStateTestEnv(t *testing.T) *stateTestEnv {
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

	return &stateTestEnv{pool: pool, state: NewStateManager(pool, config.Retry)}
}

// seedRecord inserts state as its own `_snapshot` row and points the latest-snapshot
// pointer at it, mirroring what the validator-committer's commit path writes
// atomically.
func (env *stateTestEnv) seedRecord(t *testing.T, state *committerpb.SnapshotState) {
	t.Helper()
	raw, err := Encode(state)
	require.NoError(t, err)
	env.insertRawRecord(t, state.TxRef.TxId, raw)
	env.setPointer(t, state.TxRef.TxId)
}

// insertRawRecord inserts a `_snapshot` row verbatim, so a test can also store a value
// that does not decode.
func (env *stateTestEnv) insertRawRecord(t *testing.T, txID string, raw []byte) {
	t.Helper()
	query := fmt.Sprintf("INSERT INTO %s (key, value, version) VALUES ($1, $2, 0)", TableName())
	_, err := env.pool.Exec(t.Context(), query, []byte(txID), raw)
	require.NoError(t, err)
}

func (env *stateTestEnv) setPointer(t *testing.T, txID string) {
	t.Helper()
	_, err := env.pool.Exec(t.Context(), "UPDATE metadata SET value = $2 WHERE key = $1",
		LatestRecordPointerKey, []byte(txID))
	require.NoError(t, err)
}

func (env *stateTestEnv) recordVersion(t *testing.T, txID string) int64 {
	t.Helper()
	query := fmt.Sprintf("SELECT version FROM %s WHERE key = $1", TableName())
	var version int64
	require.NoError(t, env.pool.QueryRow(t.Context(), query, []byte(txID)).Scan(&version))
	return version
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"crypto/sha256"
	"strconv"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// trailingBytesError is shared by tests that reject extra bytes in a checkpoint key.
const trailingBytesError = "trailing bytes after the block number"

const digestCheckpointCase = "digest"

// snapshotHold is the state an abort finds a `_snapshot` record in.
type snapshotHold struct {
	txID     string
	blockNum uint64
	status   committerpb.SnapshotState_Status
	digest   []byte
}

// TestCommitCheckpointOnHashMatch checks that matching hashes allow the checkpoint
// to commit and mark the snapshot CHECKPOINTED, with no feedback.
func TestCommitCheckpointOnHashMatch(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	hash := checkpointDigest("local-hash-match")
	ref := env.seedSnapshotRecord(t, "cp-match", 910100, hash)
	cpRef := committerpb.NewTxRef("cp-match-tx", 910101, 0)

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, hash))
	require.Len(t, status.Status, 1)
	require.Equal(t, committerpb.Status_COMMITTED, status.Status[0].Status)
	// A matching checkpoint returns a transaction status, not feedback.
	require.Nil(t, status.CheckpointFeedback)

	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, hash)
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_CHECKPOINTED)
}

// TestHaltCheckpointOnHashMismatch checks that different hashes produce HALT feedback.
// The checkpoint must not commit, and the snapshot record must explain the mismatch.
func TestHaltCheckpointOnHashMismatch(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	ref := env.seedSnapshotRecord(t, "cp-mismatch", 910200, checkpointDigest("local-hash"))
	cpRef := committerpb.NewTxRef("cp-mismatch-tx", 910201, 0)

	status := env.submitCheckpoint(ctx, t,
		newCheckpointPreparedTx(cpRef, ref.BlockNum, checkpointDigest("attested-hash")))
	requireFeedback(t, status.CheckpointFeedback,
		wantFeedback(servicepb.CheckpointFeedback_HALT, cpRef, ref.BlockNum), "halted")
	require.Contains(t, status.CheckpointFeedback.Reason, "does not match")
	// A halted checkpoint returns feedback instead of a transaction status.
	require.Empty(t, status.Status)

	requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
	// Keep the snapshot status and save the mismatch reason for investigation.
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_COMPLETED, "does not match")
}

// TestHoldCheckpointUntilHashIsComputed checks that a missing hash produces HOLD.
// No transaction status may be saved, so the same transaction can commit on retry.
func TestHoldCheckpointUntilHashIsComputed(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	// A record without a hash is exactly the hold condition.
	ref := env.seedSnapshotRecord(t, "cp-hold", 910300, nil)
	cpRef := committerpb.NewTxRef("cp-hold-tx", 910301, 0)
	hash := checkpointDigest("hash-that-lands-later")

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, hash))
	requireFeedback(t, status.CheckpointFeedback,
		wantFeedback(servicepb.CheckpointFeedback_HOLD, cpRef, ref.BlockNum), "held")
	require.Empty(t, status.Status)
	requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)

	// No status was persisted, so the sidecar can re-submit the identical txID.
	persisted, err := env.dbEnv.DB.readStatusWithHeight(ctx, [][]byte{[]byte(cpRef.TxId)})
	require.NoError(t, err)
	require.Empty(t, persisted)

	// Save the local hash, then retry the same checkpoint.
	require.NoError(t, env.dbEnv.DB.snapshotState.Update(ctx, ref, statedb.SnapshotUpdate{
		Status: committerpb.SnapshotState_COMPLETED, Digest: hash,
	}))
	resubmitted := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, hash))
	require.Nil(t, resubmitted.CheckpointFeedback)
	require.Len(t, resubmitted.Status, 1)
	require.Equal(t, committerpb.Status_COMMITTED, resubmitted.Status[0].Status)

	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, hash)
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_CHECKPOINTED)
}

// TestDuplicateCheckpointIsIgnored checks that a duplicate submission does not change
// the saved checkpoint or snapshot record.
func TestDuplicateCheckpointIsIgnored(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	hash := checkpointDigest("duplicate-hash")
	ref := env.seedSnapshotRecord(t, "cp-dup", 910400, hash)
	cpRef := committerpb.NewTxRef("cp-dup-tx", 910401, 0)

	first := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, hash))
	require.Equal(t, committerpb.Status_COMMITTED, first.Status[0].Status)
	committed, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
	require.True(t, found)

	second := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, hash))
	require.Len(t, second.Status, 1)

	// Neither the checkpoint value nor either record's version should change.
	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, hash)
	key := servicepb.CheckpointKey(ref.BlockNum)
	rows := env.dbEnv.FetchKeys(t, committerpb.CheckpointNamespaceID, [][]byte{key})
	require.Len(t, rows, 1)
	require.EqualValues(t, 0, rows[string(key)].Version)

	record, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
	require.True(t, found)
	require.Equal(t, committed.Version, record.Version)
	require.Equal(t, committerpb.SnapshotState_CHECKPOINTED, record.State.Status)
	require.Empty(t, record.State.Error)
}

// TestRejectCheckpointForWrongBlock checks that a wrong snapshot block number causes
// a transaction rejection, not a halt.
func TestRejectCheckpointForWrongBlock(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	ref := env.seedSnapshotRecord(t, "cp-wrong-block", 910500, checkpointDigest("local-hash"))
	cpRef := committerpb.NewTxRef("cp-wrong-block-tx", 910501, 0)
	otherBlock := ref.BlockNum + 7

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, otherBlock, checkpointDigest("local-hash")))
	// Bad input must not pause or stop the sidecar.
	require.Nil(t, status.CheckpointFeedback)
	require.Len(t, status.Status, 1)
	require.Equal(t, committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY, status.Status[0].Status)
	requireNoCheckpointRow(t, env.dbEnv, otherBlock)

	// Leave the snapshot unchanged so a correct checkpoint can still commit.
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_COMPLETED, "")
}

// TestRejectCheckpointWithoutSnapshotRecord checks that a checkpoint without a snapshot
// is rejected. Bad input must not let an authorized submitter stop the committer.
func TestRejectCheckpointWithoutSnapshotRecord(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	const blockNum = 910700
	cpRef := committerpb.NewTxRef("cp-no-record-tx", 910701, 0)

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, blockNum, checkpointDigest("attested-hash")))
	require.Nil(t, status.CheckpointFeedback)
	require.Len(t, status.Status, 1)
	require.Equal(t, committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY, status.Status[0].Status)
	requireNoCheckpointRow(t, env.dbEnv, blockNum)

	// A later snapshot and checkpoint must still succeed.
	nextRef := env.seedSnapshotRecord(t, "cp-after-reject", 910800, checkpointDigest("later-hash"))
	nextCpRef := committerpb.NewTxRef("cp-after-reject-tx", 910801, 0)
	next := env.submitCheckpoint(ctx, t,
		newCheckpointPreparedTx(nextCpRef, nextRef.BlockNum, checkpointDigest("later-hash")))
	require.Nil(t, next.CheckpointFeedback)
	require.Len(t, next.Status, 1)
	require.Equal(t, committerpb.Status_COMMITTED, next.Status[0].Status)
	requireSnapshotStatus(t, env.dbEnv, nextRef.TxId, committerpb.SnapshotState_CHECKPOINTED)
}

// TestCheckpointWriteInBatchRejectsBrokenInvariants checks that invalid keys and multiple
// checkpoints fail the batch. They must not leave unverified writes ready to commit.
func TestCheckpointWriteInBatchRejectsBrokenInvariants(t *testing.T) {
	t.Parallel()

	const blockNum = 910900
	hash := checkpointDigest("attested-hash")
	cpRef := committerpb.NewTxRef("cp-broken-tx", 910901, 0)

	t.Run("undecodable key", func(t *testing.T) {
		t.Parallel()
		prepTx := newCheckpointPreparedTx(cpRef, blockNum, hash)
		w := prepTx.txIDToNsNewWrites[TxID(cpRef.TxId)][committerpb.CheckpointNamespaceID]
		w.keys[0] = []byte{0xff}

		cp, err := checkpointWriteInBatch(prepTx)
		require.ErrorContains(t, err, "undecodable key")
		require.Nil(t, cp)
	})

	t.Run("key with "+trailingBytesError, func(t *testing.T) {
		t.Parallel()
		prepTx := newCheckpointPreparedTx(cpRef, blockNum, hash)
		w := prepTx.txIDToNsNewWrites[TxID(cpRef.TxId)][committerpb.CheckpointNamespaceID]
		w.keys[0] = append(servicepb.CheckpointKey(blockNum), []byte("junk")...)

		cp, err := checkpointWriteInBatch(prepTx)
		require.ErrorContains(t, err, trailingBytesError)
		require.Nil(t, cp)
	})

	t.Run("two checkpoints in one batch", func(t *testing.T) {
		t.Parallel()
		prepTx := newCheckpointPreparedTx(cpRef, blockNum, hash)
		// A second checkpoint for a different block, so the two do not collide on one key.
		secondRef := committerpb.NewTxRef("cp-broken-second-tx", 910901, 1)
		secondTxID := TxID(secondRef.TxId)
		secondKey := servicepb.CheckpointKey(blockNum + 1)
		prepTx.txIDToNsNewWrites.getOrCreate(secondTxID, committerpb.CheckpointNamespaceID).
			append(secondKey, hash, 0)
		prepTx.txIDToHeight[secondTxID] = servicepb.NewHeightFromTxRef(secondRef)

		cp, err := checkpointWriteInBatch(prepTx)
		require.ErrorContains(t, err, "at most one checkpoint")
		require.Nil(t, cp)
	})
}

func TestBlockNumFromCheckpointKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		blockNum uint64
	}{
		{name: "zero", blockNum: 0},
		{name: "single byte", blockNum: 42},
		{name: "max", blockNum: ^uint64(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := servicepb.BlockNumFromCheckpointKey(servicepb.CheckpointKey(tc.blockNum))
			require.NoError(t, err)
			require.Equal(t, tc.blockNum, got)
		})
	}

	for _, tc := range []struct {
		name          string
		key           []byte
		expectedError string
	}{
		{name: "empty key", key: nil, expectedError: "failed to decode block number"},
		{
			name:          "trailing bytes",
			key:           append(servicepb.CheckpointKey(7), []byte("junk")...),
			expectedError: trailingBytesError,
		},
		{
			name:          "height key carries a tx number",
			key:           servicepb.NewHeight(7, 3).ToBytes(),
			expectedError: trailingBytesError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := servicepb.BlockNumFromCheckpointKey(tc.key)
			require.ErrorContains(t, err, tc.expectedError)
		})
	}
}

// TestCommitCheckpointNonRetryableIsTerminal checks that inconsistent snapshot metadata
// stops the commit without retries. It calls the committer directly because the
// validator would reject this checkpoint before it reached the committer.
func TestCommitCheckpointNonRetryableIsTerminal(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	ctx, _ := createContext(t)

	hash := checkpointDigest("local-hash-terminal")
	ref := committerpb.NewTxRef("cp-terminal-snapshot", 911100, 0)
	env.dbEnv.SeedSnapshotRecord(t, SnapshotFixture{
		Ref:           ref,
		Status:        committerpb.SnapshotState_COMPLETED,
		CloneDatabase: snapshotDatabaseName(ref),
	})

	// Remove the pointer to simulate metadata changing after checkpoint verification.
	env.dbEnv.ClearLatestSnapshotKey(t)

	cpRef := committerpb.NewTxRef("cp-terminal-tx", 911101, 0)
	vTx := newValidatedTxsFromPrepared(newCheckpointPreparedTx(cpRef, ref.BlockNum, hash))

	// A retry would wait one minute. Returning within 30 seconds proves there was no retry.
	env.dbEnv.DB.retryProfile = &retry.Profile{
		InitialInterval: time.Minute,
		MaxInterval:     time.Minute,
		Multiplier:      1,
		MaxElapsedTime:  new(10 * time.Minute),
	}

	start := time.Now()
	status, err := env.c.commitTransactions(ctx, env.dbEnv.DB, vTx)
	require.ErrorIs(t, err, retry.ErrNonRetryable)
	require.Nil(t, status)
	require.Less(t, time.Since(start), 30*time.Second, "the commit retried a terminal error")

	// The checkpoint must not commit if its snapshot status update fails.
	requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
}

func TestDigestAfterAbort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hash []byte
	}{
		{name: "no digest"},
		{name: "digest computed", hash: make([]byte, 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, true)
			ctx, _ := createContext(t)
			ref := env.seedSnapshotHeldAt(t, snapshotHold{
				txID: "closed-" + tc.name, blockNum: 930100,
				status: committerpb.SnapshotState_ABORTED, digest: tc.hash,
			})
			cpRef := committerpb.NewTxRef("after-abort-"+tc.name, 930101, 0)
			cp := newCheckpointPreparedTx(cpRef, ref.BlockNum, make([]byte, 32))
			verdict, err := env.dbEnv.DB.verifyCheckpoint(ctx, cp.checkpoint)
			require.NoError(t, err)
			require.Nil(t, verdict.feedback)
			require.Equal(t, committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY, verdict.rejectStatus)
			status := env.submitCheckpoint(ctx, t, cp)
			require.Nil(t, status.CheckpointFeedback)
			require.Len(t, status.Status, 1)
			require.Equal(t, committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY, status.Status[0].Status)
			requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
			requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_ABORTED)
		})
	}
}

func TestCheckpointWriteInBatchRejectsInvalidValue(t *testing.T) {
	t.Parallel()
	prep := newCheckpointPreparedTx(committerpb.NewTxRef("bad-value", 8, 0), 7, []byte("hash"))
	_, err := checkpointWriteInBatch(prep)
	require.ErrorContains(t, err, "invalid value")
}

func TestCheckpointWriteInBatchRejectsMultipleCheckpoints(t *testing.T) {
	t.Parallel()
	prep := newCheckpointPreparedTx(committerpb.NewTxRef("abort", 8, 0), 6, []byte("abort"))
	prep.txIDToNsNewWrites.getOrCreate("digest", committerpb.CheckpointNamespaceID).
		append(servicepb.CheckpointKey(7), make([]byte, 32), 0)
	prep.txIDToHeight["digest"] = servicepb.NewHeight(8, 1)
	_, err := checkpointWriteInBatch(prep)
	require.ErrorContains(t, err, "at most one checkpoint")
}

func TestCheckpointMarkerPrefixedDigest(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)
	hash := append([]byte("abort"), make([]byte, 27)...)
	ref := env.seedSnapshotRecord(t, "marker-prefixed", 930200, hash)
	cp := newCheckpointPreparedTx(committerpb.NewTxRef("marker-prefixed-cp", 930201, 0), ref.BlockNum, hash)
	status := env.submitCheckpoint(ctx, t, cp)
	require.Nil(t, status.CheckpointFeedback)
	require.Len(t, status.Status, 1)
	require.Equal(t, committerpb.Status_COMMITTED, status.Status[0].Status)
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_CHECKPOINTED)
	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, hash)
}

func TestCommitSnapshotAbort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		blockNum uint64
		// held is the status the snapshot record is in when the abort arrives.
		held   committerpb.SnapshotState_Status
		digest []byte
	}{
		{name: "pending", blockNum: 920100, held: committerpb.SnapshotState_PENDING},
		{name: "in progress", blockNum: 920101, held: committerpb.SnapshotState_IN_PROGRESS},
		{name: "failed", blockNum: 920102, held: committerpb.SnapshotState_FAILED},
		{
			name:     "completed with a digest",
			blockNum: 920103,
			held:     committerpb.SnapshotState_COMPLETED,
			digest:   []byte("diverged-digest"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, true)
			ctx, _ := createContext(t)

			ref := env.seedSnapshotHeldAt(t, snapshotHold{
				txID: "abort-" + tc.name, blockNum: tc.blockNum, status: tc.held, digest: tc.digest,
			})
			abortRef := committerpb.NewTxRef("abort-tx-"+tc.name, tc.blockNum+1, 0)

			status := env.submitSnapshotAbort(ctx, t, newSnapshotAbortPreparedTx(abortRef, tc.blockNum))
			require.Len(t, status.Status, 1)
			require.Equal(t, committerpb.Status_COMMITTED, status.Status[0].Status)

			// Both must be durable: the row is the fact a replay reproduces, the status is
			// what admits the next snapshot.
			requireSnapshotAbortRow(t, env.dbEnv, tc.blockNum)
			requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_ABORTED)

			// The pointer still names the record, now ABORTED, never the abort row.
			state, err := env.dbEnv.DB.snapshotState.ReadLatest(ctx)
			require.NoError(t, err)
			require.Equal(t, ref.TxId, state.TxRef.TxId)
		})
	}
}

// TestRejectSnapshotAbortForClosedOrUnknownSnapshot covers every abort that names no
// abortable snapshot. All are per-TX rejections rather than failures: the submitter
// chose the block number, and local state is not in question.
func TestRejectSnapshotAbortForClosedOrUnknownSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// snapshotBlock is the block of the seeded snapshot, or 0 to seed none.
		snapshotBlock uint64
		seededStatus  committerpb.SnapshotState_Status
		// abortBlock is the block the abort names.
		abortBlock uint64
	}{
		{
			name:          "already checkpointed",
			snapshotBlock: 920200,
			seededStatus:  committerpb.SnapshotState_CHECKPOINTED,
			abortBlock:    920200,
		},
		{
			name:          "already aborted",
			snapshotBlock: 920201,
			seededStatus:  committerpb.SnapshotState_ABORTED,
			abortBlock:    920201,
		},
		{
			name:          "names a block that is not the latest snapshot",
			snapshotBlock: 920202,
			seededStatus:  committerpb.SnapshotState_PENDING,
			abortBlock:    920999,
		},
		{
			name:       "no snapshot was ever accepted",
			abortBlock: 920203,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, true)
			ctx, _ := createContext(t)

			if tc.snapshotBlock != 0 {
				env.seedSnapshotHeldAt(t, snapshotHold{
					txID: "abort-rej-" + tc.name, blockNum: tc.snapshotBlock, status: tc.seededStatus,
				})
			}
			abortRef := committerpb.NewTxRef("abort-rej-tx-"+tc.name, tc.abortBlock+1, 0)

			status := env.submitSnapshotAbort(ctx, t, newSnapshotAbortPreparedTx(abortRef, tc.abortBlock))
			require.Len(t, status.Status, 1)
			require.Equal(t, committerpb.Status_REJECTED_SNAPSHOT_ABORT_NO_SUCH_SNAPSHOT, status.Status[0].Status)

			// No row is left behind, so a replay cannot reproduce an abort we never accepted.
			requireNoSnapshotAbortRow(t, env.dbEnv, tc.abortBlock)
		})
	}
}

// TestSnapshotAbortDoesNotGateOnPriorSnapshot pins the interaction that makes an abort
// usable at all: the admission gate rejects a new snapshot until the latest one is
// lifecycle-closed, and an abort is submitted precisely when it is not. Were the abort
// gated like a request, the stuck snapshot could never be aborted.
func TestSnapshotAbortDoesNotGateOnPriorSnapshot(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, false)
	ctx, _ := createContext(t)

	env.seedSnapshotHeldAt(t, snapshotHold{
		txID: "abort-not-gated", blockNum: 920300, status: committerpb.SnapshotState_FAILED,
	})

	abortRef := committerpb.NewTxRef("abort-not-gated-tx", 920301, 0)
	prepTx := newSnapshotAbortPreparedTx(abortRef, 920300)
	vTx := newValidatedTxsFromPrepared(prepTx)

	require.NoError(t, env.dbEnv.DB.rejectSnapshotIfPriorNotCheckpointed(ctx, vTx))
	require.Empty(t, vTx.invalidTxStatus, "the admission gate must ignore an abort TX")
	require.NotEmpty(t, vTx.newWrites)
}

// TestSnapshotAbortBatchHelpers covers the two batch-level rules that keep an abort out of
// the snapshot-request path: it must never be mistaken for a request (which would clone a
// database for a snapshot being abandoned), and two aborts in one batch must fail the batch
// rather than let an unverified one commit.
func TestSnapshotAbortBatchHelpers(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	ctx, _ := createContext(t)

	prepTx := newSnapshotAbortPreparedTx(committerpb.NewTxRef("abort-no-clone-tx", 920400, 0), 920399)
	_, found := snapshotWriteInBatch(prepTx.txIDToNsNewWrites)
	require.False(t, found, "an abort write must not be seen as a snapshot request")
	require.NoError(t, env.dbEnv.DB.createSnapshotIfPresent(ctx, prepTx.txIDToNsNewWrites))

	twoAborts := newSnapshotAbortPreparedTx(committerpb.NewTxRef("abort-a", 3, 0), 1)
	twoAborts.txIDToNsNewWrites.getOrCreate("abort-b", committerpb.CheckpointNamespaceID).
		append(servicepb.CheckpointKey(2), []byte(servicepb.CheckpointAbortValue), 0)
	twoAborts.txIDToHeight["abort-b"] = servicepb.NewHeight(3, 1)
	_, err := checkpointWriteInBatch(twoAborts)
	require.ErrorContains(t, err, "at most one checkpoint")
}

func TestCheckpointClosureResubmission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{name: digestCheckpointCase, value: checkpointDigest("resubmitted")},
		{name: servicepb.CheckpointAbortValue, value: []byte(servicepb.CheckpointAbortValue)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, true)
			ctx, _ := createContext(t)
			ref := env.seedSnapshotRecord(t, "original-"+tc.name, 940100, checkpointDigest("resubmitted"))
			cpRef := committerpb.NewTxRef("resubmitted-"+tc.name, 940101, 0)
			first := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, tc.value))
			requireClosureStatus(t, first, committerpb.Status_COMMITTED)
			saved, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
			require.True(t, found)

			second := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, tc.value))
			requireClosureStatus(t, second, committerpb.Status_COMMITTED)
			later := env.seedSnapshotRecord(t, "later-"+tc.name, 940200, nil)
			third := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, tc.value))
			requireClosureStatus(t, third, committerpb.Status_COMMITTED)
			requireSnapshotStatus(t, env.dbEnv, later.TxId, committerpb.SnapshotState_PENDING)

			// Deduplication must not consult another snapshot, including a corrupt later record.
			_, err := env.dbEnv.DB.pool.Exec(ctx, "UPDATE ns__snapshot SET value = $1 WHERE key = $2",
				[]byte{0xff}, []byte(later.TxId))
			require.NoError(t, err)
			duplicate := newValidatedTxsFromPrepared(newCheckpointPreparedTx(cpRef, ref.BlockNum, tc.value))
			require.NoError(t, env.dbEnv.DB.rejectCheckpointIfNotVerified(ctx, duplicate))
			require.NotNil(t, duplicate.checkpoint, "commit owns duplicate removal")
			require.Contains(t, duplicate.newWrites, TxID(cpRef.TxId))
			require.Empty(t, duplicate.invalidTxStatus)
			fourth := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, tc.value))
			requireClosureStatus(t, fourth, committerpb.Status_COMMITTED)

			trueDuplicate := committerpb.NewTxRef(cpRef.TxId, cpRef.BlockNum+1, 0)
			status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(trueDuplicate, ref.BlockNum, tc.value))
			requireClosureStatus(t, status, committerpb.Status_REJECTED_DUPLICATE_TX_ID)
			requireCheckpointRow(t, env.dbEnv, ref.BlockNum, tc.value)
			unchanged, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
			require.True(t, found)
			require.Equal(t, saved.Version, unchanged.Version)
			require.Equal(t, saved.State.Status, unchanged.State.Status)
		})
	}
}

func TestCheckpointClosureRejectedResubmission(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)
	cpRef := committerpb.NewTxRef("rejected-closure", 940301, 0)
	value := []byte("abort")
	first := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, 940300, value))
	requireClosureStatus(t, first, committerpb.Status_REJECTED_SNAPSHOT_ABORT_NO_SUCH_SNAPSHOT)
	ref := env.seedSnapshotRecord(t, "now-exists", 940300, nil)
	second := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, value))
	requireClosureStatus(t, second, committerpb.Status_REJECTED_SNAPSHOT_ABORT_NO_SUCH_SNAPSHOT)
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_PENDING)
	requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
}

func TestCheckpointCompetingTransactions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		first  []byte
		second []byte
		target committerpb.SnapshotState_Status
		reject committerpb.Status
	}{
		{
			name: "abort first", first: []byte("abort"), second: checkpointDigest("competing"),
			target: committerpb.SnapshotState_ABORTED, reject: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "digest first", first: checkpointDigest("competing"), second: []byte("abort"),
			target: committerpb.SnapshotState_CHECKPOINTED,
			reject: committerpb.Status_REJECTED_SNAPSHOT_ABORT_NO_SUCH_SNAPSHOT,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, true)
			ctx, _ := createContext(t)
			ref := env.seedSnapshotRecord(t, "competing-"+tc.name, 940400, checkpointDigest("competing"))
			firstRef := committerpb.NewTxRef("first-"+tc.name, 940401, 0)
			secondRef := committerpb.NewTxRef("second-"+tc.name, 940401, 1)
			first := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(firstRef, ref.BlockNum, tc.first))
			requireClosureStatus(t, first, committerpb.Status_COMMITTED)
			saved, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
			require.True(t, found)
			second := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(secondRef, ref.BlockNum, tc.second))
			requireClosureStatus(t, second, tc.reject)
			requireCheckpointRow(t, env.dbEnv, ref.BlockNum, tc.first)
			requireSnapshotStatus(t, env.dbEnv, ref.TxId, tc.target)
			unchanged, found := env.dbEnv.ReadSnapshotRecord(ctx, ref.TxId)
			require.True(t, found)
			require.Equal(t, saved.Version, unchanged.Version)
		})
	}
}

func TestCheckpointClosureCommitCollision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		first  []byte
		second []byte
		target committerpb.SnapshotState_Status
	}{
		{
			name: "abort first", first: []byte("abort"), second: checkpointDigest("collision"),
			target: committerpb.SnapshotState_ABORTED,
		},
		{
			name: "digest first", first: checkpointDigest("collision"), second: []byte("abort"),
			target: committerpb.SnapshotState_CHECKPOINTED,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCommitterTestEnv(t)
			ctx, _ := createContext(t)
			ref := committerpb.NewTxRef("collision-"+tc.name, 940500, 0)
			env.dbEnv.SeedSnapshotRecord(t, SnapshotFixture{Ref: ref, Status: committerpb.SnapshotState_COMPLETED})
			first := newValidatedTxsFromPrepared(newCheckpointPreparedTx(
				committerpb.NewTxRef("first-"+tc.name, 940501, 0), ref.BlockNum, tc.first,
			))
			second := newValidatedTxsFromPrepared(newCheckpointPreparedTx(
				committerpb.NewTxRef("second-"+tc.name, 940501, 1), ref.BlockNum, tc.second,
			))
			status, err := env.c.commitTransactions(ctx, env.dbEnv.DB, first)
			require.NoError(t, err)
			requireClosureStatus(t, status, committerpb.Status_COMMITTED)
			status, err = env.c.commitTransactions(ctx, env.dbEnv.DB, second)
			require.NoError(t, err)
			requireClosureStatus(t, status, committerpb.Status_ABORTED_MVCC_CONFLICT)
			require.Nil(t, second.checkpoint)
			requireCheckpointRow(t, env.dbEnv, ref.BlockNum, tc.first)
			requireSnapshotStatus(t, env.dbEnv, ref.TxId, tc.target)
		})
	}
}

func TestCheckpointClosureConcurrentCommit(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	ref := committerpb.NewTxRef("concurrent-closure", 940600, 0)
	env.dbEnv.SeedSnapshotRecord(t, SnapshotFixture{Ref: ref, Status: committerpb.SnapshotState_COMPLETED})
	checkpoints := []struct {
		value []byte
		txNum uint32
	}{
		{value: []byte(servicepb.CheckpointAbortValue), txNum: 0},
		{value: checkpointDigest("concurrent"), txNum: 1},
	}
	statuses := make([]*servicepb.TxStatusBatch, len(checkpoints))
	g, ctx := errgroup.WithContext(t.Context())
	for i, cp := range checkpoints {
		g.Go(func() error {
			cpRef := committerpb.NewTxRef("concurrent-"+strconv.Itoa(i), 940601, cp.txNum)
			var err error
			statuses[i], err = env.c.commitTransactions(ctx, env.dbEnv.DB,
				newValidatedTxsFromPrepared(newCheckpointPreparedTx(cpRef, ref.BlockNum, cp.value)))
			return err
		})
	}
	require.NoError(t, g.Wait())
	var winnerValue []byte
	got := make([]committerpb.Status, len(checkpoints))
	for i, cp := range checkpoints {
		status := statuses[i]
		require.Nil(t, status.CheckpointFeedback)
		require.Len(t, status.Status, 1)
		got[i] = status.Status[0].Status
		if got[i] == committerpb.Status_COMMITTED {
			winnerValue = cp.value
		}
	}
	require.ElementsMatch(t, []committerpb.Status{
		committerpb.Status_COMMITTED, committerpb.Status_ABORTED_MVCC_CONFLICT,
	}, got)
	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, winnerValue)
	target := committerpb.SnapshotState_CHECKPOINTED
	if string(winnerValue) == servicepb.CheckpointAbortValue {
		target = committerpb.SnapshotState_ABORTED
	}
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, target)
}

func TestCheckpointClosureRollback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{name: servicepb.CheckpointAbortValue, value: []byte(servicepb.CheckpointAbortValue)},
		{name: digestCheckpointCase, value: checkpointDigest("rollback")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCommitterTestEnv(t)
			ctx, _ := createContext(t)
			ref := committerpb.NewTxRef("rollback-"+tc.name, 940700, 0)
			env.dbEnv.SeedSnapshotRecord(t, SnapshotFixture{Ref: ref, Status: committerpb.SnapshotState_COMPLETED})
			env.dbEnv.ClearLatestSnapshotKey(t)
			cpRef := committerpb.NewTxRef("rollback-cp-"+tc.name, 940701, 0)
			status, err := env.c.commitTransactions(ctx, env.dbEnv.DB,
				newValidatedTxsFromPrepared(newCheckpointPreparedTx(cpRef, ref.BlockNum, tc.value)))
			require.ErrorIs(t, err, retry.ErrNonRetryable)
			require.Nil(t, status)
			requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
			persisted, err := env.dbEnv.DB.readStatusWithHeight(ctx, [][]byte{[]byte(cpRef.TxId)})
			require.NoError(t, err)
			require.Empty(t, persisted)
			requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_COMPLETED)
		})
	}
}

func TestCheckpointDivergencePreservesAbortedSnapshot(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)
	ref := env.seedSnapshotRecord(t, "stale-divergence", 950100, checkpointDigest("local"))
	observed, err := env.dbEnv.DB.snapshotState.ReadLatest(ctx)
	require.NoError(t, err)
	abort := newCheckpointPreparedTx(committerpb.NewTxRef("stale-divergence-abort", 950101, 0), ref.BlockNum,
		[]byte("abort"))
	requireClosureStatus(t, env.submitCheckpoint(ctx, t, abort), committerpb.Status_COMMITTED)
	cp := newCheckpointPreparedTx(committerpb.NewTxRef("stale-divergence-digest", 950101, 1), ref.BlockNum,
		checkpointDigest("different"))
	verdict, err := env.dbEnv.DB.haltOnCheckpointDivergence(ctx, cp.checkpoint, observed, "stale mismatch")
	require.NoError(t, err)
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_ABORTED)
	require.Nil(t, verdict.feedback)
	require.Equal(t, committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY, verdict.rejectStatus)
	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, []byte("abort"))
}

func TestCheckpointRejectsUnknownSnapshotStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{name: servicepb.CheckpointAbortValue, value: []byte(servicepb.CheckpointAbortValue)},
		{name: digestCheckpointCase, value: checkpointDigest("unknown")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newValidatorTestEnv(t, false)
			ctx, _ := createContext(t)
			ref := env.seedSnapshotHeldAt(t, snapshotHold{
				txID: "unknown-" + tc.name, blockNum: 950200, status: 999,
			})
			cpRef := committerpb.NewTxRef("unknown-cp-"+tc.name, 950201, 0)
			cp := newValidatedTxsFromPrepared(newCheckpointPreparedTx(cpRef, ref.BlockNum, tc.value))
			err := env.dbEnv.DB.rejectCheckpointIfNotVerified(ctx, cp)
			require.ErrorContains(t, err, "unexpected snapshot status")
			requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
			persisted, err := env.dbEnv.DB.readStatusWithHeight(ctx, [][]byte{[]byte(cpRef.TxId)})
			require.NoError(t, err)
			require.Empty(t, persisted)
			_, err = env.dbEnv.DB.snapshotState.Read(ctx, ref.TxId)
			require.ErrorContains(t, err, "unexpected snapshot status")
		})
	}
}

// seedSnapshotRecord saves a snapshot and its local hash.
// A nil hash leaves the snapshot waiting for hashing, so checkpoints receive HOLD.
func (env *validatorTestEnv) seedSnapshotRecord(
	t *testing.T, txID string, blockNum uint64, hash []byte,
) *committerpb.TxRef {
	t.Helper()
	ref := committerpb.NewTxRef(txID, blockNum, 0)
	env.dbEnv.SeedSnapshotRecord(t, SnapshotFixture{
		Ref:           ref,
		Status:        committerpb.SnapshotState_PENDING,
		CloneDatabase: snapshotDatabaseName(ref),
	})
	if hash != nil {
		require.NoError(t, env.dbEnv.DB.snapshotState.Update(t.Context(), ref, statedb.SnapshotUpdate{
			Status: committerpb.SnapshotState_COMPLETED, Digest: hash,
		}))
	}
	return ref
}

// submitCheckpoint sends a prepared checkpoint through the validator and committer.
// It returns the status batch produced by the pipeline.
func (env *validatorTestEnv) submitCheckpoint(
	ctx context.Context, t *testing.T, cp *preparedTransactions,
) *servicepb.TxStatusBatch {
	t.Helper()
	channel.NewWriter(ctx, env.preparedTxs).Write(cp)
	status, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	return status
}

// newCheckpointPreparedTx builds a checkpoint with one new write.
// The key is the snapshot block number; the value is the submitted hash.
func newCheckpointPreparedTx(
	ref *committerpb.TxRef, blockNum uint64, hash []byte,
) *preparedTransactions {
	key := servicepb.CheckpointKey(blockNum)
	txID := TxID(ref.TxId)
	prepTx := newEmptyPreparedTransactions()
	prepTx.txIDToNsNewWrites.getOrCreate(txID, committerpb.CheckpointNamespaceID).append(key, hash, 0)
	// Associate the read with this transaction so a key conflict can reject it.
	prepTx.readToTxIDs[newCmpRead(committerpb.CheckpointNamespaceID, key, nil)] = []TxID{txID}
	prepTx.txIDToHeight[txID] = servicepb.NewHeightFromTxRef(ref)
	prepTx.checkpoint = &checkpointTx{txID: txID, ref: ref, snapshotBlockNum: blockNum, value: hash}
	return prepTx
}

func newEmptyPreparedTransactions() *preparedTransactions {
	return &preparedTransactions{
		nsToReads:              make(namespaceToReads),
		readToTxIDs:            make(readToTransactions),
		txIDToNsNonBlindWrites: make(transactionToWrites),
		txIDToNsBlindWrites:    make(transactionToWrites),
		txIDToNsNewWrites:      make(transactionToWrites),
		invalidTxIDStatus:      make(map[TxID]committerpb.Status),
		txIDToHeight:           make(transactionIDToHeight),
	}
}

// requireFeedback checks the signal, snapshot block number, and checkpoint reference.
// Callers check the HALT reason separately.
func requireFeedback(t *testing.T, actual, expected *servicepb.CheckpointFeedback, msg string) {
	t.Helper()
	require.NotNil(t, actual, msg)
	require.Equal(t, expected.Signal, actual.Signal, msg)
	require.Equal(t, expected.SnapshotBlockNumber, actual.SnapshotBlockNumber, msg)
	require.Equal(t, expected.Ref.TxId, actual.Ref.GetTxId(), msg)
	// Use the checkpoint's position, not the snapshot's, so the coordinator can release its node.
	require.Equal(t, expected.Ref.BlockNum, actual.Ref.GetBlockNum(), msg)
	require.Equal(t, expected.Ref.TxNum, actual.Ref.GetTxNum(), msg)
}

// wantFeedback builds the expected feedback for cpRef and its snapshot at blockNum.
func wantFeedback(
	signal servicepb.CheckpointFeedback_Signal, cpRef *committerpb.TxRef, blockNum uint64,
) *servicepb.CheckpointFeedback {
	return &servicepb.CheckpointFeedback{Signal: signal, Ref: cpRef, SnapshotBlockNumber: blockNum}
}

func requireCheckpointRow(t *testing.T, env *DatabaseTestEnv, blockNum uint64, hash []byte) {
	t.Helper()
	key := servicepb.CheckpointKey(blockNum)
	rows := env.FetchKeys(t, committerpb.CheckpointNamespaceID, [][]byte{key})
	require.NotNil(t, rows[string(key)])
	require.Equal(t, hash, rows[string(key)].Value)
}

func requireNoCheckpointRow(t *testing.T, env *DatabaseTestEnv, blockNum uint64) {
	t.Helper()
	env.rowNotExists(t, committerpb.CheckpointNamespaceID, [][]byte{servicepb.CheckpointKey(blockNum)})
}

func checkpointDigest(label string) []byte {
	digest := sha256.Sum256([]byte(label))
	return digest[:]
}

// newValidatedTxsFromPrepared copies a prepared batch without adding validation errors or feedback.
func newValidatedTxsFromPrepared(prepTx *preparedTransactions) *validatedTransactions {
	return &validatedTransactions{
		validTxNonBlindWrites: prepTx.txIDToNsNonBlindWrites,
		validTxBlindWrites:    prepTx.txIDToNsBlindWrites,
		newWrites:             prepTx.txIDToNsNewWrites,
		readToTxIDs:           prepTx.readToTxIDs,
		invalidTxStatus:       prepTx.invalidTxIDStatus,
		txIDToHeight:          prepTx.txIDToHeight,
		checkpoint:            prepTx.checkpoint,
	}
}

// submitSnapshotAbort sends an abort TX through the real validator-committer pipeline
// and returns the resulting status batch, so every case asserts the verdict the
// pipeline actually produces rather than one a direct call would fabricate.
func (env *validatorTestEnv) submitSnapshotAbort(
	ctx context.Context, t *testing.T, abort *preparedTransactions,
) *servicepb.TxStatusBatch {
	t.Helper()
	channel.NewWriter(ctx, env.preparedTxs).Write(abort)
	status, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	return status
}

// seedSnapshotHeldAt commits a `_snapshot` record for hold.blockNum and leaves it at hold.status.
func (env *validatorTestEnv) seedSnapshotHeldAt(t *testing.T, hold snapshotHold) *committerpb.TxRef {
	t.Helper()
	ref := env.seedSnapshotRecord(t, hold.txID, hold.blockNum, hold.digest)
	if hold.status != committerpb.SnapshotState_COMPLETED {
		require.NoError(t, env.dbEnv.DB.snapshotState.Update(t.Context(), ref, statedb.SnapshotUpdate{
			Status: hold.status,
		}))
	}
	return ref
}

func newSnapshotAbortPreparedTx(ref *committerpb.TxRef, blockNum uint64) *preparedTransactions {
	return newCheckpointPreparedTx(ref, blockNum, []byte(servicepb.CheckpointAbortValue))
}

func requireSnapshotAbortRow(t *testing.T, env *DatabaseTestEnv, blockNum uint64) {
	t.Helper()
	requireCheckpointRow(t, env, blockNum, []byte(servicepb.CheckpointAbortValue))
}

func requireNoSnapshotAbortRow(t *testing.T, env *DatabaseTestEnv, blockNum uint64) {
	t.Helper()
	requireNoCheckpointRow(t, env, blockNum)
}

func requireClosureStatus(t *testing.T, status *servicepb.TxStatusBatch, want committerpb.Status) {
	t.Helper()
	require.Nil(t, status.CheckpointFeedback)
	require.Len(t, status.Status, 1)
	require.Equal(t, want, status.Status[0].Status)
}

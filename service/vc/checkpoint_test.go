/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// trailingBytesError is shared by tests that reject extra bytes in a checkpoint key.
const trailingBytesError = "trailing bytes after the block number"

// TestCommitCheckpointOnHashMatch checks that matching hashes allow the checkpoint
// to commit and mark the snapshot CHECKPOINTED, with no feedback.
func TestCommitCheckpointOnHashMatch(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	hash := []byte("local-hash-match")
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

	ref := env.seedSnapshotRecord(t, "cp-mismatch", 910200, []byte("local-hash"))
	cpRef := committerpb.NewTxRef("cp-mismatch-tx", 910201, 0)

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, []byte("attested-hash")))
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
	hash := []byte("hash-that-lands-later")

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

	hash := []byte("duplicate-hash")
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

	ref := env.seedSnapshotRecord(t, "cp-wrong-block", 910500, []byte("local-hash"))
	cpRef := committerpb.NewTxRef("cp-wrong-block-tx", 910501, 0)
	otherBlock := ref.BlockNum + 7

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, otherBlock, []byte("local-hash")))
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

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, blockNum, []byte("attested-hash")))
	require.Nil(t, status.CheckpointFeedback)
	require.Len(t, status.Status, 1)
	require.Equal(t, committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY, status.Status[0].Status)
	requireNoCheckpointRow(t, env.dbEnv, blockNum)

	// A later snapshot and checkpoint must still succeed.
	nextRef := env.seedSnapshotRecord(t, "cp-after-reject", 910800, []byte("later-hash"))
	nextCpRef := committerpb.NewTxRef("cp-after-reject-tx", 910801, 0)
	next := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(nextCpRef, nextRef.BlockNum, []byte("later-hash")))
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
	hash := []byte("attested-hash")
	cpRef := committerpb.NewTxRef("cp-broken-tx", 910901, 0)

	t.Run("undecodable key", func(t *testing.T) {
		t.Parallel()
		prepTx := newCheckpointPreparedTx(cpRef, blockNum, hash)
		w := prepTx.txIDToNsNewWrites[TxID(cpRef.TxId)][committerpb.CheckpointNamespaceID]
		w.keys[0] = []byte{0xff}

		cp, err := checkpointWriteInBatch(newValidatedTxsFromPrepared(prepTx))
		require.ErrorContains(t, err, "undecodable key")
		require.Nil(t, cp)
	})

	t.Run("key with "+trailingBytesError, func(t *testing.T) {
		t.Parallel()
		prepTx := newCheckpointPreparedTx(cpRef, blockNum, hash)
		w := prepTx.txIDToNsNewWrites[TxID(cpRef.TxId)][committerpb.CheckpointNamespaceID]
		w.keys[0] = append(servicepb.CheckpointKey(blockNum), []byte("junk")...)

		cp, err := checkpointWriteInBatch(newValidatedTxsFromPrepared(prepTx))
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

		cp, err := checkpointWriteInBatch(newValidatedTxsFromPrepared(prepTx))
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

// TestCommitCheckpointNonRetryableIsTerminal checks that inconsistent snapshot metadata
// stops the commit without retries. It calls the committer directly because the
// validator would reject this checkpoint before it reached the committer.
func TestCommitCheckpointNonRetryableIsTerminal(t *testing.T) {
	t.Parallel()
	env := newCommitterTestEnv(t)
	ctx, _ := createContext(t)

	hash := []byte("local-hash-terminal")
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

// newValidatedTxsFromPrepared copies a prepared batch without adding validation errors or feedback.
func newValidatedTxsFromPrepared(prepTx *preparedTransactions) *validatedTransactions {
	return &validatedTransactions{
		validTxNonBlindWrites: prepTx.txIDToNsNonBlindWrites,
		validTxBlindWrites:    prepTx.txIDToNsBlindWrites,
		newWrites:             prepTx.txIDToNsNewWrites,
		readToTxIDs:           prepTx.readToTxIDs,
		invalidTxStatus:       prepTx.invalidTxIDStatus,
		txIDToHeight:          prepTx.txIDToHeight,
	}
}

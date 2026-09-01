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
	"github.com/hyperledger/fabric-x-committer/utils/snapshotstate"
)

// trailingBytesError is the error a checkpoint key carrying data after its block number
// produces. Asserted by both the decoder's own cases and the VC's rejection of such a key.
const trailingBytesError = "trailing bytes after the block number"

// TestCommitCheckpointOnHashMatch covers the normal path: the attestation matches this
// committer's own hash, so the `_checkpoint` row is written, the snapshot reaches its
// terminal CHECKPOINTED status, and no feedback gates the pipeline.
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
	// An absent feedback is what tells the sidecar the per-TX status is authoritative.
	require.Nil(t, status.CheckpointFeedback)

	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, hash)
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_CHECKPOINTED)
}

// TestHaltCheckpointOnHashMismatch covers the integrity condition: local state diverges
// from what the organizations attested, so nothing commits, the divergence is recorded on
// the `_snapshot` record for an operator, and the signal is terminal.
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
	// A halted checkpoint gets no per-TX verdict: the divergence needs an operator.
	require.Empty(t, status.Status)

	requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
	// The record is not advanced, and it carries the divergence for the operator.
	requireSnapshotRecord(t, env.dbEnv, ref.TxId, snapshotRecordExpectation{
		status: committerpb.SnapshotState_COMPLETED, errSubstring: "does not match",
	})
}

// TestHoldCheckpointUntilHashIsComputed covers a checkpoint that arrives before this
// committer finished hashing: it must hold rather than treat an absent hash as a
// mismatch, and it must leave NO tx_status row behind. A persisted status would make the
// sidecar's re-submission of the same txID come back as a duplicate forever, so this
// assertion is what keeps the hold recoverable.
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

	// The hash lands, and the re-submitted checkpoint now commits.
	require.NoError(t, env.dbEnv.DB.snapshotState.Update(ctx, ref, snapshotstate.Update{
		Status: committerpb.SnapshotState_COMPLETED, Digest: hash,
	}))
	resubmitted := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, ref.BlockNum, hash))
	require.Nil(t, resubmitted.CheckpointFeedback)
	require.Len(t, resubmitted.Status, 1)
	require.Equal(t, committerpb.Status_COMMITTED, resubmitted.Status[0].Status)

	requireCheckpointRow(t, env.dbEnv, ref.BlockNum, hash)
	requireSnapshotStatus(t, env.dbEnv, ref.TxId, committerpb.SnapshotState_CHECKPOINTED)
}

// TestDuplicateCheckpointIsIgnored covers a resubmitted checkpoint: the attestation is
// already durable, so the second submission must not rewrite the row or the record.
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

	// The durable attestation and the record are both untouched: same value, same row
	// version, same record version -- so this passes only if the duplicate wrote nothing.
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

// TestRejectCheckpointForWrongBlock covers a checkpoint naming a block that is not the
// snapshot awaiting one. The submitter chose that block number, so this is bad input: it
// must be rejected with a per-TX status and leave the pipeline running, not halt it.
func TestRejectCheckpointForWrongBlock(t *testing.T) {
	t.Parallel()
	env := newValidatorTestEnv(t, true)
	ctx, _ := createContext(t)

	ref := env.seedSnapshotRecord(t, "cp-wrong-block", 910500, []byte("local-hash"))
	cpRef := committerpb.NewTxRef("cp-wrong-block-tx", 910501, 0)
	otherBlock := ref.BlockNum + 7

	status := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(cpRef, otherBlock, []byte("local-hash")))
	// No feedback: nothing gates intake, because local state is not in question.
	require.Nil(t, status.CheckpointFeedback)
	require.Len(t, status.Status, 1)
	require.Equal(t, committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY, status.Status[0].Status)
	requireNoCheckpointRow(t, env.dbEnv, otherBlock)

	// The awaited snapshot is untouched, so the correct checkpoint can still arrive.
	requireSnapshotRecord(t, env.dbEnv, ref.TxId, snapshotRecordExpectation{
		status: committerpb.SnapshotState_COMPLETED,
	})
}

// TestRejectCheckpointWithoutSnapshotRecord covers a checkpoint submitted when no snapshot
// was ever requested. `_checkpoint` is client-submittable (see policy.SystemNamespacePolicies),
// so halting here would let any authorized submitter stop the committer: it must be
// rejected as bad input instead, and the pipeline must keep running afterwards.
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

	// A snapshot can still be accepted afterwards, proving the rejection left no state
	// behind that the admission gate would read as a snapshot awaiting a checkpoint.
	nextRef := env.seedSnapshotRecord(t, "cp-after-reject", 910800, []byte("later-hash"))
	nextCpRef := committerpb.NewTxRef("cp-after-reject-tx", 910801, 0)
	next := env.submitCheckpoint(ctx, t, newCheckpointPreparedTx(nextCpRef, nextRef.BlockNum, []byte("later-hash")))
	require.Nil(t, next.CheckpointFeedback)
	require.Len(t, next.Status, 1)
	require.Equal(t, committerpb.Status_COMMITTED, next.Status[0].Status)
	requireSnapshotStatus(t, env.dbEnv, nextRef.TxId, committerpb.SnapshotState_CHECKPOINTED)
}

// TestCheckpointWriteInBatchRejectsBrokenInvariants covers the two states
// checkpointWriteInBatch cannot proceed on. Both are errors rather than skips: a skipped
// write stays in the batch and commits without ever being compared against this
// committer's hash, which is the one outcome the verification exists to prevent.
//
// The sidecar's form check (checkSystemNamespace) rejects a malformed key before the VC
// sees it, and nothing routes two checkpoints into one batch, so neither case is reachable
// today. They are covered because the cost of being wrong is an unverified attestation
// becoming durable.
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

// seedSnapshotRecord commits a `_snapshot` record for a snapshot at blockNum and, when
// hash is non-nil, publishes that hash as this committer's own -- i.e. the state a
// checkpoint is verified against. A nil hash leaves the record without one, which is the
// hold condition.
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
		require.NoError(t, env.dbEnv.DB.snapshotState.Update(t.Context(), ref, snapshotstate.Update{
			Status: committerpb.SnapshotState_COMPLETED, Digest: hash,
		}))
	}
	return ref
}

// submitCheckpoint sends a `_checkpoint` TX for blockNum attesting hash through the real
// validator-committer pipeline and returns the resulting status batch. Every checkpoint
// case drives the pipeline this way, so the verdict under test is the one the pipeline
// actually produces rather than one a direct call would fabricate.
func (env *validatorTestEnv) submitCheckpoint(
	ctx context.Context, t *testing.T, cp *preparedTransactions,
) *servicepb.TxStatusBatch {
	t.Helper()
	channel.NewWriter(ctx, env.preparedTxs).Write(cp)
	status, ok := channel.NewReader(ctx, env.txStatus).Read()
	require.True(t, ok)
	return status
}

// snapshotRecordExpectation is the expected `_snapshot` record state. An empty
// errSubstring requires no error is recorded.
type snapshotRecordExpectation struct {
	status       committerpb.SnapshotState_Status
	errSubstring string
}

// requireSnapshotRecord asserts the `_snapshot` record's status together with its error
// field, so a case that expects a clean record cannot pass while carrying a stale
// divergence.
func requireSnapshotRecord(
	t *testing.T, env *DatabaseTestEnv, txID string, want snapshotRecordExpectation,
) {
	t.Helper()
	wantStatus, wantErrSubstring := want.status, want.errSubstring
	record, found := env.ReadSnapshotRecord(t.Context(), txID)
	require.True(t, found)
	require.Equal(t, wantStatus, record.State.Status)
	if wantErrSubstring == "" {
		require.Empty(t, record.State.Error)
		return
	}
	require.Contains(t, record.State.Error, wantErrSubstring)
}

// newCheckpointPreparedTx builds what the preparer produces for a `_checkpoint` TX: a
// standalone transaction whose single nil-version ReadWrite becomes one new write, key =
// block number, value = the attested hash.
func newCheckpointPreparedTx(
	ref *committerpb.TxRef, blockNum uint64, hash []byte,
) *preparedTransactions {
	key := servicepb.CheckpointKey(blockNum)
	txID := TxID(ref.TxId)
	prepTx := newEmptyPreparedTransactions()
	prepTx.txIDToNsNewWrites.getOrCreate(txID, committerpb.CheckpointNamespaceID).append(key, hash, 0)
	// The nil-version read the preparer records for the write, which is what lets a key
	// conflict be traced back to this txID.
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

// requireFeedback asserts the coordinator-facing signal together with the checkpoint it
// identifies: the sidecar needs the txID to know which transaction to re-submit, and the
// coordinator needs the full reference to release the TX's dependency-graph node, so an
// unlabelled signal is not enough. The HALT reason is asserted by each caller.
func requireFeedback(t *testing.T, actual, expected *servicepb.CheckpointFeedback, msg string) {
	t.Helper()
	require.NotNil(t, actual, msg)
	require.Equal(t, expected.Signal, actual.Signal, msg)
	require.Equal(t, expected.SnapshotBlockNumber, actual.SnapshotBlockNumber, msg)
	require.Equal(t, expected.Ref.TxId, actual.Ref.GetTxId(), msg)
	// The reference must locate the checkpoint TX itself, not the snapshot it attests to:
	// the coordinator looks its node up by height, so a wrong height finds nothing and the
	// node is never freed.
	require.Equal(t, expected.Ref.BlockNum, actual.Ref.GetBlockNum(), msg)
	require.Equal(t, expected.Ref.TxNum, actual.Ref.GetTxNum(), msg)
}

// wantFeedback is the expected signal for the checkpoint TX cpRef attesting blockNum.
func wantFeedback(
	signal servicepb.CheckpointFeedback_Signal, cpRef *committerpb.TxRef, blockNum uint64,
) *servicepb.CheckpointFeedback {
	return &servicepb.CheckpointFeedback{Signal: signal, Ref: cpRef, SnapshotBlockNumber: blockNum}
}

// TestCommitCheckpointNonRetryableIsTerminal covers the commit retry treating
// retry.ErrNonRetryable as terminal. Only the checkpointed-record invariants wrap that
// sentinel inside db.commit, and no retry can fix them: the validator verified the
// checkpoint against the very record that is now missing or for another block, so the
// record changed underneath a verified checkpoint. Without the terminal sentinel the
// commit would retry for the whole profile budget before failing anyway.
//
// The condition is reached by committing a checkpoint whose record was cleared behind the
// validator's back, which is why this drives commitTransactions directly: the validator
// would reject such a checkpoint before the committer ever saw it.
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

	// Clear the latest-snapshot pointer, so MarkCheckpointedInTx finds no record to
	// advance. This is the invariant violation the sentinel marks terminal.
	env.dbEnv.ClearLatestSnapshotKey(t)

	cpRef := committerpb.NewTxRef("cp-terminal-tx", 911101, 0)
	vTx := newValidatedCheckpointTx(cpRef, ref.BlockNum, hash)

	// A short budget with a long interval: a retrying commit could not return within it,
	// so returning promptly is what proves the sentinel was honored rather than retried.
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

	// Nothing became durable: an attestation must never outlive its failed record advance.
	requireNoCheckpointRow(t, env.dbEnv, ref.BlockNum)
}

// newValidatedCheckpointTx builds what the validator hands the committer for a verified
// `_checkpoint` TX: the write survived validation, so no status and no feedback are set.
func newValidatedCheckpointTx(
	ref *committerpb.TxRef, blockNum uint64, hash []byte,
) *validatedTransactions {
	txID := TxID(ref.TxId)
	key := servicepb.CheckpointKey(blockNum)
	newWrites := make(transactionToWrites)
	newWrites.getOrCreate(txID, committerpb.CheckpointNamespaceID).append(key, hash, 0)
	return &validatedTransactions{
		validTxNonBlindWrites: make(transactionToWrites),
		validTxBlindWrites:    make(transactionToWrites),
		newWrites:             newWrites,
		readToTxIDs: readToTransactions{
			newCmpRead(committerpb.CheckpointNamespaceID, key, nil): []TxID{txID},
		},
		invalidTxStatus: make(map[TxID]committerpb.Status),
		txIDToHeight:    transactionIDToHeight{txID: servicepb.NewHeightFromTxRef(ref)},
	}
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

// newValidatedTxsFromPrepared is what the validator hands the checkpoint verification for
// a batch whose writes all survived validation.
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

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// checkpointTx holds a digest checkpoint or a snapshot abort.
// ref identifies the checkpoint transaction, not the snapshot.
type checkpointTx struct {
	txID             TxID
	ref              *committerpb.TxRef
	snapshotBlockNum uint64
	value            []byte
}

// checkpointVerdict holds the result of verifying a checkpoint transaction:
//   - rejectStatus rejects invalid values or missing/different snapshots, and snapshots
//     already marked CHECKPOINTED or ABORTED.
//   - feedback requests HOLD if the local hash is missing, or HALT if the hashes differ.
//   - Both fields are unset for a matching digest checkpoint or a valid snapshot abort.
//
// Bad input must not halt intake. Otherwise, an authorized submitter could stop the
// committer by naming a block that has no snapshot.
type checkpointVerdict struct {
	rejectStatus committerpb.Status
	feedback     *servicepb.CheckpointFeedback
}

// rejectCheckpointIfNotVerified checks a checkpoint transaction before allowing it to commit.
// A held checkpoint gets feedback, not a saved status that would prevent its retry.
func (d *database) rejectCheckpointIfNotVerified(ctx context.Context, vTx *validatedTransactions) error {
	cp := vTx.checkpoint
	if cp == nil {
		return nil
	}

	// A known transaction must not be rechecked against the current snapshot.
	// Commit distinguishes same-height replays from IDs reused at another height.
	rows, err := d.readStatusWithHeight(ctx, [][]byte{[]byte(cp.txID)})
	if err != nil {
		return fmt.Errorf("failed to read status for checkpoint TX %s: %w", cp.txID, err)
	}
	if len(rows) > 0 {
		return nil // leave duplicate detection and write removal to commit.
	}

	verdict, err := d.verifyCheckpoint(ctx, cp)
	if err != nil {
		return err
	}
	switch {
	case verdict.rejectStatus != committerpb.Status_STATUS_UNSPECIFIED:
		vTx.updateInvalidTxs([]TxID{cp.txID}, verdict.rejectStatus)
	case verdict.feedback != nil:
		delete(vTx.newWrites, cp.txID)
		vTx.checkpoint = nil
		vTx.checkpointFeedback = verdict.feedback
	default:
		// Verified: the checkpoint transaction stays in the batch and commits.
	}
	return nil
}

// verifyCheckpoint checks a digest checkpoint or snapshot abort against the latest snapshot.
// Abort attests to nothing, so it bypasses hash comparison and never produces HOLD/HALT.
// New snapshots are admitted only after the latest lifecycle closes.
func (d *database) verifyCheckpoint(ctx context.Context, cp *checkpointTx) (checkpointVerdict, error) {
	if !servicepb.IsValidCheckpointValue(cp.value) {
		return checkpointVerdict{rejectStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_VALUE}, nil
	}

	state, err := d.snapshotState.ReadLatest(ctx)
	if err != nil {
		return checkpointVerdict{}, err
	}
	rejection := committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY
	if cp.terminalStatus() == committerpb.SnapshotState_ABORTED {
		rejection = committerpb.Status_REJECTED_SNAPSHOT_ABORT_NO_SUCH_SNAPSHOT
	}

	switch {
	case state == nil || state.TxRef == nil:
		logger.Warnf("Rejecting checkpoint TX [%s] for block [%d]: no snapshot record exists to close",
			cp.txID, cp.snapshotBlockNum)
		return checkpointVerdict{rejectStatus: rejection}, nil
	case state.TxRef.BlockNum != cp.snapshotBlockNum:
		logger.Warnf("Rejecting checkpoint TX [%s]: latest snapshot is at block [%d], not [%d]",
			cp.txID, state.TxRef.BlockNum, cp.snapshotBlockNum)
		return checkpointVerdict{rejectStatus: rejection}, nil
	case state.Status == committerpb.SnapshotState_CHECKPOINTED || state.Status == committerpb.SnapshotState_ABORTED:
		logger.Warnf("Rejecting checkpoint TX [%s] for block [%d]: snapshot is already %s",
			cp.txID, cp.snapshotBlockNum, state.Status)
		return checkpointVerdict{rejectStatus: rejection}, nil
	case cp.terminalStatus() == committerpb.SnapshotState_ABORTED:
		return checkpointVerdict{}, nil
	case len(state.Hash) == 0:
		logger.Warnf("Holding checkpoint TX [%s]: local hash for block [%d] is still computing (status %s)",
			cp.txID, cp.snapshotBlockNum, state.Status)
		return checkpointVerdict{feedback: cp.feedback(servicepb.CheckpointFeedback_HOLD, "")}, nil
	case bytes.Equal(state.Hash, cp.value):
		return checkpointVerdict{}, nil
	default:
		return d.haltOnCheckpointDivergence(ctx, cp, state, fmt.Sprintf(
			"local snapshot hash %x for block %d does not match the checkpoint hash %x",
			state.Hash, cp.snapshotBlockNum, cp.value,
		))
	}
}

// haltOnCheckpointDivergence saves the mismatch reason without changing the lifecycle status.
func (d *database) haltOnCheckpointDivergence(
	ctx context.Context, cp *checkpointTx, state *committerpb.SnapshotState, reason string,
) (checkpointVerdict, error) {
	// An abort or checkpoint may commit after we read this snapshot. Saving the
	// diagnostic with the old status could undo that change. ExpectedStatus checks
	// the locked record and refuses the write if its status has changed.
	if err := d.snapshotState.Update(ctx, state.TxRef, statedb.SnapshotUpdate{
		Status: state.Status, ErrMsg: reason,
		ExpectedStatus: []committerpb.SnapshotState_Status{state.Status},
	}); err != nil {
		if errors.Is(err, statedb.ErrUnexpectedSnapshotStatus) {
			// Check the current snapshot again instead of sending an outdated HALT.
			// For example, an aborted snapshot now requires rejection, not HALT.
			return d.verifyCheckpoint(ctx, cp)
		}
		return checkpointVerdict{}, fmt.Errorf(
			"failed to record the checkpoint divergence for block %d: %w", cp.snapshotBlockNum, err,
		)
	}
	logger.Errorf("Halting on checkpoint TX [%s] for block [%d]: %s", cp.txID, cp.snapshotBlockNum, reason)
	return checkpointVerdict{feedback: cp.feedback(servicepb.CheckpointFeedback_HALT, reason)}, nil
}

func (cp *checkpointTx) terminalStatus() committerpb.SnapshotState_Status {
	if string(cp.value) == servicepb.CheckpointAbortValue {
		return committerpb.SnapshotState_ABORTED
	}
	return committerpb.SnapshotState_CHECKPOINTED
}

// feedback identifies the checkpoint and tells the sidecar whether to wait or stop.
func (cp *checkpointTx) feedback(
	signal servicepb.CheckpointFeedback_Signal, reason string,
) *servicepb.CheckpointFeedback {
	return &servicepb.CheckpointFeedback{
		Signal:              signal,
		Ref:                 cp.ref,
		SnapshotBlockNumber: cp.snapshotBlockNum,
		Reason:              reason,
	}
}

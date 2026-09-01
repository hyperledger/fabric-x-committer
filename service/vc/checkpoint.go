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

// checkpointTx holds the checkpoint write to verify.
// ref identifies the checkpoint transaction. blockNum identifies its snapshot.
type checkpointTx struct {
	txID     TxID
	ref      *committerpb.TxRef
	blockNum uint64
	hash     []byte
}

// checkpointVerdict holds the result of checking a checkpoint:
//   - rejectStatus rejects a checkpoint for a missing or different snapshot.
//   - feedback requests HOLD if the local hash is missing, or HALT if the hashes differ.
//   - Both fields are unset when the hashes match.
//
// Bad input must not halt the committer. Otherwise, an authorized submitter could
// stop it by submitting a checkpoint for a block that has no snapshot.
type checkpointVerdict struct {
	rejectStatus committerpb.Status
	feedback     *servicepb.CheckpointFeedback
}

// rejectCheckpointIfNotVerified removes a checkpoint write unless its hash matches
// the local snapshot hash.
//
// A held checkpoint gets feedback instead of a transaction status. Saving a status
// would cause retries with the same transaction ID to be rejected as duplicates.
// The sidecar checks the transaction's format before sending it to the VC.
func (d *database) rejectCheckpointIfNotVerified(
	ctx context.Context, vTx *validatedTransactions,
) error {
	cp, err := checkpointWriteInBatch(vTx)
	if err != nil {
		return err
	}
	if cp == nil {
		return nil
	}

	verdict, err := d.verifyCheckpointHash(ctx, cp)
	if err != nil {
		return err
	}
	switch {
	case verdict.rejectStatus != committerpb.Status_STATUS_UNSPECIFIED:
		vTx.updateInvalidTxs([]TxID{cp.txID}, verdict.rejectStatus)
	case verdict.feedback != nil:
		delete(vTx.newWrites, cp.txID)
		vTx.checkpointFeedback = verdict.feedback
	default:
		// Verified: the checkpoint stays in the batch and commits.
	}
	return nil
}

// checkpointWriteInBatch returns the batch's checkpoint write, if any.
// An invalid key or a second checkpoint fails the batch. Skipping either check
// could leave an unverified checkpoint in the batch to be committed.
func checkpointWriteInBatch(vTx *validatedTransactions) (*checkpointTx, error) {
	var cp *checkpointTx
	for txID, nsWrites := range vTx.newWrites {
		w := nsWrites[committerpb.CheckpointNamespaceID]
		if w.empty() {
			continue
		}
		blockNum, err := servicepb.BlockNumFromCheckpointKey(w.keys[0])
		if err != nil {
			return nil, fmt.Errorf("checkpoint TX %s has an undecodable key: %w", txID, err)
		}
		if cp != nil {
			return nil, errors.Newf(
				"a batch carries at most one checkpoint, but it has both TX %s and TX %s", cp.txID, txID,
			)
		}
		h := vTx.txIDToHeight[txID]
		cp = &checkpointTx{
			txID:     txID,
			ref:      committerpb.NewTxRef(string(txID), h.BlockNum, h.TxNum),
			blockNum: blockNum,
			hash:     w.values[0],
		}
	}
	return cp, nil
}

// verifyCheckpointHash compares the checkpoint hash with the local snapshot hash.
// A zero verdict means the hashes match.
//
// Only the latest snapshot can be waiting for a checkpoint. The VC rejects new
// snapshot requests until that snapshot is CHECKPOINTED.
//
// TODO: add REJECTED_CHECKPOINT_NO_SUCH_SNAPSHOT to fabric-x-common for the two
// rejections below. Until then, use MALFORMED_CHECKPOINT_INVALID_KEY. Its comment
// in fabric-x-common also needs to describe block-number keys instead of TxHeight.
func (d *database) verifyCheckpointHash(
	ctx context.Context, cp *checkpointTx,
) (checkpointVerdict, error) {
	state, err := d.snapshotState.ReadLatest(ctx)
	if err != nil {
		return checkpointVerdict{}, err
	}
	switch {
	case state == nil || state.TxRef == nil:
		logger.Warnf("Rejecting checkpoint TX [%s] for block [%d]: no _snapshot record exists to checkpoint",
			cp.txID, cp.blockNum)
		return checkpointVerdict{rejectStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY}, nil
	case state.TxRef.BlockNum != cp.blockNum:
		logger.Warnf("Rejecting checkpoint TX [%s]: the snapshot awaiting a checkpoint is at block [%d], not [%d]",
			cp.txID, state.TxRef.BlockNum, cp.blockNum)
		return checkpointVerdict{rejectStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY}, nil
	case len(state.Hash) == 0:
		logger.Warnf("Holding checkpoint TX [%s]: the local hash for block [%d] is still computing (status %s)",
			cp.txID, cp.blockNum, state.Status)
		return checkpointVerdict{feedback: cp.feedback(servicepb.CheckpointFeedback_HOLD, "")}, nil
	case bytes.Equal(state.Hash, cp.hash):
		return checkpointVerdict{}, nil // verified: the checkpoint commits with the batch.
	default:
		// The same snapshot has different local and checkpoint hashes. Stop for investigation.
		return d.haltOnCheckpointDivergence(ctx, cp, state, fmt.Sprintf(
			"local snapshot hash %x for block %d does not match the checkpoint hash %x",
			state.Hash, cp.blockNum, cp.hash,
		))
	}
}

// haltOnCheckpointDivergence saves the hash mismatch reason and returns HALT feedback.
// Saving the reason lets an operator inspect it after a restart.
// The snapshot status stays unchanged because there is no HALTED snapshot status.
func (d *database) haltOnCheckpointDivergence(
	ctx context.Context, cp *checkpointTx, state *committerpb.SnapshotState, reason string,
) (checkpointVerdict, error) {
	if err := d.snapshotState.Update(ctx, state.TxRef, statedb.SnapshotUpdate{
		Status: state.Status, ErrMsg: reason,
	}); err != nil {
		return checkpointVerdict{}, fmt.Errorf(
			"failed to record the checkpoint divergence for block %d: %w", cp.blockNum, err,
		)
	}
	logger.Errorf("Halting on checkpoint TX [%s] for block [%d]: %s", cp.txID, cp.blockNum, reason)
	return checkpointVerdict{feedback: cp.feedback(servicepb.CheckpointFeedback_HALT, reason)}, nil
}

// feedback identifies the checkpoint and tells the sidecar whether to wait or stop.
func (cp *checkpointTx) feedback(
	signal servicepb.CheckpointFeedback_Signal, reason string,
) *servicepb.CheckpointFeedback {
	return &servicepb.CheckpointFeedback{
		Signal:              signal,
		Ref:                 cp.ref,
		SnapshotBlockNumber: cp.blockNum,
		Reason:              reason,
	}
}

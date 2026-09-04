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
	"github.com/hyperledger/fabric-x-committer/utils/snapshotstate"
)

// checkpointTx is the single `_checkpoint` write in a batch, decoded into the fields
// the verification needs.
//
// blockNum identifies the snapshot the checkpoint attests to; it is the decoded form
// of key, which is the raw `_checkpoint` row key the commit path writes back. hash is
// the digest the organizations agreed on, taken from the write's value. ref is the
// position of the checkpoint TX itself, which the feedback carries so the coordinator
// can release the TX's dependency-graph node without a per-TX status.
type checkpointTx struct {
	txID     TxID
	ref      *committerpb.TxRef
	blockNum uint64
	key      []byte
	hash     []byte
}

// checkpointVerdict is the outcome of verifying one `_checkpoint` write. At most one
// field is set, and which one encodes who is at fault:
//
//   - rejectStatus: the submitter named a snapshot this committer is not waiting to
//     checkpoint. Bad input, so it is rejected with a per-TX status like any other invalid
//     transaction and the pipeline keeps running.
//   - feedback: local state is behind (HOLD) or contradicts the attestation (HALT). Not
//     the submitter's fault, so it gates block intake instead of failing the transaction.
//   - neither: verified, and the attestation commits with its batch.
//
// The distinction matters because `_checkpoint` is client-submittable (see
// policy.SystemNamespacePolicies): halting on bad input would let any authorized submitter
// stop the committer with a checkpoint for a block that was never snapshotted.
type checkpointVerdict struct {
	rejectStatus committerpb.Status
	feedback     *servicepb.CheckpointFeedback
}

// rejectCheckpointIfNotVerified implements verify-before-commit for a batch carrying a
// `_checkpoint` write: the attestation may only commit once it matches this committer's
// own hash of the referenced snapshot.
//
// Any verdict but a match removes the write, so nothing commits. What replaces it depends
// on the verdict (see checkpointVerdict): a checkpoint for a snapshot this committer is
// not awaiting is rejected with a per-TX status, while local state that is behind or
// contradictory records the feedback the committer forwards to gate block intake.
//
// A held checkpoint deliberately gets no per-TX status. Every status except
// REJECTED_DUPLICATE_TX_ID is persisted to tx_status (see insertTxStatus), and a
// persisted status would make the sidecar's re-submission of the same txID come back as
// a duplicate forever. The feedback carries the txID instead.
//
// The write's form is not re-validated here: the sidecar rejects a `_checkpoint` namespace
// that is not exactly one ReadWrite whose key decodes as a block number
// (checkSystemNamespace), so a malformed one never reaches the VC. Should one arrive
// anyway, checkpointWriteInBatch reports it as an error and this returns it rather than
// letting the write commit unverified.
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
		// Drops the write and sets the status the normal path reports.
		vTx.updateInvalidTxs([]TxID{cp.txID}, verdict.rejectStatus)
	case verdict.feedback != nil:
		delete(vTx.newWrites, cp.txID)
		vTx.checkpointFeedback = verdict.feedback
	default:
		// Verified: the checkpoint stays in the batch and commits.
	}
	return nil
}

// checkpointWriteInBatch returns the batch's `_checkpoint` write, if any.
//
// A `_checkpoint` TX is standalone and carries exactly one versioned ReadWrite with a nil
// version, so the preparer routes it to the new-writes map as one key/value pair. The
// sidecar's form check (checkSystemNamespace) is what guarantees that shape, so this does
// not re-validate it -- it decodes the key it is given.
//
// The two states it cannot proceed on are returned as errors rather than skipped, because
// a skipped write stays in the batch and commits without ever being verified:
//
//   - a key that does not decode to exactly one block number. Only a bug or a caller that
//     bypassed the sidecar's form check produces one, and there is no snapshot to verify
//     it against;
//   - a second `_checkpoint` TX in the same batch. Only one can be verified against the
//     single snapshot awaiting a checkpoint, and newWrites is a map, so returning the first
//     match would pick a nondeterministic winner and commit the rest unverified.
//
// Both are broken invariants rather than bad input, so they fail the batch: the validator
// propagates the error and the VC stops, which is the same treatment MarkCheckpointedInTx
// gives a record that changed underneath a verified checkpoint. Rejecting the TX instead
// would be wrong for the second case -- there is no basis for deciding which attestation
// is the real one.
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
		cp = &checkpointTx{
			txID:     txID,
			ref:      vTx.txIDToHeight[txID].WithTxID(string(txID)),
			blockNum: blockNum,
			key:      w.keys[0],
			hash:     w.values[0],
		}
	}
	return cp, nil
}

// verifyCheckpointHash compares the submitted hash against this committer's own hash of
// the referenced snapshot. A zero verdict means verified, so the checkpoint may commit.
//
// The comparison is against the latest `_snapshot` record rather than a lookup by block
// number: a new snapshot is admitted only once the previous one is CHECKPOINTED (see
// rejectSnapshotIfPriorNotCheckpointed), so the snapshot awaiting a checkpoint is always
// the latest one. A checkpoint naming any other block therefore attests to a snapshot
// this committer is not waiting on, which is a rejection rather than a divergence: the
// submitter chose the block number, and local state is not in question.
//
// TODO: both rejections reuse MALFORMED_CHECKPOINT_INVALID_KEY, whose name describes a
// key that does not decode -- not a well-formed key naming the wrong snapshot. Its godoc
// in fabric-x-common is also stale (it still says "does not decode as a valid TxHeight",
// from before the key became a bare block number). Both want a dedicated
// REJECTED_CHECKPOINT_NO_SUCH_SNAPSHOT status, which has to land in fabric-x-common
// first.
func (d *database) verifyCheckpointHash(
	ctx context.Context, cp *checkpointTx,
) (checkpointVerdict, error) {
	state, err := d.snapshotState.ReadLatest(ctx)
	if err != nil {
		return checkpointVerdict{}, err
	}
	switch {
	case state == nil || state.TxRef == nil:
		// No snapshot was ever accepted, so there is nothing to checkpoint and no record to
		// write a divergence onto. Rejecting rather than halting matters because a
		// client-submittable checkpoint would otherwise stop the committer.
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
		// The organizations attested a hash this committer's own state contradicts. This is
		// the one genuine divergence, and the only case that halts.
		return d.haltOnCheckpointDivergence(ctx, cp, state, fmt.Sprintf(
			"local snapshot hash %x for block %d does not match the checkpoint hash %x",
			state.Hash, cp.blockNum, cp.hash,
		))
	}
}

// haltOnCheckpointDivergence records the divergence on the `_snapshot` record and
// returns the terminal HALT feedback.
//
// The reason is persisted as well as reported because HALT needs operator intervention:
// the pipeline signal is transient, while the record outlives the process that observed
// the divergence. The record's status is carried over unchanged -- a divergence is not a
// lifecycle transition, and there is no status that means "halted".
func (d *database) haltOnCheckpointDivergence(
	ctx context.Context, cp *checkpointTx, state *committerpb.SnapshotState, reason string,
) (checkpointVerdict, error) {
	if err := d.snapshotState.Update(ctx, state.TxRef, snapshotstate.Update{
		Status: state.Status, ErrMsg: reason,
	}); err != nil {
		return checkpointVerdict{}, fmt.Errorf(
			"failed to record the checkpoint divergence for block %d: %w", cp.blockNum, err,
		)
	}
	return checkpointVerdict{feedback: cp.halt(reason)}, nil
}

// halt is the terminal feedback for this checkpoint, which always carries a reason.
func (cp *checkpointTx) halt(reason string) *servicepb.CheckpointFeedback {
	logger.Errorf("Halting on checkpoint TX [%s] for block [%d]: %s", cp.txID, cp.blockNum, reason)
	return cp.feedback(servicepb.CheckpointFeedback_HALT, reason)
}

// feedback builds the coordinator-facing signal for this checkpoint.
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

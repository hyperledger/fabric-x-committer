/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicepb

import (
	"fmt"

	"github.com/hyperledger/fabric-x-committer/utils"
)

// CheckpointKey returns the `_checkpoint` namespace key for a snapshot taken at
// blockNum.
//
// A checkpoint identifies its snapshot by block number alone, not by a block+tx
// [Height]: a snapshot is a cut of committed state, and a block holds at most one
// `_snapshot` TX, so the tx number adds no distinguishing power. The encoding is
// order-preserving so administrators can compare and range-scan checkpoints by block
// number directly on the stored key.
func CheckpointKey(blockNum uint64) []byte {
	return utils.EncodeOrderPreservingVarUint64(blockNum)
}

// BlockNumFromCheckpointKey decodes a `_checkpoint` namespace key into the snapshot's
// block number.
//
// Trailing bytes are rejected rather than ignored: a key that decodes a block number
// and then carries more data is not a checkpoint key, and accepting its prefix would
// silently attribute a checkpoint to the wrong snapshot.
func BlockNumFromCheckpointKey(key []byte) (uint64, error) {
	blockNum, n, err := utils.DecodeOrderPreservingVarUint64(key)
	if err != nil {
		return 0, fmt.Errorf("failed to decode block number from checkpoint key [%v]: %w", key, err)
	}
	if n != len(key) {
		return 0, fmt.Errorf("checkpoint key [%v] has %d trailing bytes after the block number",
			key, len(key)-n)
	}
	return blockNum, nil
}

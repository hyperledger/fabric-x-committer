/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicepb

import (
	"fmt"

	"github.com/hyperledger/fabric-x-committer/utils"
)

// CheckpointKey returns the `_checkpoint` key for a snapshot at blockNum.
// A block has at most one snapshot, so the key needs no transaction number.
// The encoded keys sort in block-number order.
func CheckpointKey(blockNum uint64) []byte {
	return utils.EncodeOrderPreservingVarUint64(blockNum)
}

// BlockNumFromCheckpointKey returns the snapshot block number from a checkpoint key.
// It rejects extra bytes because the key must contain only a block number.
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

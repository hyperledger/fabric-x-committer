/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

type (
	// txIDDedup holds the TX IDs of the blocks that are in flight, so that a TX ID already in
	// flight can be rejected instead of being submitted twice.
	//
	// It is owned exclusively by the relay's preProcessBlock goroutine, which declares it, and
	// therefore needs no synchronization at all: rather than have the status goroutine remove an ID
	// once its status arrives, preProcessBlock evicts a whole block's IDs once it observes that the
	// block has been committed (see evictCommittedBelow). An ID is consequently held for slightly
	// longer than it is strictly in flight: until the last TX of its block is committed rather than
	// until its own status arrives. That only widens the window in which a resubmission of the ID
	// is rejected here, with a status that is not stored in the state DB and so is not notified,
	// rather than by the VC — a window that already exists for a TX whose status has not yet
	// arrived.
	txIDDedup struct {
		ids    map[string]struct{}
		blocks []dedupBlock
	}

	// dedupBlock holds the TX IDs one block contributed to txIDDedup.ids, so they can be evicted
	// together. The blocks form a FIFO ordered by block number.
	dedupBlock struct {
		blockNumber uint64
		txIDs       []string
	}
)

// add records txID as in flight. It returns false if the ID is already in flight, in which case
// the caller must reject the transaction as a duplicate.
func (d *txIDDedup) add(txID string) bool {
	if _, inFlight := d.ids[txID]; inFlight {
		return false
	}
	if d.ids == nil {
		// Keeps the zero value usable for a caller that maps a single block outside the relay,
		// where the dedup set is a throwaway (see appendMissingBlock).
		d.ids = make(map[string]struct{})
	}
	d.ids[txID] = struct{}{}
	return true
}

// trackBlock records the IDs that add accepted for a block, so evictCommittedBelow can release
// them once the block is committed. Blocks must be tracked in increasing block-number order. A
// block that accepted no IDs is tracked too, and evicts as a no-op.
func (d *txIDDedup) trackBlock(blockNumber uint64, txIDs []string) {
	d.blocks = append(d.blocks, dedupBlock{blockNumber: blockNumber, txIDs: txIDs})
}

// evictCommittedBelow releases the IDs of every tracked block below blockNumber. Those blocks have
// been committed, so their TX IDs are no longer in flight.
func (d *txIDDedup) evictCommittedBelow(blockNumber uint64) {
	committed := 0
	for _, blk := range d.blocks {
		if blk.blockNumber >= blockNumber {
			break
		}
		for _, txID := range blk.txIDs {
			delete(d.ids, txID)
		}
		committed++
	}
	d.blocks = d.blocks[committed:]
}

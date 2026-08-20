/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
)

// initialInFlightBlocksCapacity is the ring's first allocation. It must be a power of two; see
// inFlightBlocks.buf.
const initialInFlightBlocksCapacity = 64

type (
	// inFlightBlocks tracks the blocks the relay has submitted to the coordinator and is still
	// waiting for statuses on. Blocks are registered in increasing block-number order and retired
	// strictly in order, so the tracked block numbers always form the contiguous window
	// [nextBlockNum, nextBlockNum+count). A lookup is therefore an index computation rather than a
	// map probe, which is what makes the status path affordable: it looks a block up once per
	// transaction.
	//
	// The window is held in a ring buffer so that a steady stream of blocks neither allocates nor
	// copies: registering and retiring only move count and head. A plain slice appended at the
	// tail and resliced at the head would instead give up one slot of tail capacity per retired
	// block, and so reallocate the whole window every time it had consumed the spare capacity.
	inFlightBlocks struct {
		// nextBlockNum is the number of the block at head, and so also the next block number the
		// relay will emit as committed. It is atomic so that setLastCommittedBlockNumber and the
		// TX ID eviction can read the relay's progress without taking mu.
		nextBlockNum atomic.Uint64
		mu           sync.Mutex
		// buf holds the tracked blocks, and its length is always a power of two so that a slot is
		// a mask rather than a division. The block numbered nextBlockNum+i is at slot(head+i).
		buf   []*blockWithStatus
		head  int
		count int
	}

	// txIDDedup holds the TX IDs of the blocks that are in flight, so that a TX ID already in
	// flight can be rejected instead of being submitted twice.
	//
	// It is owned exclusively by the relay's preProcessBlock goroutine and therefore needs no
	// synchronization at all: rather than have the status goroutine remove an ID once its status
	// arrives, preProcessBlock evicts a whole block's IDs once it observes that the block has been
	// committed (see evictCommitted). An ID is consequently held for slightly longer than it is
	// strictly in flight: until the last TX of its block is committed rather than until its own
	// status arrives. That only widens the window in which a resubmission of the ID is rejected
	// here, with a status that is not stored in the state DB and so is not notified, rather than by
	// the VC — a window that already exists for a TX whose status has not yet arrived.
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

// reset starts tracking from scratch, with nextBlockNum as the next block number to be committed.
func (b *inFlightBlocks) reset(nextBlockNum uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextBlockNum.Store(nextBlockNum)
	b.buf = nil
	b.head = 0
	b.count = 0
}

// nextBlockNumber returns the number of the next block to be committed.
func (b *inFlightBlocks) nextBlockNumber() uint64 {
	return b.nextBlockNum.Load()
}

// register starts tracking blk under blockNumber. It reports a block number that is already
// tracked as alreadyTracked rather than as an error: the segments of a split snapshot block share
// their block's number, so only the first of them registers it. It returns an error for a block
// number that is neither tracked nor the next expected one, which can never occur unless there is
// a bug in the relay, since the tracked block numbers have to stay contiguous.
func (b *inFlightBlocks) register(blockNumber uint64, blk *blockWithStatus) (alreadyTracked bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// A block number below the window underflows to a large offset, and so is rejected too.
	offset := blockNumber - b.nextBlockNum.Load()
	blockCount := uint64(b.count) //nolint:gosec // count is a ring length, never negative.
	switch {
	case offset < blockCount:
		return true, nil
	case offset > blockCount:
		return false, errors.Newf("block %d is not the next block to be tracked [%d]",
			blockNumber, b.nextBlockNum.Load()+blockCount)
	}

	if b.count == len(b.buf) {
		b.grow()
	}
	b.buf[b.slot(b.head+b.count)] = blk
	b.count++
	return false, nil
}

// grow doubles the ring's capacity, re-laying the tracked blocks out from its start. The window is
// bounded by the block channels, so after the first few blocks of a relay run it is never called.
func (b *inFlightBlocks) grow() {
	size := 2 * len(b.buf)
	if size == 0 {
		size = initialInFlightBlocksCapacity
	}
	buf := make([]*blockWithStatus, size)
	for i := range b.count {
		buf[i] = b.buf[b.slot(b.head+i)]
	}
	b.buf = buf
	b.head = 0
}

// get returns the tracked block with the given number, or nil if it is not tracked.
func (b *inFlightBlocks) get(blockNumber uint64) *blockWithStatus {
	b.mu.Lock()
	defer b.mu.Unlock()

	// A block number below the window underflows to a large offset, and so is reported as
	// untracked too.
	offset := blockNumber - b.nextBlockNum.Load()
	if offset >= uint64(b.count) { //nolint:gosec // count is a ring length, never negative.
		return nil
	}
	return b.buf[b.slot(b.head+int(offset))] //nolint:gosec // offset < count, so it fits an int.
}

// first returns the oldest tracked block, the next one to be committed, or nil if no block is
// tracked.
func (b *inFlightBlocks) first() *blockWithStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count == 0 {
		return nil
	}
	return b.buf[b.head]
}

// dropFirst stops tracking the oldest block and advances the next block number to be committed.
// The caller must have taken the block from first, and must hold the relay's committedBlockMu so
// that no other caller drops a block in between.
func (b *inFlightBlocks) dropFirst() {
	b.mu.Lock()
	defer b.mu.Unlock()
	// The dropped block must not stay reachable through the ring slot it vacates.
	b.buf[b.head] = nil
	b.head = b.slot(b.head + 1)
	b.count--
	b.nextBlockNum.Add(1)
}

// slot maps a logical ring position to an index into buf. The caller must hold mu, and buf must be
// non-empty — which it is whenever a block is tracked.
func (b *inFlightBlocks) slot(position int) int {
	return position & (len(b.buf) - 1)
}

// reset drops every tracked TX ID.
func (d *txIDDedup) reset() {
	*d = txIDDedup{}
}

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

// trackBlock records the IDs that add accepted for a block, so evictCommitted can release them
// once the block is committed. Blocks must be tracked in increasing block-number order. A block
// that accepted no IDs is tracked too, and evicts as a no-op.
func (d *txIDDedup) trackBlock(blockNumber uint64, txIDs []string) {
	d.blocks = append(d.blocks, dedupBlock{blockNumber: blockNumber, txIDs: txIDs})
}

// evictCommitted releases the IDs of every tracked block below nextBlockNumber. Those blocks have
// been committed, so their TX IDs are no longer in flight.
func (d *txIDDedup) evictCommitted(nextBlockNumber uint64) {
	committed := 0
	for _, blk := range d.blocks {
		if blk.blockNumber >= nextBlockNumber {
			break
		}
		for _, txID := range blk.txIDs {
			delete(d.ids, txID)
		}
		committed++
	}
	d.blocks = d.blocks[committed:]
}

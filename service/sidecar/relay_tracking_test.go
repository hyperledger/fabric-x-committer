/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

func TestInFlightBlocksWindow(t *testing.T) {
	t.Parallel()

	var blocks inFlightBlocks
	blocks.reset(7)
	require.Equal(t, uint64(7), blocks.nextBlockNumber())
	require.Nil(t, blocks.first())
	require.Nil(t, blocks.get(7))

	tracked := make([]*blockWithStatus, 0, 3)
	for blockNumber := uint64(7); blockNumber < 10; blockNumber++ {
		blk := &blockWithStatus{blockNumber: blockNumber}
		alreadyTracked, err := blocks.register(blockNumber, blk)
		require.NoError(t, err)
		require.False(t, alreadyTracked)
		tracked = append(tracked, blk)
	}

	// Every tracked block is reachable by its number, and nothing outside the window is.
	for i, blk := range tracked {
		require.Same(t, blk, blocks.get(uint64(7+i)))
	}
	require.Nil(t, blocks.get(6))
	require.Nil(t, blocks.get(10))
	require.Same(t, tracked[0], blocks.first())

	// Retiring the oldest block advances the window, so the block number it held becomes untracked.
	blocks.dropFirst()
	require.Equal(t, uint64(8), blocks.nextBlockNumber())
	require.Nil(t, blocks.get(7))
	require.Same(t, tracked[1], blocks.first())
	require.Same(t, tracked[2], blocks.get(9))

	// A block number that is already tracked is reported rather than registered again: the segments
	// of a split snapshot block share their block's number.
	alreadyTracked, err := blocks.register(9, &blockWithStatus{blockNumber: 9})
	require.NoError(t, err)
	require.True(t, alreadyTracked)
	require.Same(t, tracked[2], blocks.get(9))

	// Registering out of order would break the window's contiguity.
	for _, tc := range []struct {
		name        string
		blockNumber uint64
	}{
		{name: "beyond the next expected block", blockNumber: 11},
		{name: "below the window", blockNumber: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := blocks.register(tc.blockNumber, &blockWithStatus{blockNumber: tc.blockNumber})
			require.ErrorContains(t, err, "is not the next block to be tracked [10]")
		})
	}
}

// TestInFlightBlocksRingWrapAround cycles the window at a fixed depth for long enough that head
// wraps the ring repeatedly, which a window shorter than the ring's capacity never does.
func TestInFlightBlocksRingWrapAround(t *testing.T) {
	t.Parallel()

	const depth = 5
	var blocks inFlightBlocks
	blocks.reset(0)
	for blockNumber := range uint64(depth) {
		requireRegister(t, &blocks, blockNumber)
	}

	for blockNumber := uint64(depth); blockNumber < 4*initialInFlightBlocksCapacity; blockNumber++ {
		requireRegister(t, &blocks, blockNumber)
		blocks.dropFirst()
		requireWindow(t, &blocks, blockNumber-depth+1, depth)
	}
}

// TestInFlightBlocksRingGrowsWhileWrapped grows the ring from a window that wraps around its end,
// so growing has to re-lay the blocks out in order rather than copy the ring's slots as they sit.
func TestInFlightBlocksRingGrowsWhileWrapped(t *testing.T) {
	t.Parallel()

	const retired = 10
	var blocks inFlightBlocks
	blocks.reset(0)

	// Fill the ring, retire part of it so head leaves slot 0, then refill so the window wraps.
	for blockNumber := range uint64(initialInFlightBlocksCapacity) {
		requireRegister(t, &blocks, blockNumber)
	}
	for range retired {
		blocks.dropFirst()
	}
	const refilled = initialInFlightBlocksCapacity + retired
	for blockNumber := uint64(initialInFlightBlocksCapacity); blockNumber < refilled; blockNumber++ {
		requireRegister(t, &blocks, blockNumber)
	}
	requireWindow(t, &blocks, retired, initialInFlightBlocksCapacity)

	// The ring is now full, so the next block grows it.
	requireRegister(t, &blocks, initialInFlightBlocksCapacity+retired)
	requireWindow(t, &blocks, retired, initialInFlightBlocksCapacity+1)

	// The grown ring must still wrap and retire correctly.
	for range initialInFlightBlocksCapacity + 1 {
		blocks.dropFirst()
	}
	requireWindow(t, &blocks, initialInFlightBlocksCapacity+retired+1, 0)
}

func TestTxIDDedupEviction(t *testing.T) {
	t.Parallel()

	var dedup txIDDedup
	dedup.reset()

	// Block 0 holds "a" and "b"; "a" repeated within the block is a duplicate.
	require.True(t, dedup.add("a"))
	require.True(t, dedup.add("b"))
	require.False(t, dedup.add("a"))
	dedup.trackBlock(0, []string{"a", "b"})

	// A TX ID in flight in an earlier block is a duplicate in a later one.
	require.False(t, dedup.add("b"))
	require.True(t, dedup.add("c"))
	dedup.trackBlock(1, []string{"c"})

	// Nothing is evicted while both blocks are still in flight.
	dedup.evictCommitted(0)
	require.Len(t, dedup.ids, 3)

	// Committing block 0 releases only its own IDs, and a released ID can be used again.
	dedup.evictCommitted(1)
	require.Equal(t, map[string]struct{}{"c": {}}, dedup.ids)
	require.True(t, dedup.add("a"))
	dedup.trackBlock(2, []string{"a"})

	// A block that contributed no IDs is tracked like any other, and evicts as a no-op.
	dedup.trackBlock(3, nil)
	require.Len(t, dedup.blocks, 3)

	dedup.evictCommitted(4)
	require.Empty(t, dedup.ids)
	require.Empty(t, dedup.blocks)
}

// TestTxIDDedupZeroValue covers the throwaway dedup set used to map a single block outside the
// relay; see appendMissingBlock.
func TestTxIDDedupZeroValue(t *testing.T) {
	t.Parallel()

	var dedup txIDDedup
	require.True(t, dedup.add("a"))
	require.False(t, dedup.add("a"))
}

func TestBlockWithStatusHolds(t *testing.T) {
	t.Parallel()

	blk := &blockWithStatus{
		blockNumber: 4,
		txs: []*servicepb.TxWithRef{
			{Ref: committerpb.NewTxRef("tx-0", 4, 0)},
			{Ref: committerpb.NewTxRef("tx-1", 4, 1)},
		},
	}

	for _, tc := range []struct {
		name     string
		ref      *committerpb.TxRef
		expected bool
	}{
		{name: "ID at its own position", ref: committerpb.NewTxRef("tx-1", 4, 1), expected: true},
		{name: "ID at another TX's position", ref: committerpb.NewTxRef("tx-1", 4, 0), expected: false},
		{name: "unknown ID", ref: committerpb.NewTxRef("tx-2", 4, 1), expected: false},
		{name: "position beyond the block", ref: committerpb.NewTxRef("tx-1", 4, 2), expected: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.expected, blk.holds(tc.ref))
		})
	}
}

// requireRegister registers a block identified by its own number, which requireWindow asserts on.
func requireRegister(t *testing.T, blocks *inFlightBlocks, blockNumber uint64) {
	t.Helper()
	alreadyTracked, err := blocks.register(blockNumber, &blockWithStatus{blockNumber: blockNumber})
	require.NoError(t, err)
	require.False(t, alreadyTracked)
}

// requireWindow asserts that the tracked window is exactly [from, from+count), and that every
// block number in it still resolves to the block registered for it.
func requireWindow(t *testing.T, blocks *inFlightBlocks, from, count uint64) {
	t.Helper()
	require.Equal(t, from, blocks.nextBlockNumber())
	for blockNumber := from; blockNumber < from+count; blockNumber++ {
		blk := blocks.get(blockNumber)
		require.NotNil(t, blk, "block %d must be tracked", blockNumber)
		require.Equal(t, blockNumber, blk.blockNumber)
	}

	// Nothing outside the window is tracked. from-1 underflows when from is 0, which is still a
	// block number the window does not hold.
	require.Nil(t, blocks.get(from-1))
	require.Nil(t, blocks.get(from+count))
	if count == 0 {
		require.Nil(t, blocks.first())
		return
	}
	require.Equal(t, from, blocks.first().blockNumber)
}

// TestInFlightBlocksSteadyStateDoesNotAllocate pins the reason the window is a ring and not a slice
// resliced at its head: once the ring is large enough, a block's whole round trip through it -- and
// the per-transaction lookups in between -- allocate nothing.
//
//nolint:paralleltest // testing.AllocsPerRun pins GOMAXPROCS, so it panics in a parallel test.
func TestInFlightBlocksSteadyStateDoesNotAllocate(t *testing.T) {
	const depth = 5
	var blocks inFlightBlocks
	blocks.reset(0)
	for blockNumber := range uint64(depth) {
		requireRegister(t, &blocks, blockNumber)
	}

	// The block registered is the same one every time, so the only allocation a run could make is
	// the ring's own.
	tracked := &blockWithStatus{}
	blockNumber := uint64(depth)
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := blocks.register(blockNumber, tracked); err != nil {
			t.Error(err)
		}
		for range 10 {
			if blocks.get(blockNumber) == nil || blocks.first() == nil {
				t.Error("the registered block must be tracked")
			}
		}
		blocks.dropFirst()
		blockNumber++
	})
	require.Zero(t, allocs)
}

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTxIDDedupEviction(t *testing.T) {
	t.Parallel()

	var dedup txIDDedup

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
	dedup.evictCommittedBelow(0)
	require.Len(t, dedup.ids, 3)

	// Committing block 0 releases only its own IDs, and a released ID can be used again.
	dedup.evictCommittedBelow(1)
	require.Equal(t, map[string]struct{}{"c": {}}, dedup.ids)
	require.True(t, dedup.add("a"))
	dedup.trackBlock(2, []string{"a"})

	// A block that contributed no IDs is tracked like any other, and evicts as a no-op.
	dedup.trackBlock(3, nil)
	require.Len(t, dedup.blocks, 3)

	dedup.evictCommittedBelow(4)
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

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import "sync/atomic"

// TxCounter is the shared transaction-index counter. All workers draw their index ranges from this one
// counter, which is what makes the generated transaction multiset independent of the worker count. It also
// holds the profile so KeyStats can report the key-generation counts without reaching into the stream.
type TxCounter struct {
	profile TransactionProfile
	counter atomic.Uint64
}

// NewTxCounter creates the shared transaction counter.
func NewTxCounter(profile TransactionProfile) *TxCounter {
	return &TxCounter{profile: profile}
}

// reserve reserves n consecutive transaction indices with a single atomic increment and returns the base
// index of the reserved range: the caller owns [base, base+n).
func (c *TxCounter) reserve(n uint64) uint64 {
	return c.counter.Add(n) - n
}

// KeyStats is a cumulative, monotone snapshot of the workload's key-generation counts, so its fields can
// back counter metrics directly.
type KeyStats struct {
	KeyFrontier         uint64 // keys introduced so far, including ones read or written but never committed
	ReferencedReadKeys  uint64 // read-only slots that reused an existing key
	ReferencedWriteKeys uint64 // write slots that reused an existing key
}

// KeyStats computes the current key-generation counts from the counter value.
func (c *TxCounter) KeyStats() KeyStats {
	p := c.profile
	n := c.counter.Load()
	w := uint64(p.ReadWriteCount) + uint64(p.BlindWriteCount)
	slotsPerTx := w + uint64(p.ReadOnlyCount)
	// Each tx introduces slotsPerTx - KeyBackrefRate new keys on average; the rest of its slots reuse keys.
	newKeysRate := float64(slotsPerTx) - p.KeyBackrefRate
	created := uint64(max(0, committedFrontier(n, newKeysRate)))
	// New keys fill write slots first; any surplus falls on read-only slots. Clamping at n*w keeps the two
	// reference counts below from underflowing when new keys outnumber the write slots.
	writeCreates := min(n*w, created)
	return KeyStats{
		KeyFrontier:         created,
		ReferencedReadKeys:  n*uint64(p.ReadOnlyCount) - (created - writeCreates),
		ReferencedWriteKeys: n*w - writeCreates,
	}
}

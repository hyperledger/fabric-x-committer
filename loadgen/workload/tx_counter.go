/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import "sync/atomic"

// TxCounter is the shared global transaction-index counter. Every worker generator reserves its
// consecutive range of indices from this one counter (a single atomic increment per batch); sharing the
// counter is what makes the generated multiset worker-count invariant (workers are pure parallelism). It
// is created before the stream and shared with it; it depends only on the TransactionProfile, not on the
// policy or the crypto artifacts.
type TxCounter struct {
	profile TransactionProfile
	counter atomic.Uint64
}

// NewTxCounter creates the shared transaction counter for the given transaction profile.
func NewTxCounter(profile TransactionProfile) *TxCounter {
	return &TxCounter{profile: profile}
}

// reserve reserves n consecutive transaction indices with a single atomic increment and returns the base
// index of the reserved range: the caller owns [base, base+n).
func (c *TxCounter) reserve(n uint64) uint64 {
	return c.counter.Add(n) - n
}

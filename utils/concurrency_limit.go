/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"context"
	"sync/atomic"
)

// ConcurrencyLimiter tracks active counter with an optional max limit.
// If limit <= 0, the counter is unbounded.
type ConcurrencyLimiter struct {
	limit        int64
	currentCount atomic.Int64
}

// NewConcurrencyLimiter creates a limiter with the given max limit.
func NewConcurrencyLimiter(limit int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{limit: int64(limit)}
}

// TryAcquire increments the active counter by 1 if capacity is available.
// Returns true if the slot was acquired, false if the limit has been reached or the context is done.
func (c *ConcurrencyLimiter) TryAcquire(ctx context.Context) bool {
	if c.limit <= 0 {
		return true
	}

	for ctx.Err() == nil {
		current := c.currentCount.Load()
		if current >= c.limit {
			return false
		}
		if c.currentCount.CompareAndSwap(current, current+1) {
			return true
		}
	}
	return false
}

// Release decrements the active counter by 1.
func (c *ConcurrencyLimiter) Release() {
	c.currentCount.Add(-1)
}

// Load returns the current active count.
func (c *ConcurrencyLimiter) Load() int64 {
	return c.currentCount.Load()
}

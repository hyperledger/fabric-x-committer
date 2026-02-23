/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrConcurrencyLimitReached indicates a bounded counter has reached its max capacity.
var ErrConcurrencyLimitReached = errors.New("counter limit reached")

// ConcurrencyLimiter tracks active counter with an optional max limit.
// If limit <= 0, the counter is unbounded.
type ConcurrencyLimiter struct {
	limit        atomic.Int64
	currentCount atomic.Int64
}

// NewConcurrencyLimiter creates a limiter with the given max limit.
func NewConcurrencyLimiter(limit int) *ConcurrencyLimiter {
	c := &ConcurrencyLimiter{}
	c.limit.Store(int64(limit))
	return c
}

// SetLimit updates the counter limit.
func (c *ConcurrencyLimiter) SetLimit(limit int) {
	c.limit.Store(int64(limit))
}

// TryAcquire increments the active counter by 1 if capacity is available.
func (c *ConcurrencyLimiter) TryAcquire(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		limit := c.limit.Load()
		if limit <= 0 {
			c.currentCount.Add(1)
			return nil
		}

		current := c.currentCount.Load()
		if current >= limit {
			return ErrConcurrencyLimitReached
		}
		if c.currentCount.CompareAndSwap(current, current+1) {
			return nil
		}
	}
}

// Release decrements the active counter by 1.
func (c *ConcurrencyLimiter) Release() {
	c.currentCount.Add(-1)
}

// Load returns the current active count.
func (c *ConcurrencyLimiter) Load() int64 {
	return c.currentCount.Load()
}

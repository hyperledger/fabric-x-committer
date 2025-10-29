/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"context"
	"sync"
	"sync/atomic"
)

// Slots manages a resource counter, allowing callers to block until at least
// one slot is available.
type Slots struct {
	available atomic.Int64
	cond      *sync.Cond
	size      int64
}

// NewSlots creates a new Slots instance with the specified initial size.
func NewSlots(size int64) *Slots {
	w := &Slots{
		cond: sync.NewCond(&sync.Mutex{}),
		size: size,
	}
	w.available.Store(size)
	return w
}

// Acquire blocks until at least one slot is available or the context is canceled.
// It then reduces the available slot count by n.
func (w *Slots) Acquire(ctx context.Context, n int64) {
	w.cond.L.Lock()
	defer w.cond.L.Unlock()
	for w.available.Load() < 1 && ctx.Err() == nil {
		w.cond.Wait()
	}
	w.available.Add(-n)
}

// Release adds 'n' slots back to the pool and signals a waiting goroutine.
func (w *Slots) Release(n int64) {
	w.cond.L.Lock()
	defer w.cond.L.Unlock()
	if w.available.Add(n) > 0 {
		w.cond.Broadcast()
	}
}

// Broadcast wakes up all goroutines waiting to acquire slots.
func (w *Slots) Broadcast() {
	w.cond.Broadcast()
}

// WaitTillEmpty waits till all available slots are empty.
func (w *Slots) WaitTillEmpty(ctx context.Context) {
	w.cond.L.Lock()
	defer w.cond.L.Unlock()
	for w.available.Load() < w.size && ctx.Err() == nil {
		w.cond.Wait()
	}
}

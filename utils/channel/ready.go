/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package channel

import (
	"context"
	"sync"
	"sync/atomic"
)

type (
	// Ready supports waiting for readiness and notifying of readiness.
	// It also supports closing to release waiters.
	// Ready uses atomic pointer indirection to allow safe Reset() while other
	// goroutines may be waiting on the channels. This prevents race conditions
	// when the internal state is replaced.
	Ready struct {
		ready atomic.Pointer[ready]
	}
	ready struct {
		ready  chan any
		closed chan any
		once   sync.Once
	}
)

// NewReady instantiate a new Ready.
func NewReady() *Ready {
	r := &Ready{}
	r.ready.Store(newReady())
	return r
}

func newReady() *ready {
	return &ready{
		ready:  make(chan any),
		closed: make(chan any),
	}
}

// Reset resets the object to be reused.
func (r *Ready) Reset() {
	rr := r.ready.Load()
	rr.once.Do(func() {
		close(rr.closed)
	})
	// We only set a new internal ready to replace the one we closed.
	// If another process already replaced it, we can continue.
	r.ready.CompareAndSwap(rr, newReady())
}

// SignalReady signals readiness.
func (r *Ready) SignalReady() {
	rr := r.ready.Load()
	rr.once.Do(func() {
		close(rr.ready)
	})
}

// Close notifies of closing.
func (r *Ready) Close() {
	rr := r.ready.Load()
	rr.once.Do(func() {
		close(rr.closed)
	})
}

// WaitForReady returns true if the object is ready,
// or false if it is closed or the context ended before that.
func (r *Ready) WaitForReady(ctx context.Context) bool {
	return WaitForAllReady(ctx, r)
}

// WaitForAllReady returns true if all objects are ready,
// or false if one of them is closed or the context ended before that.
func WaitForAllReady(ctx context.Context, ready ...*Ready) bool {
	for _, r := range ready {
		rr := r.ready.Load()
		select {
		case <-ctx.Done():
			return false
		case <-rr.closed:
			return false
		case <-rr.ready:
		}
	}
	return true
}

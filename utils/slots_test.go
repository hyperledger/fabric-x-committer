/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSlotsAcquireAndRelease(t *testing.T) {
	t.Parallel()

	w := NewSlots(10)
	require.Equal(t, int64(10), w.available.Load())

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	t.Log("acquire 5 slots")
	w.Acquire(ctx, 5)
	require.Equal(t, int64(5), w.Load(t))

	t.Log("acquire 5 more slots")
	w.Acquire(ctx, 5)
	require.Equal(t, int64(0), w.Load(t))

	t.Log("acquire 10 slots in background but available is < 1")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Acquire(ctx, 10)
	}()

	require.Never(t, func() bool {
		return w.Load(t) < 0
	}, 1*time.Second, 500*time.Millisecond)

	t.Log("release 3 slots")
	w.Release(3)

	wg.Wait()
	require.Equal(t, int64(-7), w.Load(t))

	started := make(chan any)
	done := make(chan any)
	go func() {
		close(started)
		w.WaitTillEmpty(ctx)
		close(done)
	}()

	<-started
	time.Sleep(1 * time.Second)

	select {
	case <-done:
		t.Fatal("WaitTillEmpty did not block when slots are non-empty")
	default:
		t.Log("WaitTillEmpty is correctly blocking")
	}

	w.Release(17)

	select {
	case <-done:
		t.Log("WaitTillEmpty correctly unblocked after Release")
	case <-time.After(2 * time.Second):
		t.Fatal("WaitTillEmpty did not unblock after Release")
	}
}

func TestSlotsMultipleWaiters(t *testing.T) {
	t.Parallel()
	w := NewSlots(0)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	numGoRoutines := 5
	wg.Add(numGoRoutines)
	waitingCount := int32(0)

	for range numGoRoutines {
		go func() {
			defer wg.Done()
			defer atomic.AddInt32(&waitingCount, -1)
			atomic.AddInt32(&waitingCount, 1)
			w.Acquire(ctx, 1)
		}()
	}

	require.Never(t, func() bool {
		return w.Load(t) != 0
	}, 1*time.Second, 500*time.Millisecond)

	ensureWaitingCount := func(count int32) {
		require.Eventually(t, func() bool {
			return atomic.LoadInt32(&waitingCount) == count
		}, 2*time.Second, 50*time.Millisecond)
	}

	for i := range numGoRoutines {
		ensureWaitingCount(int32(numGoRoutines - i)) //nolint:gosec //int->int32
		t.Log("Releasing 1 slot, which should unblock 1 waiter.")
		w.Release(1)
	}

	wg.Wait()

	t.Log("All goroutines finished.")
	require.Equal(t, int64(0), w.Load(t))
}

func TestSlotBroadcast(t *testing.T) {
	t.Parallel()
	w := NewSlots(0)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	var wg sync.WaitGroup
	numGoRoutines := 3
	wg.Add(numGoRoutines)
	for range numGoRoutines {
		go func() {
			defer wg.Done()
			w.Acquire(ctx, 1)
		}()
	}

	time.Sleep(100 * time.Millisecond)
	require.Zero(t, w.Load(t))

	w.Store(t, int64(numGoRoutines))
	w.Broadcast()

	wg.Wait()
}
